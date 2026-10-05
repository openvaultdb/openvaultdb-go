package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dtql"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	_ "modernc.org/sqlite"
)

// The tests of the relational handler that need no database: the executor is a
// fake, so every status the handler maps can be driven, and the mounts are
// structs that only answer the questions the handler asks of them. The helpers
// of this file all start with relFake so they cannot clash with the others of
// the package.

// relFakeCall is what the handler handed the executor.
type relFakeCall struct {
	query           dal.StructuredQuery
	profile         joinexec.Profile
	defaultDatabase string
	registry        joinexec.Registry
	authorize       joinexec.Authorize
	limits          joinexec.Limits
	options         int
}

// relFakeExecutor stands in for joinexec.Execute.
type relFakeExecutor struct {
	mu     sync.Mutex
	calls  []relFakeCall
	result joinexec.Result
	err    error
	// block, when set, is called inside the execution, after the call is recorded.
	block func()
}

func (f *relFakeExecutor) execute(_ context.Context, query dal.StructuredQuery, profile joinexec.Profile, defaultDatabase string, registry joinexec.Registry, authorize joinexec.Authorize, limits joinexec.Limits, opts ...joinexec.Option) (joinexec.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, relFakeCall{query: query, profile: profile, defaultDatabase: defaultDatabase, registry: registry, authorize: authorize, limits: limits, options: len(opts)})
	block := f.block
	f.mu.Unlock()
	if block != nil {
		block()
	}
	return f.result, f.err
}

func (f *relFakeExecutor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *relFakeExecutor) only(t *testing.T) relFakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 {
		t.Fatalf("the executor was called %d times, want 1", len(f.calls))
	}
	return f.calls[0]
}

// relFakeDB is a driver that is never reached: the executor is a fake, and the
// handler asks the mounts only for their id, engine, cache time, declared
// collections and whether they have policies.
type relFakeDB struct{ dal.DB }

// relFakeMount is a mount of engine that declares the collections orders and
// customers and holds no data.
func relFakeMount(id, engine, cacheTTL string) *core.Database {
	return relFakeOpen(&manifest.Manifest{
		Database: manifest.Database{ID: id, CacheTTL: cacheTTL, SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
	})
}

func relFakeOpen(m *manifest.Manifest) *core.Database {
	m.Schemas = &schema.Schemas{Collections: map[string]schema.Collection{"orders": {}, "customers": {}}}
	db, err := core.Open(m, relFakeDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		panic(err)
	}
	return db
}

// relFakeGitHubMount is an inGitDB mount backed by GitHub.
func relFakeGitHubMount(id string) *core.Database {
	return relFakeOpen(&manifest.Manifest{
		Database: manifest.Database{ID: id, SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "ingitdb", InGitDB: &manifest.InGitDBOptions{GitHub: &manifest.InGitDBGitHubOptions{Owner: "o", Repo: "r"}}},
	})
}

// relFakeServer serves mounts with the fake executor in place of
// joinexec.Execute.
func relFakeServer(t *testing.T, fake *relFakeExecutor, mounts []*core.Database, opts ...Option) (*Server, *httptest.Server) {
	t.Helper()
	dbs := map[string]*core.Database{}
	for _, db := range mounts {
		dbs[db.ID()] = db
	}
	service := New("test", dbs, opts...)
	service.joinExecute = fake.execute
	t.Cleanup(service.CloseSnapshots)
	host := httptest.NewServer(service.Handler())
	t.Cleanup(host.Close)
	return service, host
}

func relFakeDefaultMounts() []*core.Database {
	return []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeMount("beta", "sqlite", "")}
}

type relFakeResponse struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (r relFakeResponse) errorDetail() map[string]any {
	detail, _ := r.body["error"].(map[string]any)
	return detail
}

func (r relFakeResponse) code() string {
	code, _ := r.errorDetail()["code"].(string)
	return code
}

