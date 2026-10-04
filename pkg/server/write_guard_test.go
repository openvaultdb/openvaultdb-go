package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// writeGuardFakeDB extends guardFakeDB (query_guard_test.go), which counts the
// query path and serves one record by key, with the existence check and the
// read-write transaction, counting each. Every other method still panics (nil
// embedded DB). The transaction body is not run: a write that reaches the
// adapter is counted and reported as applied.
type writeGuardFakeDB struct {
	guardFakeDB
	exists       int
	transactions int
	absent       bool // Get reports the record as not found
}

// reached is the number of adapter calls of any kind.
func (f *writeGuardFakeDB) reached() int {
	return f.queries + f.gets + f.exists + f.transactions
}

func (f *writeGuardFakeDB) Get(ctx context.Context, rec record.Record) error {
	if f.absent {
		f.gets++
		rec.SetError(record.ErrRecordNotFound)
		return record.ErrRecordNotFound
	}
	return f.guardFakeDB.Get(ctx, rec)
}

func (f *writeGuardFakeDB) Exists(context.Context, *record.Key) (bool, error) {
	f.exists++
	return true, nil
}

func (f *writeGuardFakeDB) RunReadwriteTransaction(context.Context, dal.RWTxWorker, ...dal.TransactionOption) error {
	f.transactions++
	return nil
}

var (
	writeGuardSQLEngines      = []string{"sqlite", "postgres", "mysql"}
	writeGuardDocumentEngines = []string{"ingitdb", "firestore"}
)

// writeGuardServer serves one fake-backed database that declares the given
// collections (one string field "name" each). SQL engines are strict-only;
// document engines are served schemaless.
func writeGuardServer(t *testing.T, engine string, declared ...string) (*httptest.Server, *writeGuardFakeDB) {
	t.Helper()
	fake := &writeGuardFakeDB{}
	mode := schema.ModeStrict
	for _, e := range writeGuardDocumentEngines {
		if e == engine {
			mode = schema.ModeSchemaless
		}
	}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "guarded", SchemaMode: mode},
		Storage:  manifest.Storage{Engine: engine},
	}
	if len(declared) > 0 {
		m.Schemas = &schema.Schemas{Collections: map[string]schema.Collection{}}
		for _, name := range declared {
			m.Schemas.Collections[name] = schema.Collection{Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}
		}
	}
	db, err := core.Open(m, fake, []schema.Mode{mode}, filepath.Join(t.TempDir(), "inferred.json"))
	if err != nil {
		t.Fatal(err)
	}
	service := server.New("test", map[string]*core.Database{"guarded": db})
	t.Cleanup(service.CloseSnapshots)
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	return ts, fake
}

const (
	writeGuardData    = `{"data":{"name":"Ada"}}`
	writeGuardUpdates = `{"updates":[{"fieldName":"name","value":"Bob"}]}`
)

// writeGuardKeyCalls is every HTTP route that reaches an adapter by key or
// writes, for the record at keyPath (already escaped).
func writeGuardKeyCalls(keyPath string) []guardCall {
	path := base + "/records/" + keyPath
	return []guardCall{
		{name: "GET", method: "GET", path: path},
		{name: "GET read?key", method: "GET", path: base + "/read?key=" + url.QueryEscape(keyPath)},
		{name: "HEAD", method: "HEAD", path: path},
		{name: "PUT", method: "PUT", path: path, body: writeGuardData},
		{name: "POST", method: "POST", path: path, body: writeGuardData},
		{name: "PATCH", method: "PATCH", path: path, body: writeGuardUpdates},
		{name: "DELETE", method: "DELETE", path: path},
		{name: "batch set", method: "POST", path: base + "/batch", body: `{"ops":[{"op":"set","key":"customers/good","data":{"name":"Ada"}},{"op":"set","key":"` + keyPath + `","data":{"name":"Ada"}}]}`},
		{name: "batch insert", method: "POST", path: base + "/batch", body: `{"ops":[{"op":"insert","key":"` + keyPath + `","data":{"name":"Ada"}}]}`},
		{name: "batch update", method: "POST", path: base + "/batch", body: `{"ops":[{"op":"update","key":"` + keyPath + `","updates":[{"fieldName":"name","value":"Bob"}]}]}`},
		{name: "batch delete first", method: "POST", path: base + "/batch", body: `{"ops":[{"op":"delete","key":"` + keyPath + `"},{"op":"set","key":"customers/good","data":{"name":"Ada"}}]}`},
	}
}

