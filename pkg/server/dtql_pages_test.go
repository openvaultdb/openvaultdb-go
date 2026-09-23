package server_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
	_ "modernc.org/sqlite"
)

func pagedDTQL(t *testing.T, url, doc string, size int, token string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("OVDB-Page-Size", fmt.Sprint(size))
	if token != "" {
		req.Header.Set("OVDB-Page-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	decodeJSON(t, resp, &body)
	return resp.StatusCode, body
}

func closePagedDTQL(t *testing.T, url, doc string, size int, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("OVDB-Page-Size", fmt.Sprint(size))
	req.Header.Set("OVDB-Page-Token", token)
	req.Header.Set("OVDB-Page-Close", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer drainClose(resp)
	return resp.StatusCode
}

func TestDTQLSnapshotPagesDoNotMixWrites(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	url := base + "/v1/databases/testdb/dtql"
	for i := 1; i <= 3; i++ {
		resp := doRequest(t, http.MethodPut, fmt.Sprintf("%s/v1/databases/testdb/records/items/i%d", base, i),
			map[string]any{"data": map[string]any{"rank": i}})
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)
	}
	doc := "from: {name: items}\norderBy: [{field: rank}]\n"
	status, first := pagedDTQL(t, url, doc, 1, "")
	if status != http.StatusOK || len(first["records"].([]any)) != 1 {
		t.Fatalf("first page: %d %#v", status, first)
	}
	token := first["nextPageToken"].(string)
	resp := doRequest(t, http.MethodPut, base+"/v1/databases/testdb/records/items/i2",
		map[string]any{"data": map[string]any{"rank": 200}})
	mustStatus(t, resp, http.StatusNoContent)
	drainClose(resp)
	resp = doRequest(t, http.MethodPut, base+"/v1/databases/testdb/records/items/i4",
		map[string]any{"data": map[string]any{"rank": 4}})
	mustStatus(t, resp, http.StatusNoContent)
	drainClose(resp)
	status, second := pagedDTQL(t, url, doc, 1, token)
	if status != http.StatusOK {
		t.Fatalf("second page: %d %#v", status, second)
	}
	secondRow := second["records"].([]any)[0].(map[string]any)
	if secondRow["key"] != "items/i2" || secondRow["data"].(map[string]any)["rank"] != float64(2) {
		t.Fatalf("second page changed after write: %#v", secondRow)
	}
	status, replay := pagedDTQL(t, url, doc, 1, token)
	if status != http.StatusOK || replay["nextPageToken"] != second["nextPageToken"] ||
		replay["records"].([]any)[0].(map[string]any)["data"].(map[string]any)["rank"] != float64(2) {
		t.Fatalf("retried page differs: %d %#v", status, replay)
	}
	finalToken := second["nextPageToken"].(string)
	status, final := pagedDTQL(t, url, doc, 1, finalToken)
	if status != http.StatusOK || final["nextPageToken"] != nil || final["records"].([]any)[0].(map[string]any)["key"] != "items/i3" {
		t.Fatalf("final page: %d %#v", status, final)
	}
	status, finalRetry := pagedDTQL(t, url, doc, 1, finalToken)
	if status != http.StatusOK || finalRetry["nextPageToken"] != nil || finalRetry["records"].([]any)[0].(map[string]any)["key"] != "items/i3" {
		t.Fatalf("final page retry: %d %#v", status, finalRetry)
	}
}

func TestDTQLSnapshotRejectsChangedQuery(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	url := base + "/v1/databases/testdb/dtql"
	for _, id := range []string{"i1", "i2"} {
		resp := doRequest(t, http.MethodPut, base+"/v1/databases/testdb/records/items/"+id,
			map[string]any{"data": map[string]any{"rank": 1}})
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)
	}
	doc := "from: {name: items}\n"
	_, first := pagedDTQL(t, url, doc, 1, "")
	token := first["nextPageToken"].(string)
	status, mismatch := pagedDTQL(t, url, doc+"#changed\n", 1, token)
	if status != http.StatusBadRequest || mismatch["error"].(map[string]any)["code"] != "bad_request" {
		t.Fatalf("changed query: %d %#v", status, mismatch)
	}
	status, next := pagedDTQL(t, url, doc, 1, token)
	if status != http.StatusOK || len(next["records"].([]any)) != 1 {
		t.Fatalf("valid continuation: %d %#v", status, next)
	}
}

func TestDTQLSnapshotCloseReusesCapacity(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	url := base + "/v1/databases/testdb/dtql"
	doc := "from: {name: items}\n"
	_, first := pagedDTQL(t, url, doc, 1, "")
	_, second := pagedDTQL(t, url, doc, 1, "")
	status, capacity := pagedDTQL(t, url, doc, 1, "")
	if status != http.StatusServiceUnavailable || capacity["error"].(map[string]any)["code"] != "snapshot_capacity" {
		t.Fatalf("capacity before close: %d %#v", status, capacity)
	}
	firstToken := first["snapshotToken"].(string)
	if status := closePagedDTQL(t, url, doc, 1, firstToken+"x"); status != http.StatusBadRequest {
		t.Fatalf("invalid close status = %d", status)
	}
	status, stillFull := pagedDTQL(t, url, doc, 1, "")
	if status != http.StatusServiceUnavailable || stillFull["error"].(map[string]any)["code"] != "snapshot_capacity" {
		t.Fatalf("invalid close freed capacity: %d %#v", status, stillFull)
	}
	if status := closePagedDTQL(t, url, doc, 1, firstToken); status != http.StatusNoContent {
		t.Fatalf("close status = %d", status)
	}
	if status := closePagedDTQL(t, url, doc, 1, firstToken); status != http.StatusNoContent {
		t.Fatalf("repeated close status = %d", status)
	}
	status, expired := pagedDTQL(t, url, doc, 1, firstToken)
	if status != http.StatusGone || expired["error"].(map[string]any)["code"] != "snapshot_expired" {
		t.Fatalf("page after close: %d %#v", status, expired)
	}
	status, third := pagedDTQL(t, url, doc, 1, "")
	if status != http.StatusOK || third["snapshotToken"] == nil {
		t.Fatalf("capacity after close: %d %#v", status, third)
	}
	if status := closePagedDTQL(t, url, doc, 1, second["snapshotToken"].(string)); status != http.StatusNoContent {
		t.Fatalf("second close status = %d", status)
	}
	if status := closePagedDTQL(t, url, doc, 1, third["snapshotToken"].(string)); status != http.StatusNoContent {
		t.Fatalf("third close status = %d", status)
	}
}

func TestDTQLSnapshotStreamsLargeSQLiteResult(t *testing.T) {
	if testing.Short() {
		t.Skip("120k-row disk spool acceptance")
	}
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: large, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    items:\n      fields:\n        name: {type: string}\n"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	sqlDB, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO items (id, name) VALUES (?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 120_000; i++ {
		if _, err = stmt.Exec(fmt.Sprintf("%06d", i), "large row payload for disk spooling"); err != nil {
			t.Fatal(err)
		}
	}
	if err = stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	srv := server.New("test", map[string]*core.Database{"large": db})
	defer srv.CloseSnapshots()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	url := ts.URL + "/v1/databases/large/dtql"
	doc := "from: {name: items}\norderBy: [{field: id}]\n"
	count := 0
	token := ""
	for {
		status, body := pagedDTQL(t, url, doc, 1000, token)
		if status != http.StatusOK {
			t.Fatalf("page after %d rows: %d %#v", count, status, body)
		}
		count += len(body["records"].([]any))
		next, ok := body["nextPageToken"].(string)
		if !ok {
			break
		}
		token = next
	}
	if count != 120_000 {
		t.Fatalf("got %d rows, want 120000", count)
	}
}