func relFakeDo(t *testing.T, host *httptest.Server, method, path, token, body string, headers map[string]string) relFakeResponse {
	t.Helper()
	req, err := http.NewRequest(method, host.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := relFakeResponse{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// Documents the handler classifies as relational. relFakeJoin reads two
// collections of the database in the path; relFakeAcross reads two databases.
const (
	relFakeJoin = `from:
  name: orders
  alias: o
  joins:
    - type: inner
      from: {name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
`
	relFakeAcross = `from:
  database: alpha
  name: orders
  alias: o
  joins:
    - type: inner
      from: {database: beta, name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
`
)

// Every error the executor can return has the status and code the handler
// maps it to, and a body that repeats no more of the request than a bounded
// name.
func TestRelationalHandlerMapsEveryErrorOfTheExecutor(t *testing.T) {
	long := strings.Repeat("n", 5000)
	denied := access.DeniedError{Decision: access.Decision{Operation: access.Query, Code: access.CodeAccessDenied, Explanation: "private predicate or row"}}
	unsupported := access.DeniedError{Decision: access.Decision{Operation: access.Query, Code: access.CodeEnforcementUnsupported, Explanation: "private predicate or row"}}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
		// contains and excludes are checked against the body.
		contains, excludes []string
		header             map[string]string
		logged             bool
	}{
		{name: "no slot", err: &joinexec.CapacityError{Route: joinexec.RouteInMemory}, status: 503, code: "query_capacity", header: map[string]string{"Retry-After": "1"}, contains: []string{"in-memory"}},
		{name: "no slot, wrapped", err: fmt.Errorf("run: %w", &joinexec.CapacityError{Route: joinexec.RouteDatabase}), status: 503, code: "query_capacity", header: map[string]string{"Retry-After": "1"}},
		{
			name: "budget with a path", err: &joinexec.BudgetError{Name: joinexec.BudgetJoinRows, Limit: 10000, Route: joinexec.RouteInMemory, Path: "from.joins[0]"},
			status: 422, code: "query_budget_exceeded", contains: []string{`"name":"join_rows"`, `"limit":10000`, `"route":"in-memory"`, `"path":"from.joins[0]"`, `"hint":"`},
		},
		{
			name: "budget without a path", err: &joinexec.BudgetError{Name: joinexec.BudgetResponseRows, Limit: 1000, Route: joinexec.RouteDatabase},
			status: 422, code: "query_budget_exceeded", contains: []string{`"name":"response_rows"`, `"limit":1000`}, excludes: []string{`"path"`},
		},
		{
			name: "budget, wrapped in a source error", err: &joinexec.SourceError{Database: "alpha", Collection: "orders", Err: &joinexec.BudgetError{Name: joinexec.BudgetSourceRows, Limit: 100000, Route: joinexec.RouteInMemory, Path: long}},
			status: 422, code: "query_budget_exceeded", contains: []string{`"name":"source_rows"`},
		},
		{name: "source denied", err: &joinexec.SourceDeniedError{Database: "beta", Collection: "customers"}, status: 403, code: "forbidden", contains: []string{"customers", "beta"}},
		{name: "source denied, long names", err: &joinexec.SourceDeniedError{Database: long, Collection: long}, status: 403, code: "forbidden"},
		{name: "unknown database", err: &joinexec.UnknownDatabaseError{Database: "gamma"}, status: 404, code: "not_found", contains: []string{"gamma"}},
		{name: "unknown database, long name", err: &joinexec.UnknownDatabaseError{Database: long}, status: 404, code: "not_found"},
		{name: "engine that cannot be queried", err: &joinexec.EngineNotQueryableError{Database: "alpha", Engine: "postgres"}, status: 501, code: "query_unsupported", contains: []string{"postgres"}},
		{name: "engine outside the join set", err: &joinexec.EngineNotJoinableError{Database: "alpha", Engine: "firestore"}, status: 422, code: "join_engine_unsupported", contains: []string{"firestore"}},
		{name: "timeout", err: context.DeadlineExceeded, status: 504, code: "query_timeout"},
		{name: "timeout in a source", err: &joinexec.SourceError{Database: "alpha", Collection: "orders", Err: context.DeadlineExceeded}, status: 504, code: "query_timeout"},
		{name: "read transaction on a protected database", err: core.ErrProtectedReadTx, status: 422, code: "join_unsupported"},
		{name: "read transaction on a protected database, wrapped", err: &joinexec.SourceError{Err: core.ErrProtectedReadTx}, status: 422, code: "join_unsupported"},
		{name: "scan on a protected source", err: fmt.Errorf("%w: %q.%q", joinexec.ErrScanOnProtectedSource, "alpha", "orders"), status: 422, code: "join_unsupported"},
		{name: "policy denial", err: &denied, status: 403, code: "ACCESS_DENIED", excludes: []string{"private predicate"}},
		{name: "policy denial in a source", err: &joinexec.SourceError{Database: "alpha", Collection: "orders", Err: &denied}, status: 403, code: "ACCESS_DENIED", excludes: []string{"private predicate"}},
		{name: "shape the engine cannot compile", err: &unsupported, status: 422, code: "authorization_unsupported", excludes: []string{"private predicate"}},
		{name: "DALgo join shape", err: &dal.JoinValidationError{Category: "join_scope", Path: "from.joins[0]", Message: "unknown alias"}, status: 400, code: "invalid_dtql", contains: []string{"join_scope", "unknown alias"}},
		{name: "DALgo join shape, long message", err: &dal.JoinValidationError{Category: "join_scope", Path: "from", Message: long}, status: 400, code: "invalid_dtql"},
		{name: "DALgo query shape", err: &dal.QueryValidationError{Category: "query_shape", Path: "where", Message: "duplicate output name"}, status: 400, code: "invalid_dtql", contains: []string{"query_shape", "duplicate output name"}},
		{name: "DALgo query shape, wrapped", err: fmt.Errorf("executing: %w", &dal.QueryValidationError{Category: "query_shape", Path: "columns", Message: long}), status: 400, code: "invalid_dtql"},
		{name: "invalid document", err: fmt.Errorf("%w: $: field name %q is not a plain field name", joinexec.ErrInvalidDocument, "a b"), status: 400, code: "invalid_dtql", contains: []string{"a b"}},
		{name: "source without a database", err: fmt.Errorf("%w: collection %q", joinexec.ErrSourceWithoutDatabase, "orders"), status: 400, code: "invalid_dtql"},
		{name: "invalid document, long message", err: fmt.Errorf("%w: %s", joinexec.ErrInvalidDocument, long), status: 400, code: "invalid_dtql"},
		{name: "profile that does not match is the server's defect", err: fmt.Errorf("%w: %w: sources", joinexec.ErrInvalidDocument, joinexec.ErrProfileMismatch), status: 500, code: "internal", logged: true, excludes: []string{"profile"}},
		{name: "adapter refuses a null test", err: errors.New("unsupported condition dal.IsNullCondition"), status: 422, code: "query_unsupported"},
		{name: "adapter refuses a null test, in a source", err: &joinexec.SourceError{Database: "alpha", Collection: "orders", Err: errors.New("dalgo2ingitdb: unsupported condition type dal.IsNullCondition")}, status: 422, code: "query_unsupported"},
		{name: "adapter refuses a null test, wrapped twice", err: fmt.Errorf("executing: %w", &joinexec.SourceError{Database: "alpha", Collection: "orders", Err: fmt.Errorf("failed to build query: %w", errors.New("unsupported condition dal.IsNullCondition"))}), status: 422, code: "query_unsupported"},
		{name: "the phrase in the middle of an error is not the refusal", err: errors.New("read failed: unsupported condition in the driver"), status: 500, code: "internal", logged: true},
		{name: "a source named like the refusal, failing for another reason", err: &joinexec.SourceError{Database: "alpha", Collection: "unsupported condition", Err: errors.New("disk exploded")}, status: 500, code: "internal", logged: true, excludes: []string{"disk exploded", "unsupported condition"}},
		{name: "column the database does not know", err: errors.New("SQL logic error: no such column: Fooo (1)"), status: 400, code: "invalid_dtql", contains: []string{"Fooo"}, excludes: []string{"SQL logic error"}},
		{name: "column the database does not know, in a source", err: &joinexec.SourceError{Database: "alpha", Collection: "orders", Err: fmt.Errorf("failed to get SQL reader: %w", errors.New("SQL logic error: no such column: c.Fooo (1)"))}, status: 400, code: "invalid_dtql", contains: []string{"c.Fooo"}},
		{name: "column with a long name", err: errors.New("SQL logic error: no such column: " + long), status: 400, code: "invalid_dtql"},
		{name: "the phrase for a missing column in the middle of an error is not the refusal", err: errors.New("read failed: no such column: x"), status: 500, code: "internal", logged: true},
		{name: "a source named like a missing column, failing for another reason", err: &joinexec.SourceError{Database: "alpha", Collection: "no such column: x", Err: errors.New("disk exploded")}, status: 500, code: "internal", logged: true, excludes: []string{"disk exploded", "no such column"}},
		{name: "a join the executor could not plan, fixed refusal", err: &dal.JoinValidationError{Category: "join_plan", Path: "columns[0]", Message: "wildcard expansion requires ordered schema metadata"}, status: 400, code: "invalid_dtql", contains: []string{"wildcard expansion"}},
		{name: "a join the executor could not plan, fixed refusal, wrapped", err: fmt.Errorf("joining: %w", &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "generic JOIN does not support provider cursors"}), status: 400, code: "invalid_dtql", contains: []string{"provider cursors"}},
		{name: "a join the executor could not plan, IN without an array", err: &dal.JoinValidationError{Category: "join_plan", Path: "where", Message: "IN or NOT IN requires an array"}, status: 400, code: "invalid_dtql"},
		{name: "a join the executor could not plan, IS NULL without an operand", err: &dal.JoinValidationError{Category: "join_plan", Path: "where", Message: "IS NULL requires an operand"}, status: 400, code: "invalid_dtql"},
		{name: "a join the executor could not plan, operator", err: &dal.JoinValidationError{Category: "join_plan", Path: "where", Message: "unsupported operator like"}, status: 400, code: "invalid_dtql", contains: []string{"like"}},
		{name: "a join whose scan failed", err: &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "cannot scan c: disk I/O error"}, status: 500, code: "internal", logged: true, excludes: []string{"disk I/O", "scan"}},
		{name: "a join whose scan failed part way", err: &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "scan c: disk I/O error"}, status: 500, code: "internal", logged: true, excludes: []string{"disk I/O"}},
		{name: "a join whose scan failed to close", err: &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "close scan c: disk I/O error"}, status: 500, code: "internal", logged: true, excludes: []string{"disk I/O"}},
		{name: "a join that could not load the fields of a source", err: &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "cannot load fields for c: disk I/O error"}, status: 500, code: "internal", logged: true, excludes: []string{"disk I/O"}},
		{name: "a join that could not load the fields of a wildcard", err: &dal.JoinValidationError{Category: "join_plan", Path: "columns", Message: "cannot load wildcard fields: disk I/O error"}, status: 500, code: "internal", logged: true, excludes: []string{"disk I/O"}},
		{name: "a join whose output could not be encoded", err: &dal.JoinValidationError{Category: "join_plan", Path: "columns", Message: "output is not JSON serializable: unsupported value"}, status: 500, code: "internal", logged: true, excludes: []string{"unsupported value"}},
		{name: "a join whose scan failed with a text that looks like a refusal", err: &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "unsupported expression dal.X"}, status: 500, code: "internal", logged: true},
		{name: "structured queries unsupported on an engine", err: &joinexec.SourceError{Database: "alpha", Collection: "orders", Err: &core.QueryUnsupportedError{Engine: "postgres"}}, status: 501, code: "query_unsupported"},
		{name: "an error nothing knows", err: errors.New("disk exploded"), status: 500, code: "internal", logged: true, excludes: []string{"disk exploded"}},
		{name: "a source that fails", err: &joinexec.SourceError{Database: "alpha", Collection: "orders", Err: errors.New("disk exploded")}, status: 500, code: "internal", logged: true, excludes: []string{"disk exploded"}},
		{name: "a truncated read", err: joinexec.ErrReadTruncated, status: 500, code: "internal", logged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			fake := &relFakeExecutor{err: tc.err}
			_, host := relFakeServer(t, fake, relFakeDefaultMounts(), WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
			for _, path := range []string{"/v1/databases/alpha/dtql", "/v1/dtql"} {
				doc := relFakeJoin
				if path == "/v1/dtql" {
					doc = relFakeAcross
				}
				resp := relFakeDo(t, host, http.MethodPost, path, "", doc, nil)
				if resp.status != tc.status || resp.code() != tc.code {
					t.Fatalf("%s: status %d code %q, want %d %q: %s", path, resp.status, resp.code(), tc.status, tc.code, resp.raw)
				}
				for name, want := range tc.header {
					if got := resp.header.Get(name); got != want {
						t.Fatalf("%s: header %s = %q, want %q", path, name, got, want)
					}
				}
				for _, want := range tc.contains {
					if !strings.Contains(resp.raw, want) {
						t.Fatalf("%s: body lacks %q: %s", path, want, resp.raw)
					}
				}
				for _, unwanted := range tc.excludes {
					if strings.Contains(resp.raw, unwanted) {
						t.Fatalf("%s: body holds %q: %s", path, unwanted, resp.raw)
					}
				}
				if len(resp.raw) > 2048 {
					t.Fatalf("%s: the body is %d bytes: a name came back whole", path, len(resp.raw))
				}
				if got := resp.header.Get("Cache-Control"); got != "no-store" {
					t.Fatalf("%s: an error is cached: Cache-Control %q", path, got)
				}
			}
			if logged := strings.Contains(logs.String(), `"level":"ERROR"`); logged != tc.logged {
				t.Fatalf("logged = %v, want %v: %s", logged, tc.logged, logs.String())
			}
		})
	}
}

