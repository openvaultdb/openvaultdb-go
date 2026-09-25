package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func humanRequest(h http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://localhost:8080"+path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func testHumanDB(id string) *core.Database {
	return &core.Database{Manifest: &manifest.Manifest{Database: manifest.Database{ID: id, SchemaMode: schema.Mode("strict")}, Storage: manifest.Storage{Engine: "sqlite"}, Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"Album": {}}}}}
}

func TestHumanPagesAndDiscovery(t *testing.T) {
	s := New("test", map[string]*core.Database{"chinook": testHumanDB("chinook")}, WithPublicOrigin("https://data.example.test"))
	t.Cleanup(s.CloseSnapshots)
	h := s.Handler()
	for _, tc := range []struct{ path, text string }{{"/ovdb/", "OpenVaultDB server"}, {"/ovdb/dbs/", "chinook"}, {"/ovdb/dbs/chinook", "Album"}} {
		w := humanRequest(h, tc.path)
		if w.Code != 200 || !strings.Contains(w.Body.String(), tc.text) {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Header().Get("Link"), "describedby") {
			t.Fatalf("%s: missing HTML/discovery headers", tc.path)
		}
	}
	w := humanRequest(h, "/ovdb/dbs/missing")
	if w.Code != 404 || !strings.Contains(w.Body.String(), "Database not found") {
		t.Fatalf("missing profile: %d %s", w.Code, w.Body.String())
	}
	w = humanRequest(h, "/ovdb/style.css")
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("stylesheet: %d", w.Code)
	}
	w = humanRequest(h, "/.well-known/openvaultdb")
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("discovery must not be cached across request hosts")
	}
	var doc struct {
		Databases []struct {
			URL    string `json:"url"`
			APIURL string `json:"apiUrl"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Databases) != 1 || doc.Databases[0].URL != "https://data.example.test/ovdb/dbs/chinook" || doc.Databases[0].APIURL != "https://data.example.test/v1/databases/chinook" {
		t.Fatalf("discovery: %+v", doc)
	}
	if err := s.Mount(testHumanDB("new-db")); err != nil {
		t.Fatal(err)
	}
	if w = humanRequest(h, "/ovdb/dbs/new-db"); w.Code != 200 {
		t.Fatalf("runtime mount absent: %d", w.Code)
	}
}

func TestHumanPagesDoNotExposePrivateCatalog(t *testing.T) {
	s := New("test", map[string]*core.Database{"secret": testHumanDB("secret")}, WithAuth(&auth.Config{OwnerToken: "private-token"}))
	t.Cleanup(s.CloseSnapshots)
	h := s.Handler()
	for _, path := range []string{"/ovdb/", "/ovdb/dbs/"} {
		w := humanRequest(h, path)
		if w.Code != 200 || strings.Contains(w.Body.String(), "secret") || !strings.Contains(w.Body.String(), "requires authentication") {
			t.Fatalf("private %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := humanRequest(h, "/ovdb/dbs/secret")
	if w.Code != 404 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("private profile: %d %s", w.Code, w.Body.String())
	}
	w = humanRequest(h, "/.well-known/openvaultdb")
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("private discovery leaked: %s", w.Body.String())
	}
}

func TestHumanEscapesDatabaseID(t *testing.T) {
	id := "<script>alert(1)</script>"
	s := New("test", map[string]*core.Database{id: testHumanDB(id)})
	t.Cleanup(s.CloseSnapshots)
	w := humanRequest(s.Handler(), "/ovdb/dbs/")
	if w.Code != 200 || strings.Contains(w.Body.String(), id) || !strings.Contains(w.Body.String(), "&lt;script&gt;") {
		t.Fatalf("unescaped id: %d %s", w.Code, w.Body.String())
	}
}