func TestDTQLSnapshotCORSAndShutdown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	manifestPath := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(manifestPath, []byte(schemalessManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	srv := server.New("test", map[string]*core.Database{db.ID(): db}, server.WithCORS(server.ParseCORSOrigins([]string{"https://example.test"})))
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	preflight, err := http.NewRequest(http.MethodOptions, ts.URL+"/v1/databases/testdb/dtql", nil)
	if err != nil {
		t.Fatal(err)
	}
	preflight.Header.Set("Origin", "https://example.test")
	preflight.Header.Set("Access-Control-Request-Headers", "OVDB-Page-Size,OVDB-Page-Token,OVDB-Page-Close")
	resp, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusNoContent)
	allowed := resp.Header.Get("Access-Control-Allow-Headers")
	drainClose(resp)
	if !strings.Contains(allowed, "OVDB-Page-Size") || !strings.Contains(allowed, "OVDB-Page-Token") || !strings.Contains(allowed, "OVDB-Page-Close") {
		t.Fatalf("paging CORS headers absent: %q", allowed)
	}
	for _, id := range []string{"i1", "i2"} {
		resp = doRequest(t, http.MethodPut, ts.URL+"/v1/databases/testdb/records/items/"+id,
			map[string]any{"data": map[string]any{"rank": 1}})
		mustStatus(t, resp, http.StatusNoContent)
		drainClose(resp)
	}
	url := ts.URL + "/v1/databases/testdb/dtql"
	doc := "from: {name: items}\n"
	_, first := pagedDTQL(t, url, doc, 1, "")
	token := first["nextPageToken"].(string)
	srv.CloseSnapshots()
	status, expired := pagedDTQL(t, url, doc, 1, token)
	if status != http.StatusGone || expired["error"].(map[string]any)["code"] != "snapshot_expired" {
		t.Fatalf("token after shutdown cleanup: %d %#v", status, expired)
	}
}

func TestDTQLSnapshotRestartSweep(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	snapshotDir := filepath.Join(dir, fmt.Sprintf("ovdb-query-snapshots-%d", os.Getuid()))
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(snapshotDir, "snapshot-orphan")
	newPath := filepath.Join(snapshotDir, "snapshot-active")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, []byte("private data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-7 * time.Minute)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	_ = server.New("test", nil)
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("stale snapshot survives restart: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("recent snapshot removed by sweep: %v", err)
	}
}
