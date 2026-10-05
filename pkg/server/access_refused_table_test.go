package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

const refusedTableGrantedToken = "ovdb_test_refused_table_granted"

// refusedTables are the declared tables of refusedTableServer that the protected
// session of the SQLite adapter cannot prepare, each for one reason of the file's
// own making. None of them has a rule in the owner policy, so the policy hides
// each as it hides orders.
var refusedTables = []struct{ name, reason string }{
	{"with_default", "a column default"},
	{"fk_parent", "a foreign key that points in"},
	{"fk_child", "a foreign key that points out"},
	{"with_trigger", "a trigger"},
	{"int_key", "a primary key that is not a text id"},
	{"checked_col", "a column whose name holds a reserved word"},
}

// refusedTableServer serves a real SQLite file behind an owner policy. It
// declares customers, which the policy admits for the rows of country IE, orders,
// which supports the protected session and which the policy has no rule for, and
// every table of refusedTables, which the protected session cannot prepare. The
// owner token and a token granted every table a case names (declared or not) may
// read and write.
func refusedTableServer(t *testing.T, extra ...server.Option) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(refusedTableHandler(t, extra...))
	t.Cleanup(ts.Close)
	return ts
}

// refusedTableHandler is the handler of refusedTableServer.
func refusedTableHandler(t *testing.T, extra ...server.Option) http.Handler {
	t.Helper()
	return refusedTableHandlerInRealm(t, "", extra...)
}

// refusedTableServerInRealm is refusedTableServer with the owner ACL bound to a
// principal realm. The caller's principal has no subject, so it is not in the
// realm and the policy cannot decide any request of it.
func refusedTableServerInRealm(t *testing.T, realm string, extra ...server.Option) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(refusedTableHandlerInRealm(t, realm, extra...))
	t.Cleanup(ts.Close)
	return ts
}