// The budget refusal names the bound and the limit and never a figure the query
// reached, with a hint for every bound the executor can report.
func TestRelationalBudgetHintsCoverEveryBound(t *testing.T) {
	names := []string{
		joinexec.BudgetSourceRows, joinexec.BudgetSourceBytes,
		joinexec.BudgetJoinRows, joinexec.BudgetJoinResultRows, joinexec.BudgetJoinFetchedRows, joinexec.BudgetJoinRetainedBytes, joinexec.BudgetJoinScan, joinexec.BudgetJoinCandidateEvaluations,
		joinexec.BudgetAggregationGroups, joinexec.BudgetAggregationStates, joinexec.BudgetAggregationBytes, joinexec.BudgetAggregationDistinctValues, joinexec.BudgetAggregationTotalDistinct,
		joinexec.BudgetResponseRows, joinexec.BudgetResponseBytes,
	}
	seen := map[string]string{}
	for _, name := range names {
		hint := budgetHint(name)
		if hint == "" || hint == genericBudgetHint {
			t.Errorf("bound %q has no hint of its own", name)
		}
		if _, listed := budgetHints[name]; !listed {
			t.Errorf("bound %q is not in the table", name)
		}
		seen[name] = hint
	}
	if len(budgetHints) != len(names) {
		t.Errorf("the table has %d entries for %d bounds", len(budgetHints), len(names))
	}
	if got := budgetHint("something_new"); got != genericBudgetHint {
		t.Errorf("an unknown bound gets %q", got)
	}
}

