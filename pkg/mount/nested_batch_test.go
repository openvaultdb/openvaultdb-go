package mount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// TestApply_NestedInsertBatch reproduces the Sneat sandbox seeding shape:
// one batch inserting a root record and a record in a nested subcollection.
func TestApply_NestedInsertBatch(t *testing.T) {
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
	db, err := File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	rootKey, err := core.ParseKey("spaces", "s1")
	if err != nil {
		t.Fatal(err)
	}
	nestedKey, err := core.ParseKey("spaces", "s1", "ext", "contactus")
	if err != nil {
		t.Fatal(err)
	}

	// Pre-flight sanity: neither record exists on a fresh store.
	if _, err = db.Get(ctx, rootKey); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("fresh root Get: want ErrNotFound, got %v", err)
	}
	if _, err = db.Get(ctx, nestedKey); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("fresh nested Get: want ErrNotFound, got %v", err)
	}

	ops := []core.Op{
		{Op: "insert", Key: rootKey, Data: map[string]any{"title": "s1", "userIDs": []any{"user1"}}},
		{Op: "insert", Key: nestedKey, Data: map[string]any{"contacts": map[string]any{"m1": map[string]any{"type": "person"}}}},
	}
	if _, err = db.Apply(ctx, ops, "seed"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := db.Get(ctx, nestedKey)
	if err != nil {
		t.Fatalf("Get nested after Apply: %v", err)
	}
	if _, ok := got["contacts"]; !ok {
		t.Fatalf("nested record data = %v", got)
	}
}
