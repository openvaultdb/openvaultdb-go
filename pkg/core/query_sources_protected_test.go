package core

import (
	"context"
	"errors"
	"testing"

	"github.com/dal-go/dalgo/access"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// protSrcOpen opens a fake-backed database with one allowing access policy that
// declares exactly the given collections.
func protSrcOpen(t *testing.T, engine string, declared ...string) (*Database, *joinSrcRecorder) {
	t.Helper()
	calls := &joinSrcRecorder{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "protsrc", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	for _, name := range declared {
		m.Schemas.Collections[name] = schema.Collection{Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}
	}
	db, err := Open(m, joinSrcDB{calls: calls, tx: joinSrcTx{calls: calls}}, []schema.Mode{schema.ModeStrict}, "", joinSrcAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !db.HasAccessPolicies() {
		t.Fatal("the database must have access policies")
	}
	return db, calls
}

// TestProtectedDatabaseRefusesANestedSourceWhateverTheNames: on a database with
// access policies a structured query that reads more than its root collection
// (a subquery of any kind) is refused with ErrProtectedSingleSource before the
// driver is reached. The refusal is decided by the shape of the query alone, so
// the answer is the same whether the collections it names are declared or not.
func TestProtectedDatabaseRefusesANestedSourceWhateverTheNames(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb"} {
		for _, tc := range srcGuardCases("ghost") {
			if !tc.single || tc.name == "root" {
				continue
			}
			for entry, run := range srcGuardSingleEntries {
				t.Run(engine+"/"+tc.name+"/"+entry, func(t *testing.T) {
					var answers []string
					for _, declared := range [][]string{{"customers", "orders"}, {"customers", "orders", "ghost"}} {
						db, calls := protSrcOpen(t, engine, declared...)
						err := run(db, tc.query)
						if !errors.Is(err, ErrProtectedSingleSource) {
							t.Fatalf("declared %v: got %v, want ErrProtectedSingleSource", declared, err)
						}
						if *calls != (joinSrcRecorder{}) {
							t.Fatalf("declared %v: the driver was reached: %+v", declared, *calls)
						}
						answers = append(answers, err.Error())
					}
					if answers[0] != answers[1] {
						t.Errorf("undeclared answered %q, declared answered %q", answers[0], answers[1])
					}
				})
			}
		}
	}
}

// TestProtectedDatabaseStillReadsARootCollection is the control of the test
// above: a query of one plain collection is not refused for its shape.
func TestProtectedDatabaseStillReadsARootCollection(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []string{"sqlite", "ingitdb"} {
		db, calls := protSrcOpen(t, engine, "customers")
		query := srcGuardInner("customers")
		if _, err := db.ExecuteDTQLQuery(ctx, query); !errors.Is(err, errJoinSrcReader) {
			t.Errorf("%s ExecuteDTQLQuery: %v", engine, err)
		}
		if err := db.StreamDTQLSnapshot(ctx, query, func(Record) error { return nil }); !errors.Is(err, errJoinSrcReader) {
			t.Errorf("%s StreamDTQLSnapshot: %v", engine, err)
		}
		if calls.readers != 2 {
			t.Errorf("%s: driver reads = %d, want 2", engine, calls.readers)
		}
		if _, _, err := db.SelectAccessSample(ctx, query, 1, access.Principal{}); errors.Is(err, ErrProtectedSingleSource) {
			t.Errorf("%s SelectAccessSample: %v", engine, err)
		}
	}
}
