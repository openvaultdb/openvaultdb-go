package server_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// TestDatabaseListsTheDeclaredCanonicalCollectionsOnly: GET /v1/databases/{db} on
// a SQLite mount lists the collections the manifest declares, by canonical name.
// The file also holds a table the manifest does not declare and the table named
// with the quote characters of the quoted spelling of a declared key; neither is
// listed. Every listed name is accepted by the query route.
func TestDatabaseListsTheDeclaredCanonicalCollectionsOnly(t *testing.T) {
	f := startSQLNamesWithQuotedTable(t)
	raw, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`CREATE TABLE undeclared_extra (id TEXT PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	status, body := f.call(t, "GET", "/v1/databases/dev", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, body)
	}
	var got []string
	for _, name := range body["collections"].([]any) {
		got = append(got, name.(string))
	}
	want := []string{"My`Table", `O"Brien`, "Order Details", "Orders", "Orders Status", "order-items", "select"}
	if !slices.Equal(got, want) {
		t.Errorf("collections %q, want %q", got, want)
	}
	for _, name := range got {
		query, _ := json.Marshal(map[string]string{"collection": name})
		if status, answer := f.call(t, "POST", "/v1/databases/dev/query", string(query)); status != http.StatusOK {
			t.Errorf("query of the listed collection %q: status %d: %v", name, status, answer)
		}
	}
}
