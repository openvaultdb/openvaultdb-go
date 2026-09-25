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

func TestDTQLJSONBoundParameterJourney(t *testing.T) {
	db, _ := mountSQLite(t)
	t.Cleanup(func() { _ = db.Close() })
	handler := server.New("test", map[string]*core.Database{"notes": db}, server.WithReadOnly(true)).Handler()
	metadata := httptest.NewRecorder()
	handler.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/v1/databases/notes", nil))
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"queryFormat":"dtql-yaml+json"`) {
		t.Fatalf("metadata: %d %s", metadata.Code, metadata.Body.String())
	}
	query := map[string]any{"query": "from: {name: notes}\nwhere: {op: '==', left: {field: title}, right: {param: Title}}\nlimit: 5\n", "parameters": map[string]any{"Title": "hello"}}
	body, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/databases/notes/dtql", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"records":[]`) {
		t.Fatalf("query: %d %s", w.Code, w.Body.String())
	}
}
