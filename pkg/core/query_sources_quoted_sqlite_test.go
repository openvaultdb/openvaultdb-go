package core_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
)

const (
	quotedSrcPublic = "Order Details"
	quotedSrcQuoted = `"Order Details"`
)

// quotedSrcMount mounts a SQLite file whose manifest keys one collection by its
// SQL-quoted identifier, "Order Details". The file holds the table Order Details,
// which the manifest declares, and a table whose name carries the quote
// characters, which it does not declare. Each has one row that names its table.
func quotedSrcMount(t *testing.T) *core.Database {
	t.Helper()
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE "Order Details" (id TEXT PRIMARY KEY, name TEXT)`,
		`INSERT INTO "Order Details" VALUES ('1', 'declared table')`,
		`CREATE TABLE """Order Details""" (id TEXT PRIMARY KEY, name TEXT)`,
		`INSERT INTO """Order Details""" VALUES ('1', 'table named with quotes')`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "db.yaml")
	declaration := "database: {id: quoted, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    '\"Order Details\"':\n      fields:\n        name: {type: string}\n"
	if err := os.WriteFile(manifestPath, []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// quotedSrcNames returns the name field of each record.
func quotedSrcNames(records []core.Record) []string {
	var names []string
	for _, rec := range records {
		name, _ := rec.Data["name"].(string)
		names = append(names, name)
	}
	return names
}

// quotedSrcRows reads every row the executor returns for query.
func quotedSrcRows(executor dal.QueryExecutor, query dal.StructuredQuery) ([]string, error) {
	reader, err := executor.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	var names []string
	for {
		rec, err := reader.Next()
		if errors.Is(err, io.EOF) || errors.Is(err, dal.ErrNoMoreRecords) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		data, _ := rec.Data().(map[string]any)
		name, _ := data["name"].(string)
		names = append(names, name)
	}
}

// quotedSrcDoc is a DTQL document of one collection, as written.
func quotedSrcDoc(name string) string {
	return "from: {name: '" + strings.ReplaceAll(name, "'", "''") + "'}\n"
}

// TestQueriesNameADeclaredCollectionByItsPublicNameOnly: a SQLite manifest that
// keys a collection by its quoted identifier declares it under two spellings,
// but a query is given to the adapter as written, and the adapter quotes a name
// as one identifier. Every entry point that takes a query therefore accepts the
// public name (the declared table) and refuses the quoted spelling with
// ErrNotFound, whatever position it is in: it would address the table whose name
// carries the quote characters, which the manifest does not declare.
func TestQueriesNameADeclaredCollectionByItsPublicNameOnly(t *testing.T) {
	ctx := context.Background()
	db := quotedSrcMount(t)
	want := []string{"declared table"}
	notFound := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, core.ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", what, err)
		}
	}
	reads := func(what string, names []string, err error) {
		t.Helper()
		if err != nil || len(names) != 1 || names[0] != want[0] {
			t.Errorf("%s: read %v, %v; want %v", what, names, err, want)
		}
	}
	parse := func(doc string) dal.StructuredQuery {
		t.Helper()
		query, _, err := core.ParseDTQL([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		return query
	}
	subquery := func(name string) string {
		return quotedSrcDoc(quotedSrcPublic) + "where: {exists: {query: {from: {name: '" + strings.ReplaceAll(name, "'", "''") + "'}}}}\n"
	}

	// The wire query.
	_, err := db.Execute(ctx, core.Query{Collection: quotedSrcQuoted})
	notFound("Execute, quoted", err)
	records, err := db.Execute(ctx, core.Query{Collection: quotedSrcPublic})
	reads("Execute, public", quotedSrcNames(records), err)

	// The DTQL document, as the root and in a subquery.
	_, err = db.ExecuteDTQL(ctx, []byte(quotedSrcDoc(quotedSrcQuoted)))
	notFound("ExecuteDTQL root, quoted", err)
	records, err = db.ExecuteDTQL(ctx, []byte(quotedSrcDoc(quotedSrcPublic)))
	reads("ExecuteDTQL root, public", quotedSrcNames(records), err)
	_, err = db.ExecuteDTQL(ctx, []byte(subquery(quotedSrcQuoted)))
	notFound("ExecuteDTQL subquery, quoted", err)
	records, err = db.ExecuteDTQL(ctx, []byte(subquery(quotedSrcPublic)))
	reads("ExecuteDTQL subquery, public", quotedSrcNames(records), err)

	// The snapshot stream.
	var streamed []core.Record
	collect := func(rec core.Record) error { streamed = append(streamed, rec); return nil }
	notFound("StreamDTQLSnapshot, quoted", db.StreamDTQLSnapshot(ctx, parse(quotedSrcDoc(quotedSrcQuoted)), collect))
	notFound("StreamDTQLSnapshot subquery, quoted", db.StreamDTQLSnapshot(ctx, parse(subquery(quotedSrcQuoted)), collect))
	if len(streamed) != 0 {
		t.Errorf("a refused snapshot emitted %v", quotedSrcNames(streamed))
	}
	err = db.StreamDTQLSnapshot(ctx, parse(quotedSrcDoc(quotedSrcPublic)), collect)
	reads("StreamDTQLSnapshot, public", quotedSrcNames(streamed), err)

	// The access sample.
	_, _, err = db.SelectAccessSample(ctx, parse(quotedSrcDoc(quotedSrcQuoted)), 1, access.Principal{})
	notFound("SelectAccessSample, quoted", err)
	records, _, err = db.SelectAccessSample(ctx, parse(quotedSrcDoc(quotedSrcPublic)), 1, access.Principal{})
	reads("SelectAccessSample, public", quotedSrcNames(records), err)

	// The join source: Executor and the executor of a read transaction, with
	// the quoted spelling as the root, as a joined source and in a subquery.
	join := func(name string) dal.StructuredQuery {
		return mustDTQL(t, "from: {name: '"+quotedSrcPublic+"', alias: a, joins: [{from: {name: '"+strings.ReplaceAll(name, "'", "''")+"', alias: b}, on: [{op: '==', left: {field: id, source: a}, right: {field: id, source: b}}]}]}\ncolumns: [{field: name, source: a}]\n")
	}
	for label, executor := range map[string]func(func(dal.QueryExecutor) error) error{
		"Executor": func(f func(dal.QueryExecutor) error) error { return f(db.Executor()) },
		"ReadTx":   func(f func(dal.QueryExecutor) error) error { return db.ReadTx(ctx, f) },
	} {
		for what, query := range map[string]dal.StructuredQuery{
			"root":     parse(quotedSrcDoc(quotedSrcQuoted)),
			"join":     join(quotedSrcQuoted),
			"subquery": parse(subquery(quotedSrcQuoted)),
		} {
			var readErr error
			if err := executor(func(e dal.QueryExecutor) error {
				_, readErr = quotedSrcRows(e, query)
				return nil
			}); err != nil {
				t.Fatalf("%s %s: %v", label, what, err)
			}
			notFound(label+" "+what+", quoted", readErr)
		}
		var names []string
		var readErr error
		if err := executor(func(e dal.QueryExecutor) error {
			names, readErr = quotedSrcRows(e, parse(quotedSrcDoc(quotedSrcPublic)))
			return nil
		}); err != nil {
			t.Fatalf("%s public: %v", label, err)
		}
		reads(label+" root, public", names, readErr)
	}
	fields := db.Executor().(dal.JoinFieldsProvider)
	_, err = fields.JoinFields(ctx, dal.NewRootCollectionRef(quotedSrcQuoted, ""))
	notFound("JoinFields, quoted", err)
	if _, err = fields.JoinFields(ctx, dal.NewRootCollectionRef(quotedSrcPublic, "")); err != nil {
		t.Errorf("JoinFields, public: %v", err)
	}
}
