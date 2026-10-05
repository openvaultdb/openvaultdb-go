package core

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// writeRecordingDB is a dal.DB that runs the body of a read-write transaction
// against a transaction that records the write calls it receives, in order, as
// "<call> <key>". Any other method panics (nil embedded interfaces), so a write
// that strays onto another adapter path fails loudly. A record is reported as
// absent when absent is set.
type writeRecordingDB struct {
	writeGuardDB
	calls     []string
	insertErr error // what Insert answers, when set
}

func (f *writeRecordingDB) RunReadwriteTransaction(ctx context.Context, worker dal.RWTxWorker, _ ...dal.TransactionOption) error {
	f.transactions++
	return worker(ctx, &writeRecordingTx{db: f})
}

type writeRecordingTx struct {
	dal.ReadwriteTransaction
	db *writeRecordingDB
}

func (t *writeRecordingTx) Set(_ context.Context, rec record.Record) error {
	t.db.calls = append(t.db.calls, "set "+rec.Key().String())
	return nil
}

func (t *writeRecordingTx) Insert(_ context.Context, rec record.Record, _ ...dal.InsertOption) error {
	t.db.calls = append(t.db.calls, "insert "+rec.Key().String())
	return t.db.insertErr
}

func (t *writeRecordingTx) Delete(_ context.Context, key *record.Key) error {
	t.db.calls = append(t.db.calls, "delete "+key.String())
	return nil
}

func openWriteRecording(t *testing.T, engine string, absent bool) (*Database, *writeRecordingDB) {
	t.Helper()
	fake := &writeRecordingDB{writeGuardDB: writeGuardDB{absent: absent}}
	mode := schema.ModeStrict
	for _, e := range writeGuardDocumentEngines {
		if e == engine {
			mode = schema.ModeSchemaless
		}
	}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "recording", SchemaMode: mode},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
		}},
	}
	db, err := Open(m, fake, []schema.Mode{mode}, filepath.Join(t.TempDir(), "inferred.json"))
	if err != nil {
		t.Fatal(err)
	}
	return db, fake
}

// TestSetOfNoColumnForARecordThatDoesNotExistIsCarriedOutAsAnInsert: the adapter
// of a SQL engine refuses a set that has no column to write (the library takes it
// for an error of the call: there is nothing to put in the statement), but a set
// of no field for a record that does not exist is a write the server has always
// answered with success, and still does: the record holds only its id. The batch
// has checked, under the write lock, that the record is not there, so the write
// reaches the adapter as the insert it is. A set that carries a field, and every
// set on a document engine, still reaches the adapter as a set.
func TestSetOfNoColumnForARecordThatDoesNotExistIsCarriedOutAsAnInsert(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	for _, c := range []struct {
		name   string
		engine string
		data   map[string]any
		want   string
	}{
		{"empty data", "sqlite", map[string]any{}, "insert customers/c1"},
		{"no data", "postgres", nil, "insert customers/c1"},
		{"only the id", "mysql", map[string]any{"id": "c1"}, "insert customers/c1"},
		{"a field", "sqlite", map[string]any{"name": "Ada"}, "set customers/c1"},
		{"a field beside the id", "postgres", map[string]any{"id": "c1", "name": "Ada"}, "set customers/c1"},
		{"empty data on a document engine", "ingitdb", map[string]any{}, "set customers/c1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			db, fake := openWriteRecording(t, c.engine, true)
			if _, err := db.Apply(context.Background(), []Op{{Op: "set", Key: key, Data: c.data}}, ""); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fake.calls, []string{c.want}) {
				t.Fatalf("adapter calls = %q, want %q", fake.calls, []string{c.want})
			}
		})
	}
}

// TestSetOfNoColumnAfterADeleteOfTheRecordIsCarriedOutAsAnInsert: a record the
// batch deleted is not there, so a set of no field that follows is an insert too.
func TestSetOfNoColumnAfterADeleteOfTheRecordIsCarriedOutAsAnInsert(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	db, fake := openWriteRecording(t, "sqlite", false)
	batch := []Op{{Op: "delete", Key: key}, {Op: "set", Key: key, Data: map[string]any{}}}
	if _, err := db.Apply(context.Background(), batch, ""); err != nil {
		t.Fatal(err)
	}
	if want := []string{"delete customers/c1", "insert customers/c1"}; !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("adapter calls = %q, want %q", fake.calls, want)
	}
}

// TestAFailedInsertOfASetOfNoColumnIsReportedAsAFailedSet: the write the adapter
// refuses is reported as the set the caller sent, with the adapter's error in the
// chain, like a failed set.
func TestAFailedInsertOfASetOfNoColumnIsReportedAsAFailedSet(t *testing.T) {
	cause := errors.New("the adapter says no")
	db, fake := openWriteRecording(t, "sqlite", true)
	fake.insertErr = cause
	_, err := db.Apply(context.Background(), []Op{{Op: "set", Key: record.NewKeyWithID("customers", "c1"), Data: map[string]any{}}}, "")
	if !errors.Is(err, cause) || !strings.HasPrefix(err.Error(), "failed to set customers/c1: ") {
		t.Fatalf("error = %v", err)
	}
}
