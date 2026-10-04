package core_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dal-go/dalgo/dal"
	_ "modernc.org/sqlite"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

var errJoinSrcSQLiteWorker = errors.New("join source sqlite test: worker failed")

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
}
