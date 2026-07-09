// Package server_test is an HTTP integration test suite for the OpenVaultDB
// server package. It spins a real httptest.Server over a real ingitdb
// schemaless database and exercises every documented endpoint.
package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// schemalessManifest is the standard ingitdb schemaless manifest used by most
// tests. Each test that calls startTestServer gets its own t.TempDir() so
// databases are fully isolated.
const schemalessManifest = `
database:
  id: testdb
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
`

// startTestServer spins a real httptest.Server backed by a freshly-mounted
// ingitdb database and registers cleanup via t.Cleanup. It returns the base
// URL of the server (no trailing slash).
func startTestServer(t *testing.T, manifestYAML string) string {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ts := httptest.NewServer(server.New("test", map[string]*core.Database{db.ID(): db}).Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// jsonBody encodes v to a *strings.Reader ready for use as an http.Request body.
func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return bytes.NewReader(b)
}

// decodeJSON decodes the response body into dst (map[string]any or similar).
// It always drains the body so connections are reused.
func decodeJSON(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode JSON (status %d): %v", resp.StatusCode, err)
	}
}

// mustStatus fatals if resp.StatusCode != want.
func mustStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("expected HTTP %d, got %d; body: %s", want, resp.StatusCode, body)
	}
}

// drainClose discards the response body and closes it.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// doRequest is a thin helper that issues a request with an optional JSON body
// (v == nil means no body) and returns the response. It does not close the
// body — callers must do so.
func doRequest(t *testing.T, method, url string, v any) *http.Response {
	t.Helper()
	var body io.Reader
	if v != nil {
		body = jsonBody(t, v)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("http.NewRequest %s %s: %v", method, url, err)
	}
	if v != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	return resp
}

// ── GET /v1/status ──────────────────────────────────────────────────────────

func TestStatus(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	resp := doRequest(t, http.MethodGet, base+"/v1/status", nil)
	mustStatus(t, resp, http.StatusOK)
	var body map[string]any
	decodeJSON(t, resp, &body)
	if body["name"] != "OpenVaultDB" {
		t.Errorf("status.name: got %q, want %q", body["name"], "OpenVaultDB")
	}
}

// ── GET /v1/databases ───────────────────────────────────────────────────────

func TestDatabases(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	resp := doRequest(t, http.MethodGet, base+"/v1/databases", nil)
	mustStatus(t, resp, http.StatusOK)
	var body map[string]any
	decodeJSON(t, resp, &body)
	dbs, ok := body["databases"].([]any)
	if !ok {
		t.Fatalf("databases field missing or not an array: %v", body)
	}
	if len(dbs) != 1 {
		t.Fatalf("expected 1 database, got %d", len(dbs))
	}
	entry, ok := dbs[0].(map[string]any)
	if !ok {
		t.Fatalf("first database entry is not an object: %v", dbs[0])
	}
	if entry["id"] != "testdb" {
		t.Errorf("databases[0].id: got %q, want %q", entry["id"], "testdb")
	}
}

// ── GET /v1/databases/{db} ──────────────────────────────────────────────────

func TestDatabase_NotFound(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	resp := doRequest(t, http.MethodGet, base+"/v1/databases/nonexistent", nil)
	mustStatus(t, resp, http.StatusNotFound)
	var body map[string]any
	decodeJSON(t, resp, &body)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field missing or not an object: %v", body)
	}
	if errObj["code"] != "not_found" {
		t.Errorf("error.code: got %q, want %q", errObj["code"], "not_found")
	}
}

// ── Record CRUD ──────────────────────────────────────────────────────────────