// The handler authorises every database and collection of the document before it
// calls the executor, so a request it refuses reaches nothing; the executor gets
// the same authoriser to check again.
func TestRelationalHandlerAuthorisesBeforeItCallsTheExecutor(t *testing.T) {
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	read := auth.Capability{Action: auth.CapRecordsRead}
	for token, grant := range map[string]*auth.Grant{
		"alpha-only":   {DatabaseID: "alpha", Capabilities: []auth.Capability{read}},
		"orders-only":  {DatabaseID: "alpha", Capabilities: []auth.Capability{{Action: auth.CapRecordsRead, Collection: "orders"}}},
		"both":         {DatabaseID: "", Capabilities: []auth.Capability{read}},
		"cannot-write": {DatabaseID: "alpha", Capabilities: []auth.Capability{{Action: auth.CapRecordsWrite}}},
	} {
		if err := store.CreateGrant(grant, token); err != nil {
			t.Fatal(err)
		}
	}
	const owner = "owner-token"
	fake := &relFakeExecutor{result: joinexec.Result{Columns: []string{"id"}}}
	_, host := relFakeServer(t, fake, relFakeDefaultMounts(), WithAuth(&auth.Config{OwnerToken: owner, Store: store}))

	for _, tc := range []struct {
		name, path, token, doc string
		status                 int
		naming                 string
	}{
		{"no token", "/v1/dtql", "", relFakeAcross, 401, ""},
		{"second database not granted", "/v1/dtql", "alpha-only", relFakeAcross, 403, "beta"},
		{"collection not granted on the endpoint", "/v1/databases/alpha/dtql", "orders-only", relFakeJoin, 403, "customers"},
		{"capability not granted", "/v1/databases/alpha/dtql", "cannot-write", relFakeJoin, 403, "orders"},
		{"token of another database on the endpoint", "/v1/databases/beta/dtql", "alpha-only", relFakeJoin, 403, "orders"},
		{"foreign database named on the endpoint", "/v1/databases/alpha/dtql", "both", relFakeAcross, 400, "beta"},
		{"source without a database", "/v1/dtql", "both", relFakeJoin, 400, "orders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := relFakeDo(t, host, http.MethodPost, tc.path, tc.token, tc.doc, nil)
			if resp.status != tc.status || (tc.naming != "" && !strings.Contains(resp.raw, tc.naming)) {
				t.Fatalf("status %d, want %d naming %q: %s", resp.status, tc.status, tc.naming, resp.raw)
			}
			if fake.count() != 0 {
				t.Fatal("the executor was called for a request the handler refused")
			}
		})
	}

	t.Run("a refusal repeats no more than a bounded name", func(t *testing.T) {
		long := strings.Repeat("c", 5000)
		for name, tc := range map[string]struct{ path, doc, token string }{
			"403 on /v1/dtql":                 {"/v1/dtql", "from: {database: beta, name: " + long + "}\n", "alpha-only"},
			"400 for a source without a base": {"/v1/dtql", "from: {name: " + long + "}\n", "both"},
			"400 for a foreign database":      {"/v1/databases/alpha/dtql", "from: {database: " + strings.Repeat("d", 60) + ", name: orders}\n", "both"},
		} {
			resp := relFakeDo(t, host, http.MethodPost, tc.path, tc.token, tc.doc, nil)
			if resp.status/100 != 4 || len(resp.raw) > 600 {
				t.Errorf("%s: status %d, %d bytes: %.200s", name, resp.status, len(resp.raw), resp.raw)
			}
		}
		if fake.count() != 0 {
			t.Fatal("the executor was called")
		}
	})
	t.Run("a cleared request reaches the executor with the authoriser of the principal", func(t *testing.T) {
		resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "both", relFakeAcross, nil)
		if resp.status != 200 {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		call := fake.only(t)
		if call.defaultDatabase != "" || len(call.profile.Sources) != 2 || call.profile.Sources[1] != (joinexec.ProfileSource{Database: "beta", Collection: "customers"}) {
			t.Fatalf("call = %+v", call)
		}
		if !call.authorize("alpha", "orders") || !call.authorize("beta", "anything") {
			t.Fatal("a server-level grant is refused by the authoriser the executor got")
		}
	})
	fake.calls = nil
	t.Run("a collection-scoped token is cleared for its collection only", func(t *testing.T) {
		resp := relFakeDo(t, host, http.MethodPost, "/v1/databases/alpha/dtql", "orders-only", "from: {database: alpha, name: orders}\n", nil)
		if resp.status != 200 {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		call := fake.only(t)
		if call.defaultDatabase != "alpha" || !call.authorize("alpha", "orders") || call.authorize("alpha", "customers") || call.authorize("beta", "orders") {
			t.Fatalf("call = %+v", call)
		}
	})
	t.Run("the owner is cleared", func(t *testing.T) {
		fake.calls = nil
		if resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", owner, relFakeAcross, nil); resp.status != 200 {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
}

// With auth off every read is cleared; a principal that is missing reads nothing.
func TestRelationalAuthorizerWithoutAuthAndWithoutAPrincipal(t *testing.T) {
	off := New("test", nil)
	if !off.readAuthorizer(httptest.NewRequest("GET", "/", nil))("any", "thing") {
		t.Fatal("with auth off every read is cleared")
	}
	on := New("test", nil, WithAuth(&auth.Config{OwnerToken: "t"}))
	if on.readAuthorizer(httptest.NewRequest("GET", "/", nil))("any", "thing") {
		t.Fatal("with auth on a request without a principal reads nothing")
	}
}

// What the executor is handed.
func TestRelationalHandlerHandsTheExecutorTheLeasedDatabasesAndTheLimits(t *testing.T) {
	fake := &relFakeExecutor{result: joinexec.Result{
		Records:   []record.Record{record.NewRecordWithData(record.NewKeyWithID("x", "1"), map[string]any{"id": 1})},
		Columns:   []string{"id"},
		Execution: joinexec.Execution{Route: joinexec.RouteDatabase, RowsReturned: 1, Sources: []joinexec.ExecutionSource{{Database: "alpha", Collection: "orders"}}},
	}}
	_, host := relFakeServer(t, fake, relFakeDefaultMounts(), WithQueryLimits(QueryLimits{Timeout: 3 * time.Second, MaxSourceRows: 77, MaxSourceBytes: 1 << 20}))
	resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", relFakeAcross, nil)
	if resp.status != 200 {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
	want := `{"records":[{"data":{"id":1}}],"columns":["id"],"execution":{"route":"database","elapsedMs":0,"rowsReturned":1,"sources":[{"database":"alpha","collection":"orders"}]}}`
	if strings.TrimSpace(resp.raw) != want {
		t.Fatalf("body = %s\nwant %s", resp.raw, want)
	}
	call := fake.only(t)
	if call.limits != (joinexec.Limits{MaxSourceRows: 77, MaxSourceBytes: 1 << 20, Timeout: 3 * time.Second}) {
		t.Fatalf("limits = %+v", call.limits)
	}
	if call.options != 2 {
		t.Fatalf("options = %d, want the admission and the join engines", call.options)
	}
	for _, id := range []string{"alpha", "beta"} {
		source, ok := call.registry.Lookup(id)
		if !ok || source.ID() != id {
			t.Fatalf("Lookup(%q) = %v, %v", id, source, ok)
		}
	}
	if _, ok := call.registry.Lookup("gamma"); ok {
		t.Fatal("the registry answers for a database the request did not lease")
	}
	if _, ok := call.registry.Lookup("alpha"); !ok {
		t.Fatal("lookup is repeatable")
	}
}

// A request holds every database it reads until it ends: an unmount waits.
func TestRelationalHandlerLeasesEveryDatabaseUntilTheRequestEnds(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	fake := &relFakeExecutor{block: func() {
		close(started)
		<-release
	}}
	service, host := relFakeServer(t, fake, relFakeDefaultMounts())
	done := make(chan relFakeResponse, 1)
	go func() { done <- relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", relFakeAcross, nil) }()
	<-started
	for _, id := range []string{"alpha", "beta"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := service.UnmountContext(ctx, id); !errors.Is(err, context.Canceled) {
			t.Fatalf("UnmountContext(%q) = %v: it must wait for the request that reads it", id, err)
		}
	}
	close(release)
	if resp := <-done; resp.status != 200 {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
}

// Each database a document reads is leased once, in the order the document first
// names it, and the endpoint's own database is not leased again.
func TestLeaseRelationalDatabasesHoldsOneLeasePerDatabase(t *testing.T) {
	service := New("test", map[string]*core.Database{"alpha": relFakeMount("alpha", "sqlite", ""), "beta": relFakeMount("beta", "sqlite", ""), "gamma": relFakeMount("gamma", "sqlite", "")})
	t.Cleanup(service.CloseSnapshots)
	targets := []relationalTarget{{"gamma", "a"}, {"alpha", "b"}, {"gamma", "c"}, {"beta", "d"}, {"alpha", "e"}}
	lease := func(endpoint *core.Database, targets []relationalTarget) (leased int, order []string, status int) {
		held := &leases{}
		r := httptest.NewRequest("POST", "/v1/dtql", nil)
		r = r.WithContext(context.WithValue(r.Context(), leasesKey{}, held))
		w := httptest.NewRecorder()
		_, order, ok := service.leaseRelationalDatabases(w, r, endpoint, targets)
		if !ok {
			return len(held.done), nil, w.Code
		}
		return len(held.done), order, http.StatusOK
	}
	if leased, order, _ := lease(nil, targets); leased != 3 || strings.Join(order, ",") != "gamma,alpha,beta" {
		t.Fatalf("leased %d databases, order %v", leased, order)
	}
	// The per-database endpoint leased its database when it routed the request.
	if leased, order, _ := lease(service.getDB("alpha"), []relationalTarget{{"alpha", "a"}, {"alpha", "b"}}); leased != 0 || strings.Join(order, ",") != "alpha" {
		t.Fatalf("leased %d databases, order %v", leased, order)
	}
	// A database that is not mounted is a 404 and the leases already taken are released
	// with the request.
	if _, _, status := lease(nil, []relationalTarget{{"alpha", "a"}, {"zeta", "b"}}); status != http.StatusNotFound {
		t.Fatalf("status %d", status)
	}
}

// A collection that a database on an engine that builds SQL does not declare is
// a 404 before the executor is called, whatever the grant says. The check follows
// the refusals that do not depend on the collection (paging headers, engines); a
// document engine takes any collection.
func TestRelationalHandlerRefusesAnUndeclaredCollectionBeforeTheExecutor(t *testing.T) {
	ghost := "from: {database: alpha, name: ghost}\n"
	for _, tc := range []struct {
		name    string
		engine  string
		doc     string
		headers map[string]string
		status  int
		code    string
	}{
		{"sqlite", "sqlite", ghost, nil, 404, "not_found"},
		{"postgres, whose engine is refused first", "postgres", ghost, nil, 501, "query_unsupported"},
		{"mysql, whose engine is refused first", "mysql", ghost, nil, 501, "query_unsupported"},
		{"with a paging header, which is refused first", "sqlite", ghost, map[string]string{"OVDB-Page-Size": "10"}, 422, "snapshot_unsupported"},
		{"named by a subquery", "sqlite", "from: {database: alpha, name: orders}\nwhere: {exists: {query: {from: {database: alpha, name: ghost}}}}\n", nil, 404, "not_found"},
		{"a spelling of a declared collection that is not its canonical name", "sqlite", "from: {database: alpha, name: '\"orders\"'}\n", nil, 404, "not_found"},
		{"a declared collection is read", "sqlite", "from: {database: alpha, name: orders}\n", nil, 200, ""},
		{"ingitdb takes any collection", "ingitdb", ghost, nil, 200, ""},
		{"firestore takes any collection but is not joined", "firestore", ghost, nil, 422, "join_engine_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &relFakeExecutor{result: joinexec.Result{Columns: []string{}}}
			_, host := relFakeServer(t, fake, []*core.Database{relFakeMount("alpha", tc.engine, "")})
			resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", tc.doc, tc.headers)
			if resp.status != tc.status || (tc.code != "" && resp.code() != tc.code) {
				t.Fatalf("status %d code %q, want %d %q: %s", resp.status, resp.code(), tc.status, tc.code, resp.raw)
			}
			if tc.status != 200 && fake.count() != 0 {
				t.Fatalf("the executor was called %d times for a refused request", fake.count())
			}
			if tc.status == 404 && !strings.Contains(resp.raw, "orders") && !strings.Contains(resp.raw, "ghost") {
				t.Fatalf("the refusal names no collection: %s", resp.raw)
			}
		})
	}
	t.Run("the collection and database names are clipped", func(t *testing.T) {
		fake := &relFakeExecutor{}
		_, host := relFakeServer(t, fake, []*core.Database{relFakeMount("alpha", "sqlite", "")})
		resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", "from: {database: alpha, name: "+strings.Repeat("g", 5000)+"}\n", nil)
		if resp.status != 404 || len(resp.raw) > 400 {
			t.Fatalf("status %d, %d bytes", resp.status, len(resp.raw))
		}
	})
}

// A database that is named, granted and not mounted is a 404, and nothing runs.
func TestRelationalHandlerAnswers404ForAnUnmountedDatabase(t *testing.T) {
	fake := &relFakeExecutor{}
	_, host := relFakeServer(t, fake, []*core.Database{relFakeMount("alpha", "sqlite", "")})
	resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", relFakeAcross, nil)
	if resp.status != 404 || resp.code() != "not_found" || !strings.Contains(resp.raw, "beta") || fake.count() != 0 {
		t.Fatalf("status %d calls %d: %s", resp.status, fake.count(), resp.raw)
	}
}

// The operator's list of join engines is ANDed with the guard of structured
// queries, and a GitHub-backed inGitDB mount is never joined, whatever the list
// says. The executor is not called for a refused engine.
func TestRelationalHandlerRefusesEnginesTheGuardOrTheListLeavesOut(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mounts  []*core.Database
		engines []string
		status  int
		code    string
		naming  string
	}{
		{"sqlite and local inGitDB by default", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeMount("beta", "ingitdb", "")}, nil, 200, "", ""},
		{"firestore is cleared for queries but not in the default list", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeMount("beta", "firestore", "")}, nil, 422, "join_engine_unsupported", "firestore"},
		{"firestore when the operator lists it", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeMount("beta", "firestore", "")}, []string{"sqlite", "firestore"}, 200, "", ""},
		{"an engine the list leaves out", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeMount("beta", "ingitdb", "")}, []string{"sqlite"}, 422, "join_engine_unsupported", "ingitdb"},
		{"postgres is not cleared for queries even when listed", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeMount("beta", "postgres", "")}, []string{"sqlite", "postgres"}, 501, "query_unsupported", "postgres"},
		{"mysql is not cleared for queries even when listed", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeMount("beta", "mysql", "")}, []string{"mysql"}, 501, "query_unsupported", "mysql"},
		{"an unknown engine is not cleared for queries", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeMount("beta", "oracle", "")}, []string{"oracle"}, 501, "query_unsupported", "oracle"},
		{"GitHub-backed inGitDB is not joined by default", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeGitHubMount("beta")}, nil, 422, "join_engine_unsupported", core.EngineInGitDBGitHub},
		{"GitHub-backed inGitDB is not joined when listed", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeGitHubMount("beta")}, []string{"sqlite", "ingitdb", core.EngineInGitDBGitHub}, 422, "join_engine_unsupported", core.EngineInGitDBGitHub},
		{"GitHub-backed inGitDB is not joined when it is the only engine listed", []*core.Database{relFakeMount("alpha", "sqlite", ""), relFakeGitHubMount("beta")}, []string{core.EngineInGitDBGitHub}, 422, "join_engine_unsupported", "sqlite"},
		{"the guard is checked for every database before the list", []*core.Database{relFakeMount("alpha", "firestore", ""), relFakeMount("beta", "postgres", "")}, nil, 501, "query_unsupported", "postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &relFakeExecutor{}
			var opts []Option
			if tc.engines != nil {
				opts = append(opts, WithQueryLimits(QueryLimits{JoinEngines: tc.engines}))
			}
			_, host := relFakeServer(t, fake, tc.mounts, opts...)
			resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", relFakeAcross, nil)
			if resp.status != tc.status || (tc.code != "" && resp.code() != tc.code) || (tc.naming != "" && !strings.Contains(resp.raw, tc.naming)) {
				t.Fatalf("status %d code %q, want %d %q naming %q: %s", resp.status, resp.code(), tc.status, tc.code, tc.naming, resp.raw)
			}
			if wantCalls := map[bool]int{true: 1, false: 0}[tc.status == 200]; fake.count() != wantCalls {
				t.Fatalf("the executor was called %d times, want %d", fake.count(), wantCalls)
			}
		})
	}
}

