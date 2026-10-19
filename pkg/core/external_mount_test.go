package core

import (
	"context"
	"errors"
	"testing"

	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo/ddl"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type externalProvisionProbe struct {
	collectionsListDB
	creates int
}

func (p *externalProvisionProbe) CreateCollection(context.Context, dbschema.CollectionDef, ...ddl.Option) error {
	p.creates++
	return nil
}
func (p *externalProvisionProbe) DropCollection(context.Context, string, ...ddl.Option) error {
	panic("read-only mount attempted DropCollection")
}
func (p *externalProvisionProbe) AlterCollection(context.Context, string, ...ddl.AlterOp) error {
	panic("read-only mount attempted AlterCollection")
}

func TestExternalDALGOMountIsProgrammaticAndReadOnly(t *testing.T) {
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "evidence", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "external"},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"entries": {Fields: map[string]schema.Field{"message": {Type: schema.TypeString}}},
		}},
	}
	probe := &externalProvisionProbe{collectionsListDB: collectionsListDB{reported: []string{"entries", "unlisted"}}}
	d, err := OpenExternal(m, probe)
	if err != nil {
		t.Fatal(err)
	}
	if !d.ReadOnly() || !d.CanCollectionQuery() {
		t.Fatalf("external mount capabilities: readonly=%v query=%v", d.ReadOnly(), d.CanCollectionQuery())
	}
	if probe.creates != 0 {
		t.Fatalf("read-only external mount provisioned %d collections", probe.creates)
	}
	key := record.NewKeyWithID("entries", "one")
	if _, err := d.Get(context.Background(), key); !errors.Is(err, ErrExternalOperationUnsupported) {
		t.Fatalf("keyed read: %v", err)
	}
	if _, err := d.Exists(context.Background(), key); !errors.Is(err, ErrExternalOperationUnsupported) {
		t.Fatalf("existence read: %v", err)
	}
	if _, err := d.Apply(context.Background(), nil, ""); !errors.Is(err, ErrExternalOperationUnsupported) {
		t.Fatalf("write: %v", err)
	}
	names, err := d.Collections(context.Background())
	if err != nil || len(names) != 1 || names[0] != "entries" {
		t.Fatalf("collections = %v, %v", names, err)
	}
	if _, err := OpenExternal(&manifest.Manifest{Storage: manifest.Storage{Engine: "sqlite"}}, collectionsListDB{}); err == nil {
		t.Fatal("accepted non-external mount")
	}
	invalid := *m
	invalid.Database.ID = "../unsafe"
	if _, err := OpenExternal(&invalid, collectionsListDB{}); err == nil {
		t.Fatal("accepted invalid database ID")
	}
}
