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
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// TestBatchRecordWrittenWithNoDataIsRefusedByASQLiteMountThatRequiresAField: on a
// real SQLite file whose collection requires a field, a batch that sets or
// inserts a record with no data is refused by the schema validation (the record
// has no value for the required field), before anything is written, and the file
// is unchanged. A record written with the field is stored.
func TestBatchRecordWrittenWithNoDataIsRefusedByASQLiteMountThatRequiresAField(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	declaration := "database: {id: required, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    things:\n      fields:\n        name: {type: string, required: true}\n"
	if err := os.WriteFile(manifestPath, []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	count := func() int {
		t.Helper()
		raw, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		var n int
		if err := raw.QueryRow(`SELECT count(*) FROM things`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ctx := context.Background()
	key := record.NewKeyWithID("things", "a")
	for _, verb := range []string{"set", "insert"} {
		_, err := db.Apply(ctx, []core.Op{{Op: verb, Key: key}}, "")
		var invalid *schema.ValidationError
		if !errors.As(err, &invalid) || invalid.Field != "name" {
			t.Errorf("%s with no data: got %v, want the missing required field", verb, err)
		}
	}
	if n := count(); n != 0 {
		t.Fatalf("a refused batch left %d rows", n)
	}
	if _, err = db.Apply(ctx, []core.Op{{Op: "insert", Key: key, Data: map[string]any{"name": "alpha"}}}, ""); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}
