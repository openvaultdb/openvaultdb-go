package mount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2mysql"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// markerPassword stands for the password of a connection string. Nothing a mount
// returns may hold it.
const markerPassword = "pw-MARKER-7f3a91"

func sqlMountManifest(engine string) *manifest.Manifest {
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "sqlmount", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
			"orders":    {Fields: map[string]schema.Field{"total": {Type: schema.TypeString}}},
		}},
	}
	switch engine {
	case "postgres":
		m.Storage.Postgres = &manifest.PostgresOptions{DSNEnv: "TEST_SQL_MOUNT_DSN"}
	case "mysql":
		m.Storage.MySQL = &manifest.MySQLOptions{DSNEnv: "TEST_SQL_MOUNT_DSN"}
	}
	return m
}

// chain is every error reachable from err through errors.Unwrap and the
// Unwrap() []error form, err itself first.
func chain(err error) []error {
	var out []error
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		out = append(out, e)
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner)
			}
		}
	}
	walk(err)
	return out
}

// assertNoMarker fails when the text of err, or of any error reachable from it,
// holds a marker.
func assertNoMarker(t *testing.T, err error, markers ...string) {
	t.Helper()
	for _, e := range chain(err) {
		for _, marker := range markers {
			if strings.Contains(e.Error(), marker) {
				t.Errorf("%T %q holds %q", e, e.Error(), marker)
			}
		}
	}
}

// TestPostgresOpenErrorIsBuiltFromAFixedSentenceAndTheAdaptersText: an error from
// opening or pinging a postgres mount is built here. It is a fixed sentence that
// names the environment variable, followed by the text of the adapter's own
// *dalgo2postgres.ConnectionError when the opener returned one (that text is
// built by the adapter from a fixed sentence and checked parts, and holds no text
// of the connection string). The opener's error is not wrapped: neither its text
// nor the connection string is in what is returned or reachable from it.
func TestPostgresOpenErrorIsBuiltFromAFixedSentenceAndTheAdaptersText(t *testing.T) {
	dsn := "postgres://app:" + markerPassword + "@db.example.test:5432/orders?sslmode=require"
	t.Setenv("TEST_SQL_MOUNT_DSN", dsn)
	connection := &dalgo2postgres.ConnectionError{Kind: dalgo2postgres.FailureServer, SQLState: "28P01", Host: "db.example.test", Port: "5432", Database: "orders"}
	const sentence = "failed to open Postgres via $TEST_SQL_MOUNT_DSN"
	for name, c := range map[string]struct {
		returned error
		want     string
	}{
		"the adapter's connection error": {connection, sentence + ": " + connection.Error()},
		"a connection error inside the opener's own text": {
			fmt.Errorf("opening %s: %w", dsn, connection), sentence + ": " + connection.Error()},
		"an error of the driver that holds the connection string": {
			fmt.Errorf("dial %s: password %s rejected", dsn, markerPassword), sentence + ": the connection could not be opened or verified"},
		"an error that wraps one that does": {
			fmt.Errorf("sql.Open(%q): %w", dsn, errors.New("password "+markerPassword)), sentence + ": the connection could not be opened or verified"},
	} {
		t.Run(name, func(t *testing.T) {
			opener := func(string, dal.Schema, dalgo2sql.DbOptions, ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
				return nil, c.returned
			}
			db, modes, err := openPostgresWith(sqlMountManifest("postgres"), opener)
			if db != nil || modes != nil || err == nil {
				t.Fatalf("got %v, %v, %v; want only an error", db, modes, err)
			}
			if err.Error() != c.want {
				t.Errorf("got %q, want %q", err.Error(), c.want)
			}
			if errors.Unwrap(err) != nil || len(chain(err)) != 1 {
				t.Errorf("the error reaches %d errors through Unwrap", len(chain(err))-1)
			}
			assertNoMarker(t, err, markerPassword, dsn, "app:")
		})
	}
}