func TestRecordCRUD(t *testing.T) {
	t.Run("PutThenGet", func(t *testing.T) {
		base := startTestServer(t, schemalessManifest)
		putURL := base + "/v1/databases/testdb/records/users/u1"
		// PUT → 204
		resp := doRequest(t, http.MethodPut, putURL, map[string]any{
			"data": map[string]any{"name": "alice", "age": 30},
		})
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)

		// GET → 200, data round-trips
		resp = doRequest(t, http.MethodGet, putURL, nil)
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		decodeJSON(t, resp, &body)
		data, ok := body["data"].(map[string]any)
		if !ok {
			t.Fatalf("data field missing or not an object: %v", body)
		}
		if data["name"] != "alice" {
			t.Errorf("data.name: got %v, want %q", data["name"], "alice")
		}
		// JSON numbers decode as float64.
		if data["age"] != float64(30) {
			t.Errorf("data.age: got %v (%T), want float64(30)", data["age"], data["age"])
		}
	})

	t.Run("PostInsertConflict409", func(t *testing.T) {
		base := startTestServer(t, schemalessManifest)
		recURL := base + "/v1/databases/testdb/records/users/u2"
		// Create the record first with PUT.
		resp := doRequest(t, http.MethodPut, recURL, map[string]any{
			"data": map[string]any{"x": 1},
		})
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)

		// POST on existing key → 409 already_exists.
		resp = doRequest(t, http.MethodPost, recURL, map[string]any{
			"data": map[string]any{"x": 2},
		})
		mustStatus(t, resp, http.StatusConflict)
		var body map[string]any
		decodeJSON(t, resp, &body)
		errObj, ok := body["error"].(map[string]any)
		if !ok {
			t.Fatalf("error field missing: %v", body)
		}
		if errObj["code"] != "already_exists" {
			t.Errorf("error.code: got %q, want %q", errObj["code"], "already_exists")
		}
	})

	t.Run("PatchUpdateThenGet", func(t *testing.T) {
		base := startTestServer(t, schemalessManifest)
		recURL := base + "/v1/databases/testdb/records/users/u3"
		// PUT initial data.
		resp := doRequest(t, http.MethodPut, recURL, map[string]any{
			"data": map[string]any{"score": 10, "meta": map[string]any{"x": 1}},
		})
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)

		// PATCH: update score and nested meta.x.
		resp = doRequest(t, http.MethodPatch, recURL, map[string]any{
			"updates": []any{
				map[string]any{"fieldName": "score", "value": 99},
				map[string]any{"fieldPath": []string{"meta", "x"}, "value": 42},
			},
		})
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)

		// GET → verify mutations.
		resp = doRequest(t, http.MethodGet, recURL, nil)
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		decodeJSON(t, resp, &body)
		data, ok := body["data"].(map[string]any)
		if !ok {
			t.Fatalf("data field missing: %v", body)
		}
		if data["score"] != float64(99) {
			t.Errorf("data.score: got %v, want float64(99)", data["score"])
		}
		meta, ok := data["meta"].(map[string]any)
		if !ok {
			t.Fatalf("data.meta is not an object: %v", data["meta"])
		}
		if meta["x"] != float64(42) {
			t.Errorf("data.meta.x: got %v, want float64(42)", meta["x"])
		}
	})

	t.Run("PatchMissing404", func(t *testing.T) {
		base := startTestServer(t, schemalessManifest)
		resp := doRequest(t, http.MethodPatch,
			base+"/v1/databases/testdb/records/users/u_missing",
			map[string]any{
				"updates": []any{
					map[string]any{"fieldName": "x", "value": 1},
				},
			})
		mustStatus(t, resp, http.StatusNotFound)
		drainClose(resp)
	})

	t.Run("DeleteIdempotent", func(t *testing.T) {
		base := startTestServer(t, schemalessManifest)
		recURL := base + "/v1/databases/testdb/records/users/u4"
		// Create.
		resp := doRequest(t, http.MethodPut, recURL, map[string]any{
			"data": map[string]any{"v": 1},
		})
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)

		// First DELETE → 204.
		resp = doRequest(t, http.MethodDelete, recURL, nil)
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)

		// Second DELETE → also 204 (inGitDB is idempotent on delete).
		resp = doRequest(t, http.MethodDelete, recURL, nil)
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)
	})

	t.Run("BadKey_OddSegments_400", func(t *testing.T) {
		// A URL with only a collection name (no id) is an odd-segment key:
		// parseKeyPath("users") → ParseKey(["users"]) → error (need even segments).
		base := startTestServer(t, schemalessManifest)
		resp := doRequest(t, http.MethodGet,
			base+"/v1/databases/testdb/records/users", nil)
		// The server should return 400 bad_request.
		if resp.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("expected 400, got %d; body: %s", resp.StatusCode, body)
		}
		drainClose(resp)
	})

	t.Run("BadKey_DotDot_400", func(t *testing.T) {
		// Inject a ".." segment via a raw URL so the HTTP mux cannot clean it.
		// We build the request URL manually using req.URL.Opaque so net/http
		// does NOT percent-encode the literal dots, and the server sees the
		// path with ".." as an actual segment that ParseKeyPath will reject.
		base := startTestServer(t, schemalessManifest)
		rawURL := base + "/v1/databases/testdb/records/users%2F.."
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("http.NewRequest: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		// %2F is a literal slash: the path becomes /v1/databases/testdb/records/users/..
		// which mux.ServeMux canonicalizes to /v1/databases/testdb/records/ — the
		// empty key after /records/ yields 400 bad_request from the handler.
		// Accept either 400 or 404 (the latter if mux rejects it).
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("expected 400 or 404 for '..' key, got %d; body: %s", resp.StatusCode, body)
		}
		drainClose(resp)
	})
}

// ── POST /v1/databases/{db}/batch ───────────────────────────────────────────

