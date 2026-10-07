package mount

import (
	"context"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type nativeCatalogFixture struct{}

func (nativeCatalogFixture) ListSchemas(context.Context) ([]string, error) {
	return []string{"sales data", "public"}, nil
}

func (nativeCatalogFixture) ListSchemaCollections(_ context.Context, schemaName string) ([]dal.CollectionRef, error) {
	if schemaName == "public" {
		return []dal.CollectionRef{dal.NewQualifiedRootCollectionRef("sales data", "ignored", "")}, nil
	}
	return []dal.CollectionRef{dal.NewQualifiedRootCollectionRef("sales data", "Order Details", "")}, nil
}

func (nativeCatalogFixture) DescribeCollection(_ context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	if ref.Schema() != "sales data" || ref.Name() != "Order Details" {
		return nil, nil
	}
	precision := &dbschema.Precision{Total: 18, Scale: 4}
	return &dbschema.CollectionDef{
		Name:       ref.Name(),
		Fields:     []dbschema.FieldDef{{Name: "Order ID", Type: dbschema.Int}, {Name: "Amount", Type: dbschema.Decimal, Precision: precision}, {Name: "Created At", Type: dbschema.Time, Nullable: true}},
		PrimaryKey: []dal.FieldName{"Order ID"},
		SourceDefinition: &dbschema.SourceDefinition{Dialect: "postgres", Columns: []dbschema.SourceColumnDef{
			{Name: "Order ID", DeclaredType: "bigint", NotNull: true, PrimaryKeyPosition: 1},
			{Name: "Amount", DeclaredType: "numeric(18,4)"},
			{Name: "Created At", DeclaredType: "timestamp without time zone"},
		}},
	}, nil
}

func TestDiscoverNativePostgresSchemasPreservesPhysicalNamesAndTypes(t *testing.T) {
	got, err := discoverNativePostgresSchemas(context.Background(), nativeCatalogFixture{})
	if err != nil {
		t.Fatal(err)
	}
	id, err := schema.NativePostgresCollectionID("sales data", "Order Details")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Collections) != 1 {
		t.Fatalf("collections = %d, want one", len(got.Collections))
	}
	collection := got.Collections[id]
	if collection.Source == nil || collection.Source.Schema != "sales data" || collection.Source.Name != "Order Details" {
		t.Fatalf("source = %+v", collection.Source)
	}
	if collection.Fields["Order ID"].Type != schema.TypeInteger || !collection.Fields["Order ID"].PrimaryKey {
		t.Errorf("primary key metadata = %+v", collection.Fields["Order ID"])
	}
	amount := collection.Fields["Amount"]
	if amount.Type != schema.TypeDecimal || amount.NativeType != "numeric(18,4)" || amount.Decimal == nil || *amount.Decimal != (schema.Decimal{Precision: 18, Scale: 4, Storage: "text"}) {
		t.Errorf("decimal metadata = %+v", amount)
	}
	if created := collection.Fields["Created At"]; created.Type != schema.TypeString || created.NativeType != "timestamp without time zone" || !created.Nullable {
		t.Errorf("temporal metadata = %+v", created)
	}
}

func TestNativePostgresMountUsesExactIdentifiersAndExactNumericValues(t *testing.T) {
	t.Setenv("TEST_NATIVE_PG_DSN", "postgres://readonly:secret@db.example.test:5432/samples")
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "samples"},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{DSNEnv: "TEST_NATIVE_PG_DSN", ReadOnly: true}},
	}
	var called bool
	_, _, err := openPostgresWith(m, func(dsn string, _ dal.Schema, options dalgo2sql.DbOptions, postgresOptions ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
		called = true
		if dsn != "postgres://readonly:secret@db.example.test:5432/samples" {
			t.Errorf("DSN changed: %q", dsn)
		}
		if options.IdentifierCase != dalgo2sql.IdentifierCaseExact || !options.ExactNumericValues {
			t.Errorf("DbOptions = %+v, want exact identifiers and exact numerics", options)
		}
		if len(options.Recordsets) != 0 {
			t.Errorf("Recordsets = %v, want no provisioned collections", options.Recordsets)
		}
		if len(postgresOptions) != 1 {
			t.Errorf("PostgreSQL options = %d, want explicit exact identifier mode", len(postgresOptions))
		}
		return nil, errStopOpening
	})
	if !called || err == nil {
		t.Fatalf("called %v, err %v; want attempted open and injected refusal", called, err)
	}
}

func TestNativePostgresDiscoveryUsesStableLogicalID(t *testing.T) {
	schemas, err := discoverNativePostgresSchemas(context.Background(), nativeCatalogFixture{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := schema.NativePostgresCollectionID("sales data", "Order Details")
	if !reflect.DeepEqual(schemas.Collections[want].Source, &schema.NativeCollectionSource{Schema: "sales data", Name: "Order Details"}) {
		t.Fatalf("source = %+v", schemas.Collections[want].Source)
	}
}
