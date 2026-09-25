package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	_ "modernc.org/sqlite"
)

func TestHumanCollectionCombinesProviderAndDeclaredReferences(t *testing.T) {
	dir := t.TempDir()
	sqlDB, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE Parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE Child (id TEXT PRIMARY KEY, parent_id TEXT, extra_id TEXT,
		 FOREIGN KEY (parent_id) REFERENCES Parent(id) ON DELETE CASCADE)`,
	} {
		if _, err := sqlDB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	declaration := `database: {id: relationships, schema_mode: strict}
storage: {engine: sqlite, path: ./data.sqlite}
schemas:
  collections:
    Parent:
      fields:
        id: {type: string}
    Child:
      fields:
        id: {type: string}
        parent_id: {type: string}
        extra_id: {type: string}
      references:
        - {field: parent_id, collection: Parent, target_field: id}
        - {field: extra_id, collection: Parent, target_field: id}
`
	manifestPath := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(manifestPath, []byte(declaration), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := New("test", map[string]*core.Database{"relationships": db})
	t.Cleanup(s.CloseSnapshots)
	h := s.Handler()
	child := humanRequest(h, "/ovdb/dbs/relationships/collections/Child")
	if child.Code != 200 || !strings.Contains(child.Body.String(), "Database and OVDB declaration; enforcement: disabled") ||
		!strings.Contains(child.Body.String(), "OVDB declaration; enforcement: Informational") ||
		!strings.Contains(child.Body.String(), "on delete: CASCADE") {
		t.Fatalf("combined references: %d %s", child.Code, child.Body.String())
	}
	parent := humanRequest(h, "/ovdb/dbs/relationships/collections/Parent")
	if parent.Code != 200 || strings.Count(parent.Body.String(), `href="/ovdb/dbs/relationships/collections/Child"`) != 2 {
		t.Fatalf("incoming references: %d %s", parent.Code, parent.Body.String())
	}
}

func humanRequest(h http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://localhost:8080"+path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func testHumanDB(id string) *core.Database {
	return &core.Database{Manifest: &manifest.Manifest{Database: manifest.Database{ID: id, SchemaMode: schema.ModeStrict}, Storage: manifest.Storage{Engine: "sqlite"}, Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
		"Album":  {Fields: map[string]schema.Field{"Title": {Type: schema.TypeString, Required: true}, "AlbumId": {Type: schema.TypeInteger}, "ArtistId": {Type: schema.TypeInteger}}, References: []schema.Reference{{Field: "ArtistId", Collection: "Artist", TargetField: "ArtistId"}}},
		"Artist": {Fields: map[string]schema.Field{"ArtistId": {Type: schema.TypeInteger}}},
		"Track":  {Fields: map[string]schema.Field{"AlbumId": {Type: schema.TypeInteger}}, References: []schema.Reference{{Field: "AlbumId", Collection: "Album", TargetField: "AlbumId"}}},
	}}}}
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
	databasePage := humanRequest(h, "/ovdb/dbs/chinook")
	if !strings.Contains(databasePage.Body.String(), `href="/ovdb/dbs/chinook/collections/Album"`) {
		t.Fatalf("database does not link to Album: %s", databasePage.Body.String())
	}
	collectionPage := humanRequest(h, "/ovdb/dbs/chinook/collections/Album")
	collectionHTML := collectionPage.Body.String()
	if collectionPage.Code != 200 || !strings.Contains(collectionHTML, `<a href="/ovdb/dbs/chinook">chinook</a>`) ||
		!strings.Contains(collectionHTML, `<code>AlbumId</code>`) || !strings.Contains(collectionHTML, `<code>Title</code>`) ||
		!strings.Contains(collectionHTML, `>integer</td>`) || !strings.Contains(collectionHTML, `>Yes</td>`) ||
		!strings.Contains(collectionHTML, `href="https://data.example.test/v1/databases/chinook"`) ||
		!strings.Contains(collectionHTML, `Query records (JSON)`) ||
		!strings.Contains(collectionHTML, `<a href="/ovdb/dbs/chinook/collections/Artist">Artist</a>`) ||
		!strings.Contains(collectionHTML, `<a href="/ovdb/dbs/chinook/collections/Track">Track</a>`) ||
		!strings.Contains(collectionHTML, "OVDB declaration; enforcement: Informational") {
		t.Fatalf("collection profile: %d %s", collectionPage.Code, collectionHTML)
	}
	for _, path := range []string{"/ovdb/dbs/missing/collections/Album", "/ovdb/dbs/chinook/collections/missing"} {
		w := humanRequest(h, path)
		if w.Code != 404 || !strings.Contains(w.Body.String(), "Collection not found") {
			t.Fatalf("missing collection %s: %d %s", path, w.Code, w.Body.String())
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
	w = humanRequest(h, "/ovdb/dbs/secret/collections/Album")
	if w.Code != 404 || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "Album") {
		t.Fatalf("private collection leaked: %d %s", w.Code, w.Body.String())
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
