package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// A mount whose driver supplies no fields of its own gives the join engine the
// fields its manifest declares, when the manifest declares all of them (a strict
// database) and the database has no access policies. These tests hold that rule
// to its edges with a fake driver that has no schema.

// declaredFieldsSchemas declares "people" (the key column id is not a field) and
// "ledger" (id is a declared field).
func declaredFieldsSchemas() *schema.Schemas {
	return &schema.Schemas{Collections: map[string]schema.Collection{
		"people": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}, "age": {Type: schema.TypeInteger}}},
		"ledger": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}, "zone": {Type: schema.TypeString}, "amount": {Type: schema.TypeNumber}}},
	}}
}

// declaredFieldsOpen mounts the schemas of declaredFieldsSchemas on a fake driver
// that supplies no fields, in the given schema mode and with the given policies.
func declaredFieldsOpen(t *testing.T, engine string, mode schema.Mode, policies ...access.Policy) (*Database, *joinSrcRecorder) {
	t.Helper()
	calls := &joinSrcRecorder{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "declared", SchemaMode: mode},
		Storage:  manifest.Storage{Engine: engine},
		Schemas:  declaredFieldsSchemas(),
	}
	db, err := Open(m, joinSrcDB{calls: calls}, []schema.Mode{schema.ModeStrict, schema.ModePartial, schema.ModeSchemaless}, "", policies...)
	if err != nil {
		t.Fatal(err)
	}
	return db, calls
}

func declaredJoinFields(t *testing.T, db *Database, source dal.RecordsetSource) ([]string, error) {
	t.Helper()
	return db.Executor().(dal.JoinFieldsProvider).JoinFields(context.Background(), source)
}

func TestAStrictDatabaseSuppliesTheFieldsItDeclares(t *testing.T) {
	for name, tc := range map[string]struct {
		engine     string
		collection string
		want       []string
	}{
		// A table of a SQL engine has the key column first, as the mount provisions it,
		// and then the declared fields by name.
		"a SQL engine, the key column is not declared":        {"sqlite", "people", []string{"id", "age", "name"}},
		"a SQL engine, the key column is declared":            {"sqlite", "ledger", []string{"id", "amount", "zone"}},
		"a document engine holds only the declared fields":    {"ingitdb", "people", []string{"age", "name"}},
		"a document engine with a declared field named id":    {"ingitdb", "ledger", []string{"amount", "id", "zone"}},
		"another document engine holds only the declared set": {"firestore", "people", []string{"age", "name"}},
	} {
		t.Run(name, func(t *testing.T) {
			db, calls := declaredFieldsOpen(t, tc.engine, schema.ModeStrict)
			got, err := declaredJoinFields(t, db, dal.NewRootCollectionRef(tc.collection, "x"))
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
			if calls.fields != 0 || calls.readers != 0 {
				t.Fatalf("the driver was asked: %+v", *calls)
			}
		})
	}
}

func TestADatabaseThatDoesNotDeclareAllItsFieldsSuppliesNone(t *testing.T) {
	for _, mode := range []schema.Mode{schema.ModePartial, schema.ModeSchemaless} {
		t.Run(string(mode), func(t *testing.T) {
			db, _ := declaredFieldsOpen(t, "ingitdb", mode)
			got, err := declaredJoinFields(t, db, dal.NewRootCollectionRef("people", ""))
			if got != nil || err != nil {
				t.Fatalf("got %v, %v; want no fields: a record may hold a field the manifest does not declare", got, err)
			}
		})
	}
}

func TestADatabaseWithAccessPoliciesSuppliesNoFields(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb"} {
		t.Run(engine, func(t *testing.T) {
			db, calls := declaredFieldsOpen(t, engine, schema.ModeStrict, joinSrcAllowPolicy{})
			if !db.HasAccessPolicies() {
				t.Fatal("the database must have access policies")
			}
			// A declared collection and one the manifest does not declare get the same answer,
			// on a SQL engine too, where an undeclared source of a database without policies is
			// refused: the policy decides what a caller learns of the schema, and nothing of it
			// comes before that.
			for name, collection := range map[string]string{"a declared collection": "people", "an undeclared collection": "ghost"} {
				got, err := declaredJoinFields(t, db, dal.NewRootCollectionRef(collection, ""))
				if got != nil || err != nil {
					t.Fatalf("%s: got %v, %v; want no schema of a protected database", name, got, err)
				}
			}
			if *calls != (joinSrcRecorder{}) {
				t.Fatalf("the driver was reached: %+v", *calls)
			}
		})
	}
}

