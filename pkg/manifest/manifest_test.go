package manifest_test

import (
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// ---------------------------------------------------------------------------
// Parse / Validate tests
// ---------------------------------------------------------------------------

func TestParse_ValidSQLiteStrict(t *testing.T) {
	yaml := []byte(`
database:
  id: my-db_1
  schema_mode: strict
storage:
  engine: sqlite
  path: /var/data/my.db
schemas:
  collections:
    users:
      fields:
        name:
          type: string
          required: true
        age:
          type: integer
`)
	m, err := manifest.Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Database.ID != "my-db_1" {
		t.Errorf("Database.ID = %q, want %q", m.Database.ID, "my-db_1")
	}
	if m.Storage.Engine != "sqlite" {
		t.Errorf("Storage.Engine = %q, want %q", m.Storage.Engine, "sqlite")
	}
	if m.Database.SchemaMode != schema.ModeStrict {
		t.Errorf("Database.SchemaMode = %q, want %q", m.Database.SchemaMode, schema.ModeStrict)
	}
	if m.Schemas == nil {
		t.Fatal("Schemas is nil, want non-nil")
	}
	if _, ok := m.Schemas.Collections["users"]; !ok {
		t.Error("Schemas.Collections missing 'users'")
	}
}

func TestParse_ValidInGitDBSchemaless(t *testing.T) {
	yaml := []byte(`
database:
  id: events
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
`)
	m, err := manifest.Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Storage.Engine != "ingitdb" {
		t.Errorf("Storage.Engine = %q, want ingitdb", m.Storage.Engine)
	}
	if m.Database.SchemaMode != schema.ModeSchemaless {
		t.Errorf("Database.SchemaMode = %q, want schemaless", m.Database.SchemaMode)
	}
	if m.Schemas != nil {
		t.Errorf("Schemas should be nil for schemaless without schemas block, got %+v", m.Schemas)
	}
}

func TestParse_StrictWithoutSchemas_Error(t *testing.T) {
	yaml := []byte(`
database:
  id: mydb
  schema_mode: strict
storage:
  engine: sqlite
  path: ./db
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for strict mode without schemas, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "strict") && !strings.Contains(msg, "schemas") {
		t.Errorf("error %q should mention 'strict' or 'schemas'", msg)
	}
}

func TestParse_UnknownSchemaMode_Error(t *testing.T) {
	yaml := []byte(`
database:
  id: mydb
  schema_mode: magic
storage:
  engine: sqlite
  path: ./db
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for unknown schema_mode, got nil")
	}
}

func TestParse_MissingID_Error(t *testing.T) {
	yaml := []byte(`
database:
  schema_mode: schemaless
storage:
  engine: sqlite
  path: ./db
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for missing database.id, got nil")
	}
	if !strings.Contains(err.Error(), "database.id") {
		t.Errorf("error %q should mention 'database.id'", err.Error())
	}
}

func TestParse_MissingEngine_Error(t *testing.T) {
	yaml := []byte(`
database:
  id: mydb
  schema_mode: schemaless
storage:
  path: ./db
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for missing storage.engine, got nil")
	}
	if !strings.Contains(err.Error(), "storage.engine") {
		t.Errorf("error %q should mention 'storage.engine'", err.Error())
	}
}

func TestParse_MissingPath_Error(t *testing.T) {
	yaml := []byte(`
database:
  id: mydb
  schema_mode: schemaless
storage:
  engine: sqlite
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for missing storage.path, got nil")
	}
	if !strings.Contains(err.Error(), "storage.path") {
		t.Errorf("error %q should mention 'storage.path'", err.Error())
	}
}

func TestParse_InvalidID_PathTraversal(t *testing.T) {
	yaml := []byte(`
database:
  id: ".."
  schema_mode: schemaless
storage:
  engine: sqlite
  path: ./db
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for database.id '..', got nil")
	}
}

func TestParse_InvalidID_Slashes(t *testing.T) {
	yaml := []byte(`
database:
  id: foo/bar
  schema_mode: schemaless
storage:
  engine: sqlite
  path: ./db
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for database.id 'foo/bar', got nil")
	}
}

func TestParse_UnknownYAMLKey_Rejected(t *testing.T) {
	yaml := []byte(`
database:
  id: mydb
  schema_mode: schemaless
storage:
  engine: sqlite
  path: ./db
unexpected_key: true
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for unknown YAML key (KnownFields), got nil")
	}
}

// ---------------------------------------------------------------------------
// InGitDB push option tests
// ---------------------------------------------------------------------------

var ingitdbBase = `
database:
  id: events
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
  ingitdb:
    push: %s
`

func TestParse_InGitDB_PushNone(t *testing.T) {
	yaml := []byte(`
database:
  id: events
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
  ingitdb:
    push: none
`)
	_, err := manifest.Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error for push:none: %v", err)
	}
}

