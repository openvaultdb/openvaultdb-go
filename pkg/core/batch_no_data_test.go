package core

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// openRequiredField opens a fake-backed database whose only collection,
// customers, has a required field. The fake reports the record as absent, so a
// set or an insert creates it.
func openRequiredField(t *testing.T, engine string, mode schema.Mode) (*Database, *writeGuardDB) {
	t.Helper()
	fake := &writeGuardDB{absent: true}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "required", SchemaMode: mode},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString, Required: true}}},
		}},
	}
	db, err := Open(m, fake, []schema.Mode{mode}, filepath.Join(t.TempDir(), "inferred.json"))
	if err != nil {
		t.Fatal(err)
	}
	return db, fake
}

// TestBatchRecordWrittenWithNoDataIsValidatedAsAnEmptyRecord: a set or an insert
// that carries no data writes a record with no fields, and the final validation
// of the batch holds it to the schema like any other: on a mount that requires a
// field it is refused before the adapter writes, whether the data is absent or an
// empty object. Only a record that does not exist after the batch is not
// validated.
func TestBatchRecordWrittenWithNoDataIsValidatedAsAnEmptyRecord(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	other := record.NewKeyWithID("customers", "c2")
	for _, engine := range []string{"sqlite", "postgres", "mysql", "ingitdb"} {
		mode := schema.ModeStrict
		if engine == "ingitdb" {
			mode = schema.ModePartial
		}
		for _, verb := range []string{"set", "insert"} {
			for label, data := range map[string]map[string]any{"no data": nil, "empty data": {}} {
				t.Run(engine+"/"+verb+"/"+label, func(t *testing.T) {
					db, fake := openRequiredField(t, engine, mode)
					for name, batch := range map[string][]Op{
						"alone":             {{Op: verb, Key: key, Data: data}},
						"after a valid one": {{Op: "set", Key: other, Data: map[string]any{"name": "Ada"}}, {Op: verb, Key: key, Data: data}},
						"then updated":      {{Op: verb, Key: key, Data: data}, {Op: "update", Key: key, Updates: []UpdateOp{{FieldName: "nickname", Value: "x"}}}},
					} {
						_, err := db.Apply(context.Background(), batch, "")
						var invalid *schema.ValidationError
						if !errors.As(err, &invalid) || invalid.Field != "name" {
							t.Errorf("%s: got %v, want the missing required field", name, err)
						}
					}
					if fake.transactions != 0 {
						t.Fatalf("a batch that fails validation reached the transaction: %d", fake.transactions)
					}
				})
			}
		}
	}
}

// TestBatchRecordThatDoesNotExistAfterTheBatchIsNotValidated: a record the batch
// deletes after writing it with no data is gone, and a record written with the
// required field is valid; neither is refused.
func TestBatchRecordThatDoesNotExistAfterTheBatchIsNotValidated(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	for name, batch := range map[string][]Op{
		"written with no data, then deleted": {{Op: "insert", Key: key}, {Op: "delete", Key: key}},
		"written with no data, set again":    {{Op: "insert", Key: key}, {Op: "set", Key: key, Data: map[string]any{"name": "Ada"}}},
		"written with the field":             {{Op: "insert", Key: key, Data: map[string]any{"name": "Ada"}}},
	} {
		t.Run(name, func(t *testing.T) {
			db, fake := openRequiredField(t, "sqlite", schema.ModeStrict)
			if _, err := db.Apply(context.Background(), batch, ""); err != nil {
				t.Fatal(err)
			}
			if fake.transactions != 1 {
				t.Fatalf("transactions = %d, want 1", fake.transactions)
			}
		})
	}
}