func TestBatch(t *testing.T) {
	t.Run("InsertThenUpdateSameKey", func(t *testing.T) {
		base := startTestServer(t, schemalessManifest)
		batchURL := base + "/v1/databases/testdb/batch"
		resp := doRequest(t, http.MethodPost, batchURL, map[string]any{
			"ops": []any{
				map[string]any{
					"op":   "insert",
					"key":  "items/i1",
					"data": map[string]any{"v": 1},
				},
				map[string]any{
					"op":  "update",
					"key": "items/i1",
					"updates": []any{
						map[string]any{"fieldName": "v", "value": 2},
					},
				},
			},
		})
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		decodeJSON(t, resp, &body)
		if body["applied"] != float64(2) {
			t.Errorf("applied: got %v, want float64(2)", body["applied"])
		}

		// Verify final state via GET.
		getResp := doRequest(t, http.MethodGet,
			base+"/v1/databases/testdb/records/items/i1", nil)
		mustStatus(t, getResp, http.StatusOK)
		var getBody map[string]any
		decodeJSON(t, getResp, &getBody)
		data, ok := getBody["data"].(map[string]any)
		if !ok {
			t.Fatalf("data field missing: %v", getBody)
		}
		if data["v"] != float64(2) {
			t.Errorf("items/i1.v: got %v, want float64(2)", data["v"])
		}
	})

	t.Run("FailingOpPreventsWrite", func(t *testing.T) {
		// A batch with an update to a missing record should fail the whole batch
		// pre-flight (validateOps), so neither op is committed.
		base := startTestServer(t, schemalessManifest)
		batchURL := base + "/v1/databases/testdb/batch"
		resp := doRequest(t, http.MethodPost, batchURL, map[string]any{
			"ops": []any{
				map[string]any{
					"op":   "insert",
					"key":  "items/fresh",
					"data": map[string]any{"x": 1},
				},
				map[string]any{
					"op":  "update",
					"key": "items/does_not_exist",
					"updates": []any{
						map[string]any{"fieldName": "x", "value": 2},
					},
				},
			},
		})
		// The pre-flight validator should reject the batch with 4xx.
		if resp.StatusCode < 400 || resp.StatusCode >= 600 {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("expected 4xx/5xx for failing batch, got %d; body: %s", resp.StatusCode, body)
		}
		drainClose(resp)

		// items/fresh must NOT have been written.
		getResp := doRequest(t, http.MethodGet,
			base+"/v1/databases/testdb/records/items/fresh", nil)
		mustStatus(t, getResp, http.StatusNotFound)
		drainClose(getResp)
	})
}

// ── POST /v1/databases/{db}/query ───────────────────────────────────────────

func TestQuery(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	queryURL := base + "/v1/databases/testdb/query"

	// Insert 3 product records.
	products := []struct {
		id   string
		data map[string]any
	}{
		{"p1", map[string]any{"price": 10, "category": "A"}},
		{"p2", map[string]any{"price": 20, "category": "B"}},
		{"p3", map[string]any{"price": 30, "category": "A"}},
	}
	for _, p := range products {
		resp := doRequest(t, http.MethodPut,
			fmt.Sprintf("%s/v1/databases/testdb/records/products/%s", base, p.id),
			map[string]any{"data": p.data})
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)
	}

	t.Run("FilterByCategory", func(t *testing.T) {
		resp := doRequest(t, http.MethodPost, queryURL, map[string]any{
			"collection": "products",
			"where": []any{
				map[string]any{"field": "category", "op": "==", "value": "A"},
			},
		})
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		decodeJSON(t, resp, &body)
		records := body["records"].([]any)
		if len(records) != 2 {
			t.Errorf("FilterByCategory: expected 2 results, got %d", len(records))
		}
	})

	t.Run("OrderByPriceDesc", func(t *testing.T) {
		resp := doRequest(t, http.MethodPost, queryURL, map[string]any{
			"collection": "products",
			"orderBy": []any{
				map[string]any{"field": "price", "desc": true},
			},
		})
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		decodeJSON(t, resp, &body)
		records := body["records"].([]any)
		if len(records) != 3 {
			t.Errorf("OrderByPriceDesc: expected 3 results, got %d", len(records))
		}
		// Verify descending price order (30, 20, 10).
		prices := make([]float64, 0, len(records))
		for _, r := range records {
			rec, ok := r.(map[string]any)
			if !ok {
				continue
			}
			if data, ok := rec["data"].(map[string]any); ok {
				if p, ok := data["price"].(float64); ok {
					prices = append(prices, p)
				}
			}
		}
		expected := []float64{30, 20, 10}
		for i, want := range expected {
			if i >= len(prices) {
				break
			}
			if prices[i] != want {
				t.Errorf("OrderByPriceDesc: record[%d].price = %v, want %v", i, prices[i], want)
			}
		}
	})

	t.Run("Limit1", func(t *testing.T) {
		resp := doRequest(t, http.MethodPost, queryURL, map[string]any{
			"collection": "products",
			"limit":      1,
		})
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		decodeJSON(t, resp, &body)
		records := body["records"].([]any)
		if len(records) != 1 {
			t.Errorf("Limit1: expected 1 result, got %d", len(records))
		}
	})

	t.Run("KeysOnly", func(t *testing.T) {
		resp := doRequest(t, http.MethodPost, queryURL, map[string]any{
			"collection": "products",
			"keysOnly":   true,
		})
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		decodeJSON(t, resp, &body)
		records := body["records"].([]any)
		if len(records) == 0 {
			t.Error("KeysOnly: expected at least 1 result")
		}
		// In keys-only mode the "data" key must be absent or nil.
		for i, r := range records {
			rec, ok := r.(map[string]any)
			if !ok {
				t.Errorf("record[%d] is not an object", i)
				continue
			}
			if _, hasData := rec["data"]; hasData {
				t.Errorf("record[%d] has a 'data' key in keysOnly mode: %v", i, rec)
			}
			if rec["key"] == nil || rec["key"] == "" {
				t.Errorf("record[%d] is missing a 'key' field", i)
			}
		}
	})
}

