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
// every table it finds. On an engine that builds SQL a name is listed only when
// the database declares it under exactly that name, so the list is the one the
// routes that take a collection accept; a document engine lists what its driver
// reports.
func TestCollectionsListsOnlyDeclaredCanonicalNamesOnSQLEngines(t *testing.T) {
	reported := []string{"customers", "Customers", "ghost", "Order Details", `"Order Details"`, `"customers"`}
	for _, c := range []struct {
		engine string
		mode   schema.Mode
		want   []string
	}{
		// The quoted key of a SQLite manifest is declared by its public name, and the
		// table whose name carries the quotes is not that collection.
		{"sqlite", schema.ModeStrict, []string{"Order Details", "customers"}},
		// Other SQL engines take a key as it is written.
		{"postgres", schema.ModeStrict, []string{`"Order Details"`, "customers"}},
		{"mysql", schema.ModeStrict, []string{`"Order Details"`, "customers"}},
		{"ingitdb", schema.ModeSchemaless, []string{`"Order Details"`, `"customers"`, "Customers", "Order Details", "customers", "ghost"}},
	} {
		t.Run(c.engine, func(t *testing.T) {
			m := &manifest.Manifest{
				Database: manifest.Database{ID: "listed", SchemaMode: c.mode},
				Storage:  manifest.Storage{Engine: c.engine},
				Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
					"customers":       {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
					`"Order Details"`: {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
				}},
			}
			db, err := Open(m, collectionsListDB{reported: reported}, []schema.Mode{c.mode}, t.TempDir()+"/inferred.json")
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