func writeGuardErrorCode(body map[string]any) any {
	detail, _ := body["error"].(map[string]any)
	return detail["code"]
}

func TestUndeclaredCollectionIs404BeforeAdapterOnSQLEngines(t *testing.T) {
	hostile := `customers"; DROP TABLE customers; --`
	for _, engine := range writeGuardSQLEngines {
		t.Run(engine, func(t *testing.T) {
			ts, fake := writeGuardServer(t, engine, "customers")
			for label, keyPath := range map[string]string{
				"unknown":     "ghost/1",
				"hostile":     url.PathEscape(hostile) + "/1",
				"spaced":      url.PathEscape("Order Details") + "/1",
				"nested leaf": "customers/c1/ghost/g1",
				"nested root": "ghost/g1/customers/c1",
			} {
				for _, call := range writeGuardKeyCalls(keyPath) {
					status, body := send(t, ts, call)
					if status != http.StatusNotFound {
						t.Errorf("%s %s: status %d body %v", label, call.name, status, body)
					}
					if call.method != "HEAD" && writeGuardErrorCode(body) != "not_found" {
						t.Errorf("%s %s: code %v", label, call.name, writeGuardErrorCode(body))
					}
				}
			}
			if fake.reached() != 0 {
				t.Fatalf("adapter reached %d times (gets %d, exists %d, transactions %d)", fake.reached(), fake.gets, fake.exists, fake.transactions)
			}
		})
	}
}

// TestNestedKeysAre404BeforeAdapterOnSQLEngines: a SQL mount has no
// subcollections. Even with every segment declared, dalgo2sql would address
// another table than the root collection the capability was checked on (its
// delete names only the leaf table), so a key with a parent is a 404.
func TestNestedKeysAre404BeforeAdapterOnSQLEngines(t *testing.T) {
	for _, engine := range writeGuardSQLEngines {
		t.Run(engine, func(t *testing.T) {
			ts, fake := writeGuardServer(t, engine, "customers", "Order Details")
			for label, keyPath := range map[string]string{
				"same collection": "customers/c1/customers/c2",
				"spaced leaf":     "customers/c1/" + url.PathEscape("Order Details") + "/1",
				"three levels":    "customers/c1/" + url.PathEscape("Order Details") + "/1/customers/c3",
			} {
				for _, call := range writeGuardKeyCalls(keyPath) {
					status, body := send(t, ts, call)
					if status != http.StatusNotFound {
						t.Errorf("%s %s: status %d body %v", label, call.name, status, body)
					}
					if call.method != "HEAD" && writeGuardErrorCode(body) != "not_found" {
						t.Errorf("%s %s: code %v", label, call.name, writeGuardErrorCode(body))
					}
				}
			}
			if fake.reached() != 0 {
				t.Fatalf("adapter reached %d times (gets %d, exists %d, transactions %d)", fake.reached(), fake.gets, fake.exists, fake.transactions)
			}
		})
	}
}

func TestDeclaredCollectionsBehaveAsBeforeOnSQLEngines(t *testing.T) {
	want := map[string]int{
		"GET": http.StatusOK, "GET read?key": http.StatusOK, "HEAD": http.StatusOK,
		"PUT": http.StatusNoContent, "POST": http.StatusCreated, "PATCH": http.StatusNoContent, "DELETE": http.StatusNoContent,
		"batch set": http.StatusOK, "batch insert": http.StatusOK, "batch update": http.StatusOK, "batch delete first": http.StatusOK,
	}
	for _, engine := range writeGuardSQLEngines {
		for _, keyPath := range []string{"customers/c1", url.PathEscape("Order Details") + "/1"} {
			t.Run(engine+"/"+keyPath, func(t *testing.T) {
				ts, fake := writeGuardServer(t, engine, "customers", "Order Details")
				for _, call := range writeGuardKeyCalls(keyPath) {
					fake.absent = call.name == "POST" || call.name == "batch insert"
					status, body := send(t, ts, call)
					if status != want[call.name] {
						t.Errorf("%s: status %d body %v, want %d", call.name, status, body, want[call.name])
					}
				}
				if fake.exists != 1 || fake.transactions != 8 {
					t.Errorf("exists %d transactions %d, want 1 and 8", fake.exists, fake.transactions)
				}
			})
		}
	}
}

