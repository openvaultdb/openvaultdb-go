package mount

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo2http"
	"github.com/dal-go/dalgo2mysql"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"
	"github.com/dal-go/dalgo2sqlite"
	dalgo2github "github.com/ingitdb/dalgo2ingitdb4github"
	"github.com/ingitdb/ingitdb-go/ingitdb"
	_ "modernc.org/sqlite"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/policystore"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type mockMountDriver struct{}

var mockQueryFunc func(query string, args []driver.Value) (driver.Rows, error)

func (d *mockMountDriver) Open(name string) (driver.Conn, error) {
	return &mockMountConn{}, nil
}

type mockMountConn struct{}

func (c *mockMountConn) Prepare(query string) (driver.Stmt, error) {
	return &mockMountStmt{query: query}, nil
}
func (c *mockMountConn) Close() error              { return nil }
func (c *mockMountConn) Begin() (driver.Tx, error) { return nil, nil }

type mockMountStmt struct {
	query string
}

func (s *mockMountStmt) Close() error                                    { return nil }
func (s *mockMountStmt) NumInput() int                                   { return -1 }
func (s *mockMountStmt) Exec(args []driver.Value) (driver.Result, error) { return nil, nil }
func (s *mockMountStmt) Query(args []driver.Value) (driver.Rows, error) {
	if mockQueryFunc != nil {
		return mockQueryFunc(s.query, args)
	}
	return nil, errors.New("mock query error")
}

func init() {
	sql.Register("mockmountdriver", &mockMountDriver{})
}

type mockSimpleRows struct {
	cols    []string
	types   []string
	rows    [][]driver.Value
	idx     int
	scanErr error
	rowErr  error
}

func (r *mockSimpleRows) Columns() []string { return r.cols }
func (r *mockSimpleRows) Close() error      { return nil }
func (r *mockSimpleRows) ColumnTypeDatabaseTypeName(index int) string {
	if index < len(r.types) {
		return r.types[index]
	}
	return "TEXT"
}
func (r *mockSimpleRows) Next(dest []driver.Value) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if r.idx >= len(r.rows) {
		if r.rowErr != nil {
			return r.rowErr
		}
		return io.EOF
	}
	copy(dest, r.rows[r.idx])
	r.idx++
	return nil
}

// Not parallel: modifies environment variables
func TestMountFirestoreCoverage(t *testing.T) {
	t.Setenv("FIRESTORE_EMULATOR_HOST", "127.0.0.1:8888")

	// Project with specific database
	o1 := &manifest.FirestoreOptions{Project: "test-p1", Database: "test-db1"}
	db1, modes1, closeFn1, err := openFirestore(o1)
	if err != nil {
		t.Fatalf("openFirestore(o1): %v", err)
	}
	if len(modes1) != 3 || db1 == nil || closeFn1 == nil {
		t.Fatalf("unexpected openFirestore results: %v, %v", db1, modes1)
	}
	_ = closeFn1()

	// Project without specific database
	o2 := &manifest.FirestoreOptions{Project: "test-p2"}
	db2, modes2, closeFn2, err := openFirestore(o2)
	if err != nil {
		t.Fatalf("openFirestore(o2): %v", err)
	}
	if len(modes2) != 3 || db2 == nil || closeFn2 == nil {
		t.Fatalf("unexpected openFirestore results: %v, %v", db2, modes2)
	}
	_ = closeFn2()

	// Error path (empty project without emulator)
	t.Setenv("FIRESTORE_EMULATOR_HOST", "")
	_, _, _, err = openFirestore(&manifest.FirestoreOptions{Project: ""})
	if err == nil {
		t.Fatal("expected error for empty project without emulator")
	}

	// FileWithOptions with firestore
	t.Setenv("FIRESTORE_EMULATOR_HOST", "127.0.0.1:8888")
	dir := t.TempDir()
	fsPath := filepath.Join(dir, "fs.yaml")
	fsContent := "database: {id: fsdb, schema_mode: schemaless}\nstorage:\n  engine: firestore\n  firestore:\n    project: test-p1\n"
	if err := os.WriteFile(fsPath, []byte(fsContent), 0600); err != nil {
		t.Fatal(err)
	}
	fdb, err := FileWithOptions(fsPath, Options{})
	if err != nil {
		t.Fatalf("FileWithOptions(fsPath): %v", err)
	}
	_ = fdb.Close()
}

// Not parallel: modifies package-level seam variables
func TestMountGitPushCoverage(t *testing.T) {
	origCmd := execCommandContext
	defer func() { execCommandContext = origCmd }()

	dir := t.TempDir()

	// gitPush success
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}
	if err := gitPush(context.Background(), dir, "origin", "main"); err != nil {
		t.Fatalf("gitPush success expected, got: %v", err)
	}

	// gitPush error
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}
	if err := gitPush(context.Background(), dir, "origin", "main"); err == nil {
		t.Fatal("expected gitPush error")
	}

	// newGitPushHook: sync
	hookSync, stopSync := newGitPushHook(dir, &manifest.InGitDBOptions{Push: "sync"})
	if hookSync == nil || stopSync == nil {
		t.Fatal("expected non-nil sync hook and stop")
	}
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}
	if err := hookSync(context.Background()); err != nil {
		t.Fatalf("hookSync: %v", err)
	}
	if err := stopSync(); err != nil {
		t.Fatalf("stopSync: %v", err)
	}

	// newGitPushHook: async
	hookAsync, stopAsync := newGitPushHook(dir, &manifest.InGitDBOptions{Push: "async"})
	if hookAsync == nil || stopAsync == nil {
		t.Fatal("expected non-nil async hook and stop")
	}
	if err := hookAsync(context.Background()); err != nil {
		t.Fatalf("hookAsync 1: %v", err)
	}
	if err := hookAsync(context.Background()); err != nil {
		t.Fatalf("hookAsync 2 (coalesce): %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := stopAsync(); err != nil {
		t.Fatalf("stopAsync 1: %v", err)
	}
	if err := stopAsync(); err != nil {
		t.Fatalf("stopAsync 2 (redundant): %v", err)
	}
	if err := hookAsync(context.Background()); err != nil {
		t.Fatalf("hookAsync after stop: %v", err)
	}

	// newGitPushHook: async push fails inside loop
	hookAsyncFail, stopAsyncFail := newGitPushHook(dir, &manifest.InGitDBOptions{Push: "async"})
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}
	_ = hookAsyncFail(context.Background())
	time.Sleep(50 * time.Millisecond)
	_ = stopAsyncFail()

	// newGitPushHook: default/none
	hookNone, stopNone := newGitPushHook(dir, &manifest.InGitDBOptions{Push: "none"})
	if hookNone != nil || stopNone != nil {
		t.Fatal("expected nil hook for none")
	}
}

