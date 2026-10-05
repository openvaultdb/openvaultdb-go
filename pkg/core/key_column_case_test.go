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

// keyColumnCases are the spellings of the key column that are not "id" itself.
var keyColumnCases = []string{"ID", "Id", "iD"}

// TestWriteDataNamingTheKeyColumnInAnotherCaseIsRefusedBeforeTheAdapter: every
// spelling of the key column is the one column on sqlite, postgres and mysql, and
// the adapter skips only "id" when it lists the columns to write. A write whose
// data names the key column in another case is refused whole with
// ErrKeyColumnCase (a bad request, an ErrInvalidFieldName), before any adapter
// call, whatever else the batch holds; "id" itself is left to the adapter, which
// skips it.
func TestWriteDataNamingTheKeyColumnInAnotherCaseIsRefusedBeforeTheAdapter(t *testing.T) {
	key := record.NewKeyWithID("customers", "c1")
	good := record.NewKeyWithID("customers", "good")
	for _, engine := range writeGuardSQLEngines {
		for _, name := range keyColumnCases {
			t.Run(engine+"/"+name, func(t *testing.T) {
				db, fake := writeGuardOpen(t, engine, "customers")
				data := map[string]any{"name": "Ada", name: "c2"}
				for label, batch := range map[string][]Op{
					"set":             {{Op: "set", Key: key, Data: data}},
					"insert":          {{Op: "insert", Key: key, Data: data}},
					"after a good op": {{Op: "set", Key: good, Data: writeGuardPlain}, {Op: "insert", Key: key, Data: data}},
				} {
					_, err := db.Apply(context.Background(), batch, "")
					if !errors.Is(err, ErrKeyColumnCase) || !errors.Is(err, ErrInvalidFieldName) {
						t.Errorf("%s: got %v, want ErrKeyColumnCase", label, err)
					}
				}
				if fake.reached() != 0 {
					t.Fatalf("adapter reached %d times", fake.reached())
				}
				if err := db.ValidateDataKeys(data); !errors.Is(err, ErrKeyColumnCase) {
					t.Errorf("ValidateDataKeys: got %v", err)
				}
			})
		}
	}
}

// TestWriteDataNamingTheKeyColumnItselfOrADocumentFieldIsNotRefusedByTheCaseRule:
// "id" is not a spelling in another case, and a document engine keeps the key
// outside the record's fields.
func TestWriteDataNamingTheKeyColumnItselfOrADocumentFieldIsNotRefusedByTheCaseRule(t *testing.T) {
	for _, engine := range writeGuardSQLEngines {
		db, _ := writeGuardOpen(t, engine, "customers")
		for _, data := range []map[string]any{{"id": "c1"}, {"name": "Ada"}, {"identity": 1}, {"ID2": 1}, nil} {
			if err := db.ValidateDataKeys(data); err != nil {
				t.Errorf("%s %v: %v", engine, data, err)
			}
		}
	}
	for _, engine := range writeGuardDocumentEngines {
		db, _ := writeGuardOpen(t, engine, "customers")
		for _, name := range keyColumnCases {
			if err := db.ValidateDataKeys(map[string]any{name: "c2"}); err != nil {
				t.Errorf("%s %s: %v", engine, name, err)
			}
		}
	}
	// A name that is not plain is refused whatever the engine.
	db, _ := writeGuardOpen(t, "sqlite", "customers")
	if err := db.ValidateDataKeys(map[string]any{`a"b`: 1}); !errors.Is(err, ErrInvalidFieldName) || errors.Is(err, ErrKeyColumnCase) {
		t.Errorf("a name that is not plain: got %v", err)
	}
}

// TestManifestDeclaringTheKeyColumnInAnotherCaseDoesNotOpenOnSQLEngines: the key
// column of every collection a SQL mount declares is "id", and a field declared
// as another spelling of it would be the same column. Such a manifest is refused
// when it opens (ErrFieldNamesConflict), before the driver is asked to provision
// anything, on sqlite, postgres and mysql; a document engine keeps the key
// outside the fields and opens it, and a field named "id" opens everywhere.
func TestManifestDeclaringTheKeyColumnInAnotherCaseDoesNotOpenOnSQLEngines(t *testing.T) {
	open := func(engine, field string) (*writeGuardDB, error) {
		fake := &writeGuardDB{}
		mode := schema.ModeStrict
		if documentEngines[engine] {
			mode = schema.ModeSchemaless
		}
		m := &manifest.Manifest{
			Database: manifest.Database{ID: "guarded", SchemaMode: mode},
			Storage:  manifest.Storage{Engine: engine},
			Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
				"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}, field: {Type: schema.TypeString}}},
			}},
		}
		_, err := Open(m, fake, []schema.Mode{mode}, filepath.Join(t.TempDir(), "inferred.json"))
		return fake, err
	}
	for _, engine := range writeGuardSQLEngines {
		for _, field := range keyColumnCases {
			fake, err := open(engine, field)
			if !errors.Is(err, ErrFieldNamesConflict) {
				t.Errorf("%s, field %s: got %v, want ErrFieldNamesConflict", engine, field, err)
			}
			if fake.reached() != 0 {
				t.Errorf("%s, field %s: adapter reached %d times", engine, field, fake.reached())
			}
		}
		if _, err := open(engine, "id"); err != nil {
			t.Errorf("%s, field id: %v", engine, err)
		}
		if _, err := open(engine, "identity"); err != nil {
			t.Errorf("%s, field identity: %v", engine, err)
		}
	}
	for _, engine := range writeGuardDocumentEngines {
		for _, field := range keyColumnCases {
			if _, err := open(engine, field); err != nil {
				t.Errorf("%s, field %s: %v", engine, field, err)
			}
		}
	}
}

// TestSetsNoColumnTakesEverySpellingOfTheKeyColumnForTheKey: data that holds the
// key column in any case and no other field leaves a SQL adapter nothing to
// update, as data that holds "id" does.
func TestSetsNoColumnTakesEverySpellingOfTheKeyColumnForTheKey(t *testing.T) {
	sql, _ := writeGuardOpen(t, "sqlite", "customers")
	doc, _ := writeGuardOpen(t, "ingitdb", "customers")
	for _, c := range []struct {
		name string
		db   *Database
		data map[string]any
		want bool
	}{
		{"no data", sql, nil, true},
		{"id", sql, map[string]any{"id": "c1"}, true},
		{"ID", sql, map[string]any{"ID": "c1"}, true},
		{"id and ID", sql, map[string]any{"id": "c1", "ID": "c1"}, true},
		{"a field", sql, map[string]any{"name": "Ada"}, false},
		{"a field beside ID", sql, map[string]any{"ID": "c1", "name": "Ada"}, false},
		{"a document engine", doc, map[string]any{"id": "c1"}, false},
	} {
		if got := c.db.setsNoColumn(c.data); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
