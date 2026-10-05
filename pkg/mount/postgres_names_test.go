package mount

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// A PostgreSQL mount whose manifest declares a collection or a field name the database
// cannot keep whole is refused before the environment is read and before any connection
// is made, so that nothing is created for the entries before it. A mount on another
// engine is as it was.

func namesManifest(engine, collection, field string) *manifest.Manifest {
	m := sqlMountManifest(engine)
	m.Schemas.Collections[collection] = schema.Collection{Fields: map[string]schema.Field{field: {Type: schema.TypeString}}}
	return m
}

func TestPostgresMountRefusesANameItCannotKeepBeforeConnecting(t *testing.T) {
	// The variable is set, so a mount that got past the names would open.
	t.Setenv("TEST_SQL_MOUNT_DSN", "postgres://app@db.example.test/orders")
	long := strings.Repeat("n", 64)
	for _, tc := range []struct {
		what, collection, field, want string
	}{
		{"a collection of 64 bytes", long, "f", `schemas.collections: collection "` + long + `" cannot be mounted on PostgreSQL`},
		{"a field with a dash", "stock", "a-b", `schemas.collections.stock.fields: field "a-b" cannot be mounted on PostgreSQL`},
	} {
		t.Run(tc.what, func(t *testing.T) {
			calls := 0
			_, _, err := openPostgresWith(namesManifest("postgres", tc.collection, tc.field),
				func(string, dal.Schema, dalgo2sql.DbOptions, ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
					calls++
					return nil, nil
				})
			if calls != 0 {
				t.Errorf("the opener was called %d times, want none", calls)
			}
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal that starts %q", err, tc.want)
			}
		})
	}
	t.Run("another engine is not held to it", func(t *testing.T) {
		m := namesManifest("mysql", long, "a-b")
		if err := m.CheckPostgresNames(); err != nil {
			t.Errorf("CheckPostgresNames() = %v", err)
		}
	})
}

// TestPostgresMountOfAManifestFileWithALongNameIsRefusedAndCreatesNothing: a manifest file
// read by File is refused with the sentence of the manifest, and no database is
// opened (the variable the manifest names is not even set).
func TestPostgresMountOfAManifestFileWithALongNameIsRefusedAndCreatesNothing(t *testing.T) {
	long := strings.Repeat("n", 64)
	path := filepath.Join(t.TempDir(), "db.yaml")
	body := "database: {id: names, schema_mode: strict}\nstorage: {engine: postgres, postgres: {dsn_env: TEST_NAMES_MOUNT_DSN}}\n" +
		"schemas:\n  collections:\n    customers: {fields: {name: {type: string}}}\n    " + long + ": {fields: {name: {type: string}}}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := File(path)
	want := `schemas.collections: collection "` + long + `" cannot be mounted on PostgreSQL: its name is 64 bytes and the most is 63`
	if err == nil || !strings.HasSuffix(err.Error(), want) || !strings.Contains(err.Error(), path) {
		t.Fatalf("File = %v, want a refusal of the manifest %s ending %q", err, path, want)
	}
}

// failingServer is a dial function for pgx whose server is a few lines over an in-memory
// pipe (nothing is dialled): it completes the startup, answers the empty statement the
// driver pings with, and answers any other statement with an error, counting it. The
// adapter opens a connection by pinging it, so it opens; anything it then sends is counted.
func failingServer(statements *atomic.Int32) pgconn.DialFunc {
	return func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		deadline := time.Now().Add(30 * time.Second)
		_ = client.SetDeadline(deadline)
		_ = server.SetDeadline(deadline)
		go func() {
			defer func() { _ = server.Close() }()
			backend := pgproto3.NewBackend(server, server)
			if _, err := backend.ReceiveStartupMessage(); err != nil {
				return
			}
			backend.Send(&pgproto3.AuthenticationOk{})
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if backend.Flush() != nil {
				return
			}
			failed := &pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "XX000", Message: "the test server runs no statement"}
			discarding := false // an extended-protocol statement failed: skip to Sync
			for {
				msg, err := backend.Receive()
				if err != nil {
					return
				}
				switch m := msg.(type) {
				case *pgproto3.Query:
					if strings.TrimSpace(m.String) == "-- ping" { // what pgconn sends to ping
						backend.Send(&pgproto3.EmptyQueryResponse{})
					} else {
						statements.Add(1)
						backend.Send(failed)
					}
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				case *pgproto3.Parse:
					if !discarding {
						statements.Add(1)
						backend.Send(failed)
						discarding = true
					}
				case *pgproto3.Sync:
					discarding = false
					backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				case *pgproto3.Terminate:
					return
				}
				if backend.Flush() != nil {
					return
				}
			}
		}()
		return client, nil
	}
}

