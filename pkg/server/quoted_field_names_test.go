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

// TestSQLiteColumnWithASpaceIsQueryableOverHTTP shows that a declared SQLite
// table with a string id and a column named "zip code" is selected, filtered and
// ordered on every query route: the wire query, DTQL over GET and POST, and a
// snapshot page. It goes through the real handler and a real SQLite file, and
// the name arrives quoted, so the rows returned are the right ones. The table
// models any declared relation with such a column; the test does not show that a
// view without a string id column, which has no recordset, can be read.
func TestSQLiteColumnWithASpaceIsQueryableOverHTTP(t *testing.T) {
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TABLE \"customer_list\" (\"id\" TEXT PRIMARY KEY, \"name\" TEXT, \"zip code\" TEXT)",
		"INSERT INTO \"customer_list\" VALUES ('c1', 'Ada', '35200'), ('c2', 'Grace', '17886'), ('c3', 'Edsger', '35200')",
	} {
		if _, err := storage.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "db.yaml")
	declaration := "database: {id: sample, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    customer_list:\n      fields:\n        id: {type: string}\n        name: {type: string}\n"
	if err := os.WriteFile(manifestPath, []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	service := server.New("test", map[string]*core.Database{"sample": db}, server.WithReadOnly(true))
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()

	const dtql = "from: {name: customer_list}\n" +
		"columns: [{field: name}, {field: \"zip code\"}]\n" +
		"where: {op: '==', left: {field: \"zip code\"}, right: {value: '35200'}}\n" +
		"orderBy: [{field: \"zip code\"}, {field: name, desc: true}]\n"
	wire := `{"collection":"customer_list","where":[{"field":"zip code","op":"==","value":"35200"}],"orderBy":[{"field":"zip code"}]}`
	const wantNames = "Edsger,Ada"

	names := func(t *testing.T, body map[string]any, order string) string {
		t.Helper()
		records, _ := body["records"].([]any)
		var got string
		for i, r := range records {
			row, _ := r.(map[string]any)
			data, _ := row["data"].(map[string]any)
			if data["zip code"] != "35200" {
				t.Fatalf("a row outside the filter was returned: %v", row)
			}
			if i > 0 {
				got += ","
			}
			got += data["name"].(string)
		}
		if order != "" && got != order {
			t.Fatalf("names = %q, want %q (body %v)", got, order, body)
		}
		return got
	}
	const prefix = "/v1/databases/sample"
	t.Run("dtql POST", func(t *testing.T) {
		status, body := send(t, host, guardCall{method: "POST", path: prefix + "/dtql", body: dtql})
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, body)
		}
		names(t, body, wantNames)
	})
	t.Run("dtql GET", func(t *testing.T) {
		status, body := send(t, host, guardCall{method: "GET", path: prefix + "/dtql?q=" + url.QueryEscape(dtql)})
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, body)
		}
		names(t, body, wantNames)
	})
	t.Run("dtql snapshot page", func(t *testing.T) {
		status, body := send(t, host, guardCall{method: "POST", path: prefix + "/dtql", body: dtql, headers: map[string]string{"OVDB-Page-Size": "10"}})
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, body)
		}
		names(t, body, wantNames)
	})
	t.Run("wire query", func(t *testing.T) {
		status, body := send(t, host, guardCall{method: "POST", path: prefix + "/query", body: wire})
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, body)
		}
		if got := names(t, body, ""); got != "Ada,Edsger" && got != "Edsger,Ada" {
			t.Fatalf("names = %q", got)
		}
	})
	// The characters that could end a quoted identifier are still a 400.
	t.Run("a quote in the name", func(t *testing.T) {
		status, body := send(t, host, guardCall{method: "POST", path: prefix + "/dtql", body: "from: {name: customer_list}\norderBy: [{field: \"zip\\\" code\"}]\n"})
		if status != http.StatusBadRequest {
			t.Fatalf("status %d body %v", status, body)
		}
	})
}