func TestJoinEnginesOmitsTheGitHubEngineWhateverTheListSays(t *testing.T) {
	s := New("test", nil, WithQueryLimits(QueryLimits{JoinEngines: []string{"sqlite", core.EngineInGitDBGitHub, "ingitdb"}}))
	if got := s.joinEngines(); strings.Join(got, ",") != "sqlite,ingitdb" {
		t.Fatalf("joinEngines = %v", got)
	}
	only := New("test", nil, WithQueryLimits(QueryLimits{JoinEngines: []string{core.EngineInGitDBGitHub}}))
	if got := only.joinEngines(); len(got) != 0 {
		t.Fatalf("joinEngines = %v, want none", got)
	}
}

// relFakeSQLite mounts a real SQLite file holding statements as id, declaring the
// collections of declared (all columns are strings to the manifest).
func relFakeSQLite(t *testing.T, id string, declared map[string][]string, statements ...string) *core.Database {
	t.Helper()
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := storage.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	text := "database: {id: " + id + ", schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n"
	for name, fields := range declared {
		text += "    " + name + ":\n      fields:\n"
		for _, field := range fields {
			text += "        " + field + ": {type: string}\n"
		}
	}
	path := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// The gate is taken by the executor, on the route it chooses, once it knows the
// route and before it reads: with every slot of that route held the request is a
// 503 with Retry-After, and it is served once a slot is free. Another route's
// slots do not matter. The executor is the real one over real SQLite files, so the
// route is the one a request of this shape really takes.
func TestRelationalHandlerTakesTheGateOnTheRouteTheExecutorChooses(t *testing.T) {
	alpha := relFakeSQLite(t, "alpha", map[string][]string{"orders": {"id", "customer_id"}},
		`CREATE TABLE "orders" ("id" TEXT PRIMARY KEY, "customer_id" TEXT)`, `INSERT INTO "orders" VALUES ('o1', 'c1')`)
	beta := relFakeSQLite(t, "beta", map[string][]string{"customers": {"id", "name"}},
		`CREATE TABLE "customers" ("id" TEXT PRIMARY KEY, "name" TEXT)`, `INSERT INTO "customers" VALUES ('c1', 'Ada')`)
	across := `from:
  database: alpha
  name: orders
  alias: o
  joins:
    - type: inner
      from: {database: beta, name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
columns:
  - {field: id, source: o}
  - {field: name, source: c}
`
	for _, tc := range []struct {
		name  string
		doc   string
		route queryRoute
		other queryRoute
	}{
		{"database route", "from: {database: alpha, name: orders}\ncolumns: [{field: id}]\n", routeDatabase, routeInMemory},
		{"in-memory route", across, routeInMemory, routeDatabase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := New("test", map[string]*core.Database{"alpha": alpha, "beta": beta}, WithQueryLimits(QueryLimits{InMemory: 1, Database: 1, QueueWait: -1}))
			t.Cleanup(service.CloseSnapshots)
			host := httptest.NewServer(service.Handler())
			t.Cleanup(host.Close)
			held, ok := service.queryGate.acquire(context.Background(), tc.route)
			if !ok {
				t.Fatal("the gate has no slot")
			}
			resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", tc.doc, nil)
			if resp.status != 503 || resp.code() != "query_capacity" || resp.header.Get("Retry-After") != "1" || !strings.Contains(resp.raw, string(tc.route)) {
				t.Fatalf("with the %s slots held: status %d: %s", tc.route, resp.status, resp.raw)
			}
			held()
			otherHeld, ok := service.queryGate.acquire(context.Background(), tc.other)
			if !ok {
				t.Fatal("the slots of one route are held by the other")
			}
			resp = relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", tc.doc, nil)
			if resp.status != 200 {
				t.Fatalf("with only the %s slots held: status %d: %s", tc.other, resp.status, resp.raw)
			}
			execution, _ := resp.body["execution"].(map[string]any)
			if execution["route"] != string(tc.route) {
				t.Fatalf("route = %v, want %s", execution["route"], tc.route)
			}
			otherHeld()
			// The slot of the request that ran is back.
			if again, ok := service.queryGate.acquire(context.Background(), tc.route); !ok {
				t.Fatal("a request that ended holds its slot")
			} else {
				again()
			}
		})
	}
}

