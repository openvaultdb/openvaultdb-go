package core

import (
	"context"
	"errors"
	"testing"

	"github.com/dal-go/record"
)

// keyUpdates are updates that name the record's key column: as a field name or
// as the head of a path, in any case, as a set, a delete, a transform or a server
// timestamp.
var keyUpdates = map[string]UpdateOp{
	"field name":                 {FieldName: "id", Value: "c2"},
	"field name, upper case":     {FieldName: "ID", Value: "c2"},
	"field name, mixed case":     {FieldName: "iD", Value: "c2"},
	"field path":                 {FieldPath: []string{"id"}, Value: "c2"},
	"field path, upper case":     {FieldPath: []string{"Id"}, Value: "c2"},
	"nested field path":          {FieldPath: []string{"id", "part"}, Value: "c2"},
	"dotted field name":          {FieldName: "id.part", Value: "c2"},
	"delete":                     {FieldName: "id", Delete: true},
	"increment":                  {FieldName: "id", Transform: "increment", Value: 1},
	"server timestamp":           {FieldName: "id", ServerTimestamp: true},
	"path of a later field name": {FieldPath: []string{"ID", "x"}, Delete: true},
}

// TestUpdateOfTheKeyColumnIsRefusedBeforeTheAdapterOnSQLEngines: the key of a
// record is the id column of its table, which an update does not change. On
// sqlite, postgres and mysql an update that names it is refused whole with
// ErrKeyUpdate (a bad request), before any adapter call, whatever else the batch
// holds.
func TestUpdateOfTheKeyColumnIsRefusedBeforeTheAdapterOnSQLEngines(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	good := record.NewKeyWithID("customers", "good")
	for _, engine := range writeGuardSQLEngines {
		for name, u := range keyUpdates {
			t.Run(engine+"/"+name, func(t *testing.T) {
				db, fake := writeGuardOpen(t, engine, "customers")
				for label, batch := range map[string][]Op{
					"alone":           {{Op: "update", Key: key, Updates: []UpdateOp{u}}},
					"beside a field":  {{Op: "update", Key: key, Updates: []UpdateOp{{FieldName: "name", Value: "Bob"}, u}}},
					"after a good op": {{Op: "set", Key: good, Data: writeGuardPlain}, {Op: "update", Key: key, Updates: []UpdateOp{u}}},
				} {
					_, err := db.Apply(context.Background(), batch, "")
					if !errors.Is(err, ErrKeyUpdate) || !errors.Is(err, ErrInvalidFieldName) {
						t.Errorf("%s: got %v, want ErrKeyUpdate", label, err)
					}
				}
				if fake.reached() != 0 {
					t.Fatalf("adapter reached %d times", fake.reached())
				}
			})
		}
	}
}

// TestUpdateOfAFieldThatContainsIDIsNotTheKeyColumn: only the column that holds
// the key is refused, not a field whose name merely starts or ends with it. The
// strict mount turns such an update away for the field it does not declare, which
// is a refusal of its own and not ErrKeyUpdate.
func TestUpdateOfAFieldThatContainsIDIsNotTheKeyColumn(t *testing.T) {
	for _, engine := range writeGuardSQLEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine, "customers")
			for _, name := range []string{"identity", "uid", "id2", "customer_id"} {
				op := Op{Op: "update", Key: record.NewKeyWithID("customers", "c1"), Updates: []UpdateOp{{FieldName: name, Value: "x"}}}
				if _, err := db.Apply(context.Background(), []Op{op}, ""); err == nil || errors.Is(err, ErrKeyUpdate) || errors.Is(err, ErrInvalidFieldName) {
					t.Errorf("%s: got %v, want the strict mount's refusal of an undeclared field", name, err)
				}
			}
			op := Op{Op: "update", Key: record.NewKeyWithID("customers", "c1"), Updates: []UpdateOp{{FieldName: "name", Value: "x"}}}
			if _, err := db.Apply(context.Background(), []Op{op}, ""); err != nil || fake.transactions != 1 {
				t.Fatalf("a declared field: %v, transactions %d", err, fake.transactions)
			}
		})
	}
}

// TestUpdateOfTheKeyColumnReachesDocumentEngines: a document engine keeps the key
// outside the fields of the record, so the rule is for engines that build SQL
// only and ingitdb and firestore are unchanged.
func TestUpdateOfTheKeyColumnReachesDocumentEngines(t *testing.T) {
	for _, engine := range writeGuardDocumentEngines {
		for name, u := range keyUpdates {
			t.Run(engine+"/"+name, func(t *testing.T) {
				db, fake := writeGuardOpen(t, engine, "customers")
				op := Op{Op: "update", Key: record.NewKeyWithID("customers", "c1"), Updates: []UpdateOp{u}}
				if n, err := db.Apply(context.Background(), []Op{op}, ""); err != nil || n != 1 {
					t.Fatalf("%d %v", n, err)
				}
				if fake.transactions != 1 {
					t.Fatalf("transactions = %d", fake.transactions)
				}
			})
		}
	}
}