func TestKeyReadOfASpacedCollectionStillWorksOnSQLEngines(t *testing.T) {
	for _, engine := range writeGuardSQLEngines {
		ts, fake := writeGuardServer(t, engine, "Order Details")
		status, body := send(t, ts, guardCall{method: "GET", path: base + "/records/Order%20Details/10248"})
		data, _ := body["data"].(map[string]any)
		if status != http.StatusOK || body["key"] != "Order Details/10248" || data["name"] != "Ada" || fake.gets != 1 {
			t.Errorf("%s: status %d gets %d body %v", engine, status, fake.gets, body)
		}
	}
}

func TestDocumentEnginesKeepTodaysCollectionRuleOverHTTP(t *testing.T) {
	for _, engine := range writeGuardDocumentEngines {
		t.Run(engine, func(t *testing.T) {
			ts, fake := writeGuardServer(t, engine) // nothing declared: collections are implicit
			status, body := send(t, ts, guardCall{method: "GET", path: base + "/records/ghost/1"})
			if status != http.StatusOK || fake.gets != 1 {
				t.Fatalf("status %d gets %d body %v", status, fake.gets, body)
			}
			if status, body = send(t, ts, guardCall{method: "PUT", path: base + "/records/ghost/1", body: writeGuardData}); status != http.StatusNoContent || fake.transactions != 1 {
				t.Fatalf("status %d transactions %d body %v", status, fake.transactions, body)
			}
			// Subcollections are native to the document engines.
			if status, body = send(t, ts, guardCall{method: "PUT", path: base + "/records/spaces/s1/ext/contactus", body: writeGuardData}); status != http.StatusNoContent || fake.transactions != 2 {
				t.Fatalf("nested: status %d transactions %d body %v", status, fake.transactions, body)
			}
		})
	}
}

// writeGuardHostileNames are field names that must never reach an adapter on
// any engine.
var writeGuardHostileNames = []string{`na"me`, "na me", "name;", "na--me", "na/*me", "name#", "na'me", "name; DROP TABLE x", "name\\u0000", ""}

func TestHostileFieldNamesAre400BeforeAdapterOnEveryEngine(t *testing.T) {
	engines := append(append([]string{}, writeGuardSQLEngines...), writeGuardDocumentEngines...)
	for _, engine := range engines {
		t.Run(engine, func(t *testing.T) {
			ts, fake := writeGuardServer(t, engine, "customers")
			for _, name := range writeGuardHostileNames {
				// name is a JSON string body; "name\\u0000" carries a NUL escape.
				quoted := `"` + strings.ReplaceAll(name, `"`, `\"`) + `"`
				path := base + "/records/customers/c1"
				calls := []guardCall{
					{name: "PUT", method: "PUT", path: path, body: `{"data":{"name":"Ada",` + quoted + `:1}}`},
					{name: "POST", method: "POST", path: path, body: `{"data":{` + quoted + `:1}}`},
					{name: "PATCH fieldName", method: "PATCH", path: path, body: `{"updates":[{"fieldName":` + quoted + `,"value":1}]}`},
					{name: "PATCH delete-field", method: "PATCH", path: path, body: `{"updates":[{"fieldName":` + quoted + `,"delete":true}]}`},
					{name: "PATCH fieldPath", method: "PATCH", path: path, body: `{"updates":[{"fieldPath":[` + quoted + `],"value":1}]}`},
					{name: "PATCH fieldPath, first segment", method: "PATCH", path: path, body: `{"updates":[{"fieldPath":[` + quoted + `,"city"],"value":1}]}`},
					{name: "batch set", method: "POST", path: base + "/batch", body: `{"ops":[{"op":"set","key":"customers/good","data":{"name":"Ada"}},{"op":"set","key":"customers/c1","data":{` + quoted + `:1}}]}`},
					{name: "batch update", method: "POST", path: base + "/batch", body: `{"ops":[{"op":"update","key":"customers/c1","updates":[{"fieldName":` + quoted + `,"delete":true}]}]}`},
				}
				for _, call := range calls {
					status, body := send(t, ts, call)
					if status != http.StatusBadRequest || writeGuardErrorCode(body) != "bad_request" {
						t.Errorf("%s %q: status %d body %v", call.name, name, status, body)
					}
				}
			}
			if fake.reached() != 0 {
				t.Fatalf("adapter reached %d times", fake.reached())
			}
		})
	}
}