// ── POST /v1/databases/{db}/dtql ────────────────────────────────────────────

func TestDTQL(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	dtqlURL := base + "/v1/databases/testdb/dtql"

	// Insert a widget record first.
	putResp := doRequest(t, http.MethodPut,
		base+"/v1/databases/testdb/records/widgets/w1",
		map[string]any{"data": map[string]any{"color": "red"}})
	mustStatus(t, putResp, http.StatusNoContent)
	drainClose(putResp)

	t.Run("ValidDTQL", func(t *testing.T) {
		// Minimal DTQL YAML: just a from clause selecting all widgets.
		// Note: DTQL uses SelectIntoRecordset() which leaves IntoRecord()==nil,
		// so executeDalQuery treats it as keys-only — records carry keys but no
		// data field. We verify the inserted widget key appears in results.
		dtqlDoc := "from:\n  name: widgets\n"
		req, err := http.NewRequest(http.MethodPost, dtqlURL, strings.NewReader(dtqlDoc))
		if err != nil {
			t.Fatalf("http.NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		mustStatus(t, resp, http.StatusOK)
		var body map[string]any
		decodeJSON(t, resp, &body)
		records, ok := body["records"].([]any)
		if !ok {
			t.Fatalf("records field missing or not an array: %v", body)
		}
		if len(records) == 0 {
			t.Error("DTQL: expected at least 1 result")
		}
		// Verify the inserted widget key appears in results.
		found := false
		for _, r := range records {
			rec, ok := r.(map[string]any)
			if !ok {
				continue
			}
			if k, ok := rec["key"].(string); ok && strings.HasSuffix(k, "/w1") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DTQL: key widgets/w1 not found in results: %v", records)
		}
	})

	t.Run("InvalidDTQL_Error", func(t *testing.T) {
		// Send clearly invalid YAML that the DTQL parser must reject.
		badDTQL := "garbage: ::invalid::\n"
		req, err := http.NewRequest(http.MethodPost, dtqlURL, strings.NewReader(badDTQL))
		if err != nil {
			t.Fatalf("http.NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "text/plain")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		if resp.StatusCode < 400 {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Fatalf("expected 4xx for invalid DTQL, got %d; body: %s", resp.StatusCode, body)
		}
		var body map[string]any
		decodeJSON(t, resp, &body)
		if _, ok := body["error"]; !ok {
			t.Errorf("expected an 'error' key in the response body: %v", body)
		}
	})
}

// ── GET /v1/databases/{db}/inferred-schema ──────────────────────────────────

func TestInferredSchema(t *testing.T) {
	base := startTestServer(t, schemalessManifest)

	// Write at least one record so the inferred schema is non-empty.
	resp := doRequest(t, http.MethodPut,
		base+"/v1/databases/testdb/records/things/t1",
		map[string]any{"data": map[string]any{"color": "blue", "count": 3}})
	mustStatus(t, resp, http.StatusNoContent)
	drainClose(resp)

	resp = doRequest(t, http.MethodGet,
		base+"/v1/databases/testdb/inferred-schema", nil)
	mustStatus(t, resp, http.StatusOK)
	var body any
	decodeJSON(t, resp, &body)
	if body == nil {
		t.Error("inferred-schema: expected non-nil response body")
	}
}