// A request answered with no rows, columns or sources still has the shape of
// the answer.
func TestRelationalHandlerAnswersAnEmptyResult(t *testing.T) {
	fake := &relFakeExecutor{result: joinexec.Result{Records: []record.Record{}, Columns: []string{}, Execution: joinexec.Execution{Route: joinexec.RouteInMemory, Sources: []joinexec.ExecutionSource{}}}}
	_, host := relFakeServer(t, fake, relFakeDefaultMounts())
	resp := relFakeDo(t, host, http.MethodPost, "/v1/databases/alpha/dtql", "", relFakeJoin, nil)
	if resp.status != 200 || !strings.Contains(resp.raw, `"records":[]`) || !strings.Contains(resp.raw, `"columns":[]`) {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
}

// The cache headers of the answer come from the databases the document read.
func TestRelationalHandlerCacheHeaderFollowsTheSmallestTTLOfTheDatabasesRead(t *testing.T) {
	fake := &relFakeExecutor{result: joinexec.Result{Columns: []string{}}}
	mounts := []*core.Database{relFakeMount("alpha", "sqlite", "120s"), relFakeMount("beta", "sqlite", "30s")}
	_, host := relFakeServer(t, fake, mounts, WithReadOnly(true))
	query := "?q=" + url.QueryEscape(relFakeAcross)
	resp := relFakeDo(t, host, http.MethodGet, "/v1/dtql"+query, "", "", nil)
	if resp.status != 200 || resp.header.Get("Cache-Control") != "public, max-age=30, s-maxage=30" {
		t.Fatalf("status %d Cache-Control %q: %s", resp.status, resp.header.Get("Cache-Control"), resp.raw)
	}
	if got := resp.header.Get("Vary"); got != "OVDB-Page-Size, OVDB-Page-Token, OVDB-Page-Close" {
		t.Fatalf("Vary = %q", got)
	}
	// The answer of the per-database endpoint reads the one database of its path.
	resp = relFakeDo(t, host, http.MethodGet, "/v1/databases/alpha/dtql?q="+url.QueryEscape(relFakeJoin), "", "", nil)
	if resp.header.Get("Cache-Control") != "public, max-age=120, s-maxage=120" {
		t.Fatalf("Cache-Control %q", resp.header.Get("Cache-Control"))
	}
	if resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", relFakeAcross, nil); resp.header.Get("Cache-Control") != "no-store" || resp.header.Get("Vary") != "" {
		t.Fatalf("POST: Cache-Control %q Vary %q", resp.header.Get("Cache-Control"), resp.header.Get("Vary"))
	}
}

