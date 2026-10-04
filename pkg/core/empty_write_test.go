package core

import (
	"context"
	"errors"
	"testing"

	"github.com/dal-go/record"
)

// TestEmptyUpdateIsRefusedBeforeTheAdapterOnSQLEngines: an update with no
// operation leaves a SQL adapter nothing to put in its SET clause. The batch is
// refused whole, before any adapter call.
func TestEmptyUpdateIsRefusedBeforeTheAdapterOnSQLEngines(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	good := record.NewKeyWithID("customers", "good")
	for _, engine := range writeGuardSQLEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine, "customers")
			for name, batch := range map[string][]Op{
				"empty list": {{Op: "update", Key: key, Updates: []UpdateOp{}}},
				"no list":    {{Op: "update", Key: key}},
				"after a good op": {
					{Op: "set", Key: good, Data: writeGuardPlain},
					{Op: "update", Key: key},
				},
			} {
				if _, err := db.Apply(context.Background(), batch, ""); !errors.Is(err, ErrEmptyWrite) {
					t.Errorf("%s: %v", name, err)
				}
			}
			if fake.reached() != 0 {
				t.Fatalf("adapter reached %d times", fake.reached())
			}
		})
	}
}

// TestEmptyUpdateReachesDocumentEngines: a document engine rewrites the
// document, so the rule is for engines that build SQL only.
func TestEmptyUpdateReachesDocumentEngines(t *testing.T) {
	for _, engine := range writeGuardDocumentEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine, "customers")
			op := Op{Op: "update", Key: record.NewKeyWithID("customers", "c1")}
			if n, err := db.Apply(context.Background(), []Op{op}, ""); err != nil || n != 1 {
				t.Fatalf("%d %v", n, err)
			}
			if fake.transactions != 1 {
				t.Fatalf("transactions = %d", fake.transactions)
			}
		})
	}
}

// TestSetOfNoColumnIsRefusedForARecordThatExists: the adapter updates a record
// that exists with the fields of the data. With none but the id there is no
// column to set, so the batch is refused before the write; a record that does
// not exist is inserted with only its id and the write goes through.
func TestSetOfNoColumnIsRefusedForARecordThatExists(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	datas := map[string]map[string]any{
		"empty data":    {},
		"no data":       nil,
		"only the id":   {"id": "c1"},
		"a field is it": {"id": "c1", "name": "Ada"},
	}
	for _, engine := range writeGuardSQLEngines {
		for label, data := range datas {
			refused := label != "a field is it"
			t.Run(engine+"/"+label, func(t *testing.T) {
				db, fake := writeGuardOpen(t, engine, "customers")
				ctx := context.Background()
				// The record exists.
				_, err := db.Apply(ctx, []Op{{Op: "set", Key: key, Data: data}}, "")
				if refused != errors.Is(err, ErrEmptyWrite) || !refused && err != nil {
					t.Fatalf("existing record: %v", err)
				}
				if refused && fake.transactions != 0 {
					t.Fatalf("a refused set reached the transaction: %d", fake.transactions)
				}
				// The record does not exist.
				fake.absent = true
				before := fake.transactions
				if _, err = db.Apply(ctx, []Op{{Op: "set", Key: key, Data: data}}, ""); err != nil {
					t.Fatalf("absent record: %v", err)
				}
				if fake.transactions != before+1 {
					t.Fatalf("absent record: transactions %d, want %d", fake.transactions, before+1)
				}
			})
		}
	}
}

// TestSetOfNoColumnFollowsTheRecordThroughTheBatch: whether a record exists is
// what the batch leaves of it, op by op.
func TestSetOfNoColumnFollowsTheRecordThroughTheBatch(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	empty := map[string]any{}
	for name, c := range map[string]struct {
		absent  bool
		batch   []Op
		refused bool
	}{
		"inserted, then emptied":          {true, []Op{{Op: "insert", Key: key, Data: writeGuardPlain}, {Op: "set", Key: key, Data: empty}}, true},
		"inserted with no data, then set": {true, []Op{{Op: "insert", Key: key}, {Op: "set", Key: key, Data: empty}}, true},
		"set, then emptied":               {true, []Op{{Op: "set", Key: key, Data: writeGuardPlain}, {Op: "set", Key: key, Data: empty}}, true},
		"deleted, then emptied":           {false, []Op{{Op: "delete", Key: key}, {Op: "set", Key: key, Data: empty}}, false},
		"emptied, then deleted":           {false, []Op{{Op: "set", Key: key, Data: empty}, {Op: "delete", Key: key}}, true},
		"deleted, emptied, emptied again": {false, []Op{{Op: "delete", Key: key}, {Op: "set", Key: key, Data: empty}, {Op: "set", Key: key, Data: empty}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			db, fake := writeGuardOpen(t, "sqlite", "customers")
			fake.absent = c.absent
			_, err := db.Apply(context.Background(), c.batch, "")
			if c.refused != errors.Is(err, ErrEmptyWrite) || !c.refused && err != nil {
				t.Fatalf("%v", err)
			}
			if wantTransactions := map[bool]int{true: 0, false: 1}[c.refused]; fake.transactions != wantTransactions {
				t.Fatalf("transactions = %d, want %d", fake.transactions, wantTransactions)
			}
		})
	}
}

// TestSetOfNoColumnReachesDocumentEngines: a document engine rewrites the
// document, so the rule is for engines that build SQL only.
func TestSetOfNoColumnReachesDocumentEngines(t *testing.T) {
	for _, engine := range writeGuardDocumentEngines {
		t.Run(engine, func(t *testing.T) {
			db, fake := writeGuardOpen(t, engine, "customers")
			op := Op{Op: "set", Key: record.NewKeyWithID("customers", "c1"), Data: map[string]any{}}
			if n, err := db.Apply(context.Background(), []Op{op}, ""); err != nil || n != 1 {
				t.Fatalf("%d %v", n, err)
			}
			if fake.transactions != 1 {
				t.Fatalf("transactions = %d", fake.transactions)
			}
		})
	}
}
