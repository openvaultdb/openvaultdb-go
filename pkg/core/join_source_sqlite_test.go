package core_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	_ "modernc.org/sqlite"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

var (
	errJoinSrcSQLiteWorker = errors.New("join source sqlite test: worker failed")
	errJoinSrcSQLitePanic  = errors.New("join source sqlite test: callback panicked")
)

// joinSrcSQLiteMount mounts a temporary SQLite database holding two items.
func joinSrcSQLiteMount(t *testing.T) *core.Database {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: joinsrc, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    items:\n      fields:\n        name: {type: string}\n"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	for _, row := range [][2]string{{"a", "alpha"}, {"b", "beta"}} {
		if _, err := raw.Exec("INSERT INTO items (id, name) VALUES (?, ?)", row[0], row[1]); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func joinSrcReadAll(t *testing.T, executor dal.QueryExecutor, doc string) []string {
	t.Helper()
	query, _, err := core.ParseDTQL([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := executor.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	var ids []string
	for {
		rec, err := reader.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, dal.ErrNoMoreRecords) {
			return ids
		}
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.Key().ID.(string))
	}
}

func TestJoinSourceOnSQLiteMount(t *testing.T) {
	db := joinSrcSQLiteMount(t)
	if db.Engine() != "sqlite" || db.ID() != "joinsrc" || db.HasAccessPolicies() {
		t.Fatalf("source identity: engine %q id %q policies %v", db.Engine(), db.ID(), db.HasAccessPolicies())
	}

	t.Run("Executor reads the mount", func(t *testing.T) {
		if ids := joinSrcReadAll(t, db.Executor(), "from: {name: items}\norderBy: [{field: id}]\n"); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
			t.Fatalf("ids = %v", ids)
		}
	})

	t.Run("ReadTx passes a transaction executor to the callback", func(t *testing.T) {
		called := false
		err := db.ReadTx(context.Background(), func(tx dal.QueryExecutor) error {
			called = true
			if ids := joinSrcReadAll(t, tx, "from: {name: items}\norderBy: [{field: id}]\n"); len(ids) != 2 {
				t.Errorf("ids = %v", ids)
			}
			return nil
		})
		if err != nil || !called {
			t.Fatalf("err = %v, called = %v", err, called)
		}
	})

	t.Run("ReadTx returns the callback's error", func(t *testing.T) {
		err := db.ReadTx(context.Background(), func(dal.QueryExecutor) error { return errJoinSrcSQLiteWorker })
		if !errors.Is(err, errJoinSrcSQLiteWorker) {
			t.Fatalf("err = %v, want the callback's error", err)
		}
	})

	t.Run("a panic in the callback releases the transaction", func(t *testing.T) {
		ctx := context.Background()
		func() {
			defer func() {
				if recovered := recover(); recovered != errJoinSrcSQLitePanic {
					t.Errorf("recovered %v, want the callback's panic", recovered)
				}
			}()
			_ = db.ReadTx(ctx, func(tx dal.QueryExecutor) error {
				query, _, err := core.ParseDTQL([]byte("from: {name: items}\norderBy: [{field: id}]\n"))
				if err != nil {
					t.Error(err)
					return err
				}
				// Read one row and leave the reader open, so the transaction
				// holds the database's read lock when the callback panics.
				reader, err := tx.ExecuteQueryToRecordsReader(ctx, query)
				if err != nil {
					t.Error(err)
					return err
				}
				if _, err := reader.Next(); err != nil {
					t.Error(err)
				}
				panic(errJoinSrcSQLitePanic)
			})
		}()
		// A write needs the lock the read transaction held. The driver gives the
		// connection back when its context is cancelled, which is asynchronous,
		// so the write is retried for a short while.
		op := []core.Op{{Op: "set", Key: record.NewKeyWithID("items", "c"), Data: map[string]any{"name": "gamma"}}}
		deadline := time.Now().Add(5 * time.Second)
		for {
			_, err := db.Apply(ctx, op, "")
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the write still fails after the callback panicked: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if ids := joinSrcReadAll(t, db.Executor(), "from: {name: items}\norderBy: [{field: id}]\n"); len(ids) != 3 || ids[2] != "c" {
			t.Fatalf("ids = %v", ids)
		}
	})

	t.Run("ReadTx returns the callback's error after the context expired", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := db.ReadTx(ctx, func(tx dal.QueryExecutor) error {
			cancel()
			query, _, err := core.ParseDTQL([]byte("from: {name: items}\n"))
			if err != nil {
				t.Error(err)
				return err
			}
			// The driver has rolled the transaction back; the read fails.
			if reader, err := tx.ExecuteQueryToRecordsReader(ctx, query); err == nil {
				_ = reader.Close()
			}
			return errJoinSrcSQLiteWorker
		})
		if !errors.Is(err, errJoinSrcSQLiteWorker) {
			t.Fatalf("err = %v, want the callback's error", err)
		}
	})
}
