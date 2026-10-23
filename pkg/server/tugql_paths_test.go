package server_test

import (
	"net/http"
	"testing"
)

// The pre-rename DTQL routes are gone (TugQL cutover step 9): /dtql is not
// served, and discovery advertises only the tugql endpoint.
func TestPreRenameDTQLRoutesAreNotServed(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	for _, path := range []string{"/v1/databases/testdb/dtql", "/v1/dtql"} {
		resp, err := http.Post(base+path, "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		drainClose(resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s still served", path)
		}
	}
	resp, err := http.Get(base + "/v1/databases/testdb")
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusOK)
	var meta map[string]any
	decodeJSON(t, resp, &meta)
	eps, _ := meta["endpoints"].(map[string]any)
	if _, ok := eps["dtql"]; ok || len(eps) != 1 {
		t.Fatalf("endpoints = %v", eps)
	}
}
