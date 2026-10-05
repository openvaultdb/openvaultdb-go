package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

func immutableProfileFixture(t *testing.T, id string, configured bool) *core.Database {
	t.Helper()
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{`CREATE TABLE things (id TEXT, __ovdb_record_id TEXT, __ovdb_record_id_1 TEXT UNIQUE, name TEXT REFERENCES hidden(id))`, `INSERT INTO things VALUES ('native-c', 'wrong-a', 'c', 'gamma'), ('native-a','wrong-c','a/b:quote.$#[]','alpha'), ('native-b','wrong-b','b','beta')`, `CREATE TABLE hidden (id TEXT PRIMARY KEY)`, `INSERT INTO hidden VALUES ('secret')`}
	if !configured {
		statements = []string{`CREATE TABLE things (id TEXT PRIMARY KEY, name TEXT)`, `INSERT INTO things VALUES ('a', 'alpha')`}
	}
	for _, stmt := range statements {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	options := ""
	fields := "id: {type: string}, name: {type: string}"
	if configured {
		options = "\n  sqlite: {busy_timeout: 0s, record_keys: {things: __ovdb_record_id_1}}"
		fields += ", __ovdb_record_id: {type: string}, __ovdb_record_id_1: {type: string}"
	}
	text := fmt.Sprintf("database: {id: %s, schema_mode: strict}\nstorage:\n  engine: sqlite\n  path: data.sqlite%s\nschemas: {collections: {things: {fields: {%s}}}}\n", id, options, fields)
	file := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func profileRequest(handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	return out
}
func profileDocument(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func profileServer(t *testing.T, published bool) (*Server, *core.Database, *core.Database) {
	t.Helper()
	w1 := immutableProfileFixture(t, "w1", true)
	legacy := immutableProfileFixture(t, "legacy", false)
	srv, err := NewChecked("test", map[string]*core.Database{"w1": w1, "legacy": legacy}, WithReadOnly(true), WithDatabaseReadProfiles(map[string]ReadProfile{"w1": {Kind: BoundedImmutable, AllowOrdinaryQuery: true, PublishedQuery: published}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.CloseSnapshots)
	return srv, w1, legacy
}
func TestImmutableProfileStableProjectedPagesAndNativeID(t *testing.T) {
	srv, _, _ := profileServer(t, true)
	handler := srv.Handler()
	var keys []string
	for offset := 0; offset < 3; offset++ {
		doc := fmt.Sprintf("from: {database: w1, schema: main, name: things}\ncolumns: [{field: id}, {field: name}]\nlimit: 1\noffset: %d\n", offset)
		response := profileRequest(handler, "POST", "/v1/databases/w1/dtql", doc, nil)
		if response.Code != 200 {
			t.Fatalf("page %d: %d %s", offset, response.Code, response.Body.String())
		}
		records := profileDocument(t, response)["records"].([]any)
		if len(records) != 1 {
			t.Fatal(records)
		}
		row := records[0].(map[string]any)
		keys = append(keys, row["key"].(string))
		data := row["data"].(map[string]any)
		if len(data) != 2 || !strings.HasPrefix(data["id"].(string), "native-") {
			t.Fatalf("native values/projection: %#v", data)
		}
	}
	expected := []string{"things/a%2Fb:quote%2E%24%23%5B%5D", "things/b", "things/c"}
	// Compare IDs through the existing escaped record-key parser.
	var ids []any
	for _, key := range keys {
		parsed, err := core.ParseKeyPath(key)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, parsed.ID)
	}
	if !reflect.DeepEqual(ids, []any{"a/b:quote.$#[]", "b", "c"}) {
		t.Fatalf("unstable pages: %v (transport %v; example %v)", ids, keys, expected)
	}
	keyURL, err := url.Parse("/v1/databases/w1/records/" + keys[0])
	if err != nil {
		t.Fatal(err)
	}
	keyed := profileRequest(handler, "GET", keyURL.String(), "", nil)
	if keyed.Code != 200 || !strings.Contains(keyed.Body.String(), "native-a") {
		t.Fatalf("key roundtrip: %d %s", keyed.Code, keyed.Body.String())
	}
	doc := "from: {name: things}\nwhere: {op: '==', left: {field: id}, right: {value: native-b}}\ncolumns: [{field: name}]\nlimit: 1\n"
	response := profileRequest(handler, "GET", "/v1/databases/w1/dtql?q="+url.QueryEscape(doc), "", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "beta") {
		t.Fatalf("predicate preserved: %d %s", response.Code, response.Body.String())
	}
	for _, header := range pagingHeaders {
		resp := profileRequest(handler, "POST", "/v1/databases/w1/dtql", "from: {name: things}\n", map[string]string{header: "1"})
		if resp.Code != 422 || !strings.Contains(resp.Body.String(), "snapshot_unsupported") {
			t.Fatalf("snapshot %s bypass: %d %s", header, resp.Code, resp.Body.String())
		}
	}
	for _, field := range []string{"id", "__ovdb_record_id_1"} {
		resp := profileRequest(handler, "POST", "/v1/databases/w1/dtql", "from: {name: things}\norderBy: [{field: "+field+"}]\n", nil)
		if resp.Code != 400 || !strings.Contains(resp.Body.String(), "ordering_unsupported") {
			t.Fatalf("ordering: %d %s", resp.Code, resp.Body.String())
		}
	}
	if len(srv.snapshots) != 0 || srv.snapshotSlots != 0 {
		t.Fatal("immutable query allocated a snapshot")
	}
	if files, err := os.ReadDir(srv.snapshotDir); err != nil || len(files) != 0 {
		t.Fatalf("immutable spool files: %v %v", files, err)
	}
	alternate := profileRequest(handler, "POST", "/v1/databases/w1/query", `{"collection":"things","limit":100000}`, nil)
	if alternate.Code != 422 {
		t.Fatalf("alternate route bypass: %d", alternate.Code)
	}
}
func TestImmutableProfileRelationalRefusalUsesAllResolvedSourcesBeforeExecution(t *testing.T) {
	srv, w1, legacy := profileServer(t, true)
	calls := 0
	srv.joinExecute = func(context.Context, dal.StructuredQuery, joinexec.Profile, string, joinexec.Registry, joinexec.Authorize, joinexec.Limits, ...joinexec.Option) (joinexec.Result, error) {
		calls++
		return joinexec.Result{}, nil
	}
	if srv.relationalRefusal(w1.ID(), w1) == nil || srv.advertisesJoins(w1) {
		t.Fatal("W1 relational admission disagrees")
	}
	if srv.relationalRefusal(legacy.ID(), legacy) != nil || !srv.advertisesJoins(legacy) {
		t.Fatal("legacy admission changed")
	}
	queries := []struct{ path, doc string }{
		{"/v1/databases/w1/dtql", "from: {name: things, alias: t}\n"},
		{"/v1/databases/w1/dtql", "from: {name: things}\nwhere: {exists: {query: {from: {name: things, alias: other}}}}\n"},
		{"/v1/databases/w1/dtql", "from: {name: things}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\n"},
		{"/v1/dtql", "from: {database: w1, name: things}\n"},
		{"/v1/dtql", "from: {database: legacy, name: things}\nwhere: {exists: {query: {from: {database: w1, name: things}}}}\n"},
		{"/v1/dtql", "from: {database: legacy, name: things, alias: l, joins: [{from: {database: w1, name: things, alias: w}, on: [{op: '==', left: {field: id, source: l}, right: {field: id, source: w}}]}]}\n"},
		{"/v1/dtql", "from: {query: {as: d, from: {database: w1, name: things}}}\n"},
	}
	for _, q := range queries {
		resp := profileRequest(srv.Handler(), "POST", q.path, q.doc, nil)
		if resp.Code != 422 || !strings.Contains(resp.Body.String(), "read_profile_unsupported") {
			t.Fatalf("resolved source bypass: %d %s for %s", resp.Code, resp.Body.String(), q.doc)
		}
	}
	if calls != 0 {
		t.Fatalf("W1 executor called %d times", calls)
	}
	for _, name := range []string{"hidden", "HIDDEN", `"hidden"`} {
		resp := profileRequest(srv.Handler(), "POST", "/v1/databases/w1/dtql", "from: {name: '"+name+"'}\n", nil)
		if resp.Code != 404 {
			t.Fatalf("undeclared %s read: %d %s", name, resp.Code, resp.Body.String())
		}
	}
	resp := profileRequest(srv.Handler(), "POST", "/v1/dtql", "from: {database: legacy, name: things}\n", nil)
	if resp.Code != 200 || calls != 1 {
		t.Fatalf("legacy executor behavior: %d, calls %d", resp.Code, calls)
	}
}
func TestImmutableProfileCandidatePublishedAndMixedLegacyDiscovery(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) {
			srv, _, legacy := profileServer(t, published)
			handler := srv.Handler()
			reference := New("test", map[string]*core.Database{"legacy": legacy}, WithReadOnly(true))
			t.Cleanup(reference.CloseSnapshots)
			if !reflect.DeepEqual(srv.queryProfile(), reference.queryProfile()) {
				t.Fatal("global query features changed")
			}
			for _, id := range []string{"w1", "legacy"} {
				metadata := profileDocument(t, profileRequest(handler, "GET", "/v1/databases/"+id, "", nil))
				caps := metadata["capabilities"].(map[string]any)
				queryable := published || id == "legacy"
				joins := id == "legacy"
				if id == "w1" && !reflect.DeepEqual(metadata["collections"], []any{"things"}) {
					t.Fatalf("undeclared collection metadata: %#v", metadata)
				}
				if caps["query"] != queryable || caps["dtql"] != queryable || caps["joins"] != joins || caps["aggregation"] != joins {
					t.Fatalf("capabilities %s: %#v", id, caps)
				}
				_, endpoint := metadata["endpoints"]
				_, format := metadata["queryFormat"]
				if endpoint != queryable || format != queryable {
					t.Fatalf("endpoint/format %s: %#v", id, metadata)
				}
				html := profileRequest(handler, "GET", "/ovdb/dbs/"+id+"/collections/things", "", nil).Body.String()
				if strings.Contains(html, "Query records (JSON)") != (id == "legacy") {
					t.Fatalf("human link %s", id)
				}
				if id == "w1" && strings.Contains(html, "hidden") {
					t.Fatal("undeclared FK target exposed in human collection route")
				}
				if id == "w1" && (!strings.Contains(html, "Database metadata API") || !strings.Contains(html, "__ovdb_record_id_1")) {
					t.Fatal("human metadata hidden")
				}
			}
			discovery := profileDocument(t, profileRequest(handler, "GET", "/.well-known/openvaultdb", "", nil))
			for _, value := range discovery["databases"].([]any) {
				item := value.(map[string]any)
				caps := item["capabilities"].(map[string]any)
				id := item["id"].(string)
				if caps["query"] != (published || id == "legacy") || caps["joins"] != (id == "legacy") {
					t.Fatalf("discovery: %#v", item)
				}
			}
			smoke := profileRequest(handler, "POST", "/v1/databases/w1/dtql", "from: {name: things}\nlimit: 1\n", nil)
			if smoke.Code != 200 {
				t.Fatalf("candidate smoke: %d %s", smoke.Code, smoke.Body.String())
			}
		})
	}
}
func TestImmutableProfileInvalidConstructionFailsClosedAndCannotRemountLegacy(t *testing.T) {
	db := immutableProfileFixture(t, "w1", true)
	legacy := immutableProfileFixture(t, "w1", false)
	for _, tc := range []struct {
		db       *core.Database
		readOnly bool
		profile  ReadProfile
	}{
		{db, true, ReadProfile{Kind: "unknown", AllowOrdinaryQuery: true}},
		{db, false, ReadProfile{Kind: BoundedImmutable, AllowOrdinaryQuery: true}},
		{legacy, true, ReadProfile{Kind: BoundedImmutable, AllowOrdinaryQuery: true}},
		{db, true, ReadProfile{Kind: BoundedImmutable, PublishedQuery: true}},
	} {
		opts := []Option{WithReadOnly(tc.readOnly), WithDatabaseReadProfiles(map[string]ReadProfile{"w1": tc.profile})}
		if srv, err := NewChecked("test", map[string]*core.Database{"w1": tc.db}, opts...); err == nil || srv != nil {
			t.Fatal("invalid checked construction accepted")
		}
		srv := New("test", map[string]*core.Database{"w1": tc.db}, opts...)
		t.Cleanup(srv.CloseSnapshots)
		resp := profileRequest(srv.Handler(), "POST", "/v1/databases/w1/dtql", "from: {name: things}\n", nil)
		if resp.Code != 500 || !strings.Contains(resp.Body.String(), "configuration_error") {
			t.Fatalf("invalid option not visible/fail closed: %d", resp.Code)
		}
	}
	profile := map[string]ReadProfile{"w1": {Kind: BoundedImmutable, AllowOrdinaryQuery: true, PublishedQuery: true}}
	dbs := map[string]*core.Database{"w1": db}
	srv, err := NewChecked("test", dbs, WithReadOnly(true), WithDatabaseReadProfiles(profile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.CloseSnapshots)
	profile["w1"] = ReadProfile{}
	dbs["w1"] = legacy
	if !srv.boundedImmutable(db) || srv.getDB("w1") != db {
		t.Fatal("configuration was not frozen")
	}
	if err := srv.Unmount("w1"); err != nil {
		t.Fatal(err)
	}
	if err := srv.Mount(legacy); err == nil {
		t.Fatal("legacy remount bypassed configured profile")
	}
}

func TestImmutableProfileMountAliasesAndRemount(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprint(allow), func(t *testing.T) {
			db := immutableProfileFixture(t, "native", true)
			policy := ReadProfile{Kind: BoundedImmutable, AllowOrdinaryQuery: allow}
			srv, err := NewChecked("test", map[string]*core.Database{"alias": db, "second": db}, WithReadOnly(true), WithDatabaseReadProfiles(map[string]ReadProfile{"alias": policy}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(srv.CloseSnapshots)
			check := func(id string) {
				t.Helper()
				handler := srv.Handler()
				for _, q := range []struct {
					path, body string
					status     int
				}{
					{"/dtql", "from: {name: things}\n", map[bool]int{false: 422, true: 200}[allow]},
					{"/dtql", "from: {name: things}\norderBy: [{field: id}]\n", map[bool]int{false: 422, true: 400}[allow]},
					{"/query", `{"collection":"things"}`, 422},
				} {
					got := profileRequest(handler, "POST", "/v1/databases/"+id+q.path, q.body, nil)
					if got.Code != q.status {
						t.Fatalf("%s%s: %d %s", id, q.path, got.Code, got.Body.String())
					}
				}
				metadata := profileDocument(t, profileRequest(handler, "GET", "/v1/databases/"+id, "", nil))
				caps := metadata["capabilities"].(map[string]any)
				if caps["query"] != false || caps["joins"] != false || caps["aggregation"] != false {
					t.Fatalf("alias metadata: %#v", metadata)
				}
				html := profileRequest(handler, "GET", "/ovdb/dbs/"+id+"/collections/things", "", nil).Body.String()
				if strings.Contains(html, "Query records (JSON)") || strings.Contains(html, "hidden") {
					t.Fatal("alias human page exposed unsupported query or target")
				}
			}
			check("alias")
			check("second")
			if err := srv.Mount(db); err != nil {
				t.Fatal(err)
			}
			check("native")
			conflicting, err := NewChecked("test", map[string]*core.Database{"alias": db, "second": db}, WithReadOnly(true), WithDatabaseReadProfiles(map[string]ReadProfile{"alias": policy, "second": {Kind: BoundedImmutable, AllowOrdinaryQuery: !allow}}))
			if err == nil {
				conflicting.CloseSnapshots()
				t.Fatal("conflicting aliases accepted")
			}
			if err := srv.Unmount("alias"); err != nil {
				t.Fatal(err)
			}
			if err := srv.Unmount("second"); err != nil {
				t.Fatal(err)
			}
			if err := srv.Unmount("native"); err != nil {
				t.Fatal(err)
			}
			if err := srv.Mount(immutableProfileFixture(t, "native", false)); err == nil {
				t.Fatal("legacy remount bypassed alias-bound profile")
			}
			if err := srv.Mount(immutableProfileFixture(t, "native", true)); err != nil {
				t.Fatal(err)
			}
			check("native")
		})
	}
}
func TestImmutableProfileKeyResponsesNoStore(t *testing.T) {
	srv, _, _ := profileServer(t, true)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/databases/w1/records/things/b", 200},
		{"GET", "/v1/databases/w1/records/things/missing", 404},
		{"HEAD", "/v1/databases/w1/records/things/b", 200},
		{"HEAD", "/v1/databases/w1/records/things/missing", 404},
		{"GET", "/v1/databases/w1/records/things/%00", 400},
		{"GET", "/v1/databases/w1/read?key=things/missing", 404},
	} {
		got := profileRequest(srv.Handler(), tc.method, tc.path, "", nil)
		if got.Code != tc.status || got.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s: %d cache=%q %s", tc.method, tc.path, got.Code, got.Header().Get("Cache-Control"), got.Body.String())
		}
	}
}