// refusedTableHandlerInRealm is the handler of refusedTableServerInRealm (the
// owner ACL is bound to no realm when realm is empty).
func refusedTableHandlerInRealm(t *testing.T, realm string, extra ...server.Option) http.Handler {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: crm, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n" +
		"    customers:\n      fields:\n        name: {type: string}\n        country: {type: string}\n" +
		"    orders:\n      fields:\n        total: {type: string}\n" +
		"    with_default:\n      fields:\n        name: {type: string}\n" +
		"    fk_parent:\n      fields:\n        name: {type: string}\n" +
		"    fk_child:\n      fields:\n        name: {type: string}\n        parent: {type: string}\n" +
		"    with_trigger:\n      fields:\n        name: {type: string}\n" +
		"    int_key:\n      fields:\n        name: {type: string}\n" +
		"    checked_col:\n      fields:\n        checked: {type: string}\n"
	aclWriteFile(t, path, manifest)
	raw, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE customers (id TEXT PRIMARY KEY, name TEXT, country TEXT)`,
		`INSERT INTO customers VALUES ('01', 'Ada', 'IE'), ('02', 'Bo', 'FR')`,
		`CREATE TABLE orders (id TEXT PRIMARY KEY, total TEXT)`,
		`INSERT INTO orders VALUES ('01', '10')`,
		`CREATE TABLE with_default (id TEXT PRIMARY KEY, name TEXT DEFAULT 'x')`,
		`CREATE TABLE fk_parent (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE fk_child (id TEXT PRIMARY KEY, name TEXT, parent TEXT REFERENCES fk_parent(id))`,
		`CREATE TABLE with_trigger (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TRIGGER with_trigger_touch AFTER INSERT ON with_trigger BEGIN SELECT 1; END`,
		`CREATE TABLE int_key (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE checked_col (id TEXT PRIMARY KEY, checked TEXT)`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	// One row of each of customers and orders is large (see oversizedRowBytes).
	for _, statement := range []string{
		`INSERT INTO customers VALUES ('big', ?, 'IE')`,
		`INSERT INTO orders VALUES ('big', ?)`,
	} {
		if _, err := raw.Exec(statement, strings.Repeat("x", oversizedRowBytes)); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	policy := strings.Replace(aclPolicy("upper", "country", "IE"), "operations: [query, get]", "operations: [query, get, update]", 1)
	aclWriteFile(t, filepath.Join(dir, "upper.yaml"), policy)
	acl := "acl: {enabled: true, policies: [upper.yaml]}\n"
	if realm != "" {
		acl = "acl: {enabled: true, realm: " + realm + ", policies: [upper.yaml]}\n"
	}
	aclWriteFile(t, path, manifest+acl)
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var grants []auth.Capability
	collections := []string{"customers", "orders", "ghost", "ghost2"}
	for _, c := range refusedTables {
		collections = append(collections, c.name)
	}
	for _, collection := range collections {
		grants = append(grants,
			auth.Capability{Action: auth.CapRecordsRead, Collection: collection},
			auth.Capability{Action: auth.CapRecordsWrite, Collection: collection})
	}
	grantToken(t, store, "crm", refusedTableGrantedToken, grants...)
	options := append([]server.Option{
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}),
	}, extra...)
	service := server.New("test", map[string]*core.Database{"crm": db}, options...)
	t.Cleanup(service.CloseSnapshots)
	return service.Handler()
}

const refusedEvaluate = "/v1/databases/crm/access/evaluate"

var refusedCallers = []struct{ name, token string }{{"owner", ownerToken}, {"granted token", refusedTableGrantedToken}}

// refusedInspect is the body of an inspection of the given operations, numbered
// in the order given.
func refusedInspect(ops ...api.Operation) string {
	for i := range ops {
		ops[i].ID = "op" + string(rune('1'+i))
	}
	data, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: ops})
	return string(data)
}

func refusedGet(table string) api.Operation {
	return writeGuardProtectedOp("get", "/"+table+"/01", nil)
}

func refusedUpdate(table string) api.Operation {
	return writeGuardProtectedOp("update", "/"+table+"/01", writeGuardProtectedSet("name"))
}

// refusedPatch is the protected PATCH of the record 01 of table.
func refusedPatch(table string) (path, body string) {
	data, _ := json.Marshal(refusedUpdate(table))
	return "/v1/databases/crm/records/" + url.PathEscape(table) + "/01", string(data)
}

// refusedEvidence is the evidence request for a field of the record 01 of table.
func refusedEvidence(table string) (path, body string) {
	data, _ := json.Marshal(map[string]any{"apiVersion": az.APIVersion, "resource": az.Resource{DatabaseID: "crm", Path: "/" + table + "/01"}, "requiredFields": [][]string{{"name"}}})
	return "/v1/databases/crm/access/evidence", string(data)
}

// refusedSwap rewrites the names a body repeats from the request, so that the
// answers for two requests that differ only in those names can be compared.
func refusedSwap(body string, swaps [][2]string) string {
	for _, swap := range swaps {
		body = strings.ReplaceAll(body, swap[0], swap[1])
	}
	return body
}

// TestTableTheProtectedSessionRefusesIsAnsweredAsAnUndeclaredOne: a declared
// table that the protected session of the SQLite adapter cannot prepare (a column
// default, a foreign key in or out, a trigger, a primary key that is not a text
// id, a column whose name holds a reserved word) is one answer for the caller with
// an undeclared table. The protected PATCH answers 404 resource_unavailable and an
// inspection answers 200 with the redacted deny for that operation and the facts
// of the others, the same status and the same body but for the names the caller
// sent, for the owner and for a token granted every table.
func TestTableTheProtectedSessionRefusesIsAnsweredAsAnUndeclaredOne(t *testing.T) {
	ts := refusedTableServer(t)
	for _, caller := range refusedCallers {
		t.Run(caller.name, func(t *testing.T) {
			for i, c := range refusedTables {
				t.Run(c.reason, func(t *testing.T) {
					other := refusedTables[(i+1)%len(refusedTables)].name
					swaps := [][2]string{{c.name, "ghost"}, {other, "ghost2"}}
					inspections := []struct{ name, refused, undeclared string }{
						{"inspection", refusedInspect(refusedGet(c.name)), refusedInspect(refusedGet("ghost"))},
						{"inspection of an update", refusedInspect(refusedUpdate(c.name)), refusedInspect(refusedUpdate("ghost"))},
						{"inspection after a visible record", refusedInspect(refusedGet("customers"), refusedGet(c.name)), refusedInspect(refusedGet("customers"), refusedGet("ghost"))},
						{"inspection before a visible record", refusedInspect(refusedGet(c.name), refusedGet("customers")), refusedInspect(refusedGet("ghost"), refusedGet("customers"))},
						{"inspection with a table the policy hides", refusedInspect(refusedGet(c.name), refusedGet("orders")), refusedInspect(refusedGet("ghost"), refusedGet("orders"))},
						{"inspection with another refused table", refusedInspect(refusedGet(c.name), refusedGet(other)), refusedInspect(refusedGet("ghost"), refusedGet("ghost2"))},
					}
					headers := map[string]string{"Content-Type": "application/json"}
					for _, in := range inspections {
						refused := hiddenSourceAsk(t, ts, caller.token, "POST", refusedEvaluate, in.refused, headers)
						undeclared := hiddenSourceAsk(t, ts, caller.token, "POST", refusedEvaluate, in.undeclared, headers)
						if refused.status != http.StatusOK || !strings.Contains(refused.body, `"result":"deny"`) {
							t.Errorf("%s: want the redacted deny, got %d %s", in.name, refused.status, refused.body)
						}
						if got := refusedSwap(refused.body, swaps); refused.status != undeclared.status || got != undeclared.body {
							t.Errorf("%s: declared and refused\n  %d %s\nundeclared\n  %d %s", in.name, refused.status, refused.body, undeclared.status, undeclared.body)
						}
					}
					for _, route := range []struct {
						name, method, content string
						at                    func(table string) (path, body string)
					}{
						{"PATCH", "PATCH", "application/vnd.dtql.operation+json", refusedPatch},
						{"evidence", "POST", "application/json", refusedEvidence},
					} {
						routeHeaders := map[string]string{"Content-Type": route.content}
						refusedPath, refusedBody := route.at(c.name)
						undeclaredPath, undeclaredBody := route.at("ghost")
						refused := hiddenSourceAsk(t, ts, caller.token, route.method, refusedPath, refusedBody, routeHeaders)
						undeclared := hiddenSourceAsk(t, ts, caller.token, route.method, undeclaredPath, undeclaredBody, routeHeaders)
						if refused.status != http.StatusNotFound || !strings.Contains(refused.body, `"code":"resource_unavailable"`) {
							t.Errorf("%s: want 404 resource_unavailable, got %d %s", route.name, refused.status, refused.body)
						}
						if got := refusedSwap(refused.body, swaps); refused.status != undeclared.status || got != undeclared.body {
							t.Errorf("%s: declared and refused\n  %d %s\nundeclared\n  %d %s", route.name, refused.status, refused.body, undeclared.status, undeclared.body)
						}
					}
				})
			}
		})
	}
}

// TestHiddenTableGetsNoLayerDetailForAPrincipalWhoMayInspectProtectedRows: a
// deployment binding can allow a principal to inspect protected rows. That shows
// why a record the principal cannot read is refused, but for no operation whose
// table is hidden from it, declared or not: a declared table the policy has no
// rule for (and a declared table the protected session cannot prepare) is
// answered as an undeclared table is, with the same status and the same body but
// for the names sent. A record of a table the policy admits keeps its layer
// detail.
func TestHiddenTableGetsNoLayerDetailForAPrincipalWhoMayInspectProtectedRows(t *testing.T) {
	bindings := []struct {
		name   string
		allows func(capability string) bool
	}{
		{"protected inspection only", func(capability string) bool { return capability == auth.CapAccessInspectProtected }},
		{"every capability", func(string) bool { return true }},
	}
	hiddenTables := []string{"orders"}
	for _, c := range refusedTables {
		hiddenTables = append(hiddenTables, c.name)
	}
	for _, binding := range bindings {
		t.Run(binding.name, func(t *testing.T) {
			ts := refusedTableServer(t, server.WithOwnerAuthorization(func(_ context.Context, _ *auth.Principal, _ az.Source, capability string, _ az.Resource) bool {
				return binding.allows(capability)
			}))
			headers := map[string]string{"Content-Type": "application/json"}
			for _, caller := range refusedCallers {
				t.Run(caller.name, func(t *testing.T) {
					for _, table := range hiddenTables {
						swaps := [][2]string{{table, "ghost"}}
						for _, in := range []struct{ name, hidden, undeclared string }{
							{"inspection", refusedInspect(refusedGet(table)), refusedInspect(refusedGet("ghost"))},
							{"inspection of an update", refusedInspect(refusedUpdate(table)), refusedInspect(refusedUpdate("ghost"))},
							{"inspection after a visible record", refusedInspect(refusedGet("customers"), refusedGet(table)), refusedInspect(refusedGet("customers"), refusedGet("ghost"))},
							{"inspection before a visible record", refusedInspect(refusedGet(table), refusedGet("customers")), refusedInspect(refusedGet("ghost"), refusedGet("customers"))},
						} {
							hidden := hiddenSourceAsk(t, ts, caller.token, "POST", refusedEvaluate, in.hidden, headers)
							undeclared := hiddenSourceAsk(t, ts, caller.token, "POST", refusedEvaluate, in.undeclared, headers)
							if hidden.status != http.StatusOK || !strings.Contains(hidden.body, `"result":"deny"`) {
								t.Errorf("%s of %s: want the redacted deny, got %d %s", in.name, table, hidden.status, hidden.body)
							}
							if got := refusedSwap(hidden.body, swaps); hidden.status != undeclared.status || got != undeclared.body {
								t.Errorf("%s of %s: declared\n  %d %s\nundeclared\n  %d %s", in.name, table, hidden.status, hidden.body, undeclared.status, undeclared.body)
							}
						}
					}
					// A record of a table the policy admits, which this caller may not
					// read (the row is not one the policy admits, or there is none),
					// is not a hidden table: its layer detail is given.
					undeclared := hiddenSourceAsk(t, ts, caller.token, "POST", refusedEvaluate, refusedInspect(refusedGet("ghost")), headers)
					for _, row := range []string{"02", "zz"} {
						op := writeGuardProtectedOp("get", "/customers/"+row, nil)
						got := hiddenSourceAsk(t, ts, caller.token, "POST", refusedEvaluate, refusedInspect(op), headers)
						if got.status != http.StatusOK || got.body == refusedSwap(undeclared.body, [][2]string{{"ghost", "customers"}, {"/01", "/" + row}, {`"rowId":"01"`, `"rowId":"` + row + `"`}}) ||
							!strings.Contains(got.body, `"evaluation":"complete"`) || !strings.Contains(got.body, `"decisions":[{`) {
							t.Errorf("customers/%s: want the layer detail, got %d %s", row, got.status, got.body)
						}
					}
				})
			}
		})
	}
}

// TestProtectedRouteThatFailsForAnotherReasonIsUnavailable: only a refusal of the
// protected session before anything is assessed is answered as a table the caller
// may not see. A session that cannot be used at all (here the request was
// canceled before the session began) is still 503 authorization_unavailable, on
// the inspection and on the protected PATCH.
func TestProtectedRouteThatFailsForAnotherReasonIsUnavailable(t *testing.T) {
	handler := refusedTableHandler(t)
	patchPath, patchBody := refusedPatch("customers")
	for _, route := range []struct{ name, method, path, body, content string }{
		{"inspection", "POST", refusedEvaluate, refusedInspect(refusedGet("customers")), "application/json"},
		{"PATCH", "PATCH", patchPath, patchBody, "application/vnd.dtql.operation+json"},
	} {
		t.Run(route.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+ownerToken)
			req.Header.Set("Content-Type", route.content)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"authorization_unavailable"`) {
				t.Errorf("want 503 authorization_unavailable, got %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// refusedLog is a log that a server writes while a test reads it.
type refusedLog struct {
	mu   sync.Mutex
	logs bytes.Buffer
}

func (l *refusedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.logs.Write(p)
}

// records returns the records written since the last call, one JSON object each.
func (l *refusedLog) records(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		records = append(records, record)
	}
	l.logs.Reset()
	return records
}

// TestTableTheProtectedSessionRefusesIsLoggedOncePerRequest: the answer for a
// declared table that the protected session refuses an operation on says nothing of
// why, so the server logs one warning for the request, with the method, the route
// path and the names of the collections, for the inspection, for the protected
// PATCH and for the evidence route. A table the database does not declare, and one
// the session prepares, are not logged.
func TestTableTheProtectedSessionRefusesIsLoggedOncePerRequest(t *testing.T) {
	var logs refusedLog
	ts := refusedTableServer(t, server.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	patchPath, patchBody := refusedPatch("with_default")
	ghostPatchPath, ghostPatchBody := refusedPatch("ghost")
	evidencePath, evidenceBody := refusedEvidence("with_default")
	_, ghostEvidenceBody := refusedEvidence("ghost")
	_, preparedEvidenceBody := refusedEvidence("customers")
	for _, c := range []struct {
		name, method, path, body, content string
		want                              []string
	}{
		{"inspection", "POST", refusedEvaluate, refusedInspect(refusedGet("with_default")), "application/json", []string{"with_default"}},
		{"inspection of the same table twice", "POST", refusedEvaluate, refusedInspect(refusedGet("with_default"), refusedUpdate("with_default")), "application/json", []string{"with_default"}},
		{"inspection of two tables", "POST", refusedEvaluate, refusedInspect(refusedGet("with_trigger"), refusedGet("customers"), refusedGet("with_default")), "application/json", []string{"with_default", "with_trigger"}},
		{"PATCH", "PATCH", patchPath, patchBody, "application/vnd.dtql.operation+json", []string{"with_default"}},
		{"evidence", "POST", evidencePath, evidenceBody, "application/json", []string{"with_default"}},
		{"evidence of an undeclared table", "POST", evidencePath, ghostEvidenceBody, "application/json", nil},
		{"evidence of a table the session prepares", "POST", evidencePath, preparedEvidenceBody, "application/json", nil},
		{"inspection of a table the session prepares", "POST", refusedEvaluate, refusedInspect(refusedGet("customers"), refusedGet("orders")), "application/json", nil},
		{"inspection of an undeclared table", "POST", refusedEvaluate, refusedInspect(refusedGet("ghost")), "application/json", nil},
		{"PATCH of an undeclared table", "PATCH", ghostPatchPath, ghostPatchBody, "application/vnd.dtql.operation+json", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs.records(t)
			answer := hiddenSourceAsk(t, ts, ownerToken, c.method, c.path, c.body, map[string]string{"Content-Type": c.content})
			records := logs.records(t)
			if c.want == nil {
				if len(records) != 0 {
					t.Errorf("logged %v", records)
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("logged %d records: %v", len(records), records)
			}
			record := records[0]
			var collections []string
			for _, name := range record["collections"].([]any) {
				collections = append(collections, name.(string))
			}
			if record["level"] != "WARN" || record["method"] != c.method || record["path"] != c.path || strings.Join(collections, ",") != strings.Join(c.want, ",") {
				t.Errorf("logged %v, want the collections %v for %s %s", record, c.want, c.method, c.path)
			}
			if message, _ := record["msg"].(string); strings.Contains(answer.body, message) {
				t.Errorf("the response repeats the log message: %s", answer.body)
			}
		})
	}
}
