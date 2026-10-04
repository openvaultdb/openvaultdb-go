package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// writeGuardProtectedServer serves a real SQLite mount behind an owner ACL, the
// only engine that has the protected execution and inspection paths (they
// reach the adapter by key through a coordinator, not through core.Get/Apply).
// It declares one collection, customers, with one record, 01.
func writeGuardProtectedServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: crm, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n        country: {type: string}\n"
	aclWriteFile(t, path, manifest)
	seed, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seed.Apply(context.Background(), []core.Op{{Op: "insert", Key: record.NewKeyWithID("customers", "01"), Data: map[string]any{"name": "Original", "country": "IE"}}}, "seed"); err != nil {
		t.Fatal(err)
	}
	if err = seed.Close(); err != nil {
		t.Fatal(err)
	}
	policy := strings.Replace(aclPolicy("upper", "country", "IE"), "visibility: private", "visibility: public", 1)
	policy = strings.Replace(policy, "operations: [query, get]", "operations: [query, get, update]", 1)
	aclWriteFile(t, filepath.Join(dir, "upper.yaml"), policy)
	aclWriteFile(t, path, manifest+"acl: {enabled: true, policies: [upper.yaml]}\n")
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ts := httptest.NewServer(server.New("test", map[string]*core.Database{"crm": db},
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken}),
		server.WithOwnerAuthorization(func(_ context.Context, _ *auth.Principal, _ az.Source, capability string, _ az.Resource) bool {
			return capability == auth.CapAccessDiagnostics
		})).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// writeGuardProtectedOp is an authorization-API operation on the resource at
// path, with the given action and mutation.
func writeGuardProtectedOp(action, path string, mutation *api.Mutation) api.Operation {
	return api.Operation{ID: "op1", Action: action, Resource: az.Resource{DatabaseID: "crm", Path: path}, ExecutionClass: az.ExecutionDTQL, Mutation: mutation}
}

func writeGuardProtectedSet(path ...string) *api.Mutation {
	return &api.Mutation{Changes: []api.Change{{Op: "set", Path: path, Value: json.RawMessage(`"x"`)}}}
}

