package mount

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// batchCatalogDB is an SQLite database whose tables stand in for the answers of
// the two batched catalogue queries.
func batchCatalogDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		CREATE TABLE cols (rel TEXT, col TEXT, data_type TEXT, udt TEXT, prec INTEGER, scale INTEGER, nullable TEXT, ord INTEGER);
		INSERT INTO cols VALUES
			('Order Lines', 'line', 'smallint', 'int2', 16, 0, 'NO', 1),
			('Order Lines', 'order_id', 'bigint', 'int8', 64, 0, 'YES', 2),
			('Order Lines', 'price', 'numeric', 'numeric', 10, 2, 'YES', 3),
			('Order Lines', 'ratio', 'numeric', 'numeric', NULL, NULL, 'YES', 4),
			('Order Lines', 'note', 'text', 'text', NULL, NULL, 'NO', 5),
			('view', 'n', 'integer', 'int4', 32, 0, 'YES', 1);
		CREATE TABLE pks (rel TEXT, col TEXT);
		INSERT INTO pks VALUES ('Order Lines', 'order_id'), ('Order Lines', 'line');`); err != nil {
		t.Fatal(err)
	}
	return db
}

func stubBatchQueries(t *testing.T, columns, keys func(context.Context, *sql.DB, string) (*sql.Rows, error)) {
	t.Helper()
	origColumns, origKeys := queryPostgresSchemaColumns, queryPostgresSchemaPrimaryKeys
	t.Cleanup(func() { queryPostgresSchemaColumns, queryPostgresSchemaPrimaryKeys = origColumns, origKeys })
	queryPostgresSchemaColumns, queryPostgresSchemaPrimaryKeys = columns, keys
}

func sqliteQuery(db *sql.DB, query string) func(context.Context, *sql.DB, string) (*sql.Rows, error) {
	return func(ctx context.Context, _ *sql.DB, _ string) (*sql.Rows, error) {
		return db.QueryContext(ctx, query)
	}
}

func mockRowsQuery(t *testing.T, rows *mockSimpleRows) func(context.Context, *sql.DB, string) (*sql.Rows, error) {
	return func(ctx context.Context, _ *sql.DB, _ string) (*sql.Rows, error) {
		mockQueryFunc = func(string, []driver.Value) (driver.Rows, error) { return rows, nil }
		t.Cleanup(func() { mockQueryFunc = nil })
		db, err := sql.Open("mockmountdriver", "mock")
		if err != nil {
			t.Fatal(err)
		}
		return db.QueryContext(ctx, "SELECT")
	}
}

func TestDescribeSchemaRelationsBuildsWhatDiscoveryReads(t *testing.T) {
	db := batchCatalogDB(t)
	stubBatchQueries(t, sqliteQuery(db, "SELECT * FROM cols ORDER BY rel, ord"), sqliteQuery(db, "SELECT rel, col FROM pks"))
	defs, err := postgresSelectVisibility{db: db}.DescribeSchemaRelations(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	lines := defs["Order Lines"]
	if lines == nil || defs["view"] == nil || len(defs) != 2 {
		t.Fatalf("definitions = %#v", defs)
	}
	if !reflect.DeepEqual(lines.PrimaryKey, []dal.FieldName{"order_id", "line"}) {
		t.Fatalf("primary key = %v", lines.PrimaryKey)
	}
	want := []dbschema.FieldDef{
		{Name: "line", Type: dbschema.Int},
		{Name: "order_id", Type: dbschema.Int},
		{Name: "price", Type: dbschema.Decimal, Nullable: true, Precision: &dbschema.Precision{Total: 10, Scale: 2}},
		{Name: "ratio", Type: dbschema.Decimal, Nullable: true},
		{Name: "note", Type: dbschema.String},
	}
	if !reflect.DeepEqual(lines.Fields, want) {
		t.Fatalf("fields = %#v", lines.Fields)
	}
	columns := lines.SourceDefinition.Columns
	if columns[0].PrimaryKeyPosition != 2 || columns[1].PrimaryKeyPosition != 1 || columns[2].PrimaryKeyPosition != 0 ||
		columns[2].DeclaredType != "numeric" || !columns[4].NotNull || columns[3].NotNull {
		t.Fatalf("source columns = %#v", columns)
	}
	if defs["view"].PrimaryKey != nil || !defs["view"].Fields[0].Nullable {
		t.Fatalf("view = %#v", defs["view"])
	}
}

func TestDescribeSchemaRelationsFailuresWrapTheCause(t *testing.T) {
	db := batchCatalogDB(t)
	queryErr := errors.New("query failed")
	failing := func(context.Context, *sql.DB, string) (*sql.Rows, error) { return nil, queryErr }
	goodColumns, goodKeys := sqliteQuery(db, "SELECT * FROM cols"), sqliteQuery(db, "SELECT rel, col FROM pks")
	for name, tc := range map[string]struct {
		columns, keys func(context.Context, *sql.DB, string) (*sql.Rows, error)
		cause         error
	}{
		"keys query":   {goodColumns, failing, queryErr},
		"keys scan":    {goodColumns, sqliteQuery(db, "SELECT rel FROM pks"), nil},
		"keys rows":    {goodColumns, mockRowsQuery(t, &mockSimpleRows{cols: []string{"r", "c"}, rowErr: queryErr}), queryErr},
		"columns":      {failing, goodKeys, queryErr},
		"columns scan": {sqliteQuery(db, "SELECT rel FROM cols"), goodKeys, nil},
		"columns rows": {mockRowsQuery(t, &mockSimpleRows{cols: []string{"a", "b", "c", "d", "e", "f", "g", "h"}, rowErr: queryErr}), goodKeys, queryErr},
	} {
		t.Run(name, func(t *testing.T) {
			stubBatchQueries(t, tc.columns, tc.keys)
			_, err := postgresSelectVisibility{db: db}.DescribeSchemaRelations(context.Background(), "sales")
			var describeErr *nativePostgresSchemaDescribeError
			if !errors.As(err, &describeErr) || err.Error() != `could not describe the PostgreSQL relations of schema "sales"` {
				t.Fatalf("error = %v", err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("error %v does not wrap the cause", err)
			}
		})
	}
}

func TestNativePostgresColumnTypeMatchesTheAdapter(t *testing.T) {
	for _, tc := range []struct {
		dataType, udt string
		want          dbschema.Type
	}{
		{"numeric(10, 2)", "numeric", dbschema.Decimal},
		{"NUMERIC(99999999999999999999,1)", "x", dbschema.String},
		{"integer", "int4", dbschema.Int},
		{"double precision", "float8", dbschema.Float},
		{"boolean", "bool", dbschema.Bool},
		{"timestamp with time zone", "timestamptz", dbschema.Time},
		{"date", "date", dbschema.Time},
		{"numeric", "numeric", dbschema.Decimal},
		{"bytea", "bytea", dbschema.Bytes},
		{"USER-DEFINED", "timestamptz", dbschema.Time},
		{"USER-DEFINED", "numeric", dbschema.Decimal},
		{"USER-DEFINED", "mood", dbschema.String},
		{"character varying", "varchar", dbschema.String},
		{"ARRAY", "_int4", dbschema.String},
	} {
		if got := nativePostgresColumnType(tc.dataType, tc.udt); got != tc.want {
			t.Errorf("nativePostgresColumnType(%q, %q) = %v, want %v", tc.dataType, tc.udt, got, tc.want)
		}
	}
}

type batchDescriberFixture struct {
	calls map[string]int
	defs  map[string]map[string]*dbschema.CollectionDef
	err   error
}

func (f *batchDescriberFixture) SelectableRelations(context.Context, string) (map[string]bool, error) {
	return map[string]bool{"orders": true, "gone": true}, nil
}

func (f *batchDescriberFixture) DescribeSchemaRelations(_ context.Context, schemaName string) (map[string]*dbschema.CollectionDef, error) {
	f.calls[schemaName]++
	return f.defs[schemaName], f.err
}

func TestDescribeNativePostgresRelationsBatchedReadsEachSchemaOnce(t *testing.T) {
	orders := &dbschema.CollectionDef{Name: "orders", Fields: []dbschema.FieldDef{{Name: "id", Type: dbschema.Int}}, PrimaryKey: []dal.FieldName{"id"}}
	fixture := &batchDescriberFixture{calls: map[string]int{}, defs: map[string]map[string]*dbschema.CollectionDef{"a": {"orders": orders}, "b": {}}}
	relations := []nativePostgresRelation{
		{schema: "a", ref: dal.NewQualifiedRootCollectionRef("a", "orders", "")},
		{schema: "a", ref: dal.NewQualifiedRootCollectionRef("a", "gone", "")},
		{schema: "b", ref: dal.NewQualifiedRootCollectionRef("b", "orders", "")},
	}
	defs, err := describeNativePostgresRelationsBatched(context.Background(), fixture, relations)
	if err != nil {
		t.Fatal(err)
	}
	if defs[0] != orders || defs[1].Name != "gone" || len(defs[1].Fields) != 0 || defs[2].Name != "orders" || len(defs[2].Fields) != 0 {
		t.Fatalf("definitions = %#v", defs)
	}
	if !reflect.DeepEqual(fixture.calls, map[string]int{"a": 1, "b": 1}) {
		t.Fatalf("calls = %v", fixture.calls)
	}

	fixture.err = errors.New("describe failed")
	if _, err := describeNativePostgresRelationsBatched(context.Background(), fixture, relations); !errors.Is(err, fixture.err) {
		t.Fatalf("error = %v", err)
	}
	fixture.err = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := describeNativePostgresRelationsBatched(ctx, fixture, relations); err == nil || !strings.Contains(err.Error(), "deadline ended") {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscoveryUsesTheBatchedDescriberWhenTheAccessHasOne(t *testing.T) {
	orders := &dbschema.CollectionDef{Name: "orders", Fields: []dbschema.FieldDef{{Name: "id", Type: dbschema.Int}}, PrimaryKey: []dal.FieldName{"id"}}
	fixture := &batchDescriberFixture{calls: map[string]int{}, defs: map[string]map[string]*dbschema.CollectionDef{"public": {"orders": orders}}}
	catalog := &mockPostgresCatalog{
		schemas:     []string{"public"},
		refs:        []dal.CollectionRef{dal.NewQualifiedRootCollectionRef("public", "orders", ""), dal.NewQualifiedRootCollectionRef("public", "gone", "")},
		describeErr: errors.New("the per-relation path must not run"),
	}
	discovered, err := discoverNativePostgresSchemasExcluding(context.Background(), catalog, fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := schema.NativePostgresCollectionID("public", "orders")
	if len(discovered.Collections) != 1 || !discovered.Collections[id].Fields["id"].PrimaryKey || fixture.calls["public"] != 1 {
		t.Fatalf("discovered = %#v, calls = %v", discovered.Collections, fixture.calls)
	}
}

func TestNativeMountReportsAnEndedDiscoveryDeadlineAsATimeout(t *testing.T) {
	origOpen, origVis, origCloseVis, origClosePG, origDiscover := openPostgresDatabase, openVisibilityDB, closeVisibilityDB, closePostgresDatabase, discoverNativeSchemas
	t.Cleanup(func() {
		openPostgresDatabase, openVisibilityDB, closeVisibilityDB, closePostgresDatabase, discoverNativeSchemas = origOpen, origVis, origCloseVis, origClosePG, origDiscover
	})
	visibilityDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = visibilityDB.Close() })
	openPostgresDatabase = func(string, dal.Schema, dalgo2sql.DbOptions, ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
		return &dalgo2postgres.Database{}, nil
	}
	openVisibilityDB = func(string, string) (*sql.DB, error) { return visibilityDB, nil }
	closeVisibilityDB = func(*sql.DB) error { return nil }
	closePostgresDatabase = func(*dalgo2postgres.Database) error { return nil }
	t.Setenv("PG_BATCH_DSN", "postgres://localhost/orders")
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "pgro", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true, DSNEnv: "PG_BATCH_DSN"}},
	}
	settings := nativeSettingsOf(Options{NativePostgresDiscoveryTimeout: 20 * time.Millisecond})

	discoverNativeSchemas = func(ctx context.Context, _ nativePostgresCatalog, _ nativePostgresSelectVisibility, _ map[schema.NativeCollectionSource]struct{}) (*schema.Schemas, error) {
		<-ctx.Done()
		return nil, &nativePostgresSchemaDescribeError{schema: "s", cause: ctx.Err()}
	}
	_, _, err = openPostgresNative(m, openPostgresDatabase, settings, sqlOpenPostgresAccess)
	if !errors.Is(err, ErrNativePostgresDiscoveryTimeout) || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.HasPrefix(err.Error(), "native PostgreSQL discovery did not finish in time (20ms, context deadline exceeded): failed to discover native PostgreSQL schema") {
		t.Fatalf("error = %v", err)
	}

	// A failure before the deadline is not a timeout.
	discoverNativeSchemas = func(context.Context, nativePostgresCatalog, nativePostgresSelectVisibility, map[schema.NativeCollectionSource]struct{}) (*schema.Schemas, error) {
		return nil, errors.New("broken")
	}
	_, _, err = openPostgresNative(m, openPostgresDatabase, settings, sqlOpenPostgresAccess)
	if err == nil || errors.Is(err, ErrNativePostgresDiscoveryTimeout) {
		t.Fatalf("error = %v", err)
	}
}

func TestBatchedCatalogueQueriesSendTheirStatement(t *testing.T) {
	var queries []string
	mockQueryFunc = func(query string, args []driver.Value) (driver.Rows, error) {
		if len(args) != 1 || args[0] != "sales" {
			t.Errorf("arguments = %v", args)
		}
		queries = append(queries, query)
		return &mockSimpleRows{cols: []string{"x"}}, nil
	}
	t.Cleanup(func() { mockQueryFunc = nil })
	db, err := sql.Open("mockmountdriver", "mock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, query := range []func(context.Context, *sql.DB, string) (*sql.Rows, error){queryPostgresSchemaColumns, queryPostgresSchemaPrimaryKeys} {
		rows, err := query(context.Background(), db, "sales")
		if err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	if !reflect.DeepEqual(queries, []string{nativePostgresSchemaColumnsSQL, nativePostgresSchemaPrimaryKeysSQL}) {
		t.Fatalf("queries = %q", queries)
	}
}
