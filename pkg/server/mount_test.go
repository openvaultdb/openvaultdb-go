package server_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

const sqliteMountManifest = `
database:
  id: notes
  schema_mode: strict
storage:
  engine: sqlite
  path: ./notes.sqlite
schemas:
  collections:
    notes:
      fields:
        title: {type: string}
`

// mountSQLite mounts a seeded SQLite database and returns it with the path of
// its storage file.
func mountSQLite(t *testing.T) (*core.Database, string) {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "notes.yaml")
	if err := os.WriteFile(manifestPath, []byte(sqliteMountManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	return db, filepath.Join(dir, "notes.sqlite")
}

func TestMountUnmount_ConcurrentReaders(t *testing.T) {
	db, _ := mountSQLite(t)
	srv := server.New("test", nil)
	if err := srv.Mount(db); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	put, err := http.NewRequest(http.MethodPut, ts.URL+"/v1/databases/notes/records/notes/n1",
		strings.NewReader(`{"data":{"title":"hello"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("seed PUT: status %d", resp.StatusCode)
	}

	const readers = 50
	var ok200, gone404 atomic.Int64
	var failures sync.Map
	var started, wg sync.WaitGroup
	started.Add(readers)
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			first := true
			for {
				resp, err := http.Get(ts.URL + "/v1/databases/notes/records/notes/n1")
				if err != nil {
					failures.Store(err.Error(), true)
					if first {
						started.Done()
					}
					return
				}
				_ = resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusOK:
					ok200.Add(1)
				case http.StatusNotFound:
					gone404.Add(1)
				default:
					failures.Store(resp.Status, true)
				}
				if first {
					started.Done()
					first = false
				}
				if resp.StatusCode != http.StatusOK {
					return
				}
			}
		}()
	}
	started.Wait() // every reader has completed at least one request
	if err := srv.Unmount("notes"); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	wg.Wait()

	failures.Range(func(k, _ any) bool {
		t.Errorf("reader saw %v (want only 200 before and 404 after unmount)", k)
		return true
	})
	if ok200.Load() < readers || gone404.Load() != readers {
		t.Errorf("got %d OK and %d not-found responses; want >= %d and %d", ok200.Load(), gone404.Load(), readers, readers)
	}
	// Close reached the engine: the SQLite handle is closed.
	if _, err := db.Get(t.Context(), record.NewKeyWithID("notes", "n1")); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("driver still usable after Unmount: %v", err)
	}
	if err := srv.Unmount("notes"); !errors.Is(err, server.ErrDatabaseNotMounted) {
		t.Errorf("second Unmount = %v, want ErrDatabaseNotMounted", err)
	}
}

func TestMount_RejectsDuplicateID(t *testing.T) {
	db, _ := mountSQLite(t)
	t.Cleanup(func() { _ = db.Close() })
	srv := server.New("test", map[string]*core.Database{db.ID(): db})
	if err := srv.Mount(db); !errors.Is(err, server.ErrDatabaseMounted) {
		t.Fatalf("Mount duplicate = %v, want ErrDatabaseMounted", err)
	}
	if err := srv.Mount(nil); err == nil {
		t.Fatal("Mount(nil) succeeded")
	}
	// Remount after unmount serves the new instance.
	if err := srv.Unmount(db.ID()); err != nil {
		t.Fatal(err)
	}
	again, _ := mountSQLite(t)
	t.Cleanup(func() { _ = again.Close() })
	if err := srv.Mount(again); err != nil {
		t.Fatalf("remount: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/databases/notes")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET remounted database: status %d", resp.StatusCode)
	}
}

// TestMountUnmount_ReleasesSQLiteFile is the Windows check (CI runs it on
// windows-latest): renaming an open file fails there, so the rename proves
// Unmount released the SQLite handle.
func TestMountUnmount_ReleasesSQLiteFile(t *testing.T) {
	db, path := mountSQLite(t)
	srv := server.New("test", nil)
	if err := srv.Mount(db); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/v1/databases/notes/records/notes/missing")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := srv.Unmount("notes"); err != nil {
		t.Fatal(err)
	}
	var renameErr error
	for i := 0; i < 20; i++ { // tolerate a brief antivirus/indexer hold on Windows
		if renameErr = os.Rename(path, path+".moved"); renameErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if renameErr != nil {
		t.Fatalf("rename after Unmount: %v", renameErr)
	}
}