// failingAdapter opens the adapter over failingServer and returns it, with the count of
// statements the server was sent after the open.
func failingAdapter(t *testing.T) (*dalgo2postgres.Database, *atomic.Int32) {
	t.Helper()
	config, err := pgx.ParseConfig("postgres://app@localhost:5432/names?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	statements := new(atomic.Int32)
	config.DialFunc = failingServer(statements)
	registered := stdlib.RegisterConnConfig(config)
	t.Cleanup(func() { stdlib.UnregisterConnConfig(registered) })
	db, err := dalgo2postgres.NewDatabase(registered)
	if err != nil {
		t.Fatalf("open the adapter over the in-memory server: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, statements
}

// namesTable is the table of names the rule is held over: plain and not, short and long,
// in either case, with the multi-byte and the NUL cases the adapter's own table has.
var namesTable = []string{
	"a", "_", "_a", "A", "a1", "A1_b", "Orders", "select", "table", "id", "ID",
	strings.Repeat("n", 62), strings.Repeat("n", 63), strings.Repeat("N", 63), "a" + strings.Repeat("_", 62),
	"", "1a", "a b", `a"b`, "a'b", "a;b", "a-b", "a.b", "a/b", "a\n", "a\x00b", "a\xffb", "café", "naïve",
	strings.Repeat("n", 64), strings.Repeat("N", 64), strings.Repeat("n", 200), strings.Repeat("é", 32),
	strings.Repeat("Ⱥ", 31) + "a", strings.Repeat("n", 63) + `"`,
}

// TestPostgresNameRuleIsTheAdapters holds the check of the manifest equal to the verdict of
// the adapter: for each name of the table, the adapter's own entry is called over a handle
// that fails on any statement, and a name it refuses is the name the manifest check
// refuses, at each position the adapter writes one (the table, a column, the primary key,
// an index and a column of the index). The adapter refuses a name with an error that
// matches dalgo2sql.ErrUnsafeName and sends no statement, not even the beginning of a
// transaction; a name it accepts reaches the server, which fails the statement.
func TestPostgresNameRuleIsTheAdapters(t *testing.T) {
	db, statements := failingAdapter(t)
	ctx := context.Background()
	// def places name at one position and keeps every other name valid.
	positions := []struct {
		where string
		def   func(name string) dbschema.CollectionDef
	}{
		{"the table", func(name string) dbschema.CollectionDef { return namedDef(name, "f", "id", nil) }},
		{"a column", func(name string) dbschema.CollectionDef { return namedDef("t", name, "id", nil) }},
		{"the primary key", func(name string) dbschema.CollectionDef { return namedDef("t", "f", name, nil) }},
		{"an index", func(name string) dbschema.CollectionDef {
			return namedDef("t", "f", "id", &dbschema.IndexDef{Name: name, Fields: []dal.FieldName{"f"}})
		}},
		{"the column of an index", func(name string) dbschema.CollectionDef {
			return namedDef("t", "f", "id", &dbschema.IndexDef{Name: "ix", Fields: []dal.FieldName{dal.FieldName(name)}})
		}},
	}
	for _, name := range namesTable {
		refused := namesManifestVerdict(t, name)
		for _, position := range positions {
			before := statements.Load()
			err := db.CreateCollection(ctx, position.def(name), ddl.IfNotExists())
			sent := statements.Load() - before
			adapterRefuses := errors.Is(err, dalgo2sql.ErrUnsafeName)
			if adapterRefuses && sent != 0 {
				t.Errorf("%q as %s: the adapter refused it after %d statements, want none", name, position.where, sent)
			}
			if !adapterRefuses && (err == nil || sent == 0) {
				t.Errorf("%q as %s: the adapter neither refused it nor reached the server (%v, %d statements)", name, position.where, err, sent)
			}
			if adapterRefuses != refused {
				t.Errorf("%q as %s: the adapter refuses it = %v, the manifest check refuses it = %v", name, position.where, adapterRefuses, refused)
			}
		}
	}
}

// namedDef is a collection with one column besides its key and, when index is given, that
// index, each by the name given.
func namedDef(table, column, key string, index *dbschema.IndexDef) dbschema.CollectionDef {
	def := dbschema.CollectionDef{
		Name:       table,
		Fields:     []dbschema.FieldDef{{Name: dal.FieldName(key), Type: dbschema.String}, {Name: dal.FieldName(column), Type: dbschema.String, Nullable: true}},
		PrimaryKey: []dal.FieldName{dal.FieldName(key)},
	}
	if index != nil {
		index.Collection = table
		def.Indexes = []dbschema.IndexDef{*index}
	}
	return def
}

// namesManifestVerdict reports whether the manifest check refuses name, as a collection
// and as a field alike (they must agree: the adapter has one rule for every position).
func namesManifestVerdict(t *testing.T, name string) bool {
	t.Helper()
	asCollection := namesManifest("postgres", name, "f").CheckPostgresNames() != nil
	asField := namesManifest("postgres", "stock", name).CheckPostgresNames() != nil
	if asCollection != asField {
		t.Errorf("the manifest check judges %q differently as a collection and as a field", name)
	}
	return asCollection
}