// TestProtectedPathsRefuseUndeclaredTablesAndHostileNamesBeforeTheAdapter: the
// protected PATCH of /records and the authorization endpoints that read by key
// (inspect, evidence, sample) get the same refusals as the plain records
// routes, ahead of the coordinator that reads or writes the adapter. A mount
// with access policies hides which tables it declares (it refuses schema
// discovery), so an undeclared table is answered like GET answers it: 404
// resource_unavailable, redacted, with no message that names the table.
func TestProtectedPathsRefuseUndeclaredTablesAndHostileNamesBeforeTheAdapter(t *testing.T) {
	ts := writeGuardProtectedServer(t)
	call := func(method, path, content string, body any) (int, string) {
		t.Helper()
		data, _ := json.Marshal(body)
		return writeGuardRequest(t, ts, method, path, content, string(data))
	}
	inspect := func(op api.Operation) any {
		return api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: []api.Operation{op}}
	}
	sample := func(table string) any {
		op := writeGuardProtectedOp("update", "/"+table, writeGuardProtectedSet("name"))
		return api.Request{APIVersion: az.APIVersion, Mode: az.ModeSample, DiagnosticLevel: "ordinary", Operations: []api.Operation{op},
			Sample: &api.Sample{Limit: 1, Query: api.Query{Format: "dtql-yaml", Text: "from: {name: " + table + "}\n"}}}
	}
	evidence := func(path string, field ...string) any {
		return map[string]any{"apiVersion": az.APIVersion, "resource": az.Resource{DatabaseID: "crm", Path: path}, "requiredFields": [][]string{field}}
	}
	const vnd = "application/vnd.dtql.operation+json"
	const evaluate = "/v1/databases/crm/access/evaluate"
	const evidencePath = "/v1/databases/crm/access/evidence"
	hostileTable := `cus"tomers; --`
	for _, c := range []struct {
		name         string
		method, path string
		content      string
		body         any
		status       int
		code         string
	}{
		// A protected mount answers a read of an undeclared table exactly as it
		// answers a hidden or missing record, on every route that takes a table.
		{"GET undeclared table", "GET", "/v1/databases/crm/records/ghost/01", "application/json", nil, 404, "resource_unavailable"},
		{"GET hostile table", "GET", "/v1/databases/crm/records/cus%22tomers%3B%20--/01", "application/json", nil, 404, "resource_unavailable"},
		{"PATCH undeclared table", "PATCH", "/v1/databases/crm/records/ghost/01", vnd, writeGuardProtectedOp("update", "/ghost/01", writeGuardProtectedSet("name")), 404, "resource_unavailable"},
		{"PATCH hostile table", "PATCH", "/v1/databases/crm/records/cus%22tomers%3B%20--/01", vnd, writeGuardProtectedOp("update", "/"+hostileTable+"/01", writeGuardProtectedSet("name")), 404, "resource_unavailable"},
		{"PATCH hostile column", "PATCH", "/v1/databases/crm/records/customers/01", vnd, writeGuardProtectedOp("update", "/customers/01", writeGuardProtectedSet(`na"me`)), 400, "bad_request"},
		{"PATCH hostile nested column", "PATCH", "/v1/databases/crm/records/customers/01", vnd, writeGuardProtectedOp("update", "/customers/01", writeGuardProtectedSet("name", "x; DROP TABLE y")), 400, "bad_request"},
		{"evidence undeclared table", "POST", evidencePath, "application/json", evidence("/ghost/01", "name"), 404, "resource_unavailable"},
		{"evidence hostile table", "POST", evidencePath, "application/json", evidence("/"+hostileTable+"/01", "name"), 404, "resource_unavailable"},
		{"evidence hostile column", "POST", evidencePath, "application/json", evidence("/customers/01", `na"me`), 400, "bad_request"},
		{"inspect undeclared table", "POST", evaluate, "application/json", inspect(writeGuardProtectedOp("update", "/ghost/01", writeGuardProtectedSet("name"))), 404, "resource_unavailable"},
		{"inspect hostile table", "POST", evaluate, "application/json", inspect(writeGuardProtectedOp("get", "/"+hostileTable+"/01", nil)), 404, "resource_unavailable"},
		{"inspect hostile change", "POST", evaluate, "application/json", inspect(writeGuardProtectedOp("update", "/customers/01", writeGuardProtectedSet("na me"))), 400, "bad_request"},
		{"inspect hostile insert data", "POST", evaluate, "application/json", inspect(writeGuardProtectedOp("insert", "/customers/02", &api.Mutation{Data: map[string]any{"name": "Ada", `na"me`: 1}})), 400, "bad_request"},
		{"inspect hostile set data", "POST", evaluate, "application/json", inspect(writeGuardProtectedOp("set", "/customers/02", &api.Mutation{Data: map[string]any{"na;me": 1}})), 400, "bad_request"},
		{"inspect hostile evidence column", "POST", evaluate, "application/json", inspect(func() api.Operation {
			op := writeGuardProtectedOp("get", "/customers/01", nil)
			op.Resource.Columns = [][]string{{"name"}, {`na"me`}}
			return op
		}()), 400, "bad_request"},
		{"sample undeclared table", "POST", evaluate, "application/json", sample("ghost"), 404, "resource_unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := call(c.method, c.path, c.content, c.body)
			var out struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal([]byte(body), &out)
			if status != c.status || out.Error.Code != c.code {
				t.Fatalf("status %d code %q, want %d %q: %s", status, out.Error.Code, c.status, c.code, body)
			}
			if c.code == "resource_unavailable" && out.Error.Message != "" {
				t.Fatalf("a redacted refusal carries a message: %s", body)
			}
		})
	}
}

func writeGuardRequest(t *testing.T, ts *httptest.Server, method, path, content, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	req.Header.Set("Content-Type", content)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}
