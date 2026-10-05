package core

import (
	"context"
	"slices"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// collectionsListDB is a dal.DB with the schema reader of a driver that reports
// the given collections. Nothing else is implemented: listing the collections
// must read nothing else.
type collectionsListDB struct {
	dal.DB
	reported []string
}

func (f collectionsListDB) ListCollections(context.Context, *record.Key) ([]dal.CollectionRef, error) {
	refs := make([]dal.CollectionRef, 0, len(f.reported))
	for _, name := range f.reported {
		refs = append(refs, dal.NewRootCollectionRef(name, ""))
	}
	return refs, nil
}

func (collectionsListDB) DescribeCollection(context.Context, *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	return nil, nil
}

func (collectionsListDB) ListIndexes(context.Context, *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	return nil, nil
}

func (collectionsListDB) ListConstraints(context.Context, *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	return nil, nil
}

func (collectionsListDB) ListReferrers(context.Context, *dal.CollectionRef) ([]dbschema.Referrer, error) {
	return nil, nil
}

// TestCollectionsListsOnlyDeclaredCanonicalNamesOnSQLEngines: the driver reports
// every table it finds. On an engine that builds SQL the list is made of the
// declared collections, by their canonical names, that the driver reports, so it
// is the list the routes that take a collection accept; a document engine lists
// what its driver reports. PostgreSQL stores a name it is given lower-cased and
// reports it so, and SQLite resolves a name without regard to the case of ASCII
// letters: a declared name is reported when its ASCII lower-cased form is.
func TestCollectionsListsOnlyDeclaredCanonicalNamesOnSQLEngines(t *testing.T) {
	reported := []string{"customers", "Customers", "ghost", "Order Details", `"Order Details"`, `"customers"`}
	for _, c := range []struct {
		name     string
		engine   string
		mode     schema.Mode
		declared []string
		reported []string
		want     []string
	}{
		// The quoted key of a SQLite manifest is declared by its public name, and the
		// table whose name carries the quotes is not that collection.
		{"sqlite", "sqlite", schema.ModeStrict, []string{"customers", `"Order Details"`}, reported, []string{"Order Details", "customers"}},
		// Other SQL engines take a key as it is written.
		{"postgres", "postgres", schema.ModeStrict, []string{"customers", `"Order Details"`}, reported, []string{`"Order Details"`, "customers"}},
		{"mysql", "mysql", schema.ModeStrict, []string{"customers", `"Order Details"`}, reported, []string{`"Order Details"`, "customers"}},
		{"ingitdb", "ingitdb", schema.ModeSchemaless, []string{"customers", `"Order Details"`}, reported, []string{`"Order Details"`, `"customers"`, "Customers", "Order Details", "customers", "ghost"}},
		// PostgreSQL reports the lower-cased form of a declared name that has an
		// upper-case letter; the declared name is listed, and a table that is not
		// declared is not.
		{"postgres, a declared name with an upper-case letter", "postgres", schema.ModeStrict, []string{"Customers", "orders"}, []string{"customers", "ghost"}, []string{"Customers"}},
		{"postgres, the same name declared in two cases", "postgres", schema.ModeStrict, []string{"Customers", "customers"}, []string{"customers"}, []string{"Customers", "customers"}},
		{"postgres, a declared name the driver does not report", "postgres", schema.ModeStrict, []string{"Customers", "orders"}, []string{"orders"}, []string{"orders"}},
		// SQLite resolves a table name without regard to the case of ASCII letters,
		// and only of those.
		{"sqlite, a declared name with an upper-case letter", "sqlite", schema.ModeStrict, []string{"Customers"}, []string{"customers"}, []string{"Customers"}},
		{"sqlite, a declared name reported with another case", "sqlite", schema.ModeStrict, []string{"customers"}, []string{"CUSTOMERS"}, []string{"customers"}},
		{"sqlite, a declared name the driver does not report", "sqlite", schema.ModeStrict, []string{"Customers", "orders"}, []string{"orders"}, []string{"orders"}},
		{"sqlite, a non-ASCII letter is not folded", "sqlite", schema.ModeStrict, []string{`"Éclair"`}, []string{"éclair"}, nil},
		{"sqlite, a non-ASCII letter reported as declared", "sqlite", schema.ModeStrict, []string{`"Éclair"`}, []string{"Éclair"}, []string{"Éclair"}},
		// MySQL matches the reported name exactly: whether it folds depends on the
		// server's lower_case_table_names.
		{"mysql, a declared name with an upper-case letter", "mysql", schema.ModeStrict, []string{"Customers"}, []string{"customers"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			collections := map[string]schema.Collection{}
			for _, name := range c.declared {
				collections[name] = schema.Collection{Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}
			}
			m := &manifest.Manifest{
				Database: manifest.Database{ID: "listed", SchemaMode: c.mode},
				Storage:  manifest.Storage{Engine: c.engine},
				Schemas:  &schema.Schemas{Collections: collections},
			}
			db, err := Open(m, collectionsListDB{reported: c.reported}, []schema.Mode{c.mode}, t.TempDir()+"/inferred.json")
			if err != nil {
				t.Fatal(err)
			}
			got, err := db.Collections(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
