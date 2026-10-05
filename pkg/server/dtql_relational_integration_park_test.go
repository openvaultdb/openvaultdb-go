package server_test

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
	sqlite "modernc.org/sqlite"
)

// A read that is in flight at a point the test chooses, without a sleep: the mount
// below holds the collection Country with a generated column, name, that is computed
// by an SQL function of the SQLite driver, and while a gate is armed that function
// stops at the first row it is asked for until the test lets it go. The read is a real
// read of a real SQLite file, running inside the executor, so what a test shows
// about a request that is parked there (the slot of the concurrency gate it holds,
// the lease of the mount it holds) is what holds of a request that is slow.

// relIntParkGate is one armed gate: entered is closed when a read reaches the
// function, and the reads go on when release is closed.
type relIntParkGate struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func (g *relIntParkGate) open() { g.releaseOnce.Do(func() { close(g.release) }) }

var (
	relIntParkRegistered sync.Once
	relIntParkCurrent    atomic.Pointer[relIntParkGate]
)

// relIntArmPark arms a gate for the rest of the test; the test ends with it open.
// Call it after the servers of the test are created, so that the cleanups run in the
// order that lets in-flight requests finish before a server is closed.
func relIntArmPark(t *testing.T) *relIntParkGate {
	t.Helper()
	gate := &relIntParkGate{entered: make(chan struct{}), release: make(chan struct{})}
	relIntParkCurrent.Store(gate)
	t.Cleanup(func() {
		gate.open()
		relIntParkCurrent.CompareAndSwap(gate, nil)
	})
	return gate
}

// relIntCountryNames are the names of the four countries of the customers.
var relIntCountryNames = map[string]string{"UK": "United Kingdom", "US": "United States", "NL": "Netherlands", "IE": "Ireland"}

// relIntParkMount is a SQLite mount holding the ten customers of the fact tables and
// the four countries they live in; the generated column name of Country parks while a
// gate is armed.
func relIntParkMount(t *testing.T, id string) *core.Database {
	t.Helper()
	relIntParkRegistered.Do(func() {
		// SQLite computes a generated column with deterministic functions only.
		sqlite.MustRegisterDeterministicScalarFunction("ovdb_test_park", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			if gate := relIntParkCurrent.Load(); gate != nil {
				gate.enteredOnce.Do(func() { close(gate.entered) })
				<-gate.release
			}
			return args[0], nil
		})
	})
	statements := append(relIntCustomerStatements(),
		`CREATE TABLE "Country" ("id" TEXT PRIMARY KEY, "code" TEXT, "label" TEXT, "name" TEXT GENERATED ALWAYS AS (ovdb_test_park("label")) VIRTUAL)`)
	for i, code := range relIntCountries {
		statements = append(statements, fmt.Sprintf(`INSERT INTO "Country" ("id", "code", "label") VALUES ('%d', '%s', '%s')`, i+1, code, relIntCountryNames[code]))
	}
	return relHTTPMount(t, id, "", map[string][]string{"Customer": {"id", "name", "country"}, "Country": {"id", "code", "name"}}, statements...)
}

// relIntAnswer is a response read off the goroutine that asked for it.
type relIntAnswer struct {
	resp relHTTPResponse
	err  error
}

