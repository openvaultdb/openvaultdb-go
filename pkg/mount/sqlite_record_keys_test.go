package mount

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	"github.com/dal-go/dalgo2sqlite"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

func w1MountFile(t *testing.T, dir, options, collections string) (*core.Database, error) {
	t.Helper()
	path := filepath.Join(dir, "w1.yaml")
	text := "database: {id: w1, schema_mode: strict}\nstorage:\n  engine: sqlite\n  path: data.sqlite\n  sqlite: " + options + "\nschemas:\n  collections:\n" + collections
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := File(path)
	if db != nil {
		t.Cleanup(func() { _ = db.Close() })
	}
	return db, err
}

const w1Fields = "    things:\n      fields:\n        id: {type: integer}\n        key: {type: string}\n        name: {type: string}\n        raw: {type: any}\n"

func w1Fixture(t *testing.T) (*core.Database, string) {
	t.Helper()
	dir := sqliteFieldsStorage(t, `CREATE TABLE things (id INTEGER, key TEXT NOT NULL UNIQUE, name TEXT, raw BLOB)`, `INSERT INTO things VALUES (42, 'a/b:quote.$#[]', NULL, x'0001ff'), (99, 'z', 'last', NULL)`)
	db, err := w1MountFile(t, dir, "{busy_timeout: 0s, record_keys: {things: key}}", w1Fields)
	if err != nil {
		t.Fatal(err)
	}
	return db, filepath.Join(dir, "data.sqlite")
}
func TestSQLiteConfiguredRecordKeysPreserveNativeValuesAndProjection(t *testing.T) {
	db, _ := w1Fixture(t)
	ctx := context.Background()
	key := record.NewKeyWithID("things", "a/b:quote.$#[]")
	got, err := db.Get(ctx, key)
	if err != nil || got["id"] != int64(42) || got["name"] != nil || !reflect.DeepEqual(got["raw"], []byte{0, 1, 255}) {
		t.Fatalf("Get = %#v, %v", got, err)
	}
	if exists, err := db.Exists(ctx, key); err != nil || !exists {
		t.Fatalf("Exists=%v %v", exists, err)
	}
	if _, err := db.Get(ctx, record.NewKeyWithID("things", "42")); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("native id was used as transport key: %v", err)
	}
	driver := db.DB()
	records := []record.Record{record.NewRecordWithData(key, map[string]any{}), record.NewRecordWithData(record.NewKeyWithID("things", "z"), map[string]any{})}
	if err := driver.GetMulti(ctx, records); err != nil || !records[0].Exists() || !records[1].Exists() {
		t.Fatalf("GetMulti: %v", err)
	}
	for _, doc := range []string{"from: {name: things}\n", "from: {name: things}\ncolumns: [{field: id}]\n"} {
		out, err := db.ExecuteDTQL(ctx, []byte(doc))
		if err != nil || len(out) != 2 || out[0].Key.ID != "a/b:quote.$#[]" {
			t.Fatalf("DTQL: %#v %v", out, err)
		}
		if strings.Contains(doc, "columns") {
			if _, leaked := out[0].Data["key"]; leaked {
				t.Fatal("projected-away key leaked")
			}
		}
	}
	out, err := db.Execute(ctx, core.Query{Collection: "things", KeysOnly: true})
	if err != nil || len(out) != 2 || out[0].Key.ID != "a/b:quote.$#[]" {
		t.Fatalf("structured key-only: %#v %v", out, err)
	}
	var streamed []core.Record
	parsed, _, err := core.ParseDTQL([]byte("from: {name: things}\ncolumns: [{field: id}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.StreamDTQLSnapshot(ctx, parsed, func(rec core.Record) error { streamed = append(streamed, rec); return nil }); err != nil || len(streamed) != 2 || streamed[0].Key.ID != "a/b:quote.$#[]" {
		t.Fatalf("snapshot: %#v %v", streamed, err)
	}
	db.Manifest.Storage.SQLite.RecordKeys["things"] = "id"
	if key, ok := db.ServingKey("things"); !ok || key != "key" {
		t.Fatal("key map was not frozen")
	}
}
func TestSQLiteRecordKeyQuotedNamesAreCanonicalAndNeverDoubleEscaped(t *testing.T) {
	dir := sqliteFieldsStorage(t, `CREATE TABLE "odd.""table" ("serving key" TEXT UNIQUE, id TEXT)`, `INSERT INTO "odd.""table" VALUES ('a/b:c.$#[]', 'native')`)
	fields := "    '\"odd.\"\"table\"':\n      fields:\n        serving key: {type: string}\n        id: {type: string}\n"
	db, err := w1MountFile(t, dir, `{busy_timeout: 0s, record_keys: {'odd."table': serving key}}`, fields)
	if err != nil {
		t.Fatal(err)
	}
	key := record.NewKeyWithID(`odd."table`, "a/b:c.$#[]")
	data, err := db.Get(context.Background(), key)
	if err != nil || data["id"] != "native" {
		t.Fatalf("quoted Get: %#v %v", data, err)
	}
	parsed, err := core.ParseKeyPath(key.String())
	if err != nil || parsed.ID != key.ID {
		t.Fatalf("escaped key: %v %v", parsed, err)
	}
}
func TestSQLiteRecordKeyMapAndPhysicalIntegrityRefusals(t *testing.T) {
	for _, tc := range []struct{ name, table, options, fields string }{
		{"missing map", `CREATE TABLE things (key TEXT UNIQUE)`, `{record_keys: {other: key}}`, w1Fields},
		{"unknown mapping", `CREATE TABLE things (key TEXT UNIQUE)`, `{record_keys: {things: key, other: key}}`, w1Fields},
		{"wrong field spelling", `CREATE TABLE things (key TEXT UNIQUE)`, `{record_keys: {things: KEY}}`, w1Fields},
		{"undeclared field", `CREATE TABLE things (other TEXT UNIQUE)`, `{record_keys: {things: other}}`, w1Fields},
		{"wrong declared type", `CREATE TABLE things (id TEXT UNIQUE)`, `{record_keys: {things: id}}`, w1Fields},
		{"wrong physical type", `CREATE TABLE things (key INTEGER UNIQUE)`, `{record_keys: {things: key}}`, w1Fields},
		{"wrong physical case", `CREATE TABLE things (KEY TEXT UNIQUE)`, `{record_keys: {things: key}}`, w1Fields},
		{"no index", `CREATE TABLE things (key TEXT)`, `{record_keys: {things: key}}`, w1Fields},
		{"nonunique ordering collation", `CREATE TABLE things (key TEXT COLLATE NOCASE); CREATE UNIQUE INDEX idx ON things(key COLLATE BINARY); INSERT INTO things VALUES('A'),('a')`, `{record_keys: {things: key}}`, w1Fields},
		{"partial index", `CREATE TABLE things (key TEXT); CREATE UNIQUE INDEX idx ON things(key) WHERE key IS NOT NULL`, `{record_keys: {things: key}}`, w1Fields},
		{"composite index", `CREATE TABLE things (key TEXT, id INTEGER, UNIQUE(key,id))`, `{record_keys: {things: key}}`, w1Fields},
		{"null key", `CREATE TABLE things (key TEXT UNIQUE); INSERT INTO things VALUES(NULL)`, `{record_keys: {things: key}}`, w1Fields},
		{"empty key", `CREATE TABLE things (key TEXT UNIQUE); INSERT INTO things VALUES('')`, `{record_keys: {things: key}}`, w1Fields},
		{"invalid UTF-8", `CREATE TABLE things (key TEXT UNIQUE); INSERT INTO things VALUES(CAST(x'80' AS TEXT))`, `{record_keys: {things: key}}`, w1Fields},
		{"reserved percent", `CREATE TABLE things (key TEXT UNIQUE); INSERT INTO things VALUES('a%2Fb')`, `{record_keys: {things: key}}`, w1Fields},
		{"unsafe traversal", `CREATE TABLE things (key TEXT UNIQUE); INSERT INTO things VALUES('../secret')`, `{record_keys: {things: key}}`, w1Fields},
		{"control ID", `CREATE TABLE things (key TEXT UNIQUE); INSERT INTO things VALUES(char(1))`, `{record_keys: {things: key}}`, w1Fields},
		{"view", `CREATE TABLE source (key TEXT UNIQUE); CREATE VIEW things AS SELECT * FROM source`, `{record_keys: {things: key}}`, w1Fields},
		{"quoted map alias", `CREATE TABLE things (key TEXT UNIQUE)`, `{record_keys: {'"things"': key}}`, w1Fields},
		{"duplicate canonical alias", `CREATE TABLE things (key TEXT UNIQUE)`, `{record_keys: {things: key}}`, w1Fields + "    '\"things\"': {fields: {key: {type: string}}}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := sqliteFieldsStorage(t, tc.table)
			db, err := w1MountFile(t, dir, tc.options, tc.fields)
			if err == nil || db != nil {
				t.Fatalf("accepted invalid mount: %v", err)
			}
		})
	}
}
func TestSQLiteW1ZeroLockWaitBothHandlesAndTransactionRecovery(t *testing.T) {
	db, path := w1Fixture(t)
	driver := db.DB().(*sqliteMount)
	var wait int
	if err := driver.columns.QueryRow("PRAGMA busy_timeout").Scan(&wait); err != nil || wait != 0 {
		t.Fatalf("metadata wait=%d %v", wait, err)
	}
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	conn, err := writer.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	key := record.NewKeyWithID("things", "z")
	calls := map[string]func(context.Context) error{
		"Get":    func(ctx context.Context) error { _, err := db.Get(ctx, key); return err },
		"Exists": func(ctx context.Context) error { _, err := db.Exists(ctx, key); return err },
		"GetMulti": func(ctx context.Context) error {
			return driver.GetMulti(ctx, []record.Record{record.NewRecordWithData(key, map[string]any{})})
		},
		"metadata": func(ctx context.Context) error {
			_, err := driver.JoinFields(ctx, dal.NewRootCollectionRef("things", ""))
			return err
		},
		"transaction": func(ctx context.Context) error {
			return driver.RunReadonlyTransaction(ctx, func(ctx context.Context, tx dal.ReadTransaction) error {
				return tx.Get(ctx, record.NewRecordWithData(key, map[string]any{}))
			})
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			start := time.Now()
			if err := call(ctx); err == nil {
				t.Fatal("locked read succeeded")
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("automatic lock wait: %v", elapsed)
			}
		})
	}
	if _, err = conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Get(context.Background(), key); err != nil {
		t.Fatalf("read after lock release: %v", err)
	}
	for name, call := range calls {
		t.Run(name+" canceled", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := call(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("%v", err)
			}
		})
	}
}
func TestSQLiteW1CanceledPoolWaitReleasesMountedDriverAndMetadata(t *testing.T) {
	// The existing mount factory seam exposes an actual modernc pool for this
	// test without reaching into the driver's private fields.
	previous := newSQLiteDatabase
	var raw *sql.DB
	newSQLiteDatabase = func(dsn string, schema dal.Schema, options dalgo2sql.DbOptions) (*dalgo2sqlite.Database, error) {
		var err error
		raw, err = sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		raw.SetMaxOpenConns(1)
		return &dalgo2sqlite.Database{DB: dalgo2sql.NewDatabase(raw, schema, options)}, nil
	}
	t.Cleanup(func() {
		newSQLiteDatabase = previous
		if raw != nil {
			_ = raw.Close()
		}
	})
	db, _ := w1Fixture(t)
	driver := db.DB().(*sqliteMount)
	driver.columns.SetMaxOpenConns(1)
	for name, pool := range map[string]*sql.DB{"driver": raw, "metadata": driver.columns} {
		t.Run(name, func(t *testing.T) {
			held, err := pool.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if name == "driver" {
				_, err = db.Get(ctx, record.NewKeyWithID("things", "z"))
			} else {
				_, err = driver.JoinFields(ctx, dal.NewRootCollectionRef("things", ""))
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("pool wait: %v", err)
			}
			if name == "driver" {
				txCtx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
				err = driver.RunReadonlyTransaction(txCtx, func(context.Context, dal.ReadTransaction) error {
					t.Error("transaction acquired exhausted pool")
					return nil
				})
				stop()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("BeginTx pool wait: %v", err)
				}
			}
			if err = held.Close(); err != nil {
				t.Fatal(err)
			}
			ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
			defer cancel2()
			if name == "driver" {
				_, err = db.Get(ctx2, record.NewKeyWithID("things", "z"))
			} else {
				_, err = driver.JoinFields(ctx2, dal.NewRootCollectionRef("things", ""))
			}
			if err != nil {
				t.Fatalf("pool recovery: %v", err)
			}
			if pool.Stats().InUse != 0 {
				t.Fatalf("pool lease leaked: %+v", pool.Stats())
			}
		})
	}
}

