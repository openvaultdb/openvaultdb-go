package core

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
	"github.com/dal-go/record"
	"github.com/dal-go/record/update"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// The messages of Get, Apply and the schema calls repeat the key or the
// collection they were asked about. The fakes below answer every adapter call
// with a failure, so each message is built, and the tests check how much of a
// name it repeats.

var errClipKeyAdapter = errors.New("adapter refused")

// clipKeyMode is how clipKeyDB answers a Get.
type clipKeyMode string

const (
	clipKeyPresent     clipKeyMode = "present"      // the record exists
	clipKeyAbsent      clipKeyMode = "absent"       // not found, reported as an error
	clipKeyAbsentQuiet clipKeyMode = "absent-quiet" // not found, reported only by the record
	clipKeyBroken      clipKeyMode = "broken"       // any other error
)

// clipKeyDB is a dal.DB that answers Get as its mode says and runs a
// read-write transaction against clipKeyTx, whose writes all fail. Every other
// method panics (nil embedded DB).
type clipKeyDB struct {
	dal.DB
	mode clipKeyMode
}

func (f clipKeyDB) Get(_ context.Context, rec record.Record) error {
	switch f.mode {
	case clipKeyAbsent:
		rec.SetError(record.ErrRecordNotFound)
		return record.ErrRecordNotFound
	case clipKeyAbsentQuiet:
		rec.SetError(record.ErrRecordNotFound)
		return nil
	case clipKeyBroken:
		return errClipKeyAdapter
	}
	rec.SetError(nil)
	rec.Data().(map[string]any)["name"] = "Ada"
	return nil
}

func (f clipKeyDB) RunReadwriteTransaction(ctx context.Context, worker dal.RWTxWorker, _ ...dal.TransactionOption) error {
	return worker(ctx, clipKeyTx{})
}

type clipKeyTx struct{ dal.ReadwriteTransaction }

func (clipKeyTx) Set(context.Context, record.Record) error { return errClipKeyAdapter }
func (clipKeyTx) Insert(context.Context, record.Record, ...dal.InsertOption) error {
	return errClipKeyAdapter
}
func (clipKeyTx) Update(context.Context, *record.Key, []update.Update, ...dal.Precondition) error {
	return errClipKeyAdapter
}
func (clipKeyTx) Delete(context.Context, *record.Key) error { return errClipKeyAdapter }

// clipKeyReaderDB and clipKeyModifierDB add the schema surface: a describe and a
// create that fail.
type clipKeyReaderDB struct {
	clipKeyDB
	dbschema.SchemaReader
}

func (clipKeyReaderDB) DescribeCollection(context.Context, *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	return nil, errClipKeyAdapter
}

type clipKeyModifierDB struct {
	clipKeyDB
	ddl.SchemaModifier
}

func (clipKeyModifierDB) CreateCollection(context.Context, dbschema.CollectionDef, ...ddl.Option) error {
	return errClipKeyAdapter
}

// clipKeyOpen opens a schemaless ingitdb database (any collection name is
// allowed) over db.
func clipKeyOpen(t *testing.T, db dal.DB) *Database {
	t.Helper()
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "clipkey", SchemaMode: schema.ModeSchemaless},
		Storage:  manifest.Storage{Engine: "ingitdb"},
	}
	opened, err := Open(m, db, []schema.Mode{schema.ModeSchemaless}, filepath.Join(t.TempDir(), "inferred.json"))
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