// Not parallel: modifies package-level seam variables and environment variables
func TestMountInGitDBGitHubCoverage(t *testing.T) {
	origNewDB := newBatchingGitHubDB
	defer func() { newBatchingGitHubDB = origNewDB }()

	// Pure functions: ingitdbColumnType
	for _, tc := range []struct {
		in   schema.FieldType
		want ingitdb.ColumnType
	}{
		{schema.TypeString, ingitdb.ColumnTypeString},
		{schema.TypeNumber, ingitdb.ColumnTypeFloat},
		{schema.TypeInteger, ingitdb.ColumnTypeInt},
		{schema.TypeBoolean, ingitdb.ColumnTypeBool},
		{schema.FieldType("unknown"), ingitdb.ColumnTypeAny},
	} {
		if got := ingitdbColumnType(tc.in); got != tc.want {
			t.Errorf("ingitdbColumnType(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// Pure functions: buildInGitDBDefinition
	schemas := &schema.Schemas{
		Collections: map[string]schema.Collection{
			"users": {
				Fields: map[string]schema.Field{
					"name": {Type: schema.TypeString, Required: true},
					"age":  {Type: schema.TypeInteger},
				},
			},
		},
	}
	def := buildInGitDBDefinition(schemas)
	if def == nil || def.Collections["users"] == nil || def.Collections["users"].Columns["name"].Type != ingitdb.ColumnTypeString {
		t.Fatalf("buildInGitDBDefinition failed: %#v", def)
	}

	// openInGitDBGitHub: invalid env var name
	mInvalid := &manifest.Manifest{
		Storage: manifest.Storage{
			Engine: "ingitdb",
			InGitDB: &manifest.InGitDBOptions{
				GitHub: &manifest.InGitDBGitHubOptions{TokenEnv: "123-bad-var"},
			},
		},
	}
	if _, _, err := openInGitDBGitHub(mInvalid); err == nil {
		t.Fatal("expected error for invalid token_env")
	}

	// openInGitDBGitHub: token not set (default)
	t.Setenv("OVDB_GITHUB_TOKEN", "")
	mDefault := &manifest.Manifest{
		Storage: manifest.Storage{
			Engine: "ingitdb",
			InGitDB: &manifest.InGitDBOptions{
				GitHub: &manifest.InGitDBGitHubOptions{},
			},
		},
	}
	if _, _, err := openInGitDBGitHub(mDefault); err == nil {
		t.Fatal("expected error for unset default token")
	}

	// openInGitDBGitHub: token not set (custom)
	t.Setenv("CUSTOM_GH_TOKEN", "")
	mCustom := &manifest.Manifest{
		Storage: manifest.Storage{
			Engine: "ingitdb",
			InGitDB: &manifest.InGitDBOptions{
				GitHub: &manifest.InGitDBGitHubOptions{TokenEnv: "CUSTOM_GH_TOKEN"},
			},
		},
	}
	if _, _, err := openInGitDBGitHub(mCustom); err == nil {
		t.Fatal("expected error for unset custom token")
	}

	// openInGitDBGitHub: missing collections
	t.Setenv("CUSTOM_GH_TOKEN", "valid_token")
	if _, _, err := openInGitDBGitHub(mCustom); err == nil {
		t.Fatal("expected error for missing collections")
	}

	// openInGitDBGitHub: newBatchingGitHubDB error
	mCustom.Schemas = schemas
	newBatchingGitHubDB = func(_ dalgo2github.Config, _ *ingitdb.Definition, _ string) (*dalgo2github.BatchingGitHubDB, error) {
		return nil, errors.New("github connection failed")
	}
	if _, _, err := openInGitDBGitHub(mCustom); err == nil {
		t.Fatal("expected error when newBatchingGitHubDB fails")
	}

	// openInGitDBGitHub: success
	newBatchingGitHubDB = func(_ dalgo2github.Config, _ *ingitdb.Definition, _ string) (*dalgo2github.BatchingGitHubDB, error) {
		return &dalgo2github.BatchingGitHubDB{}, nil
	}
	db, modes, err := openInGitDBGitHub(mCustom)
	if err != nil {
		t.Fatalf("openInGitDBGitHub success expected: %v", err)
	}
	if db == nil || len(modes) != 2 {
		t.Fatalf("unexpected db or modes: %v, %v", db, modes)
	}
}

type mockProtectedAccessDB struct {
	dal.DB
}

func (m *mockProtectedAccessDB) ConfigureProtectedAccess(_ ...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	return m, nil, nil
}

// Not parallel: modifies mockQueryFunc seam variable and sqliteDecimalRegistrationErr
func TestMountSQLiteProtectedAccessAndVerifyKeys(t *testing.T) {
	origDecErr := sqliteDecimalRegistrationErr
	defer func() {
		mockQueryFunc = nil
		sqliteDecimalRegistrationErr = origDecErr
	}()

	// ConfigureProtectedAccess: unavailable
	tempSQLite := filepath.Join(t.TempDir(), "db.sqlite")
	rawDB, err := dalgo2sqlite.NewDatabase(tempSQLite)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawDB.Close() }()

	sUnavail := &sqliteMount{Database: rawDB}
	if _, _, err := sUnavail.ConfigureProtectedAccess(); err == nil {
		t.Fatal("expected error when DB does not implement ConfigureProtectedAccess")
	}

	// ConfigureProtectedAccess: available
	rawDB.DB = &mockProtectedAccessDB{}
	sAvail := &sqliteMount{Database: rawDB}
	if _, _, err := sAvail.ConfigureProtectedAccess(); err != nil {
		t.Fatalf("ConfigureProtectedAccess: %v", err)
	}

	// sqliteRecordKeys coverage
	if keys, err := sqliteRecordKeys(&manifest.Manifest{}); keys != nil || err != nil {
		t.Fatalf("expected nil, nil, got %v, %v", keys, err)
	}
	if keys, err := sqliteRecordKeys(&manifest.Manifest{Storage: manifest.Storage{SQLite: &manifest.SQLiteOptions{}}}); keys != nil || err != nil {
		t.Fatalf("expected nil, nil, got %v, %v", keys, err)
	}
	// empty record keys
	if _, err := sqliteRecordKeys(&manifest.Manifest{Storage: manifest.Storage{SQLite: &manifest.SQLiteOptions{RecordKeys: map[string]string{}}}}); err == nil {
		t.Fatal("expected error for empty record keys")
	}
	// missing collection
	mMissing := &manifest.Manifest{
		Storage: manifest.Storage{SQLite: &manifest.SQLiteOptions{RecordKeys: map[string]string{"users": "id"}}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"other": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}}}},
	}
	if _, err := sqliteRecordKeys(mMissing); err == nil {
		t.Fatal("expected error for missing key in configured")
	}
	// non-string key
	mNonString := &manifest.Manifest{
		Storage: manifest.Storage{SQLite: &manifest.SQLiteOptions{RecordKeys: map[string]string{"users": "id"}}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"id": {Type: schema.TypeInteger}}}}},
	}
	if _, err := sqliteRecordKeys(mNonString); err == nil {
		t.Fatal("expected error for non-string key")
	}
	// ambiguous collection spelling
	mAmbiguous := &manifest.Manifest{
		Storage: manifest.Storage{SQLite: &manifest.SQLiteOptions{RecordKeys: map[string]string{"Users": "id", "users": "id"}}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"Users": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}},
			"users": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}},
		}},
	}
	if _, err := sqliteRecordKeys(mAmbiguous); err == nil {
		t.Fatal("expected error for ambiguous collection spelling")
	}
	// extra configured key not in schemas
	mExtra := &manifest.Manifest{
		Storage: manifest.Storage{SQLite: &manifest.SQLiteOptions{RecordKeys: map[string]string{"users": "id", "extra": "id"}}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"users": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}},
		}},
	}
	if _, err := sqliteRecordKeys(mExtra); err == nil {
		t.Fatal("expected error for extra configured key")
	}

	// openSQLite error branches
	// 0. decimal registration error
	sqliteDecimalRegistrationErr = errors.New("fake decimal registration error")
	if _, _, err := openSQLite(tempSQLite, &manifest.Manifest{Storage: manifest.Storage{Engine: "sqlite"}}); err == nil {
		t.Fatal("expected decimal registration error")
	}
	sqliteDecimalRegistrationErr = nil

	// 1. mkdir failure
	if _, _, err := openSQLite(filepath.Join(tempSQLite, "child", "db.sqlite"), &manifest.Manifest{Storage: manifest.Storage{Engine: "sqlite"}}); err == nil {
		t.Fatal("expected mkdir error")
	}
	// 2. lockWait failure
	badWait := "invalid"
	mBadWait := &manifest.Manifest{
		Storage: manifest.Storage{Engine: "sqlite", SQLite: &manifest.SQLiteOptions{BusyTimeout: &badWait}},
	}
	if _, _, err := openSQLite(tempSQLite, mBadWait); err == nil {
		t.Fatal("expected LockWait error")
	}
	mBadKeys := &manifest.Manifest{
		Storage: manifest.Storage{Engine: "sqlite", SQLite: &manifest.SQLiteOptions{RecordKeys: map[string]string{}}},
	}
	if _, _, err := openSQLite(tempSQLite, mBadKeys); err == nil {
		t.Fatal("expected sqliteRecordKeys error in openSQLite")
	}
	mBadVerify := &manifest.Manifest{
		Storage: manifest.Storage{Engine: "sqlite", SQLite: &manifest.SQLiteOptions{RecordKeys: map[string]string{"nonexistent": "id"}}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"nonexistent": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}}}},
	}
	if _, _, err := openSQLite(tempSQLite, mBadVerify); err == nil {
		t.Fatal("expected verifySQLiteKeys error in openSQLite")
	}

	// verifySQLiteKeys error paths on real SQLite
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	// Table does not exist
	if err := verifySQLiteKeys(ctx, db, map[string]string{"missing": "id"}); err == nil {
		t.Fatal("expected error for missing physical table")
	}

	// Table exists, but key column is INTEGER
	if _, err := db.ExecContext(ctx, "CREATE TABLE t1 (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteKeys(ctx, db, map[string]string{"t1": "id"}); err == nil {
		t.Fatal("expected error for non-TEXT key column")
	}

	// Key is TEXT, but no unique index
	if _, err := db.ExecContext(ctx, "CREATE TABLE t2 (id TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteKeys(ctx, db, map[string]string{"t2": "id"}); err == nil {
		t.Fatal("expected error for missing unique index")
	}

	// Key is TEXT with unique index, but ordering collation duplicate
	if _, err := db.ExecContext(ctx, "CREATE TABLE t3 (id TEXT COLLATE NOCASE); CREATE UNIQUE INDEX idx_t3 ON t3 (id COLLATE BINARY);"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t3 VALUES ('abc'), ('ABC')"); err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteKeys(ctx, db, map[string]string{"t3": "id"}); err == nil {
		t.Fatal("expected error for duplicate under ordering collation")
	}

	// Key contains invalid transport ID
	if _, err := db.ExecContext(ctx, "CREATE TABLE t4 (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t4 VALUES ('../escape')"); err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteKeys(ctx, db, map[string]string{"t4": "id"}); err == nil {
		t.Fatal("expected error for invalid transport ID")
	}

	// verifySQLiteKeys query error paths using mockmountdriver
	mockDB, err := sql.Open("mockmountdriver", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mockDB.Close() }()

	// 1. Serving index check fails
	mockQueryFunc = func(query string, _ []driver.Value) (driver.Rows, error) {
		if strings.Contains(query, "sqlite_schema") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"table"}}}, nil
		}
		if strings.Contains(query, "pragma_table_xinfo") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"TEXT"}}}, nil
		}
		if strings.Contains(query, "pragma_index_list") {
			return nil, errors.New("index check query failed")
		}
		return nil, errors.New("unexpected query")
	}
	if err := verifySQLiteKeys(ctx, mockDB, map[string]string{"tbl": "id"}); err == nil || !strings.Contains(err.Error(), "verify SQLite serving index") {
		t.Fatalf("expected verify SQLite serving index error, got: %v", err)
	}

	// 2. Serving order check fails
	mockQueryFunc = func(query string, _ []driver.Value) (driver.Rows, error) {
		if strings.Contains(query, "sqlite_schema") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"table"}}}, nil
		}
		if strings.Contains(query, "pragma_table_xinfo") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"TEXT"}}}, nil
		}
		if strings.Contains(query, "pragma_index_list") {
			return &mockSimpleRows{cols: []string{"indexed"}, rows: [][]driver.Value{{true}}}, nil
		}
		if strings.Contains(query, "GROUP BY") {
			return nil, errors.New("order check query failed")
		}
		return nil, errors.New("unexpected query")
	}
	if err := verifySQLiteKeys(ctx, mockDB, map[string]string{"tbl": "id"}); err == nil || !strings.Contains(err.Error(), "verify SQLite serving order") {
		t.Fatalf("expected verify SQLite serving order error, got: %v", err)
	}

	// 3. Serving values query fails
	mockQueryFunc = func(query string, _ []driver.Value) (driver.Rows, error) {
		if strings.Contains(query, "sqlite_schema") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"table"}}}, nil
		}
		if strings.Contains(query, "pragma_table_xinfo") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"TEXT"}}}, nil
		}
		if strings.Contains(query, "pragma_index_list") {
			return &mockSimpleRows{cols: []string{"indexed"}, rows: [][]driver.Value{{true}}}, nil
		}
		if strings.Contains(query, "GROUP BY") {
			return &mockSimpleRows{cols: []string{"dup"}, rows: [][]driver.Value{{false}}}, nil
		}
		if strings.Contains(query, "SELECT \"id\", typeof") {
			return nil, errors.New("values query failed")
		}
		return nil, errors.New("unexpected query")
	}
	if err := verifySQLiteKeys(ctx, mockDB, map[string]string{"tbl": "id"}); err == nil || !strings.Contains(err.Error(), "verify SQLite serving values") {
		t.Fatalf("expected verify SQLite serving values query error, got: %v", err)
	}

	// 4. Serving values scan fails
	mockQueryFunc = func(query string, _ []driver.Value) (driver.Rows, error) {
		if strings.Contains(query, "sqlite_schema") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"table"}}}, nil
		}
		if strings.Contains(query, "pragma_table_xinfo") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"TEXT"}}}, nil
		}
		if strings.Contains(query, "pragma_index_list") {
			return &mockSimpleRows{cols: []string{"indexed"}, rows: [][]driver.Value{{true}}}, nil
		}
		if strings.Contains(query, "GROUP BY") {
			return &mockSimpleRows{cols: []string{"dup"}, rows: [][]driver.Value{{false}}}, nil
		}
		if strings.Contains(query, "SELECT \"id\", typeof") {
			return &mockSimpleRows{cols: []string{"id"}, rows: [][]driver.Value{{"id"}}}, nil
		}
		return nil, errors.New("unexpected query")
	}
	if err := verifySQLiteKeys(ctx, mockDB, map[string]string{"tbl": "id"}); err == nil || !strings.Contains(err.Error(), "verify SQLite serving values") {
		t.Fatalf("expected verify SQLite serving values scan error, got: %v", err)
	}

	// 5. Serving values rows.Err() fails
	mockQueryFunc = func(query string, _ []driver.Value) (driver.Rows, error) {
		if strings.Contains(query, "sqlite_schema") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"table"}}}, nil
		}
		if strings.Contains(query, "pragma_table_xinfo") {
			return &mockSimpleRows{cols: []string{"type"}, rows: [][]driver.Value{{"TEXT"}}}, nil
		}
		if strings.Contains(query, "pragma_index_list") {
			return &mockSimpleRows{cols: []string{"indexed"}, rows: [][]driver.Value{{true}}}, nil
		}
		if strings.Contains(query, "GROUP BY") {
			return &mockSimpleRows{cols: []string{"dup"}, rows: [][]driver.Value{{false}}}, nil
		}
		if strings.Contains(query, "SELECT \"id\", typeof") {
			return &mockSimpleRows{cols: []string{"id", "typeof"}, rowErr: errors.New("rows iteration failed")}, nil
		}
		return nil, errors.New("unexpected query")
	}
	if err := verifySQLiteKeys(ctx, mockDB, map[string]string{"tbl": "id"}); err == nil || !strings.Contains(err.Error(), "verify SQLite serving values") {
		t.Fatalf("expected verify SQLite serving values rowErr, got: %v", err)
	}
}