func TestSQLiteW1ActualStatementAndIterationCancellationRecover(t *testing.T) {
	db, _ := w1Fixture(t)
	driver := db.DB().(*sqliteMount)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	// The raw statement is test-only on the mounted driver; core/server reject raw
	// queries. It makes modernc interrupt actual execution, rather than a mock call.
	query := dal.NewTextQuery(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000000) SELECT sum(x) AS total FROM n`, nil)
	reader, err := driver.ExecuteQueryToRecordsReader(ctx, query)
	if err == nil {
		_, err = reader.Next()
		_ = reader.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("actual statement cancellation: %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	parsed, _, err := core.ParseDTQL([]byte("from: {name: things}\n"))
	if err != nil {
		t.Fatal(err)
	}
	emitted := 0
	err = db.StreamDTQLSnapshot(ctx2, parsed, func(core.Record) error { emitted++; cancel2(); return nil })
	if !errors.Is(err, context.Canceled) || emitted != 1 {
		t.Fatalf("iteration cancellation: %v after %d records", err, emitted)
	}
	for _, mode := range []string{"GetMulti", "Exists"} {
		err = driver.RunReadonlyTransaction(context.Background(), func(_ context.Context, tx dal.ReadTransaction) error {
			canceled, stop := context.WithCancel(context.Background())
			stop()
			key := record.NewKeyWithID("things", "z")
			if mode == "GetMulti" {
				return tx.GetMulti(canceled, []record.Record{record.NewRecordWithData(key, map[string]any{})})
			}
			_, err := tx.Exists(canceled, key)
			return err
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("transaction %s cancellation: %v", mode, err)
		}
	}
	ctx3, cancel3 := context.WithTimeout(context.Background(), time.Second)
	defer cancel3()
	if _, err = db.Get(ctx3, record.NewKeyWithID("things", "z")); err != nil {
		t.Fatalf("statement/iteration/transaction recovery: %v", err)
	}
}

func TestSQLiteServingForeignKeyMetadataExcludesUndeclaredRelations(t *testing.T) {
	dir := sqliteFieldsStorage(t,
		`CREATE TABLE hidden (key TEXT PRIMARY KEY)`,
		`CREATE TABLE visible (key TEXT PRIMARY KEY)`,
		`CREATE TABLE things (key TEXT PRIMARY KEY, hidden_key TEXT REFERENCES hidden(key), visible_key TEXT REFERENCES visible(key))`)
	fields := "    things: {fields: {key: {type: string}, hidden_key: {type: string}, visible_key: {type: string}}}\n    visible: {fields: {key: {type: string}}}\n"
	db, err := w1MountFile(t, dir, `{busy_timeout: 0s, record_keys: {things: key, visible: key}}`, fields)
	if err != nil {
		t.Fatal(err)
	}
	for _, collection := range []string{"hidden", `"hidden"`, "HIDDEN"} {
		if _, err := db.CollectionForeignKeys(context.Background(), collection); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("undeclared metadata %s: %v", collection, err)
		}
	}
	keys, err := db.CollectionForeignKeys(context.Background(), "things")
	if err != nil || len(keys) != 1 || keys[0].ReferencedCollection != "visible" {
		t.Fatalf("public FK metadata: %#v %v", keys, err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var count int
	if err := raw.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list('things')`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("native FK definitions changed: %d %v", count, err)
	}
}

func TestSQLiteServingKeyUTF8PreservesNativeText(t *testing.T) {
	dir := sqliteFieldsStorage(t, `CREATE TABLE things (id INTEGER, key TEXT UNIQUE, name TEXT)`, `INSERT INTO things VALUES(42,'é/東京🙂',CAST(x'80' AS TEXT))`)
	db, err := w1MountFile(t, dir, `{busy_timeout: 0s, record_keys: {things: key}}`, w1Fields)
	if err != nil {
		t.Fatal(err)
	}
	key := record.NewKeyWithID("things", "é/東京🙂")
	data, err := db.Get(context.Background(), key)
	if err != nil || !reflect.DeepEqual([]byte(data["name"].(string)), []byte{0x80}) {
		t.Fatalf("native text changed: %#v %v", data, err)
	}
	wire, err := json.Marshal(key.String())
	if err != nil {
		t.Fatal(err)
	}
	var encoded string
	if err = json.Unmarshal(wire, &encoded); err != nil {
		t.Fatal(err)
	}
	decoded, err := core.ParseKeyPath(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ID != key.ID {
		t.Fatalf("unicode key changed: %#v", decoded)
	}
	if _, err = db.Get(context.Background(), decoded); err != nil {
		t.Fatal(err)
	}
}
