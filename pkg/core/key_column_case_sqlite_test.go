package core_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

// TestSQLiteKeyColumnSpellingsAreRefusedOnAnExistingTable: on a real SQLite file
// whose table already exists, a manifest that declares a field named as another
// spelling of the key column (ID) does not open and the file is unchanged; a
// manifest that declares only other fields opens, and a write whose data names
// that spelling is refused before the file is touched.
func TestSQLiteKeyColumnSpellingsAreRefusedOnAnExistingTable(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "data.sqlite")
	raw, err := sql.Open("sqlite", dataPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE things (id TEXT PRIMARY KEY, name TEXT)`,
		`INSERT INTO things VALUES ('1', 'alpha')`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	rows := func() string {
		t.Helper()
		raw, err := sql.Open("sqlite", dataPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		var id, name string
		if err := raw.QueryRow(`SELECT id, name FROM things`).Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		return id + "=" + name
	}
	manifestPath := filepath.Join(dir, "db.yaml")
	declare := func(fields string) {
		t.Helper()
		text := "database: {id: keyed, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    things:\n      fields:\n" + fields
		if err := os.WriteFile(manifestPath, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	declare("        name: {type: string}\n        ID: {type: string}\n")
	if db, err := mount.File(manifestPath); !errors.Is(err, core.ErrFieldNamesConflict) {
		if db != nil {
			_ = db.Close()
		}
		t.Fatalf("a manifest that declares ID: got %v, want ErrFieldNamesConflict", err)
	}
	if got := rows(); got != "1=alpha" {
		t.Fatalf("file holds %q after the refused open", got)
	}
	declare("        name: {type: string}\n")
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	key := record.NewKeyWithID("things", "1")
	_, err = db.Apply(context.Background(), []core.Op{{Op: "set", Key: key, Data: map[string]any{"name": "beta", "ID": "2"}}}, "")
	if !errors.Is(err, core.ErrKeyColumnCase) {
		t.Fatalf("a set that names ID: got %v, want ErrKeyColumnCase", err)
	}
	if got := rows(); got != "1=alpha" {
		t.Fatalf("file holds %q after the refused set", got)
	}
	if _, err = db.Apply(context.Background(), []core.Op{{Op: "set", Key: key, Data: map[string]any{"name": "beta"}}}, ""); err != nil {
		t.Fatal(err)
	}
	if got := rows(); got != "1=beta" {
		t.Fatalf("file holds %q after a set of a field", got)
	}
}