type mockPostgresCatalog struct {
	listSchemasErr     error
	listCollectionsErr error
	describeErr        error
	def                *dbschema.CollectionDef
	nilDef             bool
	schemas            []string
	refs               []dal.CollectionRef
	onDescribe         func()
}

func (m *mockPostgresCatalog) ListSchemas(context.Context) ([]string, error) {
	if m.listSchemasErr != nil {
		return nil, m.listSchemasErr
	}
	if m.schemas != nil {
		return m.schemas, nil
	}
	return []string{"public"}, nil
}

func (m *mockPostgresCatalog) ListSchemaCollections(_ context.Context, schemaName string) ([]dal.CollectionRef, error) {
	if m.listCollectionsErr != nil {
		return nil, m.listCollectionsErr
	}
	if m.refs != nil {
		return m.refs, nil
	}
	return []dal.CollectionRef{dal.NewQualifiedRootCollectionRef(schemaName, "orders", "")}, nil
}

func (m *mockPostgresCatalog) DescribeCollection(_ context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	if m.onDescribe != nil {
		m.onDescribe()
	}
	if m.describeErr != nil {
		return nil, m.describeErr
	}
	if m.nilDef {
		return nil, nil
	}
	if m.def != nil {
		return m.def, nil
	}
	return &dbschema.CollectionDef{Name: ref.Name(), Fields: []dbschema.FieldDef{{Name: "id", Type: dbschema.Int}}}, nil
}

