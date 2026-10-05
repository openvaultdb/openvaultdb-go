package mount

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	"github.com/dal-go/dalgo2sqlite"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// These tests hold the behaviour that came with the releases of the libraries
// taken in the same change (dalgo v0.89.6, dalgo2sql v0.26.5, dalgo2postgres
// v0.4.1): what a mount answers where a library now refuses what it used to carry
// out, or answers differently.

// TestAMountWithAccessControlOnAndNoPolicyStoredDoesNotOpen: a secured database
// that has no policy in force denies every request (dalgo access.SecureDB), and
// the first-party callers pass a policy or a provider. A mount that has access
// control on and no policy stored never reaches that point: it is refused when it
// opens, whichever way the policy is meant to be supplied, so no handle of it
// exists to serve a request. This is what such a mount answers, and it answered
// the same before: the refusals are the ones of this repository and of the access
// package's file loader, which come before any secured database is built.
func TestAMountWithAccessControlOnAndNoPolicyStoredDoesNotOpen(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb"} {
		storage := "data"
		if engine == "sqlite" {
			storage = "data.sqlite"
		}
		base := fmt.Sprintf("database: {id: crm, schema_mode: strict}\nstorage: {engine: %s, path: %s}\nschemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n", engine, storage)
		for _, c := range []struct {
			name, extra string
			storageFile string // a file of the storage that holds the policy of the second layer
			want        string // a fragment of the error
		}{
			{"access control on, no policy file", "acl: {enabled: true}\n", "", "enabled file policies require at least one policy file"},
			{"access control on, an empty list of policy files", "acl: {enabled: true, policies: []}\n", "", "enabled file policies require at least one policy file"},
			{"access control on, a policy store with nothing activated", "acl: {enabled: true}\nacl_store: {path: owner}\n", "", "active.json"},
		} {
			t.Run(engine+"/"+c.name, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "db.yaml")
				if err := os.WriteFile(path, []byte(base+c.extra), 0o600); err != nil {
					t.Fatal(err)
				}
				db, err := File(path)
				if err == nil {
					_ = db.Close()
					t.Fatal("the mount opened")
				}
				if !strings.Contains(err.Error(), "load OpenVaultDB policies") || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("error = %v, want one that holds %q", err, c.want)
				}
			})
		}
	}
	t.Run("ingitdb/the policies of the storage hold none", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "db.yaml")
		manifest := "database: {id: crm, schema_mode: strict}\nstorage: {engine: ingitdb, path: data}\nschemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n"
		if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(dir, "data", ".ingitdb", "access")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("enabled: true\ndatabase: crm\npolicies: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		db, err := File(path)
		if err == nil {
			_ = db.Close()
			t.Fatal("the mount opened")
		}
		if !strings.Contains(err.Error(), "requires at least one policy") {
			t.Fatalf("error = %v", err)
		}
	})
}

// TestASecuredSQLiteDatabaseWithNoPolicyInForceDeniesEveryRequest pins the library
// behaviour the answer above rests on: a database secured with no policy (a
// provider that supplies none, an empty list) denies a read, a query and a write,
// on a real SQLite database, and a nil policy is refused when the database is
// secured. Before dalgo v0.89.6 a request was not denied for that reason.
func TestASecuredSQLiteDatabaseWithNoPolicyInForceDeniesEveryRequest(t *testing.T) {
	ctx := context.Background()
	open := func(t *testing.T, options ...access.DBOption) (dal.DB, *sql.DB) {
		t.Helper()
		file := filepath.Join(t.TempDir(), "data.sqlite")
		raw, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		if _, err = raw.Exec(`CREATE TABLE things (id TEXT PRIMARY KEY, name TEXT)`); err != nil {
			t.Fatal(err)
		}
		if _, err = raw.Exec(`INSERT INTO things VALUES ('a', 'alpha')`); err != nil {
			t.Fatal(err)
		}
		recordsets := map[string]*dalgo2sql.Recordset{"things": dalgo2sql.NewRecordset("things", dalgo2sql.Table, []dal.FieldRef{dal.Field("id")})}
		driver, err := dalgo2sqlite.NewDatabaseWithOptions(file, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{Recordsets: recordsets, StructuredQueryDialect: "sqlite"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = driver.Close() })
		secured, err := access.SecureDB(driver, options...)
		if err != nil {
			t.Fatal(err)
		}
		return secured, raw
	}
	noPolicy := map[string]access.DBOption{
		"a provider that supplies none": access.WithDatabasePolicyProvider(func(context.Context) ([]access.Policy, error) { return nil, nil }),
		"an empty list":                 access.WithDatabasePolicies(),
	}
	for name, option := range noPolicy {
		t.Run(name, func(t *testing.T) {
			db, raw := open(t, option)
			key := record.NewKeyWithID("things", "a")
			if err := db.Get(ctx, record.NewRecordWithData(key, map[string]any{})); !errors.Is(err, access.ErrAccessDenied) {
				t.Errorf("get: %v, want a denial", err)
			}
			query := dal.From(dal.NewRootCollectionRef("things", "")).NewQuery().SelectIntoRecord(func() record.Record { return record.NewRecordWithData(key, map[string]any{}) })
			if _, err := db.ExecuteQueryToRecordsReader(ctx, query); !errors.Is(err, access.ErrAccessDenied) {
				t.Errorf("query: %v, want a denial", err)
			}
			err := db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
				return tx.Set(ctx, record.NewRecordWithData(key, map[string]any{"name": "changed"}))
			})
			if !errors.Is(err, access.ErrAccessDenied) {
				t.Errorf("write: %v, want a denial", err)
			}
			var name string
			if err := raw.QueryRow(`SELECT name FROM things WHERE id = 'a'`).Scan(&name); err != nil || name != "alpha" {
				t.Errorf("the row holds %q (%v), want it unchanged", name, err)
			}
		})
	}
	t.Run("a nil policy", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "data.sqlite")
		driver, err := dalgo2sqlite.NewDatabaseWithOptions(file, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = driver.Close() }()
		if _, err = access.SecureDB(driver, access.WithDatabasePolicies(nil)); err == nil {
			t.Fatal("a nil policy was accepted")
		}
	})
}

