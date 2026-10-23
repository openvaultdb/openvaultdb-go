package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLegacyDTQLMediaTypeAndCacheEndpoints(t *testing.T) {
	for _, ct := range []string{"application/vnd.tugql.operation+json", legacyDTQLOperationMediaType} {
		r := httptest.NewRequest(http.MethodPatch, "/x", nil)
		r.Header.Set("Content-Type", ct)
		if !isProtectedUpdate(r) {
			t.Errorf("%s not treated as a protected update", ct)
		}
	}
	for _, path := range []string{"/v1/dtql", "/v1/tugql", "/v1/databases/a/dtql", "/v1/databases/a/tugql"} {
		if !isReadCacheEndpoint(httptest.NewRequest(http.MethodGet, path, nil)) {
			t.Errorf("%s is not a read-cache endpoint", path)
		}
	}
}
