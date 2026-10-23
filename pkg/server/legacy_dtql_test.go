package server_test

import (
	"net/http"
	"strings"
	"testing"
)

// The pre-rename DTQL routes and discovery key stay served until TugQL cutover
// step 9; this test is deleted with legacy_dtql.go.
func TestLegacyDTQLWireCompat(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	put := doRequest(t, http.MethodPut, base+"/v1/databases/testdb/records/widgets/w1",
		map[string]any{"data": map[string]any{"color": "red"}})
	mustStatus(t, put, http.StatusNoContent)
	drainClose(put)

	post := func(url, doc string) map[string]any {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(doc))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		decodeJSON(t, resp, &body)
		return body
	}
	for _, c := range []struct{ name, url, doc string }{
		{"database tugql", "/v1/databases/testdb/tugql", "from:\n  name: widgets\n"},
		{"database dtql", "/v1/databases/testdb/dtql", "from:\n  name: widgets\n"},
		{"cross tugql", "/v1/tugql", "from:\n  database: testdb\n  name: widgets\n"},
		{"cross dtql", "/v1/dtql", "from:\n  database: testdb\n  name: widgets\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if r, ok := post(base+c.url, c.doc)["records"].([]any); !ok || len(r) == 0 {
				t.Fatalf("no records from %s", c.url)
			}
		})
	}

	t.Run("metadata carries both endpoint keys", func(t *testing.T) {
		resp, err := http.Get(base + "/v1/databases/testdb")
		if err != nil {
			t.Fatal(err)
		}
		mustStatus(t, resp, http.StatusOK)
		var meta map[string]any
		decodeJSON(t, resp, &meta)
		eps, _ := meta["endpoints"].(map[string]any)
		tq, _ := eps["tugql"].(string)
		dq, _ := eps["dtql"].(string)
		if !strings.HasSuffix(tq, "/v1/databases/testdb/tugql") || !strings.HasSuffix(dq, "/v1/databases/testdb/dtql") {
			t.Fatalf("endpoints = %v", eps)
		}
	})
}
