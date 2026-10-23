package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func TestTugQLJSONBoundParameterJourney(t *testing.T) {
	db, _ := mountSQLite(t)
	t.Cleanup(func() { _ = db.Close() })
	handler := server.New("test", map[string]*core.Database{"notes": db}, server.WithReadOnly(true)).Handler()
	metadata := httptest.NewRecorder()
	handler.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/v1/databases/notes", nil))
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"queryFormat":"tugql-yaml+json"`) {
		t.Fatalf("metadata: %d %s", metadata.Code, metadata.Body.String())
	}
	query := map[string]any{"query": "from: {name: notes}\nwhere: {op: '==', left: {field: title}, right: {param: Title}}\nlimit: 5\n", "parameters": map[string]any{"Title": "hello"}}
	body, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/databases/notes/tugql", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"records":[]`) {
		t.Fatalf("query: %d %s", w.Code, w.Body.String())
	}
}

// On an engine other than native PostgreSQL an integer parameter above 2^53 is
// refused with a message that says so, and a whole number written as a float
// still binds as it always did.
func TestTugQLParameterIntegersOnSQLite(t *testing.T) {
	db, _ := mountSQLite(t)
	t.Cleanup(func() { _ = db.Close() })
	handler := server.New("test", map[string]*core.Database{"notes": db}, server.WithReadOnly(true)).Handler()
	post := func(path string, parameters string) *httptest.ResponseRecorder {
		query := "from: {name: notes}\nwhere: {op: '==', left: {field: title}, right: {param: P}}\nlimit: 5\n"
		if path == "/v1/tugql" {
			query = "from: {name: notes, database: notes}\nwhere: {op: '==', left: {field: title}, right: {param: P}}\nlimit: 5\n"
		}
		body := `{"query":` + string(mustJSON(t, query)) + `,"parameters":{"P":` + parameters + `}}`
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/v1/databases/notes/tugql", "/v1/tugql"} {
		if w := post(path, "9007199254740993"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "exceeds the exact range (2^53-1)") {
			t.Fatalf("%s unsafe integer: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, value := range []string{"100.0", "1e0", "1.5", "9007199254740991"} {
		if w := post("/v1/databases/notes/tugql", value); w.Code != http.StatusOK {
			t.Fatalf("parameter %s: %d %s", value, w.Code, w.Body.String())
		}
	}
}