type mockPostgresVisibility struct {
	err        error
	selectable map[string]bool
}

func (m *mockPostgresVisibility) SelectableRelations(context.Context, string) (map[string]bool, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.selectable != nil {
		return m.selectable, nil
	}
	return map[string]bool{"orders": true}, nil
}

// Not parallel: modifies package-level seam variables
func TestMountPostgresTypesAndVisibility(t *testing.T) {
	origQuery := queryPostgresSelectableRelations
	origOpenPG := openPostgresDatabase
	origVisDB := openVisibilityDB
	origCloseVis := closeVisibilityDB
	origClosePGDB := closePostgresDatabase
	origDisc := discoverNativeSchemas
	defer func() {
		queryPostgresSelectableRelations = origQuery
		openPostgresDatabase = origOpenPG
		openVisibilityDB = origVisDB
		closeVisibilityDB = origCloseVis
		closePostgresDatabase = origClosePGDB
		discoverNativeSchemas = origDisc
	}()
	closePostgresDatabase = func(*dalgo2postgres.Database) error { return nil }

	// nativePostgresFieldType
	for _, tc := range []struct {
		in   dbschema.Type
		want schema.FieldType
	}{
		{dbschema.Bool, schema.TypeBoolean},
		{dbschema.Float, schema.TypeNumber},
		{dbschema.Bytes, schema.TypeString},
		{dbschema.Time, schema.TypeString},
		{dbschema.Type(99), schema.TypeString},
	} {
		if got := nativePostgresFieldType(tc.in); got != tc.want {
			t.Errorf("nativePostgresFieldType(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// nativePostgresDeclaredType
	if got := nativePostgresDeclaredType(nil, "col"); got != "" {
		t.Errorf("expected empty string for nil def, got: %q", got)
	}
	srcDef := &dbschema.SourceDefinition{Columns: []dbschema.SourceColumnDef{{Name: "other", DeclaredType: "text"}}}
	if got := nativePostgresDeclaredType(srcDef, "col"); got != "" {
		t.Errorf("expected empty string for missing column, got: %q", got)
	}

	// postgresReadOnlyDSN
	dsnWithParams, err := postgresReadOnlyDSN("postgres://u:p@localhost:5432/db?sslmode=disable", nativeSettingsOf(Options{}))
	if err != nil || !strings.Contains(dsnWithParams, "default_transaction_read_only=on") {
		t.Fatalf("postgresReadOnlyDSN: %v, %v", dsnWithParams, err)
	}
	if _, err := postgresReadOnlyDSN("postgres://%ZZ", nativeSettingsOf(Options{})); err == nil {
		t.Fatal("expected error for invalid URL in postgresReadOnlyDSN")
	}

	// openPostgresWithExclusions validation errors
	mWrite := &manifest.Manifest{Storage: manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: false}}}
	if _, _, err := openPostgresWithExclusions(mWrite, nil, []schema.NativeCollectionSource{{Schema: "s", Name: "t"}}); err == nil {
		t.Fatal("expected error for exclusions on non-read-only mount")
	}
	mRO := &manifest.Manifest{Storage: manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true, DSNEnv: "PG_DSN"}}}
	if _, _, err := openPostgresWithExclusions(mRO, nil, []schema.NativeCollectionSource{{Schema: strings.Repeat("x", 64), Name: "t"}}); err == nil {
		t.Fatal("expected error for invalid exclusion name")
	}
	dupExclusions := []schema.NativeCollectionSource{{Schema: "s", Name: "t"}, {Schema: "s", Name: "t"}}
	if _, _, err := openPostgresWithExclusions(mRO, nil, dupExclusions); err == nil {
		t.Fatal("expected error for duplicate exclusions")
	}

	// openPostgresWithExclusions read-only DSN error
	t.Setenv("PG_BAD_DSN", "postgres://%ZZ")
	mBadDSN := &manifest.Manifest{Storage: manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true, DSNEnv: "PG_BAD_DSN"}}}
	if _, _, err := openPostgresWithExclusions(mBadDSN, nil, nil); err == nil {
		t.Fatal("expected DSN error for read-only mount")
	}

	// postgresSelectVisibility.SelectableRelations with closed db
	closedDB, _ := sql.Open("sqlite", ":memory:")
	_ = closedDB.Close()
	vClosed := postgresSelectVisibility{db: closedDB}
	if _, err := vClosed.SelectableRelations(context.Background(), "public"); err == nil {
		t.Fatal("expected error on closed db")
	}

	// postgresSelectVisibility.SelectableRelations success via seam
	testDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = testDB.Close() }()
	ctx := context.Background()
	if _, err := testDB.ExecContext(ctx, "CREATE TABLE test_rels (name TEXT, can_select BOOLEAN); INSERT INTO test_rels VALUES ('orders', 1), ('secret', 0)"); err != nil {
		t.Fatal(err)
	}
	queryPostgresSelectableRelations = func(ctx context.Context, _ *sql.DB, _ string) (*sql.Rows, error) {
		return testDB.QueryContext(ctx, "SELECT name, can_select FROM test_rels")
	}
	vSuccess := postgresSelectVisibility{db: testDB}
	selectable, err := vSuccess.SelectableRelations(ctx, "public")
	if err != nil || !selectable["orders"] || selectable["secret"] {
		t.Fatalf("SelectableRelations success failed: %v, %#v", err, selectable)
	}

	// SelectableRelations scan error
	queryPostgresSelectableRelations = func(ctx context.Context, _ *sql.DB, _ string) (*sql.Rows, error) {
		return testDB.QueryContext(ctx, "SELECT 123, 'not-a-bool' FROM test_rels")
	}
	if _, err := vSuccess.SelectableRelations(ctx, "public"); err == nil {
		t.Fatal("expected scan error")
	}

	// SelectableRelations rows.Err() error
	queryPostgresSelectableRelations = func(ctx context.Context, _ *sql.DB, _ string) (*sql.Rows, error) {
		mockQueryFunc = func(_ string, _ []driver.Value) (driver.Rows, error) {
			return &mockSimpleRows{cols: []string{"name", "can_select"}, rowErr: errors.New("rows err")}, nil
		}
		mockVisDB, err := sql.Open("mockmountdriver", "mock")
		if err != nil {
			t.Fatal(err)
		}
		return mockVisDB.QueryContext(ctx, "SELECT")
	}
	if _, err := vSuccess.SelectableRelations(ctx, "public"); err == nil {
		t.Fatal("expected rows.Err() error")
	}

	// WriteCapabilities: error querying
	origWriteCaps := queryPostgresWriteCapabilities
	defer func() { queryPostgresWriteCapabilities = origWriteCaps }()
	_ = func() error {
		_, err := origWriteCaps(ctx, closedDB, []string{"s"}, []string{"s"}, []string{"t"})
		return err
	}()
	queryPostgresWriteCapabilities = func(ctx context.Context, _ *sql.DB, _, _, _ []string) (*sql.Rows, error) {
		return nil, errors.New("query failed")
	}
	if _, err := vSuccess.WriteCapabilities(ctx, []string{"s"}, []string{"s"}, []string{"t"}); err == nil {
		t.Fatal("expected error on query failure")
	}

	// WriteCapabilities: scan error
	queryPostgresWriteCapabilities = func(ctx context.Context, _ *sql.DB, _, _, _ []string) (*sql.Rows, error) {
		return testDB.QueryContext(ctx, "SELECT name, can_select FROM test_rels")
	}
	if _, err := vSuccess.WriteCapabilities(ctx, []string{"s"}, []string{"s"}, []string{"t"}); err == nil {
		t.Fatal("expected scan error on 2 columns")
	}

	// WriteCapabilities: rows.Err() error
	queryPostgresWriteCapabilities = func(ctx context.Context, _ *sql.DB, _, _, _ []string) (*sql.Rows, error) {
		mockQueryFunc = func(_ string, _ []driver.Value) (driver.Rows, error) {
			return &mockSimpleRows{cols: []string{"reason"}, rowErr: errors.New("rows err")}, nil
		}
		mockVisDB, err := sql.Open("mockmountdriver", "mock")
		if err != nil {
			t.Fatal(err)
		}
		return mockVisDB.QueryContext(ctx, "SELECT")
	}
	if _, err := vSuccess.WriteCapabilities(ctx, []string{"s"}, []string{"s"}, []string{"t"}); err == nil {
		t.Fatal("expected rows.Err() error")
	}

	// WriteCapabilities: success with reasons
	if _, err := testDB.ExecContext(ctx, "CREATE TABLE test_caps (reason TEXT); INSERT INTO test_caps VALUES ('superuser')"); err != nil {
		t.Fatal(err)
	}
	queryPostgresWriteCapabilities = func(ctx context.Context, _ *sql.DB, _, _, _ []string) (*sql.Rows, error) {
		return testDB.QueryContext(ctx, "SELECT reason FROM test_caps")
	}
	reasons, err := vSuccess.WriteCapabilities(ctx, []string{"s"}, []string{"s"}, []string{"t"})
	if err != nil || len(reasons) != 1 || reasons[0] != "superuser" {
		t.Fatalf("expected 1 reason, got %v, err: %v", reasons, err)
	}

	// refuseWritableRole with reasons
	schemasForRefuse := &schema.Schemas{
		Collections: map[string]schema.Collection{
			"orders": {Source: &schema.NativeCollectionSource{Schema: "public", Name: "orders"}},
		},
	}
	if err := refuseWritableRole(ctx, vSuccess, schemasForRefuse); err == nil || !strings.Contains(err.Error(), "the role is not read-only: superuser") {
		t.Fatalf("expected refuseWritableRole error, got: %v", err)
	}

	// refuseWritableRole without reasons
	queryPostgresWriteCapabilities = func(ctx context.Context, _ *sql.DB, _, _, _ []string) (*sql.Rows, error) {
		return testDB.QueryContext(ctx, "SELECT reason FROM test_caps WHERE 1=0")
	}
	if err := refuseWritableRole(ctx, vSuccess, schemasForRefuse); err != nil {
		t.Fatalf("expected no error when no reasons, got: %v", err)
	}

	queryPostgresWriteCapabilities = origWriteCaps

	// Default openVisibilityDB and closePostgresDatabase
	if vDB, err := origVisDB("mockmountdriver", "mock"); err != nil {
		t.Fatal(err)
	} else {
		_ = vDB.Close()
	}
	if err := origClosePGDB(nil); err != nil {
		t.Fatal(err)
	}

	// openPostgresWithExclusions read-only branches
	t.Setenv("PG_DSN", "postgres://localhost/orders")
	mROValid := &manifest.Manifest{
		Database: manifest.Database{ID: "pgro", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true, DSNEnv: "PG_DSN"}},
	}
	fakeDB := &dalgo2postgres.Database{}
	openPostgresDatabase = func(_ string, _ dal.Schema, _ dalgo2sql.DbOptions, _ ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
		return fakeDB, nil
	}

	// 1. openVisibilityDB error
	openVisibilityDB = func(_, _ string) (*sql.DB, error) {
		return nil, errors.New("open visibility db failed")
	}
	if _, _, err := openPostgresWithExcludedRelations(mROValid, nil); err == nil || !strings.Contains(err.Error(), "could not check PostgreSQL read privileges") {
		t.Fatalf("expected check read privileges error, got: %v", err)
	}

	// 2. discoverNativeSchemas error
	openVisibilityDB = func(driverName, dataSourceName string) (*sql.DB, error) {
		return testDB, nil
	}
	discoverNativeSchemas = func(context.Context, nativePostgresCatalog, nativePostgresSelectVisibility, map[schema.NativeCollectionSource]struct{}) (*schema.Schemas, error) {
		return nil, errors.New("discovery failed")
	}
	if _, _, err := openPostgresWithExcludedRelations(mROValid, nil); err == nil || !strings.Contains(err.Error(), "failed to discover native PostgreSQL schema") {
		t.Fatalf("expected discover error, got: %v", err)
	}

	// 3. closeVisibilityDB error
	discoverNativeSchemas = func(context.Context, nativePostgresCatalog, nativePostgresSelectVisibility, map[schema.NativeCollectionSource]struct{}) (*schema.Schemas, error) {
		return &schema.Schemas{Collections: map[string]schema.Collection{"orders": {}}}, nil
	}
	closeVisibilityDB = func(_ *sql.DB) error {
		return errors.New("failed to close visibility db")
	}
	if _, _, err := openPostgresWithExcludedRelations(mROValid, nil); err == nil || !strings.Contains(err.Error(), "could not finish checking PostgreSQL read privileges") {
		t.Fatalf("expected finish checking privileges error, got: %v", err)
	}

	// 4. openPostgresWithExclusions success
	closeVisibilityDB = func(_ *sql.DB) error { return nil }
	if db, _, err := openPostgresWithExcludedRelations(mROValid, nil); err != nil {
		t.Fatalf("expected success, got: %v", err)
	} else if db == nil || mROValid.Schemas == nil {
		t.Fatal("expected schemas to be set")
	}

	// 5. openPostgresWith success
	if db, _, err := openPostgresWith(mROValid, openPostgresDatabase); err != nil {
		t.Fatalf("expected openPostgresWith success, got: %v", err)
	} else if db == nil {
		t.Fatal("expected db not nil")
	}

	// 6. non-catalog DB (postgres.go:223-225)
	if _, _, err := openPostgresNative(mROValid, func(string, dal.Schema, dalgo2sql.DbOptions, ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
		return nil, nil
	}, nativeSettingsOf(Options{}), sqlOpenPostgresAccess); err == nil || !strings.Contains(err.Error(), "PostgreSQL driver does not provide native schema discovery") {
		t.Fatalf("expected non-catalog error, got: %v", err)
	}

	// 7. RequireReadOnlyNativePostgresRole in openPostgresWithOptions (postgres.go:237-238)
	cleanDB, errClean := sql.Open("sqlite", ":memory:")
	if errClean != nil {
		t.Fatal(errClean)
	}
	defer func() { _ = cleanDB.Close() }()
	openPostgresDatabase = func(_ string, _ dal.Schema, _ dalgo2sql.DbOptions, _ ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
		return fakeDB, nil
	}
	openVisibilityDB = func(_, _ string) (*sql.DB, error) {
		return cleanDB, nil
	}
	closeVisibilityDB = func(_ *sql.DB) error { return nil }
	discoverNativeSchemas = func(context.Context, nativePostgresCatalog, nativePostgresSelectVisibility, map[schema.NativeCollectionSource]struct{}) (*schema.Schemas, error) {
		return schemasForRefuse, nil
	}
	origWriteCaps2 := queryPostgresWriteCapabilities
	defer func() { queryPostgresWriteCapabilities = origWriteCaps2 }()
	queryPostgresWriteCapabilities = func(ctx context.Context, _ *sql.DB, _, _, _ []string) (*sql.Rows, error) {
		return cleanDB.QueryContext(ctx, "SELECT 1 WHERE 1=0")
	}
	if _, _, err := openPostgresWithOptions(mROValid, Options{AllowInsecureNativePostgresTransport: true, RequireReadOnlyNativePostgresRole: true}); err != nil {
		t.Fatalf("expected success for clean read-only role, got: %v", err)
	}
	queryPostgresWriteCapabilities = origWriteCaps2

	// discoverNativePostgresSchemasExcluding edge cases:
	// 1. ListSchemas error
	catListErr := &mockPostgresCatalog{listSchemasErr: errors.New("schemas error")}
	if _, err := discoverNativePostgresSchemasExcluding(ctx, catListErr, &mockPostgresVisibility{}, nil); err == nil {
		t.Fatal("expected ListSchemas error")
	}

	// 2. SelectableRelations error
	visErr := &mockPostgresVisibility{err: errors.New("vis error")}
	if _, err := discoverNativePostgresSchemasExcluding(ctx, &mockPostgresCatalog{}, visErr, nil); err == nil {
		t.Fatal("expected SelectableRelations error")
	}

	// 3. ListSchemaCollections error
	catCollErr := &mockPostgresCatalog{listCollectionsErr: errors.New("colls error")}
	if _, err := discoverNativePostgresSchemasExcluding(ctx, catCollErr, &mockPostgresVisibility{}, nil); err == nil {
		t.Fatal("expected ListSchemaCollections error")
	}

	// 4. Bad collection ID skipped (> 63 bytes) + no collections error
	badRef := dal.NewQualifiedRootCollectionRef("public", strings.Repeat("x", 64), "")
	catBadRef := &mockPostgresCatalog{refs: []dal.CollectionRef{badRef}}
	visBadRef := &mockPostgresVisibility{selectable: map[string]bool{badRef.Name(): true}}
	if _, err := discoverNativePostgresSchemasExcluding(ctx, catBadRef, visBadRef, nil); err == nil || !strings.Contains(err.Error(), "no supported PostgreSQL relations were discovered") {
		t.Fatalf("expected no supported relations error, got: %v", err)
	}

	// 5. Bad column name in definition
	badColDef := &dbschema.CollectionDef{
		Name:   "orders",
		Fields: []dbschema.FieldDef{{Name: dal.FieldName(strings.Repeat("x", 64)), Type: dbschema.Int}},
	}
	catBadCol := &mockPostgresCatalog{def: badColDef}
	if _, err := discoverNativePostgresSchemasExcluding(ctx, catBadCol, &mockPostgresVisibility{}, nil); err == nil || !strings.Contains(err.Error(), "column outside the native name contract") {
		t.Fatalf("expected column name contract error, got: %v", err)
	}

	// 6. Zero fields in definition -> collection skipped
	emptyFieldsDef := &dbschema.CollectionDef{Name: "orders", Fields: nil}
	catEmptyFields := &mockPostgresCatalog{def: emptyFieldsDef}
	if _, err := discoverNativePostgresSchemasExcluding(ctx, catEmptyFields, &mockPostgresVisibility{}, nil); err == nil {
		t.Fatal("expected error when all collections have zero fields")
	}

	// 7. describeNativePostgresRelations with 0 relations
	defs, err := describeNativePostgresRelations(ctx, &mockPostgresCatalog{}, nil)
	if err != nil || defs != nil {
		t.Fatalf("expected nil, nil for 0 relations, got: %v, %v", defs, err)
	}

	// 8. DescribeCollection returns nil, nil
	catNilDef := &mockPostgresCatalog{nilDef: true}
	if _, err := describeNativePostgresRelations(ctx, catNilDef, []nativePostgresRelation{{schema: "public", ref: dal.NewQualifiedRootCollectionRef("public", "orders", "")}}); err == nil {
		t.Fatal("expected error for nil collection definition")
	}

	// 9. Context canceled during discovery loop
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := discoverNativePostgresSchemasExcluding(cancelCtx, &mockPostgresCatalog{}, &mockPostgresVisibility{}, nil); err == nil {
		t.Fatal("expected context canceled error")
	}

	// 10. Deadline ended in describeNativePostgresRelations
	cancelCtx2, cancel2 := context.WithCancel(context.Background())
	catCancel := &mockPostgresCatalog{
		onDescribe: cancel2,
	}
	if _, err := describeNativePostgresRelations(cancelCtx2, catCancel, []nativePostgresRelation{{schema: "public", ref: dal.NewQualifiedRootCollectionRef("public", "orders", "")}}); err == nil || !strings.Contains(err.Error(), "native PostgreSQL discovery deadline ended") {
		t.Fatalf("expected deadline ended error, got: %v", err)
	}
}

