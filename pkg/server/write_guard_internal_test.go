package server

import (
	"errors"
	"testing"

	"github.com/dal-go/dalgo/dal"
	az "github.com/dal-go/dalgo/dtql/authorization"

	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// TestGuardOperation covers the pure refusal rules. guardOperation does not
// depend on Operation.Normalize having mirrored an update's change paths into
// the resource's columns: the change paths are checked on their own, so a
// future change to Normalize cannot reopen the hole.
func TestGuardOperation(t *testing.T) {
	declared := func(engine string) *core.Database {
		t.Helper()
		m := &manifest.Manifest{
			Database: manifest.Database{ID: "crm", SchemaMode: schema.ModeStrict},
			Storage:  manifest.Storage{Engine: engine},
			Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
		}
		db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	resource := func(table string, columns ...[]string) az.Resource {
		return az.Resource{DatabaseID: "crm", Table: table, RowID: "01", Columns: columns}
	}
	set := func(path ...string) *api.Mutation {
		return &api.Mutation{Changes: []api.Change{{Op: "set", Path: path}}}
	}
	for _, c := range []struct {
		name   string
		engine string
		op     api.Operation
		want   error
	}{
		{"declared table, plain names", "sqlite", api.Operation{Resource: resource("customers", []string{"name"}), Mutation: set("name")}, nil},
		{"declared table, no mutation", "sqlite", api.Operation{Resource: resource("customers")}, nil},
		{"insert data, plain names", "sqlite", api.Operation{Resource: resource("customers"), Mutation: &api.Mutation{Data: map[string]any{"name": "Ada"}}}, nil},
		{"undeclared table on SQL", "sqlite", api.Operation{Resource: resource("ghost")}, core.ErrNotFound},
		{"undeclared table on a document engine", "ingitdb", api.Operation{Resource: resource("ghost")}, nil},
		{"hostile column", "ingitdb", api.Operation{Resource: resource("customers", []string{"name"}, []string{`a"b`})}, core.ErrInvalidFieldName},
		{"hostile data key", "ingitdb", api.Operation{Resource: resource("customers"), Mutation: &api.Mutation{Data: map[string]any{"a;b": 1}}}, core.ErrInvalidFieldName},
		{"hostile first change segment, columns not mirrored", "ingitdb", api.Operation{Resource: resource("customers"), Mutation: set("a b", "x")}, core.ErrInvalidFieldName},
		{"hostile later change segment on SQL, columns not mirrored", "sqlite", api.Operation{Resource: resource("customers"), Mutation: set("name", "a b")}, core.ErrInvalidFieldName},
		{"map key as later change segment on a document engine", "ingitdb", api.Operation{Resource: resource("customers"), Mutation: set("related", "c1@space2")}, nil},
		{"map key as later column segment on a document engine", "ingitdb", api.Operation{Resource: resource("customers", []string{"related", "c1@space2"})}, nil},
		{"map key as later column segment on SQL", "sqlite", api.Operation{Resource: resource("customers", []string{"related", "c1@space2"})}, core.ErrInvalidFieldName},
		{"blank later change segment on a document engine", "ingitdb", api.Operation{Resource: resource("customers"), Mutation: set("related", " ")}, core.ErrInvalidFieldName},
		{"empty change path", "ingitdb", api.Operation{Resource: resource("customers"), Mutation: set()}, core.ErrInvalidFieldName},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := guardOperation(declared(c.engine), c.op)
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// guardOperationDB is a dal.DB with no behaviour: guardOperation never reaches
// the adapter, and every method panics (nil embedded DB) if it ever did.
type guardOperationDB struct{ dal.DB }
