package server

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type retentionDriver struct {
	previewPGDriver
	keys atomic.Int32
}

func (d *retentionDriver) Get(_ context.Context, rec record.Record) error {
	d.keys.Add(1)
	rec.SetError(nil)
	rec.Data().(map[string]any)["name"] = "fabricated"
	return nil
}
func (d *retentionDriver) Exists(context.Context, *record.Key) (bool, error) {
	d.keys.Add(1)
	return true, nil
}

func retentionFixture(t *testing.T, engine, retention string) (*core.Database, *retentionDriver) {
	t.Helper()
	driver := &retentionDriver{}
	m := &manifest.Manifest{Database: manifest.Database{ID: "live", SchemaMode: schema.ModeStrict, Retention: retention},
		Storage: manifest.Storage{Engine: engine}, Schemas: &schema.Schemas{Collections: previewPGCollections()}}
	if engine == "http" {
		m.Storage.HTTP = &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "customers"}
		m.Schemas = &schema.Schemas{Collections: map[string]schema.Collection{"customers": {Fields: map[string]schema.Field{"time": {Type: schema.TypeString}, "currency": {Type: schema.TypeString}, "rate": {Type: schema.TypeString}}}}}
	}
	db, err := core.Open(m, driver, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db, driver
}

func retentionRequest(handler http.Handler, method, path, body string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if headers != nil {
		r.Header = headers.Clone()
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func assertRetentionRefusal(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != 422 || !strings.Contains(w.Body.String(), `"code":"retention_not_authorized"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response %d %v %s", w.Code, w.Header(), w.Body.String())
	}
}

func TestNoRetentionRefusesAllContinuationBeforeAdapterOrSnapshot(t *testing.T) {
	for _, engine := range []string{"sqlite", "http", "future-live-engine"} {
		t.Run(engine, func(t *testing.T) {
			retention := ""
			if engine == "sqlite" {
				retention = "none"
			}
			db, driver := retentionFixture(t, engine, retention)
			s := New("test", map[string]*core.Database{"live": db}, WithReadOnly(true))
			t.Cleanup(s.CloseSnapshots)
			// Mutating public metadata cannot turn retained copies on.
			db.Manifest.Database.Retention = ""
			db.Manifest.Storage.Engine = "sqlite"
			db.Manifest.Database.CacheTTL = "1h"
			headers := []http.Header{
				{"OVDB-Page-Size": {"1"}}, {"OVDB-Page-Token": {"old"}}, {"OVDB-Page-Close": {"true"}},
				{"OVDB-Page-Size": {""}}, {"OVDB-Page-Token": {}}, {"OVDB-Page-Close": {"true", "true"}},
				{"OVDB-Page-Size": {"1", "2"}}, {"ovdb-page-token": {"old"}, "OVDB-Page-Token": {"old"}},
				{"OVDB-Page-Future": {""}},
			}
			routes := []struct{ method, path, body string }{
				{"POST", "/v1/databases/live/dtql", "from: {name: customers}\n"},
				{"GET", "/v1/databases/live/dtql?q=" + url.QueryEscape("from: {name: customers}\n"), ""},
				{"POST", "/v1/databases/live/dtql", "from: {name: customers, alias: c}\n"},
				{"POST", "/v1/dtql", "from: {database: live, name: customers}\n"},
				{"GET", "/v1/databases/live/read?key=customers/a", ""},
				{"GET", "/v1/databases/live/records/customers/a", ""},
				{"HEAD", "/v1/databases/live/records/customers/a", ""},
				{"POST", "/v1/databases/live/query", `{"collection":"customers"}`},
				{"GET", "/v1/databases/live/query?q=" + url.QueryEscape(`{"collection":"customers"}`), ""},
				{"GET", "/v1/databases/live", ""},
				{"GET", "/v1/databases/live/inferred-schema", ""},
				{"GET", "/ovdb/dbs/live/collections/customers", ""},
			}
			for _, route := range routes {
				for _, h := range headers {
					w := retentionRequest(s.Handler(), route.method, route.path, route.body, h)
					if w.Code != 422 {
						t.Logf("%s %s headers=%v", route.method, route.path, h)
					}
					assertRetentionRefusal(t, w)
				}
			}
			for _, name := range []string{"snapshotToken", "pageToken", "nextPageToken", "continuationToken", "continuation"} {
				assertRetentionRefusal(t, retentionRequest(s.Handler(), "GET", "/v1/databases/live/read?key=customers/a&"+name+"=", "", nil))
				assertRetentionRefusal(t, retentionRequest(s.Handler(), "POST", "/v1/databases/live/query", `{"collection":"customers","`+name+`":""}`, nil))
				assertRetentionRefusal(t, retentionRequest(s.Handler(), "GET", "/v1/databases/live/query?q="+url.QueryEscape(`{"collection":"customers","`+name+`":null}`), "", nil))
			}
			query, _, err := core.ParseDTQL([]byte("from: {name: customers}\n"))
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			s.handlePagedDTQL(w, httptest.NewRequest("POST", "/", nil), db, query, nil)
			assertRetentionRefusal(t, w)
			if _, _, err := spoolDTQL(context.Background(), db, query, s.snapshotDir, DefaultSnapshotLimits(), 0); err != errRetentionNotAuthorized {
				t.Fatal(err)
			}
			for _, close := range []bool{false, true} {
				w = httptest.NewRecorder()
				if close {
					s.closeSnapshot(w, "old", db, sha256.Sum256(nil), sha256.Sum256(nil), 1)
				} else {
					s.serveSnapshotPage(w, "old", db, sha256.Sum256(nil), sha256.Sum256(nil), 1)
				}
				assertRetentionRefusal(t, w)
			}
			entries, err := os.ReadDir(s.snapshotDir)
			if err != nil {
				t.Fatal(err)
			}
			if driver.reads.Load() != 0 || driver.begins.Load() != 0 || driver.keys.Load() != 0 || s.snapshotSlots != 0 || len(s.snapshots) != 0 || len(entries) != 0 {
				t.Fatalf("side effects: reads=%d begins=%d keys=%d slots=%d snapshots=%d files=%d", driver.reads.Load(), driver.begins.Load(), driver.keys.Load(), s.snapshotSlots, len(s.snapshots), len(entries))
			}
		})
	}
}

func TestNoRetentionOrdinaryReadsAndDiscoveryAreNoStore(t *testing.T) {
	db, driver := retentionFixture(t, "sqlite", "none")
	s := New("test", map[string]*core.Database{"live": db}, WithReadOnly(true), WithSourceRights("stable-server", nil))
	t.Cleanup(s.CloseSnapshots)
	db.Manifest.Database.CacheTTL = "1h" // defense even after admission
	routes := []struct{ method, path, body string }{
		{"POST", "/v1/databases/live/dtql", "from: {name: customers}\nlimit: 2\n"},
		{"GET", "/v1/databases/live/dtql?q=" + url.QueryEscape("from: {name: customers}\nlimit: 2\n"), ""},
		{"POST", "/v1/databases/live/query", `{"collection":"customers","limit":2}`},
		{"GET", "/v1/databases/live/query?q=" + url.QueryEscape(`{"collection":"customers","limit":2}`), ""},
		{"GET", "/v1/databases/live/read?key=customers/a", ""},
		{"GET", "/v1/databases/live/records/customers/a", ""},
		{"HEAD", "/v1/databases/live/records/customers/a", ""},
		{"GET", "/v1/databases/live", ""},
		{"GET", "/v1/databases/live/read?key=bad", ""},
		{"POST", "/v1/databases/live/query", "invalid"},
	}
	for _, route := range routes {
		w := retentionRequest(s.Handler(), route.method, route.path, route.body, nil)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s %s: %v", route.method, route.path, w.Header())
		}
		if route.body != "invalid" && !strings.Contains(route.path, "key=bad") && w.Code != 200 {
			t.Fatalf("%s: %d %s", route.path, w.Code, w.Body.String())
		}
	}
	if driver.reads.Load() == 0 || driver.keys.Load() == 0 {
		t.Fatal("ordinary reads were not executed")
	}
	w := retentionRequest(s.Handler(), "GET", "/v1/databases/live", "", nil)
	metadata := profileDocument(t, w)
	if metadata["id"] != "live" || metadata["serverId"] != "stable-server" || metadata["retention"] != "none" || metadata["queryFormat"] != queryFormat || metadata["capabilities"].(map[string]any)["dtql"] != true {
		t.Fatal(metadata)
	}
	if relationalCacheControl(true, false, "GET", []cacheFacts{{TTL: 60 * 1e9, NoRetention: true}}) != "no-store" {
		t.Fatal("relational cache weakened retention")
	}
}

func TestNoRetentionContinuationDocumentsFailClosed(t *testing.T) {
	cases := []struct {
		name, method, path, body string
		headers                  http.Header
		status                   int
	}{
		{"trailing-junk", "POST", "/v1/databases/live/query", `{"collection":"customers","snapshotToken":"old"}junk`, nil, 422},
		{"trailing-object", "POST", "/v1/databases/live/query", `{"collection":"customers"}{"snapshotToken":"old"}`, nil, 422},
		{"malformed-escape", "GET", "/v1/databases/live/read?key=customers/a&snapshotToken=%ZZ", "", nil, 422},
		{"yaml-post", "POST", "/v1/databases/live/dtql", "from: {name: customers}\nsnapshotToken: old\n", nil, 422},
		{"yaml-get", "GET", "/v1/databases/live/dtql?q=" + url.QueryEscape("from: {name: customers}\nsnapshotToken: null\n"), "", nil, 422},
		{"envelope-token", "POST", "/v1/databases/live/dtql", `{"query":"from: {name: customers}\n","snapshotToken":"old"}`, http.Header{"Content-Type": {"application/json"}}, 422},
		{"envelope-yaml-token", "POST", "/v1/databases/live/dtql", `{"query":"from: {name: customers}\nsnapshotToken: old\n"}`, http.Header{"Content-Type": {"application/json"}}, 422},
		{"ordinary-trailing-junk", "POST", "/v1/databases/live/query", `{"collection":"customers"}junk`, nil, 400},
		{"ordinary-trailing-object", "POST", "/v1/databases/live/query", `{"collection":"customers"}{}`, nil, 400},
		{"ordinary-malformed-escape", "GET", "/v1/databases/live/read?key=customers/a&extra=%ZZ", "", nil, 400},
		{"ordinary-malformed-yaml", "POST", "/v1/databases/live/dtql", "from: [", nil, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, driver := retentionFixture(t, "sqlite", "none")
			s := New("test", map[string]*core.Database{"live": db}, WithReadOnly(true))
			t.Cleanup(s.CloseSnapshots)
			w := retentionRequest(s.Handler(), c.method, c.path, c.body, c.headers)
			if c.status == 422 {
				assertRetentionRefusal(t, w)
			} else if w.Code != c.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response %d %v %s", w.Code, w.Header(), w.Body.String())
			}
			entries, err := os.ReadDir(s.snapshotDir)
			if err != nil {
				t.Fatal(err)
			}
			if driver.reads.Load() != 0 || driver.begins.Load() != 0 || driver.keys.Load() != 0 || s.snapshotSlots != 0 || len(s.snapshots) != 0 || len(entries) != 0 {
				t.Fatal("refused request reached an adapter or snapshot admission")
			}
		})
	}
}

func TestNoRetentionCrossDatabaseContinuationBeforeRead(t *testing.T) {
	db, driver := retentionFixture(t, "sqlite", "none")
	fake := &relFakeExecutor{result: joinexec.Result{}}
	s := New("test", map[string]*core.Database{"live": db, "native": relFakeMount("native", "sqlite", "1h")}, WithReadOnly(true))
	s.joinExecute = fake.execute
	s.joinStreamExecute = legacyJoinStream(fake.execute)
	t.Cleanup(s.CloseSnapshots)
	doc := strings.ReplaceAll(strings.ReplaceAll(relFakeAcross, "alpha", "native"), "beta", "live") + "\nsnapshotToken: old\n"
	assertRetentionRefusal(t, retentionRequest(s.Handler(), "POST", "/v1/dtql", doc, nil))
	assertRetentionRefusal(t, retentionRequest(s.Handler(), "GET", "/v1/dtql?q="+url.QueryEscape(doc), "", nil))
	for _, body := range []string{
		`{"query":` + strconv.Quote(doc) + `}`,
		`{"query":` + strconv.Quote(strings.TrimSuffix(doc, "\nsnapshotToken: old\n")) + `,"snapshotToken":"old"}`,
	} {
		assertRetentionRefusal(t, retentionRequest(s.Handler(), "POST", "/v1/dtql", body, http.Header{"Content-Type": {"application/json"}}))
	}
	entries, err := os.ReadDir(s.snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	if fake.count() != 0 || driver.reads.Load() != 0 || driver.begins.Load() != 0 || driver.keys.Load() != 0 || s.snapshotSlots != 0 || len(s.snapshots) != 0 || len(entries) != 0 {
		t.Fatal("cross-database continuation reached an adapter or snapshot admission")
	}
}

func TestNativeContinuationAndTrailingDocumentsStayInvalid(t *testing.T) {
	db, driver := retentionFixture(t, "sqlite", "")
	fake := &relFakeExecutor{result: joinexec.Result{}}
	s := New("test", map[string]*core.Database{"live": db}, WithReadOnly(true))
	s.joinExecute = fake.execute
	s.joinStreamExecute = legacyJoinStream(fake.execute)
	t.Cleanup(s.CloseSnapshots)
	for _, body := range []string{`{"collection":"customers"}junk`, `{"collection":"customers"}{}`} {
		w := retentionRequest(s.Handler(), "POST", "/v1/databases/live/query", body, nil)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	doc := "from: {database: live, name: customers}\nsnapshotToken: old\n"
	for _, body := range []string{doc, `{"query":"from: {database: live, name: customers}\n","snapshotToken":"old"}`} {
		headers := http.Header{}
		if strings.HasPrefix(body, "{") {
			headers.Set("Content-Type", "application/json")
		}
		w := retentionRequest(s.Handler(), "POST", "/v1/dtql", body, headers)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if driver.reads.Load() != 0 || fake.count() != 0 || s.snapshotSlots != 0 {
		t.Fatal("invalid native request reached execution")
	}
}

func TestNoRetentionAdmissionRejectsConflictingCache(t *testing.T) {
	db, _ := retentionFixture(t, "sqlite", "none")
	db.Manifest.Database.CacheTTL = "1h"
	s := New("test", map[string]*core.Database{"live": db})
	t.Cleanup(s.CloseSnapshots)
	w := retentionRequest(s.Handler(), "GET", "/v1/databases/live", "", nil)
	if w.Code != 500 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header())
	}
	s = New("test", nil)
	t.Cleanup(s.CloseSnapshots)
	if err := s.Mount(db); err == nil {
		t.Fatal("conflicting cache admitted")
	}
}

func TestNativePagingRejectsDuplicateAndEmptyHeadersBeforeRead(t *testing.T) {
	db, driver := retentionFixture(t, "sqlite", "")
	s := New("test", map[string]*core.Database{"live": db})
	t.Cleanup(s.CloseSnapshots)
	for _, headers := range []http.Header{{"OVDB-Page-Size": {""}}, {"OVDB-Page-Size": {"1", "1"}}, {"OVDB-Page-Size": {"1"}, "OVDB-Page-Token": {""}}, {"OVDB-Page-Size": {"1"}, "OVDB-Page-Close": {"true", "true"}}} {
		w := retentionRequest(s.Handler(), "POST", "/v1/databases/live/dtql", "from: {name: customers}\n", headers)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if driver.reads.Load() != 0 || s.snapshotSlots != 0 {
		t.Fatal("malformed paging reached capture")
	}
}

func TestNoRetentionMixedRelationalQueryGuardsBeforeAdmission(t *testing.T) {
	db, driver := retentionFixture(t, "sqlite", "none")
	fake := &relFakeExecutor{result: joinexec.Result{}}
	s := New("test", map[string]*core.Database{"live": db, "native": relFakeMount("native", "sqlite", "1h")}, WithReadOnly(true))
	s.joinExecute = fake.execute
	s.joinStreamExecute = legacyJoinStream(fake.execute)
	t.Cleanup(s.CloseSnapshots)
	db.Manifest.Database.CacheTTL = "1h"
	doc := strings.ReplaceAll(strings.ReplaceAll(relFakeAcross, "alpha", "native"), "beta", "live")
	for _, headers := range []http.Header{{"OVDB-Page-Size": {"1"}}, {"OVDB-Page-Token": {""}}} {
		assertRetentionRefusal(t, retentionRequest(s.Handler(), "POST", "/v1/dtql", doc, headers))
	}
	if fake.count() != 0 || driver.reads.Load() != 0 || s.snapshotSlots != 0 {
		t.Fatal("mixed query reached admission or upstream")
	}
	w := retentionRequest(s.Handler(), "GET", "/v1/dtql?q="+url.QueryEscape(doc), "", nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
}