// relIntAsyncPost posts a document from a goroutine and delivers the answer on the
// channel it returns.
func relIntAsyncPost(base, path, token, doc string) <-chan relIntAnswer {
	out := make(chan relIntAnswer, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(doc))
		if err != nil {
			out <- relIntAnswer{err: err}
			return
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- relIntAnswer{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		answer := relIntAnswer{resp: relHTTPResponse{status: resp.StatusCode, header: resp.Header, raw: string(raw)}, err: err}
		_ = json.Unmarshal(raw, &answer.resp.body)
		out <- answer
	}()
	return out
}

// relIntFailer is what relIntAwaitParked reports a failure to: a *testing.T, or a
// recorder in the test of the helper itself.
type relIntFailer interface {
	Helper()
	Fatalf(format string, args ...any)
}

// relIntAwaitParked waits until a read of the parked mount has stopped at the gate.
// It selects on the answer of the request too: a request that is answered before its
// read parks (a refusal, an error) never reaches the gate, and nothing else would ever
// close it, so the test fails at once with the answer the request got instead of
// waiting for the end of the test run.
func relIntAwaitParked(t relIntFailer, gate *relIntParkGate, answer <-chan relIntAnswer) {
	t.Helper()
	select {
	case <-gate.entered:
	case got := <-answer:
		t.Fatalf("the request was answered before its read parked: error %v, status %d: %s", got.err, got.resp.status, got.resp.raw)
	}
}

// Documents of the parked mount. relIntParkedJoin reads Country inside the mount, so
// it parks on the database route (the whole join is one statement of SQLite);
// relIntParkedAcross reads it from another mount and parks on the in-memory one. Both
// select the key column of the first source unaliased, which the SQLite adapter needs
// to compile a join, so that the first document is not read again in memory.
const (
	relIntParkedJoin = `from:
  name: Customer
  alias: c
  joins:
    - type: inner
      from: {name: Country, alias: k}
      on:
        - {left: {field: country, source: c}, op: '==', right: {field: code, source: k}}
orderBy:
  - {field: name, source: c}
columns:
  - {field: id, source: c}
  - {field: name, source: c, as: customer}
  - {field: name, source: k, as: country}
`
	relIntParkedAcross = `from:
  database: chinook
  name: Customer
  alias: c
  joins:
    - type: inner
      from: {database: slow, name: Country, alias: k}
      on:
        - {left: {field: country, source: c}, op: '==', right: {field: code, source: k}}
orderBy:
  - {field: name, source: c}
columns:
  - {field: id, source: c}
  - {field: name, source: c, as: customer}
  - {field: name, source: k, as: country}
`
	// relIntSameCountryAsC1 reads Customer twice and no Country, so it runs while a
	// read of Country is parked; the subquery keeps it in memory.
	relIntSameCountryAsC1 = `from: {database: chinook, name: Customer, alias: c}
where:
  op: In
  left: {field: country, source: c}
  right:
    query:
      from: {database: chinook, name: Customer, alias: d}
      where: {op: '==', left: {field: id, source: d}, right: {value: c1}}
      columns: [{field: country, source: d}]
orderBy: [{field: id, source: c}]
columns: [{field: id, source: c}]
`
)

// relIntParkedRows are the rows both parked documents return: the ten customers by
// name, each with the name of its country.
func relIntParkedRows() []map[string]any {
	rows := make([]map[string]any, 10)
	for n := range rows {
		rows[n] = map[string]any{"id": fmt.Sprintf("c%d", n), "customer": fmt.Sprintf("Customer %d", n), "country": relIntCountryNames[relIntCountries[n%4]]}
	}
	return rows
}

// relIntAnswered waits for the answer of a parked request, which the test has let go,
// and fails unless it is the whole result.
func relIntAnswered(t *testing.T, what string, answer <-chan relIntAnswer) {
	t.Helper()
	got := <-answer
	if got.err != nil {
		t.Fatalf("%s: %v", what, got.err)
	}
	if got.resp.status != http.StatusOK {
		t.Fatalf("%s: status %d: %s", what, got.resp.status, got.resp.raw)
	}
	if rows := got.resp.rows(t); !reflect.DeepEqual(rows, relIntParkedRows()) {
		t.Fatalf("%s: rows = %v, want %v", what, rows, relIntParkedRows())
	}
}

// relIntCheckCapacity runs the two cases of the concurrency gate against a server that
// holds the mounts chinook (relIntShop) and slow (relIntParkMount) and allows one
// query at a time on each route, refusing the next at once. On each route a query
// that is parked inside its read holds the only slot; the next query of that route
// is a 503 query_capacity with a Retry-After and no rows, the instance still answers,
// the other route has a slot of its own, and when the parked query has been let go
// it finishes with its whole result and a later query is served. token is the bearer
// token the requests carry, or empty.
func relIntCheckCapacity(t *testing.T, base, token string) {
	t.Helper()
	for _, route := range []struct {
		name, label, path, doc string
		otherPath, otherDoc    string
		otherLabel             string
		otherIDs               []any
	}{
		{"the in-memory route", "in-memory", "/v1/dtql", relIntParkedAcross, "/v1/databases/chinook/dtql", relIntRevenue("", "", true), "database", nil},
		{"the database route", "database", "/v1/databases/slow/dtql", relIntParkedJoin, "/v1/dtql", relIntSameCountryAsC1, "in-memory", []any{"c1", "c5", "c9"}},
	} {
		t.Run(route.name, func(t *testing.T) {
			gate := relIntArmPark(t)
			parked := relIntAsyncPost(base, route.path, token, route.doc)
			relIntAwaitParked(t, gate, parked)

			refused := relHTTPDo(t, base, http.MethodPost, route.path, token, route.doc, nil)
			if refused.status != http.StatusServiceUnavailable || refused.errorField("code") != "query_capacity" || refused.header.Get("Retry-After") == "" {
				t.Fatalf("status %d, Retry-After %q, want a 503 query_capacity with one: %s", refused.status, refused.header.Get("Retry-After"), refused.raw)
			}
			if refused.body["records"] != nil {
				t.Fatalf("a refused query returned rows: %s", refused.raw)
			}
			if status := relHTTPDo(t, base, http.MethodGet, "/.well-known/openvaultdb", "", "", nil); status.status != http.StatusOK {
				t.Fatalf("the server does not answer while a query is parked: %d", status.status)
			}
			other := relHTTPDo(t, base, http.MethodPost, route.otherPath, token, route.otherDoc, nil)
			if other.status != http.StatusOK || other.execution(t)["route"] != route.otherLabel {
				t.Fatalf("the other route: status %d: %s", other.status, other.raw)
			}
			if route.otherIDs != nil && !reflect.DeepEqual(relHTTPNames(other.rows(t), "id"), route.otherIDs) {
				t.Fatalf("the other route: rows = %v", other.rows(t))
			}
			gate.open()
			relIntAnswered(t, "the parked query", parked)

			later := relHTTPDo(t, base, http.MethodPost, route.path, token, route.doc, nil)
			if later.status != http.StatusOK || later.execution(t)["route"] != route.label {
				t.Fatalf("after the slot is free: status %d: %s", later.status, later.raw)
			}
		})
	}
}

// A query that finds the slots of its route taken is refused with a 503 and a
// Retry-After, on the in-memory route and on the database route, while the instance
// and the other route go on serving.
func TestAQueryThatFindsItsRouteFullIsRefusedWithRetryAfter(t *testing.T) {
	// A negative queue wait refuses a query at once instead of queueing it.
	base := relIntServe(t, map[string]*core.Database{"chinook": relIntShop(t, "chinook", ""), "slow": relIntParkMount(t, "slow")},
		server.WithQueryLimits(server.QueryLimits{InMemory: 1, Database: 1, QueueWait: -1}))
	relIntCheckCapacity(t, base, "")
}

// A cross-database query that is reading a mount when the mount is unmounted
// completes with its whole result, and the unmount finishes only after it: the mount
// leaves the routing table at once (a request for it is a 404), the running query
// holds it open (it is not closed), and once the query has answered the mount is
// closed and Unmount returns.
func TestUnmountingAMountWhileAQueryReadsItWaitsForTheQuery(t *testing.T) {
	slow := relIntParkMount(t, "slow")
	var closed atomic.Bool
	slow.OnClose(func() error { closed.Store(true); return nil })
	service := server.New("test", map[string]*core.Database{"chinook": relIntShop(t, "chinook", ""), "slow": slow})
	t.Cleanup(service.CloseSnapshots)
	host := httptest.NewServer(service.Handler())
	t.Cleanup(host.Close)
	gate := relIntArmPark(t)

	running := relIntAsyncPost(host.URL, "/v1/dtql", "", relIntParkedAcross)
	relIntAwaitParked(t, gate, running)

	unmounted := make(chan error, 1)
	go func() { unmounted <- service.Unmount("slow") }()
	// Unmount takes the mount out of the routing table before it waits, so the
	// first 404 shows that it has begun.
	for tries := 0; ; tries++ {
		if tries == 1_000_000 {
			t.Fatal("the mount is still routed after a million requests")
		}
		if relHTTPDo(t, host.URL, http.MethodGet, "/v1/databases/slow", "", "", nil).status == http.StatusNotFound {
			break
		}
		runtime.Gosched()
	}
	if resp := relHTTPPost(t, host.URL, "/v1/dtql", "", relIntParkedAcross); resp.status != http.StatusNotFound || resp.errorField("code") != "not_found" {
		t.Fatalf("a new query that names the mount: status %d: %s", resp.status, resp.raw)
	}
	select {
	case err := <-unmounted:
		t.Fatalf("Unmount returned (%v) while a query was reading the mount", err)
	default:
	}
	if closed.Load() {
		t.Fatal("the mount was closed while a query was reading it")
	}

	gate.open()
	relIntAnswered(t, "the query that was running", running)
	if err := <-unmounted; err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if !closed.Load() {
		t.Fatal("Unmount returned and the mount is not closed")
	}
}

// relIntRecorder is a relIntFailer that records what it is told instead of ending the
// test.
type relIntRecorder struct{ failures []string }

func (*relIntRecorder) Helper() {}

func (r *relIntRecorder) Fatalf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// A request that is answered before its read parks fails the test with its answer,
// where waiting on the gate alone would hang until the test run times out; a read that
// parks is waited for as before.
func TestAParkedRequestThatIsAnsweredEarlyFailsTheTestInsteadOfHangingIt(t *testing.T) {
	newGate := func() *relIntParkGate {
		return &relIntParkGate{entered: make(chan struct{}), release: make(chan struct{})}
	}

	t.Run("answered before the read parks", func(t *testing.T) {
		answer := make(chan relIntAnswer, 1)
		answer <- relIntAnswer{resp: relHTTPResponse{status: http.StatusServiceUnavailable, raw: `{"error":{"code":"query_capacity"}}`}}
		recorder := &relIntRecorder{}
		relIntAwaitParked(recorder, newGate(), answer)
		if len(recorder.failures) != 1 || !strings.Contains(recorder.failures[0], "answered before its read parked") ||
			!strings.Contains(recorder.failures[0], "503") || !strings.Contains(recorder.failures[0], "query_capacity") {
			t.Fatalf("failures = %q, want one that carries the answer", recorder.failures)
		}
	})

	t.Run("answered with an error of the transport", func(t *testing.T) {
		answer := make(chan relIntAnswer, 1)
		answer <- relIntAnswer{err: io.ErrUnexpectedEOF}
		recorder := &relIntRecorder{}
		relIntAwaitParked(recorder, newGate(), answer)
		if len(recorder.failures) != 1 || !strings.Contains(recorder.failures[0], io.ErrUnexpectedEOF.Error()) {
			t.Fatalf("failures = %q", recorder.failures)
		}
	})

	t.Run("the read parks", func(t *testing.T) {
		gate := newGate()
		close(gate.entered)
		recorder := &relIntRecorder{}
		relIntAwaitParked(recorder, gate, make(chan relIntAnswer))
		if len(recorder.failures) != 0 {
			t.Fatalf("failures = %q, want none for a read that parked", recorder.failures)
		}
	})
}
