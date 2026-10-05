package mount

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	_ "modernc.org/sqlite"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// The SQLite mount supplies the fields of a declared collection to the join
// engine from the table itself: its columns in the order the table declares them,
// the key column included. The tests of this file use real SQLite files; the
// seams of the reading (the open of the second handle, the query and the rows) are
// replaced only to reach the failures a real file cannot be made to give.

// sqliteFieldsStorage creates a SQLite file with the given statements and returns
// the directory that holds it.
func sqliteFieldsStorage(t *testing.T, statements ...string) string {
	t.Helper()
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := storage.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// sqliteFieldsMount mounts the SQLite file of dir under the given schemas block.
func sqliteFieldsMount(t *testing.T, dir, schemas string) *core.Database {
	t.Helper()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: fields, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n" + schemas
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func sqliteJoinFieldsOf(t *testing.T, db *core.Database, source dal.RecordsetSource) ([]string, error) {
	t.Helper()
	provider, ok := db.Executor().(dal.JoinFieldsProvider)
	if !ok {
		t.Fatal("the executor of a database does not offer JoinFields")
	}
	return provider.JoinFields(context.Background(), source)
}

func TestTheSQLiteMountSuppliesTheColumnsOfATableInTheOrderItDeclaresThem(t *testing.T) {
	// The table exists before it is mounted, with its columns in an order that is
	// neither the sorted one nor the one of the manifest.
	dir := sqliteFieldsStorage(t, `CREATE TABLE "people" ("id" INTEGER PRIMARY KEY, "zeta" TEXT, "alpha" TEXT, "mid")`)
	db := sqliteFieldsMount(t, dir, "    people:\n      fields:\n        alpha: {type: string}\n        mid: {type: string}\n        zeta: {type: string}\n")
	got, err := sqliteJoinFieldsOf(t, db, dal.NewRootCollectionRef("people", ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"id", "zeta", "alpha", "mid"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want the columns of the table in its order %v", got, want)
	}
}

func TestTheSQLiteMountSuppliesTheColumnsOfATableTheMountCreated(t *testing.T) {
	db := sqliteFieldsMount(t, t.TempDir(), "    people:\n      fields:\n        name: {type: string}\n        age: {type: integer}\n")
	got, err := sqliteJoinFieldsOf(t, db, dal.NewRootCollectionRef("people", "p"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"id", "age", "name"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want the key column and then the declared fields by name %v", got, want)
	}
}

func TestTheSQLiteMountSuppliesTheColumnsOfACollectionKeyedByAQuotedIdentifierUnderItsCanonicalName(t *testing.T) {
	dir := sqliteFieldsStorage(t, `CREATE TABLE "Order Details" ("id" INTEGER PRIMARY KEY, "qty" INTEGER, "note" TEXT)`)
	db := sqliteFieldsMount(t, dir, "    '\"Order Details\"':\n      fields:\n        qty: {type: integer}\n        note: {type: string}\n")
	got, err := sqliteJoinFieldsOf(t, db, dal.NewRootCollectionRef("Order Details", ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"id", "qty", "note"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
	// The quoted spelling is not a name a query may use: the guard of the database
	// answers it before the mount is asked.
	if _, err := sqliteJoinFieldsOf(t, db, dal.NewRootCollectionRef(`"Order Details"`, "")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("the quoted spelling: %v, want ErrNotFound", err)
	}
}

func TestTheSQLiteMountSuppliesTheGeneratedColumnsASelectReturnsAndNotTheHiddenOnes(t *testing.T) {
	dir := sqliteFieldsStorage(t,
		`CREATE TABLE "people" ("id" INTEGER PRIMARY KEY, "first" TEXT, "last" TEXT, "full" TEXT GENERATED ALWAYS AS ("first" || ' ' || "last") VIRTUAL)`)
	db := sqliteFieldsMount(t, dir, "    people:\n      fields:\n        first: {type: string}\n        last: {type: string}\n")
	got, err := sqliteJoinFieldsOf(t, db, dal.NewRootCollectionRef("people", ""))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"id", "first", "last", "full"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v, want %v", got, want)
	}
}

func TestTheSQLiteMountSuppliesNothingForASourceThatIsNotAPlainTable(t *testing.T) {
	db := sqliteFieldsMount(t, t.TempDir(), "    people:\n      fields:\n        name: {type: string}\n")
	mount, ok := db.DB().(*sqliteMount)
	if !ok {
		t.Fatalf("the driver is %T, want the SQLite mount", db.DB())
	}
	plain := dal.NewRootCollectionRef("people", "")
	// The control: the same table, as a plain source, has fields.
	if fields, err := mount.JoinFields(context.Background(), plain); err != nil || len(fields) == 0 {
		t.Fatalf("the control: %v, %v", fields, err)
	}
	for name, source := range map[string]dal.RecordsetSource{
		"a derived source":                    dal.NewQuerySource(dal.From(plain).NewQuery().SelectIntoRecord(nil), "d"),
		"a source qualified by a schema":      dal.NewDatabaseCollectionRef("fields", "other", "people", ""),
		"a source qualified by a parent":      dal.NewCollectionRef("people", "", record.NewKeyWithID("parents", "p1")),
		"a collection group":                  dal.NewCollectionGroupRef("people", ""),
		"a pointer to a collection reference": &plain,
	} {
		if fields, err := mount.JoinFields(context.Background(), source); fields != nil || err != nil {
			t.Errorf("%s: %v, %v; want nothing", name, fields, err)
		}
	}
}

func TestTheSQLiteMountSuppliesNothingForATableThatIsNotThere(t *testing.T) {
	db := sqliteFieldsMount(t, t.TempDir(), "    people:\n      fields:\n        name: {type: string}\n")
	mount := db.DB().(*sqliteMount)
	fields, err := mount.JoinFields(context.Background(), dal.NewRootCollectionRef("ghost", ""))
	if fields != nil || err != nil {
		t.Fatalf("a table that does not exist: %v, %v; want nothing", fields, err)
	}
}

func TestTheSQLiteMountReportsAFailureToReadTheColumns(t *testing.T) {
	db := sqliteFieldsMount(t, t.TempDir(), "    people:\n      fields:\n        name: {type: string}\n")
	mount := db.DB().(*sqliteMount)
	if err := mount.columns.Close(); err != nil {
		t.Fatal(err)
	}
	fields, err := mount.JoinFields(context.Background(), dal.NewRootCollectionRef("people", ""))
	if err == nil || fields != nil {
		t.Fatalf("a closed handle: %v, %v; want an error", fields, err)
	}
	if !strings.Contains(err.Error(), "people") {
		t.Fatalf("the error does not name the table: %v", err)
	}
}

// fakeColumnRows is the rows of a column query that end in a chosen way.
type fakeColumnRows struct {
	names   []string
	scanErr error
	err     error
	next    int
	closed  bool
}

func (r *fakeColumnRows) Next() bool {
	if r.next >= len(r.names) {
		return false
	}
	r.next++
	return true
}

func (r *fakeColumnRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	*(dest[0].(*string)) = r.names[r.next-1]
	return nil
}

func (r *fakeColumnRows) Err() error   { return r.err }
func (r *fakeColumnRows) Close() error { r.closed = true; return nil }

func TestTheColumnsOfATableAreReadThroughTheRowsOfAQuery(t *testing.T) {
	errScan, errRows, errQuery := errors.New("scan failed"), errors.New("rows failed"), errors.New("query failed")
	for name, tc := range map[string]struct {
		rows    *fakeColumnRows
		queryEr error
		want    []string
		wantErr error
	}{
		"the columns in the order of the rows": {rows: &fakeColumnRows{names: []string{"b", "a"}}, want: []string{"b", "a"}},
		"a table with no column":               {rows: &fakeColumnRows{}},
		"a row that cannot be scanned":         {rows: &fakeColumnRows{names: []string{"a"}, scanErr: errScan}, wantErr: errScan},
		"rows that end with a failure":         {rows: &fakeColumnRows{names: []string{"a"}, err: errRows}, wantErr: errRows},
		"a query that fails":                   {queryEr: errQuery, wantErr: errQuery},
	} {
		t.Run(name, func(t *testing.T) {
			var asked string
			query := func(_ context.Context, statement string, args ...any) (columnRows, error) {
				asked = statement + "|" + args[0].(string)
				if tc.queryEr != nil {
					return nil, tc.queryEr
				}
				return tc.rows, nil
			}
			got, err := tableColumns(context.Background(), query, "people")
			if !errors.Is(err, tc.wantErr) || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, %v; want %v, %v", got, err, tc.want, tc.wantErr)
			}
			if !strings.HasSuffix(asked, "|people") {
				t.Fatalf("the table is not passed as a parameter: %q", asked)
			}
			if tc.rows != nil && !tc.rows.closed {
				t.Fatal("the rows are not closed")
			}
		})
	}
}

func TestOpeningASQLiteMountReportsTheFailureToOpenItsSecondHandleAndReleasesTheFirst(t *testing.T) {
	errOpen := errors.New("open failed")
	previous := openColumnsDB
	openColumnsDB = func(string) (*sql.DB, error) { return nil, errOpen }
	t.Cleanup(func() { openColumnsDB = previous })

	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: fields, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    people:\n      fields:\n        name: {type: string}\n"
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := File(path)
	if db != nil || !errors.Is(err, errOpen) {
		t.Fatalf("got %v, %v; want the failure to open", db, err)
	}
}

func TestClosingASQLiteMountClosesBothHandles(t *testing.T) {
	db := sqliteFieldsMount(t, t.TempDir(), "    people:\n      fields:\n        name: {type: string}\n")
	mount := db.DB().(*sqliteMount)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mount.columns.PingContext(context.Background()); err == nil {
		t.Fatal("the second handle is still open after the mount was closed")
	}
}
