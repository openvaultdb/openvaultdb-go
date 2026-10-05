package core_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dtql"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

// srcSQLiteMount mounts a SQLite file that declares the collection things and
// holds, besides it, a table the manifest does not declare. Both hold rows.
func srcSQLiteMount(t *testing.T) *core.Database {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	declaration := "database: {id: sources, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    things:\n      fields:\n        name: {type: string}\n"
	if err := os.WriteFile(manifestPath, []byte(declaration), 0o600); err != nil {
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
	for _, statement := range []string{
		`INSERT INTO things (id, name) VALUES ('a', 'alpha'), ('b', 'beta')`,
		`CREATE TABLE undeclared_table (id TEXT PRIMARY KEY, name TEXT)`,
		`INSERT INTO undeclared_table (id, name) VALUES ('x', 'ex'), ('y', 'why')`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// srcSQLiteDocuments are DTQL documents that name a collection the mount does
// not declare, in every position a document can place one.
func srcSQLiteDocuments(name string) map[string]string {
	return map[string]string{
		"root":               fmt.Sprintf("from: {name: %s}\n", name),
		"exists":             fmt.Sprintf("from: {name: things}\nwhere: {exists: {query: {from: {name: %s}}}}\n", name),
		"not exists":         fmt.Sprintf("from: {name: things}\nwhere: {notExists: {query: {from: {name: %s}}}}\n", name),
		"exists with a join": fmt.Sprintf("from: {name: things}\nwhere: {exists: {query: {from: {name: things, alias: t2, joins: [{from: {name: %s, alias: u}, on: [{op: '==', left: {field: id, source: t2}, right: {field: id, source: u}}]}]}}}}\n", name),
		"scalar":             fmt.Sprintf("from: {name: things}\nwhere: {op: '==', left: {field: name}, right: {query: {from: {name: %s}, columns: [{field: name}], limit: 1}}}\n", name),
	}
}

func TestSQLiteMountQueriesReadOnlyDeclaredCollections(t *testing.T) {
	db := srcSQLiteMount(t)
	ctx := context.Background()
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, core.ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", what, err)
		}
	}
	for _, name := range []string{"undeclared_table", "sqlite_master", "sqlite_schema", "sqlite_temp_master", "sqlite_sequence", "Things"} {
		for label, doc := range srcSQLiteDocuments(name) {
			_, err := db.ExecuteDTQL(ctx, []byte(doc))
			refused(name+" dtql "+label, err)
		}
		_, err := db.Execute(ctx, core.Query{Collection: name})
		refused(name+" wire", err)
		_, err = db.Execute(ctx, core.Query{Collection: name, KeysOnly: true})
		refused(name+" wire keys", err)

		query, _, err := core.ParseDTQL([]byte(srcSQLiteDocuments(name)["exists"]))
		if err != nil {
			t.Fatal(err)
		}
		refused(name+" snapshot", db.StreamDTQLSnapshot(ctx, query, func(core.Record) error { return nil }))
		_, _, err = db.SelectAccessSample(ctx, query, 1, access.Principal{})
		refused(name+" sample", err)

		// Through the join source: as a root, a joined source and a derived one.
		for label, query := range map[string]dal.StructuredQuery{
			"root":    mustDTQL(t, srcSQLiteDocuments(name)["root"]),
			"join":    mustDTQL(t, fmt.Sprintf("from: {name: things, alias: t, joins: [{from: {name: %s, alias: u}, on: [{op: '==', left: {field: id, source: t}, right: {field: id, source: u}}]}]}\n", name)),
			"derived": mustDTQL(t, fmt.Sprintf("from:\n  query:\n    as: d\n    from: {name: %s}\n", name)),
		} {
			_, err := db.Executor().ExecuteQueryToRecordsReader(ctx, query)
			refused(name+" executor "+label, err)
			err = db.ReadTx(ctx, func(tx dal.QueryExecutor) error {
				_, err := tx.ExecuteQueryToRecordsReader(ctx, query)
				return err
			})
			refused(name+" read transaction "+label, err)
		}
		_, err = db.Executor().(dal.JoinFieldsProvider).JoinFields(ctx, dal.NewRootCollectionRef(name, ""))
		refused(name+" join fields", err)
	}
	// The table is still there and so is the data: nothing was read or changed.
	records, err := db.ExecuteDTQL(ctx, []byte("from: {name: things}\norderBy: [{field: id}]\n"))
	if err != nil || len(records) != 2 {
		t.Fatalf("declared collection: %d records, %v", len(records), err)
	}
}

// mustDTQL decodes a DTQL document without the single-collection restrictions
// of ParseDTQL, so a join or a derived source can be handed to the join source.
func mustDTQL(t *testing.T, doc string) dal.StructuredQuery {
	t.Helper()
	query, err := dtql.Deserialize([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return query
}

// TestSQLiteMountStillQueriesDeclaredCollections: the declared collection is
// read on every route, alone and as the source of a join or a subquery.
func TestSQLiteMountStillQueriesDeclaredCollections(t *testing.T) {
	db := srcSQLiteMount(t)
	ctx := context.Background()
	records, err := db.ExecuteDTQL(ctx, []byte("from: {name: things}\norderBy: [{field: id}]\n"))
	if err != nil || len(records) != 2 || records[0].Data["name"] != "alpha" {
		t.Fatalf("dtql: %v %v", records, err)
	}
	records, err = db.ExecuteDTQL(ctx, []byte("from: {name: things}\nwhere: {exists: {query: {from: {name: things, alias: t2}, where: {op: '==', left: {field: name}, right: {value: beta}}}}}\norderBy: [{field: id}]\n"))
	if err != nil || len(records) != 2 {
		t.Fatalf("dtql with a subquery on a declared collection: %v %v", records, err)
	}
	wire, err := db.Execute(ctx, core.Query{Collection: "things", OrderBy: []core.OrderBy{{Field: "id"}}})
	if err != nil || len(wire) != 2 {
		t.Fatalf("wire: %v %v", wire, err)
	}
	if ids := joinSrcReadAll(t, db.Executor(), "from: {name: things}\norderBy: [{field: id}]\n"); len(ids) != 2 {
		t.Fatalf("executor: %v", ids)
	}
	if _, err := db.Executor().(dal.JoinFieldsProvider).JoinFields(ctx, dal.NewRootCollectionRef("things", "")); errors.Is(err, core.ErrNotFound) {
		t.Fatalf("join fields of a declared collection: %v", err)
	}
	joined := mustDTQL(t, "from: {name: things, alias: t, joins: [{from: {name: things, alias: u}, on: [{op: '==', left: {field: id, source: t}, right: {field: id, source: u}}]}]}\ncolumns: [{field: id, source: t}]\n")
	reader, err := db.Executor().ExecuteQueryToRecordsReader(ctx, joined)
	if err != nil {
		t.Fatalf("a join of a declared collection with itself: %v", err)
	}
	_ = reader.Close()
}