func TestParse_InGitDB_PushSync(t *testing.T) {
	yaml := []byte(`
database:
  id: events
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
  ingitdb:
    push: sync
`)
	_, err := manifest.Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error for push:sync: %v", err)
	}
}

func TestParse_InGitDB_PushAsync(t *testing.T) {
	yaml := []byte(`
database:
  id: events
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
  ingitdb:
    push: async
`)
	_, err := manifest.Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error for push:async: %v", err)
	}
}

func TestParse_InGitDB_PushInvalid_Error(t *testing.T) {
	yaml := []byte(`
database:
  id: events
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
  ingitdb:
    push: instant
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for invalid push value 'instant', got nil")
	}
	if !strings.Contains(err.Error(), "instant") {
		t.Errorf("error %q should mention the bad value 'instant'", err.Error())
	}
}

func TestParse_InGitDB_OptionsWithSQLiteEngine_Error(t *testing.T) {
	yaml := []byte(`
database:
  id: mydb
  schema_mode: schemaless
storage:
  engine: sqlite
  path: ./db
  ingitdb:
    push: none
`)
	_, err := manifest.Parse(yaml)
	if err == nil {
		t.Fatal("expected error for ingitdb options with sqlite engine, got nil")
	}
	if !strings.Contains(err.Error(), "ingitdb") {
		t.Errorf("error %q should mention 'ingitdb'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// InGitDBOptions method defaults
// ---------------------------------------------------------------------------

func TestInGitDBOptions_NilReceiver(t *testing.T) {
	var o *manifest.InGitDBOptions

	if got := o.PushMode(); got != "none" {
		t.Errorf("nil.PushMode() = %q, want %q", got, "none")
	}
	if got := o.PushRemote(); got != "origin" {
		t.Errorf("nil.PushRemote() = %q, want %q", got, "origin")
	}
	if got := o.PushBranch(); got != "HEAD" {
		t.Errorf("nil.PushBranch() = %q, want %q", got, "HEAD")
	}
}

func TestInGitDBOptions_ZeroValue(t *testing.T) {
	o := &manifest.InGitDBOptions{} // all fields empty

	if got := o.PushMode(); got != "none" {
		t.Errorf("zero.PushMode() = %q, want %q", got, "none")
	}
	if got := o.PushRemote(); got != "origin" {
		t.Errorf("zero.PushRemote() = %q, want %q", got, "origin")
	}
	if got := o.PushBranch(); got != "HEAD" {
		t.Errorf("zero.PushBranch() = %q, want %q", got, "HEAD")
	}
}

func TestInGitDBOptions_SetValues(t *testing.T) {
	o := &manifest.InGitDBOptions{
		Push:   "sync",
		Remote: "upstream",
		Branch: "main",
	}

	if got := o.PushMode(); got != "sync" {
		t.Errorf("PushMode() = %q, want %q", got, "sync")
	}
	if got := o.PushRemote(); got != "upstream" {
		t.Errorf("PushRemote() = %q, want %q", got, "upstream")
	}
	if got := o.PushBranch(); got != "main" {
		t.Errorf("PushBranch() = %q, want %q", got, "main")
	}
}

// ---------------------------------------------------------------------------
// Table-driven parse error cases
// ---------------------------------------------------------------------------

func TestParse_InvalidDatabaseIDs(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"path traversal", ".."},
		{"slashes", "foo/bar"},
		{"leading dash", "-mydb"},
		{"leading underscore", "_mydb"},
		{"spaces", "my db"},
		{"at sign", "my@db"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			yaml := []byte(`
database:
  id: "` + tc.id + `"
  schema_mode: schemaless
storage:
  engine: sqlite
  path: ./db
`)
			_, err := manifest.Parse(yaml)
			if err == nil {
				t.Errorf("expected error for database.id %q, got nil", tc.id)
			}
		})
	}
}

func TestParse_ValidDatabaseIDs(t *testing.T) {
	cases := []string{
		"a",
		"mydb",
		"my-db",
		"my_db",
		"MyDB123",
		"db-1_v2",
	}

	for _, id := range cases {
		id := id
		t.Run(id, func(t *testing.T) {
			yaml := []byte(`
database:
  id: ` + id + `
  schema_mode: schemaless
storage:
  engine: sqlite
  path: ./db
`)
			_, err := manifest.Parse(yaml)
			if err != nil {
				t.Errorf("unexpected error for valid database.id %q: %v", id, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Strict mode with schemas
// ---------------------------------------------------------------------------

func TestParse_StrictMode_WithSchemas_Valid(t *testing.T) {
	yaml := []byte(`
database:
  id: catalog
  schema_mode: strict
storage:
  engine: sqlite
  path: ./catalog.db
schemas:
  collections:
    products:
      fields:
        name:
          type: string
          required: true
        price:
          type: number
`)
	m, err := manifest.Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Database.SchemaMode != schema.ModeStrict {
		t.Errorf("SchemaMode = %q, want strict", m.Database.SchemaMode)
	}
	if m.Schemas == nil || len(m.Schemas.Collections) == 0 {
		t.Error("expected non-empty Schemas.Collections")
	}
}

func TestParse_PartialMode_WithoutSchemas_Valid(t *testing.T) {
	yaml := []byte(`
database:
  id: metrics
  schema_mode: partial
storage:
  engine: sqlite
  path: ./metrics.db
`)
	_, err := manifest.Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error for partial mode without schemas: %v", err)
	}
}

// ---------------------------------------------------------------------------
// InGitDB with full options
// ---------------------------------------------------------------------------

func TestParse_InGitDB_FullOptions(t *testing.T) {
	yaml := []byte(`
database:
  id: repo-db
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
  ingitdb:
    push: sync
    remote: upstream
    branch: main
`)
	m, err := manifest.Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	o := m.Storage.InGitDB
	if o == nil {
		t.Fatal("Storage.InGitDB is nil")
	}
	if o.PushMode() != "sync" {
		t.Errorf("PushMode() = %q, want sync", o.PushMode())
	}
	if o.PushRemote() != "upstream" {
		t.Errorf("PushRemote() = %q, want upstream", o.PushRemote())
	}
	if o.PushBranch() != "main" {
		t.Errorf("PushBranch() = %q, want main", o.PushBranch())
	}
}

// ---------------------------------------------------------------------------
// InGitDB without explicit ingitdb block — defaults via nil pointer
// ---------------------------------------------------------------------------

func TestParse_InGitDB_NoOptionsBlock_Defaults(t *testing.T) {
	yaml := []byte(`
database:
  id: logs
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./logs
`)
	m, err := manifest.Parse(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Storage.InGitDB != nil {
		t.Error("ingitdb block was not specified; pointer should be nil")
	}
	o := m.Storage.InGitDB // nil: defaults must still work via methods
	if o.PushMode() != "none" {
		t.Errorf("PushMode() = %q, want none", o.PushMode())
	}
	if o.PushRemote() != "origin" {
		t.Errorf("PushRemote() = %q, want origin", o.PushRemote())
	}
	if o.PushBranch() != "HEAD" {
		t.Errorf("PushBranch() = %q, want HEAD", o.PushBranch())
	}
}

// ingitdbBase is declared above but unused as a variable; suppress the lint
// warning by referencing it here via a blank identifier.
var _ = ingitdbBase
