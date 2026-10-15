package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// nativeRoutesBody is a request body valid for every route that reads one, so a
// refusal is the mount's and not a malformed-request answer.
const nativeRoutesBody = `{"data":{"a":1},"updates":[{"field":"a","value":1}],"ops":[{"op":"set","key":"%KEY%","data":{"a":1}}]}`

// TestNativePostgresMountNeverAnswers500ForUnsupportedOperations: on a native read-only
// PostgreSQL mount every route that takes a database answers an unsupported operation
// with a refusal of its own (4xx or 501), never a 500, and the body leaks nothing of the
// mount. The keyed read of a record is the one that used to answer 500 internal.
func TestNativePostgresMountNeverAnswers500ForUnsupportedOperations(t *testing.T) {
	collection := capabilitiesNativePostgresCollection(t)
	_, host := relFakeServer(t, &relFakeExecutor{}, []*core.Database{capabilitiesNativePostgres(t, "pg")})
	key := url.PathEscape(collection) + "/1"
	body := strings.ReplaceAll(nativeRoutesBody, "%KEY%", collection+"/1")
	routes := []struct {
		method, path string
		body         string // "": the common body
		want         int    // 0: any status the test accepts below
		code         string // "": not checked
	}{
		{http.MethodGet, "/v1/databases/pg/records/" + key, "", http.StatusNotImplemented, "operation_unsupported"},
		{http.MethodHead, "/v1/databases/pg/records/" + key, "", http.StatusNotImplemented, ""},
		{http.MethodGet, "/v1/databases/pg/read?key=" + url.QueryEscape(collection+"/1"), "", http.StatusNotImplemented, "operation_unsupported"},
		{http.MethodPut, "/v1/databases/pg/records/" + key, "", 0, ""},
		{http.MethodPost, "/v1/databases/pg/records/" + key, "", 0, ""},
		{http.MethodPatch, "/v1/databases/pg/records/" + key, "", 0, ""},
		{http.MethodDelete, "/v1/databases/pg/records/" + key, "", 0, ""},
		{http.MethodPost, "/v1/databases/pg/batch", "", 0, ""},
		{http.MethodPost, "/v1/databases/pg/query", `{"collection":"` + collection + `"}`, http.StatusNotImplemented, "query_unsupported"},
		{http.MethodGet, "/v1/databases/pg/query", "", 0, ""},
		{http.MethodGet, "/v1/databases/pg", "", http.StatusOK, ""},
		{http.MethodGet, "/v1/databases/pg/inferred-schema", "", 0, ""},
		{http.MethodPost, "/v1/databases/pg/dtql", "", 0, ""},
		{http.MethodGet, "/v1/databases/pg/dtql", "", 0, ""},
		{http.MethodPost, "/v1/databases/pg/tugql", "", 0, ""},
		{http.MethodGet, "/v1/databases/pg/access/layers", "", 0, ""},
		{http.MethodGet, "/v1/databases/pg/access/policies", "", 0, ""},
		{http.MethodPost, "/v1/databases/pg/access/evaluate", "", 0, ""},
		{http.MethodPost, "/v1/databases/pg/access/evidence", "", 0, ""},
		{http.MethodPost, "/v1/databases/pg/reset", "", http.StatusNotFound, ""},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			sent := rt.body
			if sent == "" {
				sent = body
			}
			resp := relFakeDo(t, host, rt.method, rt.path, "", sent, map[string]string{"Content-Type": "application/json"})
			if resp.status >= 500 && resp.status != http.StatusNotImplemented {
				t.Fatalf("status %d, want a refusal below 500 or 501: %s", resp.status, resp.raw)
			}
			if rt.want != 0 && resp.status != rt.want {
				t.Fatalf("status %d, want %d: %s", resp.status, rt.want, resp.raw)
			}
			if rt.code != "" && resp.code() != rt.code {
				t.Fatalf("code %q, want %q: %s", resp.code(), rt.code, resp.raw)
			}
			if resp.code() == "internal" {
				t.Fatalf("answered internal: %s", resp.raw)
			}
			for _, leak := range []string{"structured reads", "dal:", "not supported:"} {
				if strings.Contains(resp.raw, leak) {
					t.Fatalf("body leaks %q: %s", leak, resp.raw)
				}
			}
		})
	}
}