// TestDeleteOfASQLiteMountFollowsTheDeclaredPrimaryKey: every collection a SQLite
// mount declares is registered with the one primary-key column id, which Delete
// now follows (a recordset declared with none or with several is an error of the
// call instead of a statement on the column ID). Delete by key removes the row of
// that id and no other, from a plain collection and from one with a quoted schema
// key, singly and in a batch, and a record that is not there is not an error.
func TestDeleteOfASQLiteMountFollowsTheDeclaredPrimaryKey(t *testing.T) {
	ctx := context.Background()
	dir := sqliteFieldsStorage(t,
		`CREATE TABLE "people" ("id" TEXT PRIMARY KEY, "name" TEXT, "code" TEXT)`,
		`CREATE TABLE "Order Details" ("id" TEXT PRIMARY KEY, "note" TEXT)`,
		`INSERT INTO "people" VALUES ('p1', 'Ada', 'p2'), ('p2', 'Bea', 'p1'), ('p3', 'Cy', 'p3')`,
		`INSERT INTO "Order Details" VALUES ('o1', 'one'), ('o2', 'two'), ('o3', 'three')`)
	db := sqliteFieldsMount(t, dir, "    people:\n      fields:\n        name: {type: string}\n        code: {type: string}\n    '\"Order Details\"':\n      fields:\n        note: {type: string}\n")
	rows := func(table string) string {
		t.Helper()
		raw, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = raw.Close() }()
		found, err := raw.Query(`SELECT id FROM "` + table + `" ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = found.Close() }()
		var ids []string
		for found.Next() {
			var id string
			if err := found.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return strings.Join(ids, ",")
	}
	apply := func(ops ...core.Op) {
		t.Helper()
		if _, err := db.Apply(ctx, ops, ""); err != nil {
			t.Fatal(err)
		}
	}
	apply(core.Op{Op: "delete", Key: record.NewKeyWithID("people", "p2")})
	if got := rows("people"); got != "p1,p3" {
		t.Fatalf("people hold %q after deleting p2, want p1,p3 (a code of p1 or p2 is not a key)", got)
	}
	apply(core.Op{Op: "delete", Key: record.NewKeyWithID("people", "p2")}) // not there: not an error
	apply(core.Op{Op: "delete", Key: record.NewKeyWithID("people", "p1")}, core.Op{Op: "delete", Key: record.NewKeyWithID("Order Details", "o2")})
	if people, details := rows("people"), rows("Order Details"); people != "p3" || details != "o1,o3" {
		t.Fatalf("people hold %q and Order Details hold %q, want p3 and o1,o3", people, details)
	}
}

// failingReadMount mounts a SQLite file that declares things, then drops its table,
// so that every read of things fails in the driver.
func failingReadMount(t *testing.T) *core.Database {
	t.Helper()
	dir := sqliteFieldsStorage(t, `CREATE TABLE "things" ("id" TEXT PRIMARY KEY, "name" TEXT)`)
	db := sqliteFieldsMount(t, dir, "    things:\n      fields:\n        name: {type: string}\n")
	raw, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err = raw.Exec(`DROP TABLE things`); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestAFailedReadInATransactionOfASQLiteMountReturnsNoReader: a read that fails
// inside a read transaction answers an error and a nil reader, on the records path
// and on the recordset path. The recordset path used to return a typed nil
// pointer, which is a reader that is not nil and whose Close panics, so a caller
// that closed what it was given crashed.
func TestAFailedReadInATransactionOfASQLiteMountReturnsNoReader(t *testing.T) {
	ctx := context.Background()
	db := failingReadMount(t)
	query := dal.From(dal.NewRootCollectionRef("things", "")).NewQuery().SelectIntoRecordset()
	recordsQuery := dal.From(dal.NewRootCollectionRef("things", "")).NewQuery().SelectIntoRecord(func() record.Record {
		return record.NewRecordWithData(record.NewKeyWithID("things", "x"), map[string]any{})
	})
	var recordsReader dal.RecordsReader
	var recordsetReader dal.RecordsetReader
	var recordsErr, recordsetErr error
	err := db.ReadTx(ctx, func(tx dal.QueryExecutor) error {
		recordsReader, recordsErr = tx.ExecuteQueryToRecordsReader(ctx, recordsQuery)
		recordsetReader, recordsetErr = tx.ExecuteQueryToRecordsetReader(ctx, query)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if recordsErr == nil || recordsetErr == nil {
		t.Fatalf("the reads of a table that is not there succeeded: %v, %v", recordsErr, recordsetErr)
	}
	if recordsReader != nil {
		t.Errorf("records path: reader %#v with the error %v, want no reader", recordsReader, recordsErr)
	}
	if recordsetReader != nil {
		t.Errorf("recordset path: reader %#v with the error %v, want no reader", recordsetReader, recordsetErr)
	}
}
