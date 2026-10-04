package server_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// TestQuotedCollectionKeyReadsSurviveTheManifestRename follows the sequence of
// the production embedder openvaultdb/cloud on a real SQLite mount: the
// manifest declares a table by its quoted storage identifier ('"Order Details"'),
// the mount opens, the embedder renames the live manifest's keys to the public
// names, and its middleware rewrites GET and HEAD of /records and /read?key= to
// the quoted name, the only form dalgo2sql can read. The write guard's
// allow-list is what the mount registered, so those reads keep answering 200
// while a collection nobody declared is a 404.
func TestQuotedCollectionKeyReadsSurviveTheManifestRename(t *testing.T) {
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE "Order Details" (id TEXT PRIMARY KEY, name TEXT)`,
		`INSERT INTO "Order Details" VALUES ('10248', 'Ada')`,
	} {
		if _, err := storage.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "db.yaml")
	declaration := "database: {id: relational, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    '\"Order Details\"':\n      fields:\n        id: {type: string}\n        name: {type: string}\n"
	if err := os.WriteFile(path, []byte(declaration), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// What the embedder does once the database is mounted.
	db.Manifest.Schemas.Collections["Order Details"] = db.Manifest.Schemas.Collections[`"Order Details"`]
	delete(db.Manifest.Schemas.Collections, `"Order Details"`)

	service := server.New("test", map[string]*core.Database{"relational": db}, server.WithReadOnly(true))
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()

	const records = "/v1/databases/relational/records/"
	quoted := url.PathEscape(`"Order Details"`)
	for _, c := range []struct {
		name         string
		method, path string
		status       int
	}{
		{"GET quoted name", "GET", records + quoted + "/10248", http.StatusOK},
		{"HEAD quoted name", "HEAD", records + quoted + "/10248", http.StatusOK},
		{"read?key quoted name", "GET", "/v1/databases/relational/read?key=" + url.QueryEscape(quoted+"/10248"), http.StatusOK},
		{"GET undeclared", "GET", records + "ghost/1", http.StatusNotFound},
		{"HEAD undeclared", "HEAD", records + "ghost/1", http.StatusNotFound},
		{"read?key undeclared", "GET", "/v1/databases/relational/read?key=ghost%2F1", http.StatusNotFound},
		{"GET hostile", "GET", records + url.PathEscape(`"Order Details"; DROP TABLE x; --`) + "/1", http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := send(t, host, guardCall{method: c.method, path: c.path})
			if status != c.status {
				t.Fatalf("status %d, want %d: %v", status, c.status, body)
			}
			if c.status == http.StatusOK && c.method == "GET" {
				if data, _ := body["data"].(map[string]any); data["name"] != "Ada" {
					t.Fatalf("body %v", body)
				}
			}
		})
	}
}