// The URL form of /v1/dtql is a read that defaults to no-store before any
// authentication or lookup, as the per-database forms do.
func TestCrossDatabaseEndpointDefaultsToNoStoreBeforeAuthentication(t *testing.T) {
	fake := &relFakeExecutor{}
	_, host := relFakeServer(t, fake, relFakeDefaultMounts(), WithAuth(&auth.Config{OwnerToken: "t"}))
	resp := relFakeDo(t, host, http.MethodGet, "/v1/dtql?q=x", "", "", nil)
	if resp.status != 401 || resp.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d Cache-Control %q", resp.status, resp.header.Get("Cache-Control"))
	}
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/dtql", true},
		{"HEAD", "/v1/dtql", true},
		{"POST", "/v1/dtql", false},
		{"GET", "/v1/dtql/", false},
		{"GET", "/v1/databases/x/dtql", true},
		{"GET", "/v1/databases/x/records/a/b", false},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if got := isReadCacheEndpoint(r); got != tc.want {
			t.Errorf("isReadCacheEndpoint(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// A body over the limit is a 400 on both endpoints and a GET with no q is too.
func TestDTQLRequestsThatCarryNoDocument(t *testing.T) {
	fake := &relFakeExecutor{}
	_, host := relFakeServer(t, fake, relFakeDefaultMounts())
	huge := strings.Repeat("x", 2<<20)
	for _, path := range []string{"/v1/databases/alpha/dtql", "/v1/dtql"} {
		if resp := relFakeDo(t, host, http.MethodPost, path, "", huge, nil); resp.status != 400 || !strings.Contains(resp.raw, "failed to read body") {
			t.Errorf("%s: oversized body: status %d: %s", path, resp.status, resp.raw)
		}
		if resp := relFakeDo(t, host, http.MethodGet, path, "", "", nil); resp.status != 400 {
			t.Errorf("%s: GET without q: status %d: %s", path, resp.status, resp.raw)
		}
		if resp := relFakeDo(t, host, http.MethodPost, path, "", "", nil); resp.status != 400 {
			t.Errorf("%s: empty body: status %d: %s", path, resp.status, resp.raw)
		}
		if resp := relFakeDo(t, host, http.MethodPost, path, "", `{"query": "from: {name: x}", "nope": 1}`, map[string]string{"Content-Type": "application/json"}); resp.status != 400 || resp.code() != "invalid_dtql" {
			t.Errorf("%s: bad JSON: status %d: %s", path, resp.status, resp.raw)
		}
		if resp := relFakeDo(t, host, http.MethodPost, path, "", "- a list\n", nil); resp.status != 400 || resp.code() != "invalid_dtql" {
			t.Errorf("%s: not a DTQL document: status %d: %s", path, resp.status, resp.raw)
		}
	}
	if resp := relFakeDo(t, host, http.MethodPost, "/v1/databases/nowhere/dtql", "", relFakeJoin, nil); resp.status != 404 || resp.code() != "not_found" {
		t.Errorf("a database that is not mounted: status %d: %s", resp.status, resp.raw)
	}
	if fake.count() != 0 {
		t.Fatal("the executor was called")
	}
}

func TestRelationalTargetsSettlesTheDatabaseOfEverySource(t *testing.T) {
	alpha := relFakeMount("alpha", "sqlite", "")
	profile := core.Profile{Sources: []core.ProfileSource{{Collection: "a"}, {Database: "alpha", Collection: "b"}}}
	targets, refusal := relationalTargets(profile, alpha)
	if refusal != "" || len(targets) != 2 || targets[0] != (relationalTarget{"alpha", "a"}) || targets[1] != (relationalTarget{"alpha", "b"}) {
		t.Fatalf("targets = %v, refusal = %q", targets, refusal)
	}
	if _, refusal := relationalTargets(core.Profile{Sources: []core.ProfileSource{{Database: strings.Repeat("d", 500), Collection: "b"}}}, alpha); !strings.Contains(refusal, "/v1/dtql") || len(refusal) > 300 {
		t.Fatalf("refusal = %q", refusal)
	}
	if _, refusal := relationalTargets(core.Profile{Sources: []core.ProfileSource{{Collection: strings.Repeat("c", 500)}}}, nil); !strings.Contains(refusal, "names its database") || len(refusal) > 300 {
		t.Fatalf("refusal = %q", refusal)
	}
	targets, refusal = relationalTargets(core.Profile{Sources: []core.ProfileSource{{Database: "x", Collection: "a"}}}, nil)
	if refusal != "" || targets[0] != (relationalTarget{"x", "a"}) {
		t.Fatalf("targets = %v, refusal = %q", targets, refusal)
	}
}

func TestClipTextCutsOnACharacterBoundary(t *testing.T) {
	if got := clipName("short"); got != "short" {
		t.Fatalf("clipName = %q", got)
	}
	exact := strings.Repeat("a", maxEchoLen)
	if got := clipName(exact); got != exact {
		t.Fatalf("a name of exactly %d bytes is kept whole", maxEchoLen)
	}
	if got := clipName(exact + "b"); got != exact+"..." {
		t.Fatalf("clipName = %q", got)
	}
	straddling := strings.Repeat("a", maxEchoLen-1) + "€€"
	if got := clipName(straddling); got != strings.Repeat("a", maxEchoLen-1)+"..." {
		t.Fatalf("clipName = %q", got)
	}
	if got := clipText(strings.Repeat("é", 400), maxEchoText); len(got) > maxEchoText+3 {
		t.Fatalf("clipText kept %d bytes", len(got))
	}
}

func TestUnknownColumnReadsTheNameSQLiteGives(t *testing.T) {
	for text, want := range map[string]string{
		"SQL logic error: no such column: Foo (1)": "Foo",
		"no such column: t.col":                    "t.col",
	} {
		if got, ok := unknownColumn(errors.New(text)); !ok || got != want {
			t.Errorf("unknownColumn(%q) = %q, %v, want %q", text, got, ok, want)
		}
	}
	if _, ok := unknownColumn(errors.New("no such table: x")); ok {
		t.Error("a missing table is not a missing column")
	}
	if isUnsupportedConditionError(errors.New("no such column: x")) || !isUnsupportedConditionError(errors.New("x: unsupported condition dal.T")) {
		t.Error("isUnsupportedConditionError")
	}
}

// A root source that carries the engine's default schema reads as one that
// carries none. Everything else is left as it came, for the classifier.
func TestWithoutDefaultSchemaDropsOnlyTheDefaultSchemaOfTheRootSource(t *testing.T) {
	const bare = "from: {name: Customer}\norderBy: [{field: id}]\n"
	schemaOf := func(t *testing.T, doc []byte) string {
		t.Helper()
		query, err := dtql.Deserialize(doc)
		if err != nil {
			t.Fatalf("Deserialize(%q): %v", doc, err)
		}
		return query.From().Base().(dal.CollectionRef).Schema()
	}
	for name, doc := range map[string]string{
		"flow":     "from: {schema: main, name: Customer}\norderBy: [{field: id}]\n",
		"block":    "from:\n  schema: main\n  name: Customer\norderBy: [{field: id}]\n",
		"quoted":   "from: {schema: \"main\", name: Customer}\norderBy: [{field: id}]\n",
		"last":     "from: {name: Customer, schema: main}\norderBy: [{field: id}]\n",
		"join too": "from: {schema: main, name: Customer, alias: c, joins: [{from: {name: Invoice, alias: i}, on: [{left: {field: a, source: c}, op: '==', right: {field: b, source: i}}]}]}\n",
	} {
		t.Run("dropped, "+name, func(t *testing.T) {
			got := withoutDefaultSchema([]byte(doc), "sqlite")
			if string(got) == doc || strings.Contains(string(got), "schema") {
				t.Fatalf("the schema is still there: %s", got)
			}
			if schemaOf(t, got) != "" {
				t.Fatalf("schema = %q", schemaOf(t, got))
			}
		})
	}
	for name, tc := range map[string]struct{ doc, engine string }{
		"another schema":                {"from: {schema: other, name: Customer}\n", "sqlite"},
		"capitals":                      {"from: {schema: MAIN, name: Customer}\n", "sqlite"},
		"two schema keys":               {"from: {schema: main, name: Customer, schema: main}\n", "sqlite"},
		"a schema that is not a scalar": {"from: {schema: [main], name: Customer}\n", "sqlite"},
		"a schema that is a mapping":    {"from: {schema: {x: main}, name: Customer}\n", "sqlite"},
		"an engine with no default":     {"from: {schema: main, name: Customer}\n", "ingitdb"},
		"an unknown engine":             {"from: {schema: main, name: Customer}\n", ""},
		"no schema":                     {bare, "sqlite"},
		"no from":                       {"where: {op: '==', left: {field: a}, right: {value: 1}}\n", "sqlite"},
		"from that is not a mapping":    {"from: Customer\n", "sqlite"},
		"a document that is a list":     {"- from: {schema: main, name: Customer}\n", "sqlite"},
		"not YAML":                      {"from: {schema: main, name: Customer\n", "sqlite"},
		"an empty document":             {"", "sqlite"},
		"two documents":                 {"from: {schema: main, name: A}\n---\nfrom: {name: B}\n", "sqlite"},
		"a key that is not a scalar":    {"? [a]\n: b\nfrom: {schema: other, name: Customer}\n", "sqlite"},
		"the schema of a subquery":      {"from: {name: A, alias: a}\nwhere: {exists: {query: {from: {schema: main, name: B}}}}\n", "sqlite"},
	} {
		t.Run("kept, "+name, func(t *testing.T) {
			if got := withoutDefaultSchema([]byte(tc.doc), tc.engine); string(got) != tc.doc {
				t.Fatalf("the document changed: %q", got)
			}
		})
	}
	t.Run("kept, the encoder fails", func(t *testing.T) {
		doc := "from: {schema: main, name: Customer}\n"
		failing := func(any) ([]byte, error) { return nil, errors.New("cannot encode") }
		if got := stripDefaultSchema([]byte(doc), "sqlite", failing); string(got) != doc {
			t.Fatalf("the document changed: %q", got)
		}
	})
	t.Run("the rest of the document is kept", func(t *testing.T) {
		got := withoutDefaultSchema([]byte("from: {schema: main, name: Customer, alias: c}\nwhere: {op: '>', left: {field: n, source: c}, right: {value: 1}}\nlimit: 5\ncolumns: [{field: id, source: c}]\n"), "sqlite")
		query, err := dtql.Deserialize(got)
		if err != nil {
			t.Fatal(err)
		}
		if query.Limit() != 5 || query.Where() == nil || len(query.Columns()) != 1 || query.From().Base().Alias() != "c" {
			t.Fatalf("query = %v", query)
		}
	})
}