// TestLaterFieldPathSegmentsOverHTTP: the segments of an update path after the
// first are map keys. A SQL engine holds them to the plain-name rule; a
// document engine accepts what Sneat's linkage writes (`id@spaceID`) and
// refuses only an empty, blank or control-character segment, before the
// adapter either way.
func TestLaterFieldPathSegmentsOverHTTP(t *testing.T) {
	patch := func(path string) guardCall {
		return guardCall{method: "PATCH", path: base + "/records/customers/c1", body: `{"updates":[{"fieldPath":` + path + `,"value":1}]}`}
	}
	linkage := patch(`["related","contactus","contacts","c1@space2"]`)
	refusedEverywhere := []guardCall{
		patch(`["related",""]`),
		patch(`["related"," "]`),
		patch(`["related","a\u0000b"]`),
		patch(`["related","a\nb"]`),
	}
	for _, engine := range writeGuardSQLEngines {
		t.Run(engine, func(t *testing.T) {
			ts, fake := writeGuardServer(t, engine, "customers")
			for _, call := range append([]guardCall{linkage, patch(`["related","a b"]`)}, refusedEverywhere...) {
				if status, body := send(t, ts, call); status != http.StatusBadRequest || writeGuardErrorCode(body) != "bad_request" {
					t.Errorf("%s: status %d body %v", call.body, status, body)
				}
			}
			if fake.reached() != 0 {
				t.Fatalf("adapter reached %d times", fake.reached())
			}
		})
	}
	for _, engine := range writeGuardDocumentEngines {
		t.Run(engine, func(t *testing.T) {
			ts, fake := writeGuardServer(t, engine, "customers")
			for _, call := range []guardCall{linkage, patch(`["related","a b"]`)} {
				if status, body := send(t, ts, call); status != http.StatusNoContent {
					t.Errorf("%s: status %d body %v", call.body, status, body)
				}
			}
			reached := fake.reached()
			for _, call := range refusedEverywhere {
				if status, body := send(t, ts, call); status != http.StatusBadRequest || writeGuardErrorCode(body) != "bad_request" {
					t.Errorf("%s: status %d body %v", call.body, status, body)
				}
			}
			if fake.reached() != reached {
				t.Fatalf("a refused path reached the adapter %d times", fake.reached()-reached)
			}
		})
	}
}

func TestUpdateThatNamesNoFieldIs400BeforeAdapter(t *testing.T) {
	ts, fake := writeGuardServer(t, "postgres", "customers")
	status, body := send(t, ts, guardCall{method: "PATCH", path: base + "/records/customers/c1", body: `{"updates":[{"value":1}]}`})
	if status != http.StatusBadRequest || writeGuardErrorCode(body) != "bad_request" || fake.reached() != 0 {
		t.Fatalf("status %d reached %d body %v", status, fake.reached(), body)
	}
}

// TestUndeclaredCollectionWinsOverHostileFieldName: a SQL engine answers 404
// for a collection it does not declare, whatever the body carries.
func TestUndeclaredCollectionWinsOverHostileFieldName(t *testing.T) {
	ts, fake := writeGuardServer(t, "mysql", "customers")
	status, body := send(t, ts, guardCall{method: "PUT", path: base + "/records/ghost/1", body: `{"data":{"na me":1}}`})
	if status != http.StatusNotFound || writeGuardErrorCode(body) != "not_found" || fake.reached() != 0 {
		t.Fatalf("status %d reached %d body %v", status, fake.reached(), body)
	}
}

func TestPlainFieldNamesStillAcceptedOnEveryEngine(t *testing.T) {
	engines := append(append([]string{}, writeGuardSQLEngines...), writeGuardDocumentEngines...)
	for _, engine := range engines {
		t.Run(engine, func(t *testing.T) {
			ts, fake := writeGuardServer(t, engine, "customers")
			for _, call := range []guardCall{
				{method: "PUT", path: base + "/records/customers/c1", body: `{"data":{"name":"Ada"}}`},
				{method: "PATCH", path: base + "/records/customers/c1", body: `{"updates":[{"fieldName":"name","delete":true},{"fieldPath":["address","zip-code"],"value":1}]}`},
			} {
				// Strict engines reject the undeclared nested field later, with a
				// 422; what matters here is that the guard did not refuse it.
				if status, body := send(t, ts, call); status == http.StatusBadRequest || status == http.StatusNotFound {
					t.Errorf("%s: status %d body %v", call.method, status, body)
				}
			}
			if fake.gets == 0 {
				t.Fatal("adapter never reached")
			}
		})
	}
}
