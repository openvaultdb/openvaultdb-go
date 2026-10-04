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

func TestRelationalQueryKeysUseAdapterID(t *testing.T) {
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE "Order Details" (first INTEGER, second INTEGER, id TEXT UNIQUE, PRIMARY KEY(first, second))`,
		`INSERT INTO "Order Details" VALUES (10248, 11, 'composite-10248-11')`,
	} {
		if _, err := storage.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "db.yaml")
	declaration := "database: {id: relational, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    '\"Order Details\"':\n      fields:\n        first: {type: integer}\n        second: {type: integer}\n        id: {type: string}\n"
	if err := os.WriteFile(path, []byte(declaration), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// The public schema exposes native names while the SQL driver retains its quoted storage alias.
	db.Manifest.Schemas.Collections["Order Details"] = db.Manifest.Schemas.Collections[`"Order Details"`]
	delete(db.Manifest.Schemas.Collections, `"Order Details"`)
	service := server.New("test", map[string]*core.Database{"relational": db}, server.WithReadOnly(true))
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	endpoint := host.URL + "/v1/databases/relational/dtql"
	doc := "from: {name: 'Order Details'}\n"
	response, err := http.Get(endpoint + "?q=" + url.QueryEscape(doc))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	decodeJSON(t, response, &result)
	assertKey := func(result map[string]any) {
		t.Helper()
		records := result["records"].([]any)
		if len(records) != 1 {
			t.Fatalf("records: %#v", result)
		}
		row := records[0].(map[string]any)
		if row["key"] != "Order Details/composite-10248-11" {
			t.Fatalf("query key must use adapter ID: %#v", row)
		}
	}
	assertKey(result)
	status, page := pagedDTQL(t, endpoint, doc, 1, "")
	if status != http.StatusOK {
		t.Fatalf("paged status: %d %#v", status, page)
	}
	assertKey(page)
}
