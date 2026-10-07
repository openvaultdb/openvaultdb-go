package mount

import (
	"context"
	"net/url"
	"reflect"
	"strings"
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
	return []dal.CollectionRef{
		dal.NewQualifiedRootCollectionRef("sales data", "Order Details", ""),
		dal.NewQualifiedRootCollectionRef("sales data", "Insert Only", ""),
	}, nil
}

func (nativeCatalogFixture) SelectableRelations(_ context.Context, schemaName string) (map[string]bool, error) {
	if schemaName == "public" {
		return map[string]bool{"ignored": false}, nil
	}
	return map[string]bool{"Order Details": true, "Insert Only": false}, nil
}

func (nativeCatalogFixture) DescribeCollection(_ context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	if ref.Schema() != "sales data" || ref.Name() != "Order Details" {
		return nil, nil
	}
	precision := &dbschema.Precision{Total: 18, Scale: 4}
	return &dbschema.CollectionDef{
		Name:       ref.Name(),
		Fields:     []dbschema.FieldDef{{Name: "Order ID", Type: dbschema.Int}, {Name: "Amount", Type: dbschema.Decimal, Precision: precision}, {Name: "Unbounded Amount", Type: dbschema.Decimal}, {Name: "Created At", Type: dbschema.Time, Nullable: true}},
		PrimaryKey: []dal.FieldName{"Order ID"},
		SourceDefinition: &dbschema.SourceDefinition{Dialect: "postgres", Columns: []dbschema.SourceColumnDef{
			{Name: "Order ID", DeclaredType: "bigint", NotNull: true, PrimaryKeyPosition: 1},
			{Name: "Amount", DeclaredType: "numeric(18,4)"},
			{Name: "Unbounded Amount", DeclaredType: "numeric"},
			{Name: "Created At", DeclaredType: "timestamp without time zone"},
		}},
	}, nil
}

func TestDiscoverNativePostgresSchemasPreservesPhysicalNamesAndTypes(t *testing.T) {
	got, err := discoverNativePostgresSchemas(context.Background(), nativeCatalogFixture{}, nativeCatalogFixture{})
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
	insertOnlyID, err := schema.NativePostgresCollectionID("sales data", "Insert Only")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := got.Collections[insertOnlyID]; exists {
		t.Fatal("relation without SELECT privilege was exposed as a queryable collection")
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
	unbounded := collection.Fields["Unbounded Amount"]
	if unbounded.Type != schema.TypeDecimal || unbounded.NativeType != "numeric" || unbounded.Decimal == nil || *unbounded.Decimal != (schema.Decimal{Unbounded: true, Storage: "text"}) {
		t.Errorf("unbounded decimal metadata = %+v", unbounded)
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
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil || parsed.Query().Get("default_transaction_read_only") != "on" {
			t.Errorf("DSN = %q, want enforced read-only session parameter (parse error %v)", dsn, parseErr)
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

func TestPostgresReadOnlyDSNEnforcesSettingForURLAndKeywordForms(t *testing.T) {
	for _, tc := range []struct{ name, dsn string }{
		{"url", "postgres://reader:p%40ss@db.example.test/samples?sslmode=require&default_transaction_read_only=off"},
		{"keyword", "host=db.example.test user=reader password='p ss' dbname=samples default_transaction_read_only=off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := postgresReadOnlyDSN(tc.dsn)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(tc.name, "url") {
				parsed, err := url.Parse(got)
				if err != nil || parsed.Query().Get("default_transaction_read_only") != "on" {
					t.Fatalf("dsn = %q, parse error %v", got, err)
				}
				if parsed.User.Username() != "reader" || parsed.Query().Get("sslmode") != "require" {
					t.Fatalf("other URL settings changed: %q", got)
				}
			} else if !strings.HasSuffix(got, "default_transaction_read_only='on'") || !strings.Contains(got, "password='p ss'") {
				t.Fatalf("keyword DSN = %q", got)
			}
		})
	}
}

func TestPostgresReadOnlyDSNRejectsOptionsOverrides(t *testing.T) {
	for _, dsn := range []string{
		"postgres://reader@db.example.test/samples?options=-cdefault_transaction_read_only%3Doff",
		"host=db.example.test user=reader dbname=samples options='-c default_transaction_read_only=off'",
	} {
		got, err := postgresReadOnlyDSN(dsn)
		if err == nil || got != "" || strings.Contains(err.Error(), "reader") || strings.Contains(err.Error(), "db.example") {
			t.Errorf("postgresReadOnlyDSN(%q) = %q, %v; want safe refusal", dsn, got, err)
		}
	}
}

func TestNativePostgresDiscoveryUsesStableLogicalID(t *testing.T) {
	schemas, err := discoverNativePostgresSchemas(context.Background(), nativeCatalogFixture{}, nativeCatalogFixture{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := schema.NativePostgresCollectionID("sales data", "Order Details")
	if !reflect.DeepEqual(schemas.Collections[want].Source, &schema.NativeCollectionSource{Schema: "sales data", Name: "Order Details"}) {
		t.Fatalf("source = %+v", schemas.Collections[want].Source)
	}
}