// Not parallel: modifies package-level seam variables and environment variables
func TestMountOptionsAndFileCoverage(t *testing.T) {
	origNewDB := newBatchingGitHubDB
	origOpenPG := openPostgresDatabase
	origOpenMY := openMySQLDatabase
	origCmd := execCommandContext
	defer func() {
		newBatchingGitHubDB = origNewDB
		openPostgresDatabase = origOpenPG
		openMySQLDatabase = origOpenMY
		execCommandContext = origCmd
	}()

	// closeAll
	closeAll(nil)
	closeAll(map[string]*core.Database{})

	// openHTTP validation error
	mBadHTTP := &manifest.Manifest{Storage: manifest.Storage{Engine: "http"}}
	if _, _, err := openHTTP(mBadHTTP, nil); err == nil {
		t.Fatal("expected openHTTP validation error")
	}

	// openInGitDB directory creation error
	dir := t.TempDir()
	regFile := filepath.Join(dir, "reg_file")
	if err := os.WriteFile(regFile, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openInGitDB(filepath.Join(regFile, "sub"), false); err == nil {
		t.Fatal("expected openInGitDB mkdir error")
	}

	// FileWithOptions validation errors
	// 1. ExcludedNativePostgresRelations on non-postgres manifest
	pNonPG := filepath.Join(dir, "nonpg.yaml")
	if err := os.WriteFile(pNonPG, []byte("database: {id: db1, schema_mode: schemaless}\nstorage: {engine: sqlite, path: data.sqlite}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	optsNonPG := Options{ExcludedNativePostgresRelations: []schema.NativeCollectionSource{{Schema: "s", Name: "t"}}}
	if _, err := FileWithOptions(pNonPG, optsNonPG); err == nil {
		t.Fatal("expected error for excluded relations on non-postgres engine")
	}

	// 2. ACL enabled on postgres engine
	pACLPG := filepath.Join(dir, "aclpg.yaml")
	contentACLPG := "database: {id: db2, schema_mode: schemaless}\nstorage: {engine: postgres, postgres: {dsn_env: DSN}}\nacl: {enabled: true}\n"
	if err := os.WriteFile(pACLPG, []byte(contentACLPG), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pACLPG, Options{}); err == nil {
		t.Fatal("expected error for ACL on postgres engine")
	}

	// 3. ACL enabled on InGitDB GitHub
	pACLGH := filepath.Join(dir, "aclgh.yaml")
	contentACLGH := "database: {id: db3, schema_mode: schemaless}\nstorage: {engine: ingitdb, ingitdb: {github: {owner: o, repo: r}}}\nacl: {enabled: true}\n"
	if err := os.WriteFile(pACLGH, []byte(contentACLGH), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pACLGH, Options{}); err == nil {
		t.Fatal("expected error for ACL on InGitDB GitHub")
	}

	// 4. ACL database mismatch
	pACLMismatch := filepath.Join(dir, "aclmismatch.yaml")
	contentMismatch := "database: {id: db4, schema_mode: schemaless}\nstorage: {engine: sqlite, path: data.sqlite}\nacl: {enabled: true, database: wrong}\n"
	if err := os.WriteFile(pACLMismatch, []byte(contentMismatch), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pACLMismatch, Options{}); err == nil {
		t.Fatal("expected error for ACL database mismatch")
	}

	// 5. ACLStore open error
	pACLStore := filepath.Join(dir, "aclstore.yaml")
	contentStore := "database: {id: db5, schema_mode: schemaless}\nstorage: {engine: sqlite, path: data.sqlite}\nacl: {enabled: true}\nacl_store: {path: \"does/not/exist\"}\n"
	if err := os.WriteFile(pACLStore, []byte(contentStore), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pACLStore, Options{}); err == nil {
		t.Fatal("expected error for invalid acl_store path")
	}

	// 6. Unknown engine
	pUnknown := filepath.Join(dir, "unknown.yaml")
	if err := os.WriteFile(pUnknown, []byte("database: {id: db6, schema_mode: schemaless}\nstorage: {engine: unknown}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pUnknown, Options{}); err == nil {
		t.Fatal("expected error for unknown engine")
	}

	// 7. HTTP error
	pHTTP := filepath.Join(dir, "http.yaml")
	if err := os.WriteFile(pHTTP, []byte("database: {id: db7, schema_mode: schemaless}\nstorage: {engine: http}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pHTTP, Options{}); err == nil {
		t.Fatal("expected error for incomplete HTTP storage")
	}

	// 8. MySQL DSN unset error
	pMySQL := filepath.Join(dir, "mysql.yaml")
	if err := os.WriteFile(pMySQL, []byte("database: {id: db8, schema_mode: schemaless}\nstorage: {engine: mysql, mysql: {dsn_env: UNSET_DSN}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pMySQL, Options{}); err == nil {
		t.Fatal("expected error for unset MySQL DSN")
	}

	// 9. InGitDB GitHub in FileWithOptions: error when token unset
	pGHFail := filepath.Join(dir, "ghfail.yaml")
	contentGHFail := "database: {id: ghfail, schema_mode: schemaless}\nstorage: {engine: ingitdb, ingitdb: {github: {owner: o, repo: r, token_env: UNSET_GH_TOK}}}\n"
	if err := os.WriteFile(pGHFail, []byte(contentGHFail), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pGHFail, Options{}); err == nil {
		t.Fatal("expected error for InGitDB GitHub with unset token")
	}

	// 10. InGitDB GitHub in FileWithOptions: success path
	t.Setenv("VALID_GH_TOK", "gh_token_123")
	newBatchingGitHubDB = func(_ dalgo2github.Config, _ *ingitdb.Definition, _ string) (*dalgo2github.BatchingGitHubDB, error) {
		return &dalgo2github.BatchingGitHubDB{}, nil
	}
	pGHSuccess := filepath.Join(dir, "ghsuccess.yaml")
	contentGHSuccess := "database: {id: ghsuccess, schema_mode: partial}\nstorage: {engine: ingitdb, ingitdb: {github: {owner: o, repo: r, token_env: VALID_GH_TOK}}}\nschemas: {collections: {c: {fields: {f: {type: string}}}}}\n"
	if err := os.WriteFile(pGHSuccess, []byte(contentGHSuccess), 0600); err != nil {
		t.Fatal(err)
	}
	ghDB, err := FileWithOptions(pGHSuccess, Options{})
	if err != nil {
		t.Fatalf("FileWithOptions ghsuccess: %v", err)
	}
	_ = ghDB.Close()

	// 11. InGitDB local with push hook in FileWithOptions
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "true")
	}
	pIGPush := filepath.Join(dir, "igpush.yaml")
	contentIGPush := "database: {id: igpush, schema_mode: schemaless}\nstorage: {engine: ingitdb, path: igpush_repo, ingitdb: {push: sync}}\n"
	if err := os.WriteFile(pIGPush, []byte(contentIGPush), 0600); err != nil {
		t.Fatal(err)
	}
	igPushDB, err := FileWithOptions(pIGPush, Options{})
	if err != nil {
		t.Fatalf("FileWithOptions igpush: %v", err)
	}
	_ = igPushDB.Close()

	// 12. InGitDB local with ACL policies (len(policies) > 0)
	policyFile := filepath.Join(dir, "test_policy.yaml")
	policyContent := `apiVersion: dtql.org/access/v1
kind: AccessPolicy
metadata: {name: customers}
target: {database: igacl}
composition: dalgo-hierarchical-v1
default: deny
scopes:
  - path: /customers
    rules: [{id: read, effect: allow, operations: [query], fields: [id]}]
`
	if err := os.WriteFile(policyFile, []byte(policyContent), 0600); err != nil {
		t.Fatal(err)
	}
	pIGACL := filepath.Join(dir, "igacl.yaml")
	contentIGACL := "database: {id: igacl, schema_mode: schemaless}\nstorage: {engine: ingitdb, path: igacl_repo}\nacl: {enabled: true, policies: [\"test_policy.yaml\"]}\n"
	if err := os.WriteFile(pIGACL, []byte(contentIGACL), 0600); err != nil {
		t.Fatal(err)
	}
	igACLDB, err := FileWithOptions(pIGACL, Options{})
	if err != nil {
		t.Fatalf("FileWithOptions igacl: %v", err)
	}
	_ = igACLDB.Close()

	// 13. FileWithOptions with valid policystore controller
	storeDir := filepath.Join(dir, "valid_ps")
	st, openErr := policystore.Open(storeDir, policystore.Owner{Enabled: true, Database: "psdb", Realm: "test"})
	if openErr != nil {
		t.Fatal(openErr)
	}
	psDoc, parseErr := access.ParseDTQLPolicy([]byte(strings.ReplaceAll(policyContent, "igacl", "psdb")))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if _, err := st.Activate(context.Background(), "", []access.DTQLDocument{psDoc}); err != nil {
		t.Fatal(err)
	}
	pValidPS := filepath.Join(dir, "valid_ps.yaml")
	contentValidPS := "database: {id: psdb, schema_mode: schemaless}\nstorage: {engine: ingitdb, path: ps_repo}\nacl: {enabled: true, realm: test}\nacl_store: {path: \"" + storeDir + "\"}\n"
	if err := os.WriteFile(pValidPS, []byte(contentValidPS), 0600); err != nil {
		t.Fatal(err)
	}
	psDB, err := FileWithOptions(pValidPS, Options{})
	if err != nil {
		t.Fatalf("FileWithOptions valid_ps: %v", err)
	}
	_ = psDB.Close()

	// 14. Dir non-existent directory error
	if _, err := Dir(filepath.Join(dir, "missing_directory")); err == nil {
		t.Fatal("expected Dir error for missing directory")
	}

	// 15. Dir with valid manifests (hits line 230 and 232)
	validDir := filepath.Join(dir, "validdir")
	if err := os.MkdirAll(validDir, 0700); err != nil {
		t.Fatal(err)
	}
	validManifest := filepath.Join(validDir, "v.yaml")
	if err := os.WriteFile(validManifest, []byte("database: {id: vdb, schema_mode: schemaless}\nstorage: {engine: ingitdb, path: v_repo}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dbs, err := Dir(validDir)
	if err != nil || len(dbs) != 1 {
		t.Fatalf("Dir validdir: %v, %#v", err, dbs)
	}
	closeAll(dbs)

	// 16. Dir where one succeeds and another fails -> closeAll called
	mixedDir := filepath.Join(dir, "mixeddir")
	if err := os.MkdirAll(mixedDir, 0700); err != nil {
		t.Fatal(err)
	}
	f1 := filepath.Join(mixedDir, "a.yaml")
	f2 := filepath.Join(mixedDir, "b.yaml")
	if err := os.WriteFile(f1, []byte("database: {id: mdb1, schema_mode: schemaless}\nstorage: {engine: ingitdb, path: m1_repo}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("database: {id: mdb2, schema_mode: schemaless}\nstorage: {engine: unknown}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Dir(mixedDir); err == nil {
		t.Fatal("expected error in mixedDir")
	}

	// 17. Dir duplicate ID error
	dupDir := filepath.Join(dir, "dupdir")
	if err := os.MkdirAll(dupDir, 0700); err != nil {
		t.Fatal(err)
	}
	d1 := filepath.Join(dupDir, "a.yaml")
	d2 := filepath.Join(dupDir, "b.yaml")
	dupContent := "database: {id: dupdb, schema_mode: schemaless}\nstorage: {engine: sqlite, path: d.sqlite}\n"
	if err := os.WriteFile(d1, []byte(dupContent), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d2, []byte(dupContent), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Dir(dupDir); err == nil {
		t.Fatal("expected error for duplicate database id in Dir")
	}

	// 18. FileWithOptions bad acl_store path (hits line 91)
	pBadACLStore := filepath.Join(dir, "bad_acl_store.yaml")
	if err := os.WriteFile(pBadACLStore, []byte("database: {id: badas, schema_mode: schemaless}\nstorage: {engine: ingitdb, path: badas_repo}\nacl: {enabled: true, realm: test}\nacl_store: {path: \""+filepath.Join(regFile, "sub")+"\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pBadACLStore, Options{}); err == nil {
		t.Fatal("expected error for bad acl_store path")
	}

	// 19. FileWithOptions http error (hits line 116)
	pHTTPFail := filepath.Join(dir, "http_fail.yaml")
	if err := os.WriteFile(pHTTPFail, []byte("database: {id: hfail, schema_mode: schemaless}\nstorage: {engine: http, http: {url: \"::invalid-url\"}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pHTTPFail, Options{}); err == nil {
		t.Fatal("expected error for invalid http url")
	}

	// 19b. FileWithOptions http openHTTP error (hits lines 116-117)
	pHTTPOpenFail := filepath.Join(dir, "http_open_fail.yaml")
	httpContent := "database: {id: ecb, schema_mode: strict, retention: none}\nstorage:\n  engine: http\n  http: {profile: ecb-daily/1, collection: daily}\nschemas:\n  collections:\n    daily:\n      fields:\n        time: {type: string}\n        currency: {type: string}\n        rate: {type: string}\n"
	if err := os.WriteFile(pHTTPOpenFail, []byte(httpContent), 0600); err != nil {
		t.Fatal(err)
	}
	oldNewHTTPDB := newHTTPDB
	newHTTPDB = func(_ dalgo2http.Config) (dal.DB, error) {
		return nil, errors.New("injected http open error")
	}
	if _, err := FileWithOptions(pHTTPOpenFail, Options{}); err == nil {
		t.Fatal("expected error from FileWithOptions when openHTTP fails")
	}
	newHTTPDB = oldNewHTTPDB

	// 20. FileWithOptions firestore error (hits line 141)
	t.Setenv("FIRESTORE_EMULATOR_HOST", "")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(dir, "nonexistent-creds.json"))
	pFSFail := filepath.Join(dir, "fs_fail.yaml")
	if err := os.WriteFile(pFSFail, []byte("database: {id: fsfail, schema_mode: schemaless}\nstorage: {engine: firestore, firestore: {project: valid-project}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pFSFail, Options{}); err == nil {
		t.Fatal("expected error for firestore open failure")
	}

	// Hit closePostgresDatabase with non-nil database
	_ = closePostgresDatabase(&dalgo2postgres.Database{})

	// 21. FileWithOptions postgres success (hits line 150)
	t.Setenv("PG_FILE_DSN", "postgres://localhost/orders")
	pPGSuccess := filepath.Join(dir, "pg_success.yaml")
	if err := os.WriteFile(pPGSuccess, []byte("database: {id: pgsuccess, schema_mode: strict}\nstorage: {engine: postgres, postgres: {read_only: true, dsn_env: PG_FILE_DSN}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	canonicalPGID, _ := schema.NativePostgresCollectionID("public", "orders")
	fakePG := &dalgo2postgres.Database{}
	openPostgresDatabase = func(_ string, _ dal.Schema, _ dalgo2sql.DbOptions, _ ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
		return fakePG, nil
	}
	discoverNativeSchemas = func(_ context.Context, _ nativePostgresCatalog, _ nativePostgresSelectVisibility, _ map[schema.NativeCollectionSource]struct{}) (*schema.Schemas, error) {
		return &schema.Schemas{
			Collections: map[string]schema.Collection{
				canonicalPGID: {
					Source: &schema.NativeCollectionSource{Schema: "public", Name: "orders"},
					Fields: map[string]schema.Field{"id": {Type: "string"}},
				},
			},
		}, nil
	}
	pgDB, err := FileWithOptions(pPGSuccess, Options{})
	if err != nil {
		t.Fatalf("FileWithOptions pg_success: %v", err)
	}
	_ = pgDB.Close()

	// 22. FileWithOptions mysql success (hits line 155)
	t.Setenv("MY_FILE_DSN", "user:pass@tcp(localhost:3306)/orders")
	pMYSuccess := filepath.Join(dir, "my_success.yaml")
	if err := os.WriteFile(pMYSuccess, []byte("database: {id: mysuccess, schema_mode: strict}\nstorage: {engine: mysql, mysql: {dsn_env: MY_FILE_DSN}}\nschemas: {collections: {orders: {fields: {ID: {type: string}}}}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeMY := &dalgo2mysql.Database{}
	openMySQLDatabase = func(_ string, _ dal.Schema, _ dalgo2sql.DbOptions) (*dalgo2mysql.Database, error) {
		return fakeMY, nil
	}
	_, _ = FileWithOptions(pMYSuccess, Options{})

	// 23. FileWithOptions unknown storage engine (hits line 157-158)
	pUnsupported := filepath.Join(dir, "unsupported.yaml")
	if err := os.WriteFile(pUnsupported, []byte("database: {id: dbun, schema_mode: schemaless}\nstorage: {engine: unsupported, path: somepath}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pUnsupported, Options{}); err == nil || !strings.Contains(err.Error(), "unknown storage engine") {
		t.Fatalf("expected unknown storage engine error, got: %v", err)
	}

	// 24. FileWithOptions firestore closeClient when core.Open fails (hits lines 177-178)
	t.Setenv("FIRESTORE_EMULATOR_HOST", "127.0.0.1:8888")
	pBadFS := filepath.Join(dir, "bad_fs.yaml")
	contentBadFS := "database: {id: badfs, schema_mode: schemaless}\nstorage: {engine: firestore, firestore: {project: test-proj}}\n"
	if err := os.WriteFile(pBadFS, []byte(contentBadFS), 0600); err != nil {
		t.Fatal(err)
	}
	pBadCat := filepath.Join(dir, "badfs.inferred.json")
	if err := os.WriteFile(pBadCat, []byte("{invalid-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileWithOptions(pBadFS, Options{}); err == nil {
		t.Fatal("expected error when core.Open fails on firestore mount")
	}
}
