package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// startKeyValidationServer mounts an auth-enabled inGitDB database "dev" and
// returns the server plus the manifest directory (the database root is
// <dir>/data), so tests can prove nothing was written outside a collection.
func startKeyValidationServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(manifestPath, []byte(`
database:
  id: dev
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New("test", map[string]*core.Database{"dev": db},
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store})).Handler())
	t.Cleanup(ts.Close)
	return ts, dir
}

// snapshotFiles lists every file path under root (relative), for before/after
// comparison.
func snapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		b, _ := os.ReadFile(path)
		files[rel] = string(b)
		return nil
	})
	return files
}

func errorCode(t *testing.T, body string) string {
	t.Helper()
	var eb struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &eb); err != nil {
		t.Fatalf("response is not the /v1 error shape: %q", body)
	}
	return eb.Error.Code
}

// Repro strings from the path-traversal report: escaped separators inside a
// record id or collection name decode to "..", escaping the collection on the
// inGitDB engine. Every one must be rejected at the HTTP boundary with
// 400 invalid_key, for every method, before any file is touched.
func TestRecordKeyTraversalRejected(t *testing.T) {
	ts, dir := startKeyValidationServer(t)
	if status, body := request(t, ts, http.MethodPut, "/v1/databases/dev/records/secrets/s1", ownerToken, `{"data":{"hush":true}}`); status != http.StatusNoContent {
		t.Fatalf("seed: %d %s", status, body)
	}
	before := snapshotFiles(t, dir)

	keys := []string{
		"items/%2E%2E%2F%2E%2E%2Fp2",
		"items/%2E%2E%2F%2E%2E%2F.git%2Fhooks%2Fpre-commit",
		"notes/%2E%2E%2F%2E%2E%2Fsecrets%2F%24records%2Fs1",
		"items/%2E",
		"items/%2E%2E",
		"%2E%2E/p2",
		"%2E%2E%2Fsecrets/s1",
		"items/..%5C..%5Cp2",
		"lists/%2E%2E%2F%2E%2E/items/x",
		"lists/l1/%2E%2E%2F%2E%2E%2Fsecrets/x",
		"items/a%00b",
		"items/a%0Ab",
		"items/a%1Fb",
		"items/a%7Fb",
		"items/%2F",
	}
	methods := []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodHead, ""},
		{http.MethodPut, `{"data":{"pwned":true}}`},
		{http.MethodPost, `{"data":{"pwned":true}}`},
		{http.MethodPatch, `{"updates":[{"field":"pwned","value":true}]}`},
		{http.MethodDelete, ""},
	}
	for _, key := range keys {
		for _, m := range methods {
			status, body := request(t, ts, m.method, "/v1/databases/dev/records/"+key, ownerToken, m.body)
			if status != http.StatusBadRequest {
				t.Errorf("%s %s: status %d (want 400): %s", m.method, key, status, body)
				continue
			}
			if m.method != http.MethodHead {
				if code := errorCode(t, body); code != "invalid_key" {
					t.Errorf("%s %s: code %q (want invalid_key)", m.method, key, code)
				}
			}
		}
	}

	after := snapshotFiles(t, dir)
	for path, content := range after {
		if before[path] != content {
			t.Errorf("rejected request changed %s", path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Errorf("rejected request removed %s", path)
		}
	}
}

// Legitimate ids that merely contain dots or an escaped slash keep working.
func TestRecordKeyDottedIDsAccepted(t *testing.T) {
	ts, _ := startKeyValidationServer(t)
	for _, key := range []string{"items/a.b", "items/...", "items/..x", "items/foo%2Fbar"} {
		if status, body := request(t, ts, http.MethodPut, "/v1/databases/dev/records/"+key, ownerToken, `{"data":{"ok":true}}`); status != http.StatusNoContent {
			t.Errorf("PUT %s: %d %s", key, status, body)
		}
		if status, body := request(t, ts, http.MethodGet, "/v1/databases/dev/records/"+key, ownerToken, ""); status != http.StatusOK {
			t.Errorf("GET %s: %d %s", key, status, body)
		}
	}
}

func TestBatchQueryDTQLKeyValidation(t *testing.T) {
	ts, dir := startKeyValidationServer(t)
	before := snapshotFiles(t, dir)
	for _, tc := range []struct{ name, path, body string }{
		{"batch traversal id", "/v1/databases/dev/batch", `{"ops":[{"op":"set","key":"items/%2E%2E%2F%2E%2E%2Fp2","data":{"x":1}}]}`},
		{"batch traversal into secrets", "/v1/databases/dev/batch", `{"ops":[{"op":"set","key":"notes/%2E%2E%2F%2E%2E%2Fsecrets%2F%24records%2Fs1","data":{"x":1}}]}`},
		{"batch control char", "/v1/databases/dev/batch", `{"ops":[{"op":"delete","key":"items/a%00b"}]}`},
		{"query dotdot collection", "/v1/databases/dev/query", `{"collection":".."}`},
		{"query traversal collection", "/v1/databases/dev/query", `{"collection":"../../secrets"}`},
		{"query control collection", "/v1/databases/dev/query", `{"collection":"a\u0000b"}`},
		{"query empty collection", "/v1/databases/dev/query", `{"collection":""}`},
		{"query traversal parent", "/v1/databases/dev/query", `{"collection":"items","parent":"lists/%2E%2E%2F%2E%2E"}`},
		{"query odd parent", "/v1/databases/dev/query", `{"collection":"items","parent":"lists"}`},
		{"dtql dotdot collection", "/v1/databases/dev/dtql", "from:\n  name: ..\n"},
		{"dtql traversal collection", "/v1/databases/dev/dtql", "from:\n  name: ../../secrets\n"},
		{"dtql control collection", "/v1/databases/dev/dtql", "from:\n  name: \"a\\u0001b\"\n"},
	} {
		status, body := request(t, ts, http.MethodPost, tc.path, ownerToken, tc.body)
		if status != http.StatusBadRequest || errorCode(t, body) != "invalid_key" {
			t.Errorf("%s: status %d body %s (want 400 invalid_key)", tc.name, status, body)
		}
	}
	after := snapshotFiles(t, dir)
	if len(after) != len(before) {
		t.Errorf("rejected requests changed the file set: before %d, after %d", len(before), len(after))
	}
}

// Capability scoping is checked on the root collection the driver will write
// to: a notes-scoped token can neither smuggle a write into secrets through an
// escaped id nor query a secrets subcollection by naming it as a parent.
func TestScopeBypassViaCraftedKeys(t *testing.T) {
	ts, _ := startKeyValidationServer(t)
	for _, kv := range [][2]string{
		{"secrets/s1", `{"data":{"hush":true}}`},
		{"secrets/s1/items/i1", `{"data":{"hush":"nested"}}`},
		{"notes/n1", `{"data":{"text":"note"}}`},
	} {
		if status, body := request(t, ts, http.MethodPut, "/v1/databases/dev/records/"+kv[0], ownerToken, kv[1]); status != http.StatusNoContent {
			t.Fatalf("seed %s: %d %s", kv[0], status, body)
		}
	}
	appToken := connectFlow(t, ts, "records:read:notes,records:write:notes")

	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"escaped id write into secrets", http.MethodPut, "/v1/databases/dev/records/notes/%2E%2E%2F%2E%2E%2Fsecrets%2F%24records%2Fs1", `{"data":{"pwned":true}}`, http.StatusBadRequest},
		{"batch escaped id write into secrets", http.MethodPost, "/v1/databases/dev/batch", `{"ops":[{"op":"set","key":"notes/%2E%2E%2F%2E%2E%2Fsecrets%2F%24records%2Fs1","data":{"pwned":true}}]}`, http.StatusBadRequest},
		{"query secrets subcollection via parent", http.MethodPost, "/v1/databases/dev/query", `{"collection":"items","parent":"secrets/s1"}`, http.StatusForbidden},
		{"query subcollection named like granted root", http.MethodPost, "/v1/databases/dev/query", `{"collection":"notes","parent":"secrets/s1"}`, http.StatusForbidden},
		{"query own subcollection allowed", http.MethodPost, "/v1/databases/dev/query", `{"collection":"items","parent":"notes/n1"}`, http.StatusOK},
	} {
		status, body := request(t, ts, tc.method, tc.path, appToken, tc.body)
		if status != tc.want {
			t.Errorf("%s: status %d (want %d): %s", tc.name, status, tc.want, body)
		}
	}
	status, body := request(t, ts, http.MethodGet, "/v1/databases/dev/records/secrets/s1", ownerToken, "")
	if status != http.StatusOK || strings.Contains(body, "pwned") {
		t.Errorf("secrets/s1 was modified: %d %s", status, body)
	}
}

// Queries on a nested collection return keys relative to the database root,
// including the parent path, so a key can be fed straight back to /records.
func TestNestedQueryReturnsFullKeys(t *testing.T) {
	ts, _ := startKeyValidationServer(t)
	for _, key := range []string{"lists/to-buy/items/x", "lists/to-buy/items/y.z", "lists/other/items/w"} {
		if status, body := request(t, ts, http.MethodPut, "/v1/databases/dev/records/"+key, ownerToken, `{"data":{"n":1}}`); status != http.StatusNoContent {
			t.Fatalf("seed %s: %d %s", key, status, body)
		}
	}
	for _, keysOnly := range []bool{false, true} {
		body := `{"collection":"items","parent":"lists/to-buy"}`
		if keysOnly {
			body = `{"collection":"items","parent":"lists/to-buy","keysOnly":true}`
		}
		status, resp := request(t, ts, http.MethodPost, "/v1/databases/dev/query", ownerToken, body)
		if status != http.StatusOK {
			t.Fatalf("query keysOnly=%v: %d %s", keysOnly, status, resp)
		}
		var out struct {
			Records []struct {
				Key string `json:"key"`
			} `json:"records"`
		}
		if err := json.Unmarshal([]byte(resp), &out); err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, r := range out.Records {
			got[r.Key] = true
		}
		for _, want := range []string{"lists/to-buy/items/x", "lists/to-buy/items/y%2Ez"} {
			if !got[want] {
				t.Errorf("keysOnly=%v: key %q missing from %s", keysOnly, want, resp)
			}
		}
		if len(out.Records) != 2 {
			t.Errorf("keysOnly=%v: want 2 records, got %s", keysOnly, resp)
		}
		for key := range got {
			if status, body := request(t, ts, http.MethodGet, "/v1/databases/dev/records/"+key, ownerToken, ""); status != http.StatusOK {
				t.Errorf("returned key %q does not round-trip: %d %s", key, status, body)
			}
		}
	}
}