// TestPostgresOpenPassesTheDeclaredCollectionsToTheOpener: with an opener that
// succeeds, the mount returns its database and the strict mode, and gave the
// opener the connection string of the environment variable, a recordset keyed on
// id for each declared collection and the dollar placeholder of PostgreSQL.
func TestPostgresOpenPassesTheDeclaredCollectionsToTheOpener(t *testing.T) {
	t.Setenv("TEST_SQL_MOUNT_DSN", "postgres://app@db.example.test/orders")
	var gotDSN string
	var gotOpts dalgo2sql.DbOptions
	opened := &dalgo2postgres.Database{}
	opener := func(dsn string, _ dal.Schema, opts dalgo2sql.DbOptions, _ ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
		gotDSN, gotOpts = dsn, opts
		return opened, nil
	}
	db, modes, err := openPostgresWith(sqlMountManifest("postgres"), opener)
	if err != nil {
		t.Fatal(err)
	}
	if db != dal.DB(opened) || len(modes) != 1 || modes[0] != schema.ModeStrict {
		t.Errorf("got %v, %v", db, modes)
	}
	if gotDSN != "postgres://app@db.example.test/orders" || gotOpts.Placeholder != dalgo2sql.PlaceholderDollar || len(gotOpts.Recordsets) != 2 || gotOpts.Recordsets["customers"] == nil || gotOpts.Recordsets["orders"] == nil {
		t.Errorf("the opener was given %q and %+v", gotDSN, gotOpts)
	}
}

// TestMySQLOpenErrorIsAFixedSentence: an error from opening or pinging a mysql
// mount is a fixed sentence that names the environment variable and nothing of
// the driver's error or of the connection string, and nothing is reachable from
// it.
func TestMySQLOpenErrorIsAFixedSentence(t *testing.T) {
	dsn := "app:" + markerPassword + "@tcp(db.example.test:3306)/orders"
	t.Setenv("TEST_SQL_MOUNT_DSN", dsn)
	opener := func(string, dal.Schema, dalgo2sql.DbOptions) (*dalgo2mysql.Database, error) {
		return nil, fmt.Errorf("dalgo2mysql: sql.Open(%q): %w", dsn, errors.New("Access denied for user 'app' (using password: "+markerPassword+")"))
	}
	db, modes, err := openMySQLWith(sqlMountManifest("mysql"), opener)
	if db != nil || modes != nil || err == nil {
		t.Fatalf("got %v, %v, %v; want only an error", db, modes, err)
	}
	if want := "failed to open MySQL via $TEST_SQL_MOUNT_DSN: the connection could not be opened or verified"; err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
	if errors.Unwrap(err) != nil || len(chain(err)) != 1 {
		t.Errorf("the error reaches %d errors through Unwrap", len(chain(err))-1)
	}
	assertNoMarker(t, err, markerPassword, dsn, "app:", "Access denied")
}

// TestMySQLOpenPassesTheDeclaredCollectionsToTheOpener is the success case of the
// test above.
func TestMySQLOpenPassesTheDeclaredCollectionsToTheOpener(t *testing.T) {
	t.Setenv("TEST_SQL_MOUNT_DSN", "app@tcp(db.example.test:3306)/orders")
	var gotDSN string
	var gotOpts dalgo2sql.DbOptions
	opened := &dalgo2mysql.Database{}
	opener := func(dsn string, _ dal.Schema, opts dalgo2sql.DbOptions) (*dalgo2mysql.Database, error) {
		gotDSN, gotOpts = dsn, opts
		return opened, nil
	}
	db, modes, err := openMySQLWith(sqlMountManifest("mysql"), opener)
	if err != nil {
		t.Fatal(err)
	}
	if db != dal.DB(opened) || len(modes) != 1 || modes[0] != schema.ModeStrict {
		t.Errorf("got %v, %v", db, modes)
	}
	if gotDSN != "app@tcp(db.example.test:3306)/orders" || len(gotOpts.Recordsets) != 2 || gotOpts.Recordsets["customers"] == nil || gotOpts.Recordsets["orders"] == nil {
		t.Errorf("the opener was given %q and %+v", gotDSN, gotOpts)
	}
}

// TestSQLMountWithoutADSNNamesTheVariableAndOpensNothing: a manifest of a
// postgres or mysql mount whose environment variable is empty does not open, and
// the error names the variable, through the same entry point File uses.
func TestSQLMountWithoutADSNNamesTheVariableAndOpensNothing(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			t.Setenv("TEST_SQL_MOUNT_DSN", "")
			path := filepath.Join(t.TempDir(), "db.yaml")
			text := "database: {id: sqlmount, schema_mode: strict}\nstorage:\n  engine: " + engine + "\n  " + engine + ": {dsn_env: TEST_SQL_MOUNT_DSN}\nschemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n"
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := File(path)
			if db != nil || err == nil || !strings.Contains(err.Error(), "$TEST_SQL_MOUNT_DSN") || !strings.Contains(err.Error(), "DSN not set") {
				t.Fatalf("got %v, %v", db, err)
			}
		})
	}
}
