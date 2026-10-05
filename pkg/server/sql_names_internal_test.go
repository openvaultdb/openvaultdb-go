package server

import (
	"errors"
	"testing"

	az "github.com/dal-go/dalgo/dtql/authorization"

	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// TestGuardOperationDeclaresTheCanonicalNameOfACollection: guardOperation asks
// the database which collections it declares and takes the canonical name only.
// The public name of a SQLite manifest key that is a quoted SQL identifier is a
// declared table; the quoted spelling, which names the table whose name carries
// the quote characters, and a name that is neither are not.
func TestGuardOperationDeclaresTheCanonicalNameOfACollection(t *testing.T) {
	open := func(engine string) *core.Database {
		t.Helper()
		m := &manifest.Manifest{
			Database: manifest.Database{ID: "crm", SchemaMode: schema.ModeStrict},
			Storage:  manifest.Storage{Engine: engine},
			Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
				`"Order Details"`: {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
			}},
		}
		db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	operation := func(table string) api.Operation {
		return api.Operation{Resource: az.Resource{DatabaseID: "crm", Table: table, RowID: "1"}}
	}
	for _, c := range []struct {
		engine, table string
		want          error
	}{
		{"sqlite", "Order Details", nil},
		{"sqlite", `"Order Details"`, core.ErrNotFound},
		{"sqlite", "Order", core.ErrNotFound},
		{"sqlite", `Order Details"`, core.ErrNotFound},
		{"postgres", `"Order Details"`, nil},
		{"postgres", "Order Details", core.ErrNotFound},
	} {
		err := guardOperation(open(c.engine), operation(c.table))
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: table %q: got %v, want %v", c.engine, c.table, err, c.want)
		}
	}
}