func TestImmutableProfileLegacyAliasUnchanged(t *testing.T) {
	db := immutableProfileFixture(t, "native", false)
	srv, err := NewChecked("test", map[string]*core.Database{"alias": db}, WithReadOnly(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.CloseSnapshots)
	response := profileRequest(srv.Handler(), "POST", "/v1/databases/alias/query", `{"collection":"things"}`, nil)
	if response.Code != 200 {
		t.Fatalf("legacy alias query: %d %s", response.Code, response.Body.String())
	}
	for _, method := range []string{"GET", "HEAD"} {
		response := profileRequest(srv.Handler(), method, "/v1/databases/alias/records/things/a", "", nil)
		if response.Code != 200 || response.Header().Get("Cache-Control") != "" {
			t.Fatalf("legacy alias %s: %d %v", method, response.Code, response.Header())
		}
	}
}

func TestImmutableProfilePublishedAliasEndpointIsUsable(t *testing.T) {
	db := immutableProfileFixture(t, "native", true)
	srv, err := NewChecked("test", map[string]*core.Database{"alias": db}, WithReadOnly(true), WithDatabaseReadProfiles(map[string]ReadProfile{"alias": {Kind: BoundedImmutable, AllowOrdinaryQuery: true, PublishedQuery: true}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.CloseSnapshots)
	meta := profileDocument(t, profileRequest(srv.Handler(), "GET", "/v1/databases/alias", "", nil))
	endpoint := meta["endpoints"].(map[string]any)["dtql"].(string)
	if !strings.HasSuffix(endpoint, "/v1/databases/alias/dtql") {
		t.Fatalf("alias endpoint: %s", endpoint)
	}
	response := profileRequest(srv.Handler(), "POST", endpoint, "from: {database: alias, name: things}\nlimit: 1\n", nil)
	if response.Code != 200 {
		t.Fatalf("alias endpoint query: %d %s", response.Code, response.Body.String())
	}
}

func TestImmutableProfileSharedAliasLifetime(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprint(published), func(t *testing.T) { testImmutableProfileSharedAliasLifetime(t, published) })
	}
}

func testImmutableProfileSharedAliasLifetime(t *testing.T, published bool) {
	db := immutableProfileFixture(t, "native", true)
	var closeCount atomic.Int32
	closed := make(chan struct{})
	db.OnClose(func() error {
		if closeCount.Add(1) == 1 {
			close(closed)
		}
		return nil
	})
	srv, err := NewChecked("test", map[string]*core.Database{"alias": db, "second": db}, WithReadOnly(true), WithDatabaseReadProfiles(map[string]ReadProfile{"alias": {Kind: BoundedImmutable, AllowOrdinaryQuery: true, PublishedQuery: published}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.CloseSnapshots)
	heldFirst, heldSecond := &leases{}, &leases{}
	for id, held := range map[string]*leases{"alias": heldFirst, "second": heldSecond} {
		r := httptest.NewRequest("GET", "/v1/databases/"+id, nil)
		r = r.WithContext(context.WithValue(r.Context(), leasesKey{}, held))
		if srv.acquire(r, id) != db {
			t.Fatal("missing lease")
		}
		t.Cleanup(held.release)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.UnmountContext(canceled, "alias"); err != nil {
		t.Fatalf("nonfinal alias removal: %v", err)
	}
	if closeCount.Load() != 0 {
		t.Fatal("shared instance closed early")
	}
	if srv.getDB("alias") != nil || srv.getDB("second") != db {
		t.Fatal("incorrect remaining routes")
	}
	for _, q := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/v1/databases/second/records/things/b", "", 200},
		{"POST", "/v1/databases/second/dtql", "from: {database: second, name: things}\nlimit: 1\n", 200},
		{"POST", "/v1/databases/second/dtql", "from: {name: things}\norderBy: [{field: id}]\n", 400},
		{"POST", "/v1/databases/second/query", `{"collection":"things"}`, 422},
		{"POST", "/v1/dtql", "from: {database: second, name: things}\n", 422},
	} {
		resp := profileRequest(srv.Handler(), q.method, q.path, q.body, nil)
		if resp.Code != q.status {
			t.Fatalf("remaining alias %s: %d %s", q.path, resp.Code, resp.Body.String())
		}
	}
	meta := profileDocument(t, profileRequest(srv.Handler(), "GET", "/v1/databases/second", "", nil))
	caps := meta["capabilities"].(map[string]any)
	if caps["query"] != published || caps["joins"] != false || caps["aggregation"] != false {
		t.Fatalf("remaining alias metadata: %#v", meta)
	}
	discovery := profileDocument(t, profileRequest(srv.Handler(), "GET", "/.well-known/openvaultdb", "", nil))
	items := discovery["databases"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != "second" {
		t.Fatalf("remaining discovery: %#v", discovery)
	}
	html := profileRequest(srv.Handler(), "GET", "/ovdb/dbs/second/collections/things", "", nil)
	if html.Code != 200 || strings.Contains(html.Body.String(), "hidden") || strings.Contains(html.Body.String(), "Query records (JSON)") {
		t.Fatal("remaining human metadata changed")
	}
	if err := srv.Mount(db); err != nil {
		t.Fatalf("live instance alias reattachment: %v", err)
	}
	if err := srv.UnmountContext(canceled, "second"); err != nil {
		t.Fatalf("nonfinal reattached alias removal: %v", err)
	}
	if srv.getDB("native") != db || closeCount.Load() != 0 {
		t.Fatal("live alias reattachment lost instance")
	}
	if err := srv.UnmountContext(canceled, "native"); !errors.Is(err, context.Canceled) {
		t.Fatalf("final removal cancellation: %v", err)
	}
	if srv.getDB("second") != nil || closeCount.Load() != 0 {
		t.Fatal("final removal did not retain leased instance while draining")
	}
	if err := srv.Mount(db); err == nil {
		t.Fatal("retiring instance remounted")
	}
	if err := srv.Mount(immutableProfileFixture(t, "native", false)); err == nil {
		t.Fatal("legacy remount bypassed policy")
	}
	replacement := immutableProfileFixture(t, "native", true)
	if err := srv.Mount(replacement); err != nil {
		t.Fatal(err)
	}
	resp := profileRequest(srv.Handler(), "GET", "/v1/databases/native/records/things/b", "", nil)
	if resp.Code != 200 {
		t.Fatalf("replacement read: %d %s", resp.Code, resp.Body.String())
	}
	heldFirst.release()
	select {
	case <-closed:
		t.Fatal("closed before all aliases' leases ended")
	case <-time.After(20 * time.Millisecond):
	}
	heldSecond.release()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("final close did not finish")
	}
	if closeCount.Load() != 1 {
		t.Fatal("close count")
	}
	if err := srv.Mount(db); err == nil {
		t.Fatal("closed instance remounted")
	}
	if err := srv.Unmount("native"); err != nil {
		t.Fatal(err)
	}
}