// clipKeySQLOpen opens a strict sqlite database that declares customers over db.
func clipKeySQLOpen(t *testing.T, db dal.DB) *Database {
	t.Helper()
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "clipkey", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
		}},
	}
	opened, err := Open(m, db, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

// TestMessagesClipTheKeyTheyRepeat: whatever the name of a collection or the id
// of a record, the message of a failed Get or Apply repeats at most
// maxEchoedNameLen bytes of each segment of the key, and a short key is shown
// whole.
func TestMessagesClipTheKeyTheyRepeat(t *testing.T) {
	huge := strings.Repeat("x", 1<<16)
	short := record.NewKeyWithParentAndID(record.NewKeyWithID("customers", "c1"), "orders", "o1")
	long := record.NewKeyWithParentAndID(record.NewKeyWithID(huge, huge), huge, huge)
	setData := map[string]any{"name": "Ada"}
	rename := []UpdateOp{{FieldName: "name", Value: "Bob"}}
	apply := func(op string, data map[string]any, updates []UpdateOp) func(*Database, *record.Key) error {
		return func(db *Database, key *record.Key) error {
			_, err := db.Apply(context.Background(), []Op{{Op: op, Key: key, Data: data, Updates: updates}}, "")
			return err
		}
	}
	for _, c := range []struct {
		name string
		mode clipKeyMode
		run  func(*Database, *record.Key) error
		is   error
		sql  bool // on a sqlite database that declares customers, with keys that have no parent
	}{
		{"Get, not found by the adapter", clipKeyAbsent, func(db *Database, key *record.Key) error { _, err := db.Get(context.Background(), key); return err }, ErrNotFound, false},
		{"Get, not found by the record", clipKeyAbsentQuiet, func(db *Database, key *record.Key) error { _, err := db.Get(context.Background(), key); return err }, ErrNotFound, false},
		{"set, refused by the adapter", clipKeyAbsent, apply("set", setData, nil), errClipKeyAdapter, false},
		{"insert, refused by the adapter", clipKeyAbsent, apply("insert", setData, nil), errClipKeyAdapter, false},
		{"update, refused by the adapter", clipKeyPresent, apply("update", nil, rename), errClipKeyAdapter, false},
		{"delete, refused by the adapter", clipKeyPresent, apply("delete", nil, nil), errClipKeyAdapter, false},
		{"batch check, record unreadable", clipKeyBroken, apply("set", setData, nil), errClipKeyAdapter, false},
		{"batch check, insert of an existing record", clipKeyPresent, apply("insert", setData, nil), ErrAlreadyExists, false},
		{"batch check, update of a missing record", clipKeyAbsent, apply("update", nil, rename), ErrUpdateOfMissingRecord, false},
		{"batch check, update that does not apply", clipKeyPresent, apply("update", nil, []UpdateOp{{FieldName: "name", FieldPath: []string{"name"}, Value: "Bob"}}), nil, false},
		{"batch check, set of an existing record that names no field", clipKeyPresent, apply("set", map[string]any{}, nil), ErrEmptyWrite, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, shortKey, longKey, wantShort := clipKeyOpen(t, clipKeyDB{mode: c.mode}), short, long, "customers/c1/orders/o1"
			if c.sql {
				db, shortKey, longKey, wantShort = clipKeySQLOpen(t, clipKeyDB{mode: c.mode}), record.NewKeyWithID("customers", "c1"), record.NewKeyWithID("customers", huge), "customers/c1"
			}
			err := c.run(db, shortKey)
			if err == nil || !strings.Contains(err.Error(), wantShort) {
				t.Fatalf("a short key is shown whole: %v", err)
			}
			err = c.run(db, longKey)
			if err == nil || (c.is != nil && !errors.Is(err, c.is)) {
				t.Fatalf("long key: %v", err)
			}
			if len(err.Error()) > 4*(maxEchoedNameLen+32)+256 || strings.Contains(err.Error(), strings.Repeat("x", 2*maxEchoedNameLen)) {
				t.Fatalf("%d bytes of message: %.200v", len(err.Error()), err)
			}
		})
	}
}

// TestSchemaMessagesClipTheCollectionTheyRepeat: the messages of a failed
// describe and a failed create repeat at most maxEchoedNameLen bytes of the
// collection name, and a short name is shown whole.
func TestSchemaMessagesClipTheCollectionTheyRepeat(t *testing.T) {
	huge := strings.Repeat("x", 1<<16)
	ctx := context.Background()
	reader := clipKeyOpen(t, clipKeyReaderDB{})
	modifier := clipKeyOpen(t, clipKeyModifierDB{})
	for _, c := range []struct {
		name string
		run  func(collection string) error
	}{
		{"describe", func(collection string) error { _, err := reader.CollectionForeignKeys(ctx, collection); return err }},
		{"create", func(collection string) error { return modifier.ensureCollection(ctx, collection, nil) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run("customers"); !errors.Is(err, errClipKeyAdapter) || !strings.Contains(err.Error(), `"customers"`) {
				t.Fatalf("a short name is shown whole: %v", err)
			}
			err := c.run(huge)
			if !errors.Is(err, errClipKeyAdapter) || len(err.Error()) > 2*maxEchoedNameLen+128 || !strings.Contains(err.Error(), "65536 bytes") {
				t.Fatalf("%v", err)
			}
		})
	}
}