func TestADocumentEngineSuppliesNoFieldsForASourceThatIsNotADeclaredRootCollection(t *testing.T) {
	db, _ := declaredFieldsOpen(t, "ingitdb", schema.ModeStrict)
	plain := dal.NewRootCollectionRef("people", "")
	if got, err := declaredJoinFields(t, db, plain); err != nil || len(got) == 0 {
		t.Fatalf("the control: %v, %v", got, err)
	}
	// The source guard does not walk a document engine, so these reach the rule.
	for name, source := range map[string]dal.RecordsetSource{
		"a collection the manifest does not declare": dal.NewRootCollectionRef("ghost", ""),
		"a derived source":                           dal.NewQuerySource(dal.From(plain).NewQuery().SelectIntoRecord(nil), "d"),
		"a source qualified by a schema":             dal.NewDatabaseCollectionRef("declared", "other", "people", ""),
		"a source qualified by a parent record":      dal.NewCollectionRef("people", "", record.NewKeyWithID("parents", "p1")),
		"a source of another database":               dal.NewDatabaseCollectionRef("elsewhere", "", "people", ""),
		"a collection group":                         dal.NewCollectionGroupRef("people", ""),
		"a pointer to a collection reference":        &plain,
	} {
		if got, err := declaredJoinFields(t, db, source); got != nil || err != nil {
			t.Errorf("%s: %v, %v; want no fields", name, got, err)
		}
	}
	// A source that names the database it is read from is the same source.
	if got, err := declaredJoinFields(t, db, dal.NewDatabaseCollectionRef("declared", "", "people", "")); err != nil || !reflect.DeepEqual(got, []string{"age", "name"}) {
		t.Errorf("a source that names its own database: %v, %v", got, err)
	}
}

func TestASQLDatabaseRefusesAnUndeclaredSourceBeforeTheDeclaredFieldsAreLooked(t *testing.T) {
	db, _ := declaredFieldsOpen(t, "sqlite", schema.ModeStrict)
	if got, err := declaredJoinFields(t, db, dal.NewRootCollectionRef("ghost", "")); !errors.Is(err, ErrNotFound) || got != nil {
		t.Fatalf("got %v, %v; want ErrNotFound", got, err)
	}
}

func TestACollectionThatDeclaresNoFieldSuppliesNone(t *testing.T) {
	// Validate refuses such a manifest; a database built by hand is not validated.
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "declared", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "ingitdb"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"empty": {}}},
	}
	db, err := Open(m, joinSrcDB{calls: &joinSrcRecorder{}}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := declaredJoinFields(t, db, dal.NewRootCollectionRef("empty", "")); got != nil || err != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestACollectionKeyedByAQuotedIdentifierSuppliesTheFieldsItDeclaresUnderItsCanonicalName(t *testing.T) {
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "declared", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			`"Order Details"`: {Fields: map[string]schema.Field{"qty": {Type: schema.TypeInteger}, "note": {Type: schema.TypeString}}},
		}},
	}
	db, err := Open(m, joinSrcDB{calls: &joinSrcRecorder{}}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := declaredJoinFields(t, db, dal.NewRootCollectionRef("Order Details", ""))
	if err != nil || !reflect.DeepEqual(got, []string{"id", "note", "qty"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// A driver that supplies fields keeps its answer: the manifest is the source only
// for a driver that has none, and a driver's error is its own.
func TestADriverThatSuppliesFieldsIsNotOverriddenByTheManifest(t *testing.T) {
	calls := &joinSrcRecorder{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "declared", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  declaredFieldsSchemas(),
	}
	db, err := Open(m, joinSrcFieldsDB{joinSrcDB{calls: calls}}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := declaredJoinFields(t, db, dal.NewRootCollectionRef("people", ""))
	if !errors.Is(err, errJoinSrcFields) || !reflect.DeepEqual(got, []string{"id"}) || calls.fields != 1 {
		t.Fatalf("got %v, %v, driver calls %+v; want the driver's answer", got, err, *calls)
	}
}

// A record of a collection that declares an object, or a field of any type, holds a
// value the manifest does not list the insides of, and DALgo reads a field by walking
// its dotted path while it compares the name with the list by equality. A list of the
// top-level names would refuse a path such as address.city, which a read has always
// answered, so such a collection supplies none, as a partial database does: a path is
// read as it is written, and what a list is needed for (a wildcard, an unqualified
// field of a query of several sources) is refused instead of guessed.
func TestACollectionThatDeclaresAnObjectOrAnyFieldSuppliesNone(t *testing.T) {
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "declared", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "ingitdb"},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"flat":    {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}, "tags": {Type: schema.TypeArray}}},
			"nested":  {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}, "address": {Type: schema.TypeObject}}},
			"untyped": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}, "extra": {Type: schema.TypeAny}}},
		}},
	}
	db, err := Open(m, joinSrcDB{calls: &joinSrcRecorder{}}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	// The control: an array is a value, not a path to walk, so its collection keeps its list.
	if got, err := declaredJoinFields(t, db, dal.NewRootCollectionRef("flat", "")); err != nil || !reflect.DeepEqual(got, []string{"name", "tags"}) {
		t.Fatalf("the control: %v, %v", got, err)
	}
	for _, collection := range []string{"nested", "untyped"} {
		if got, err := declaredJoinFields(t, db, dal.NewRootCollectionRef(collection, "")); got != nil || err != nil {
			t.Errorf("%s: %v, %v; want no fields: a path inside the field is read", collection, got, err)
		}
	}
}
