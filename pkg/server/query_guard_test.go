package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// guardFakeDB is a dal.DB that counts query-path calls and serves one record
// by key. Every other method panics (nil embedded DB), so a refused request
// that strays onto any other adapter path fails loudly.
type guardFakeDB struct {
	dal.DB
	queries int
	gets    int
}

func (f *guardFakeDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	f.queries++
	return nil, io.ErrUnexpectedEOF
}

func (f *guardFakeDB) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	f.queries++
	return nil, io.ErrUnexpectedEOF
}

func (f *guardFakeDB) Get(_ context.Context, rec record.Record) error {
	f.gets++
	rec.SetError(nil)
	rec.Data().(map[string]any)["name"] = "Ada"
	return nil
}

func guardServer(t *testing.T, engine string) (*httptest.Server, *guardFakeDB) {
	t.Helper()
	fake := &guardFakeDB{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "guarded", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
	}
	db, err := core.Open(m, fake, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	service := server.New("test", map[string]*core.Database{"guarded": db})
	t.Cleanup(service.CloseSnapshots)
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	return ts, fake
}

type guardCall struct {
	name    string
	method  string
	path    string
	body    string
	headers map[string]string
}

func send(t *testing.T, ts *httptest.Server, c guardCall) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(c.method, ts.URL+c.path, strings.NewReader(c.body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const base = "/v1/databases/guarded"

// queryEndpoints is every HTTP route that builds a structured query.
func queryEndpoints() []guardCall {
	wire := `{"collection":"customers"}`
	dtql := "from: {name: customers}\n"
	return []guardCall{
		{name: "query GET", method: "GET", path: base + "/query?q=" + url.QueryEscape(wire)},
		{name: "query POST", method: "POST", path: base + "/query", body: wire},
		{name: "query keysOnly", method: "POST", path: base + "/query", body: `{"collection":"customers","keysOnly":true}`},
		{name: "dtql GET", method: "GET", path: base + "/dtql?q=" + url.QueryEscape(dtql)},
		{name: "dtql POST", method: "POST", path: base + "/dtql", body: dtql},
		{name: "dtql POST JSON parameters", method: "POST", path: base + "/dtql",
			body:    `{"query":"from: {name: customers}\nwhere: {op: '==', left: {field: name}, right: {param: n}}\n","parameters":{"n":"x"}}`,
			headers: map[string]string{"Content-Type": "application/json"}},
		{name: "dtql snapshot page", method: "POST", path: base + "/dtql", body: dtql,
			headers: map[string]string{"OVDB-Page-Size": "10"}},
	}
}

func TestStructuredQueryRefusedOnPostgresAndMySQLEndpoints(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			ts, fake := guardServer(t, engine)
			for _, call := range queryEndpoints() {
				status, body := send(t, ts, call)
				detail, _ := body["error"].(map[string]any)
				message, _ := detail["message"].(string)
				if status != http.StatusNotImplemented || detail["code"] != "query_unsupported" {
					t.Errorf("%s: status %d body %v", call.name, status, body)
				}
				if !strings.Contains(message, engine) || !strings.Contains(message, "not yet supported") {
					t.Errorf("%s: message must name the engine and say it is not yet supported: %q", call.name, message)
				}
			}
			if fake.queries != 0 {
				t.Fatalf("adapter query path called %d times", fake.queries)
			}
		})
	}
}

func TestKeyReadsStillWorkOnPostgresAndMySQL(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		ts, fake := guardServer(t, engine)
		status, body := send(t, ts, guardCall{method: "GET", path: base + "/records/customers/1"})
		if status != http.StatusOK || fake.gets != 1 || fake.queries != 0 {
			t.Errorf("%s: status %d gets %d queries %d body %v", engine, status, fake.gets, fake.queries, body)
		}
	}
}

func TestUnsafeFieldNamesRefusedOnEveryEngine(t *testing.T) {
	names := []string{`na"me`, "na me", "name;", "na--me", "na/*me", "name#", "na'me"}
	for _, engine := range []string{"sqlite", "ingitdb", "postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			ts, fake := guardServer(t, engine)
			for _, name := range names {
				wireWhere, _ := json.Marshal(map[string]any{"collection": "customers", "where": []map[string]any{{"field": name, "op": "==", "value": 1}}})
				wireOrder, _ := json.Marshal(map[string]any{"collection": "customers", "orderBy": []map[string]any{{"field": name}}})
				yamlName, _ := json.Marshal(name) // JSON strings are valid YAML flow scalars
				calls := []guardCall{
					{name: "query where", method: "POST", path: base + "/query", body: string(wireWhere)},
					{name: "query orderBy", method: "POST", path: base + "/query", body: string(wireOrder)},
					{name: "query GET where", method: "GET", path: base + "/query?q=" + url.QueryEscape(string(wireWhere))},
					{name: "dtql where", method: "POST", path: base + "/dtql", body: "from: {name: customers}\nwhere: {op: '==', left: {field: " + string(yamlName) + "}, right: {value: 1}}\n"},
					{name: "dtql orderBy", method: "POST", path: base + "/dtql", body: "from: {name: customers}\norderBy: [{field: " + string(yamlName) + "}]\n"},
					{name: "dtql column", method: "GET", path: base + "/dtql?q=" + url.QueryEscape("from: {name: customers}\ncolumns: [{field: "+string(yamlName)+"}]\n")},
					{name: "dtql snapshot", method: "POST", path: base + "/dtql", body: "from: {name: customers}\norderBy: [{field: " + string(yamlName) + "}]\n", headers: map[string]string{"OVDB-Page-Size": "5"}},
				}
				for _, call := range calls {
					status, body := send(t, ts, call)
					if status != http.StatusBadRequest {
						t.Errorf("%s %q: status %d body %v", call.name, name, status, body)
					}
				}
			}
			if fake.queries != 0 {
				t.Fatalf("adapter query path called %d times", fake.queries)
			}
		})
	}
}

// TestAdvertisedQueryCapabilityMatchesGuard checks that metadata never tells a
// client a mount is queryable when the guard refuses structured queries on it.
func TestAdvertisedQueryCapabilityMatchesGuard(t *testing.T) {
	for engine, want := range map[string]bool{"sqlite": true, "ingitdb": true, "firestore": true, "postgres": false, "mysql": false} {
		t.Run(engine, func(t *testing.T) {
			ts, _ := guardServer(t, engine)
			status, body := send(t, ts, guardCall{method: "GET", path: base})
			caps, _ := body["capabilities"].(map[string]any)
			if status != http.StatusOK || caps["query"] != want || caps["dtql"] != want || caps["read"] != true {
				t.Errorf("database metadata: status %d capabilities %v, want query/dtql %v", status, caps, want)
			}
			status, body = send(t, ts, guardCall{method: "GET", path: "/.well-known/openvaultdb"})
			dbs, _ := body["databases"].([]any)
			if status != http.StatusOK || len(dbs) != 1 {
				t.Fatalf("well-known: status %d body %v", status, body)
			}
			entry, _ := dbs[0].(map[string]any)
			caps, _ = entry["capabilities"].(map[string]any)
			if caps["query"] != want || caps["dtql"] != want || caps["read"] != true {
				t.Errorf("well-known capabilities %v, want query/dtql %v", caps, want)
			}
		})
	}
}
