package server_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// The sentences of docs/api.md about the launch limits that the runnable examples
// do not show are pinned here, against real SQLite files. The helpers of this file
// all start with docLimit so they cannot clash with the others of the package.

// docLimitFlags mounts a SQLite file whose collection Flag declares a boolean.
func docLimitFlags(t *testing.T) *core.Database {
	t.Helper()
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE "Flag" ("id" TEXT PRIMARY KEY, "active" INTEGER)`,
		`INSERT INTO "Flag" VALUES ('f1', 1)`,
	} {
		if _, err := storage.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	declaration := "database: {id: flags, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    Flag:\n      fields:\n        id: {type: string}\n        active: {type: boolean}\n"
	path := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(path, []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func docLimitServer(t *testing.T) *httptest.Server {
	t.Helper()
	flags := docLimitFlags(t)
	service := server.New("test", map[string]*core.Database{"flags": flags, "chinook": relHTTPChinook(t, "")})
	t.Cleanup(service.CloseSnapshots)
	host := httptest.NewServer(service.Handler())
	t.Cleanup(host.Close)
	return host
}

// A declared boolean of a SQLite mount is true in a single-collection record and 1
// in a relational row: the relational answer is not coerced to the schema.
func TestDocumentedBooleanOfARelationalRowIsNotCoerced(t *testing.T) {
	host := docLimitServer(t)
	single := relHTTPPost(t, host.URL, "/v1/databases/flags/dtql", "", "from: {name: Flag}\ncolumns: [{field: active}]\n")
	relational := relHTTPPost(t, host.URL, "/v1/databases/flags/dtql", "", "from: {name: Flag, alias: f}\ncolumns: [{field: active, source: f}]\n")
	if single.status != http.StatusOK || relational.status != http.StatusOK {
		t.Fatalf("single %d: %s; relational %d: %s", single.status, single.raw, relational.status, relational.raw)
	}
	if !strings.Contains(single.raw, `"active":true`) {
		t.Fatalf("a single-collection record carries the boolean as true: %s", single.raw)
	}
	if rows := relational.rows(t); len(rows) != 1 || rows[0]["active"] != float64(1) {
		t.Fatalf("relational rows = %v", rows)
	}
}

// A root that names the endpoint's own database is read as if it named none when
// the key is written plainly; written through a YAML alias or a merge key it is not
// recognised and the document is answered as a relational one. The alias is
// anchored on a value that is not a relational feature (a where operand, written
// before the root), so that the document is relational only because the database
// is not stripped: an alias on the source itself would make it relational whatever
// the database did. The same document with a plain key is the control.
func TestDocumentedOwnDatabaseRulingHoldsForAPlainKey(t *testing.T) {
	host := docLimitServer(t)
	const columns = "columns: [{field: id}]\n"
	const where = "where: {op: '<', left: {field: name}, right: {value: %s}}\n"
	plain := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", "from: {database: chinook, name: Customer}\n"+columns)
	if plain.status != http.StatusOK || plain.body["columns"] != nil || !strings.Contains(plain.raw, `"key"`) {
		t.Fatalf("a plainly written own database is answered as a single collection, with keys: %d %s", plain.status, plain.raw)
	}
	control := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", fmt.Sprintf(where, "chinook")+"from: {database: chinook, name: Customer}\n"+columns)
	if control.status != http.StatusOK || control.body["columns"] != nil || !strings.Contains(control.raw, `"key"`) {
		t.Fatalf("the control document with a plain key is answered as a single collection: %d %s", control.status, control.raw)
	}
	for name, doc := range map[string]string{
		"an alias": fmt.Sprintf(where, "&d chinook") + "from: {database: *d, name: Customer}\n" + columns,
		"a merge":  "from: {<<: {database: chinook}, name: Customer}\n" + columns,
	} {
		resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", doc)
		if resp.status != http.StatusOK || resp.body["columns"] == nil || strings.Contains(resp.raw, `"key"`) {
			t.Errorf("%s: a database written this way is answered as a relational document: %d %s", name, resp.status, resp.raw)
		}
	}
}

// first and last are not in the relational profile, so the classifier refuses a
// document that uses either, in any letter case, on every route and before
// anything is read: the same four shapes the advertised aggregates are answered on
// (TestEveryAdvertisedAggregateIsAnsweredOnSQLite), over real SQLite files. The
// sentence of docs/api.md states it.
func TestDocumentedFirstAndLastAreRefusedOnEveryRoute(t *testing.T) {
	service := server.New("0.1.0", map[string]*core.Database{"chinook": relHTTPChinook(t, ""), "countries": relHTTPCountries(t, "")})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	for _, function := range []string{"first", "last", "FIRST", "Last"} {
		for name, document := range docAggregateShapes(function) {
			t.Run(function+" "+name, func(t *testing.T) {
				resp := relHTTPDo(t, host.URL, http.MethodPost, document.path, "", document.body, nil)
				if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
				if !strings.Contains(resp.errorField("message"), strings.ToLower(function)+" is not in the relational profile") {
					t.Fatalf("the refusal does not name the function: %s", resp.raw)
				}
			})
		}
	}
}
