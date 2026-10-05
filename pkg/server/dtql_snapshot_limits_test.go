package server_test

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
	_ "modernc.org/sqlite"
)

// snapshotLimitsDB mounts a SQLite database whose items hold about 2.5 MiB.
func snapshotLimitsDB(t *testing.T) *core.Database {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: big, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    items:\n      fields:\n        name: {type: string}\n"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqlDB, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	payload := strings.Repeat("x", 64<<10)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z", "A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L", "M", "N"} {
		if _, err := sqlDB.Exec("INSERT INTO items (id, name) VALUES (?, ?)", id, payload); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestDTQLSnapshotLimitsRefuseResultOverConfiguredBytes(t *testing.T) {
	db := snapshotLimitsDB(t)
	url := func(opts ...server.Option) string {
		srv := server.New("test", map[string]*core.Database{"big": db}, opts...)
		t.Cleanup(srv.CloseSnapshots)
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		return ts.URL + "/v1/databases/big/dtql"
	}
	doc := "from: {name: items}\norderBy: [{field: id}]\n"

	limited := url(server.WithSnapshotLimits(server.SnapshotLimits{Slots: 2, Bytes: 1 << 20, Rows: 1_000_000}))
	status, body := pagedDTQL(t, limited, doc, 10, "")
	if status != http.StatusRequestEntityTooLarge || body["error"].(map[string]any)["code"] != "snapshot_too_large" {
		t.Fatalf("limited server: %d %#v", status, body)
	}

	status, body = pagedDTQL(t, url(), doc, 10, "")
	if status != http.StatusOK || body["snapshotToken"] == nil {
		t.Fatalf("default server: %d", status)
	}
}

func TestDTQLSnapshotLimitsRefuseResultOverConfiguredRows(t *testing.T) {
	db := snapshotLimitsDB(t)
	srv := server.New("test", map[string]*core.Database{"big": db},
		server.WithSnapshotLimits(server.SnapshotLimits{Slots: 2, Bytes: 512 << 20, Rows: 5}))
	defer srv.CloseSnapshots()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	status, body := pagedDTQL(t, ts.URL+"/v1/databases/big/dtql", "from: {name: items}\n", 10, "")
	if status != http.StatusRequestEntityTooLarge || body["error"].(map[string]any)["code"] != "snapshot_too_large" {
		t.Fatalf("row limit: %d %#v", status, body)
	}
}

func TestDTQLSnapshotLimitsConfigureSlots(t *testing.T) {
	db := snapshotLimitsDB(t)
	srv := server.New("test", map[string]*core.Database{"big": db},
		server.WithSnapshotLimits(server.SnapshotLimits{Slots: 1, Bytes: 512 << 20, Rows: 1_000_000}))
	defer srv.CloseSnapshots()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	url := ts.URL + "/v1/databases/big/dtql"
	doc := "from: {name: items}\n"
	if status, _ := pagedDTQL(t, url, doc, 1, ""); status != http.StatusOK {
		t.Fatalf("first snapshot: %d", status)
	}
	status, body := pagedDTQL(t, url, doc, 1, "")
	if status != http.StatusServiceUnavailable || body["error"].(map[string]any)["code"] != "snapshot_capacity" {
		t.Fatalf("second snapshot: %d %#v", status, body)
	}
}
