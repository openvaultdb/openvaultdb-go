package core

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dtql"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func nativePostgresCoreFixture(t *testing.T) (*Database, *namesRecordingDB, string) {
	t.Helper()
	id, err := schema.NativePostgresCollectionID("sales data", "Order Details")
	if err != nil {
		t.Fatal(err)
	}
	driver := &namesRecordingDB{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "samples", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			id: {
				Source: &schema.NativeCollectionSource{Schema: "sales data", Name: "Order Details"},
				Fields: map[string]schema.Field{
					"Order ID":     {Type: schema.TypeInteger, PrimaryKey: true, NativeType: "bigint"},
					"Total Amount": {Type: schema.TypeDecimal, Decimal: &schema.Decimal{Precision: 18, Scale: 4, Storage: "text"}, NativeType: "numeric(18,4)"},
				},
			},
		}},
	}
	db, err := Open(m, driver, []schema.Mode{schema.ModeStrict}, t.TempDir()+"/inferred.json")
	if err != nil {
		t.Fatal(err)
	}
	return db, driver, id
}

func TestNativePostgresOpenIsReadOnlyAndDoesNotProvision(t *testing.T) {
	db, driver, id := nativePostgresCoreFixture(t)
	if !db.NativePostgresReadOnly() || !db.ReadOnly() || !db.CanQuery() || db.CanCollectionQuery() || EngineCanQuery("postgres") {
		t.Fatalf("capabilities: native=%v readonly=%v query=%v collectionQuery=%v engineQuery=%v", db.NativePostgresReadOnly(), db.ReadOnly(), db.CanQuery(), db.CanCollectionQuery(), EngineCanQuery("postgres"))
	}
	if len(driver.created) != 0 {
		t.Fatalf("native mount provisioned collections: %v", driver.created)
	}
	collections, err := db.Collections(context.Background())
	if err != nil || !slices.Equal(collections, []string{id}) {
		t.Fatalf("collections = %q, %v; want only stable logical ID %q", collections, err, id)
	}
	if source, ok := db.NativePostgresSource(id); !ok || source != (schema.NativeCollectionSource{Schema: "sales data", Name: "Order Details"}) {
		t.Fatalf("source = %+v, %v", source, ok)
	}
	if got, ok := db.ResolveNativePostgresCollection("sales data", "Order Details"); !ok || got != id {
		t.Fatalf("resolved collection = %q, %v", got, ok)
	}
	if _, ok := db.ResolveNativePostgresCollection("public", "Order Details"); ok {
		t.Fatal("resolved a collection from a different schema")
	}
}

func TestNativePostgresCollectionQueryIsExplicitlyUnsupported(t *testing.T) {
	db, driver, id := nativePostgresCoreFixture(t)
	if _, err := db.Execute(context.Background(), Query{Collection: id}); !errors.Is(err, ErrNativePostgresCollectionQueryUnsupported) {
		t.Fatalf("Execute error = %v, want native collection-query refusal", err)
	}
	if driver.queries != 0 || len(driver.seen) != 0 || driver.transactions != 0 {
		t.Fatalf("collection query reached DAL: queryCalls=%d keys=%v transactions=%d", driver.queries, driver.seen, driver.transactions)
	}
}

func TestNativePostgresGuardAllowsOnlyCatalogMappedQualifiedReads(t *testing.T) {
	db, _, id := nativePostgresCoreFixture(t)
	parse := func(t *testing.T, text string) dal.StructuredQuery {
		t.Helper()
		query, err := dtql.Deserialize([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return query
	}
	valid := parse(t, "from: {schema: 'sales data', name: 'Order Details'}\ncolumns: [{field: 'Order ID'}]\nwhere: {op: '>', left: {field: 'Total Amount'}, right: {value: 10.25}}\nlimit: 5\n")
	if err := db.guardSources(valid); err != nil {
		t.Fatalf("guard valid native source: %v", err)
	}
	for name, doc := range map[string]string{
		"unqualified":         "from: {name: 'Order Details'}\n",
		"unknown schema":      "from: {schema: public, name: 'Order Details'}\n",
		"unknown relation":    "from: {schema: 'sales data', name: Customers}\n",
		"unknown field":       "from: {schema: 'sales data', name: 'Order Details'}\ncolumns: [{field: missing}]\n",
		"wrong field case":    "from: {schema: 'sales data', name: 'Order Details'}\ncolumns: [{field: 'order id'}]\n",
		"invented key column": "from: {schema: 'sales data', name: 'Order Details'}\ncolumns: [{field: id}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.guardSources(parse(t, doc)); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInvalidDTQL) {
				t.Fatalf("guard error = %v, want an explicit source/field refusal", err)
			}
		})
	}
	if err := db.GuardCollection(id); err != nil {
		t.Fatalf("logical collection ID is not authorized: %v", err)
	}
}

func TestNativePostgresDeclaredJoinFieldsResolveQualifiedSource(t *testing.T) {
	db, _, id := nativePostgresCoreFixture(t)
	provider, ok := db.Executor().(dal.JoinFieldsProvider)
	if !ok {
		t.Fatal("database executor must provide join fields")
	}
	ref := dal.NewQualifiedRootCollectionRef("sales data", "Order Details", "orders")
	fields, err := provider.JoinFields(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fields, []string{"Order ID", "Total Amount"}) {
		t.Fatalf("join fields = %q, want declared fields for %q", fields, id)
	}
	unknown := dal.NewQualifiedRootCollectionRef("sales data", "Not Listed", "x")
	if fields := db.declaredJoinFields(unknown); fields != nil {
		t.Fatalf("unknown native source fields = %v; want no fields", fields)
	}
	if fields := db.declaredJoinFields(dal.NewRootCollectionRef(id, "x")); fields != nil {
		t.Fatalf("unqualified native logical ID fields = %v; want no fields", fields)
	}
}

func TestNativePostgresKeyedAndMutationAPIsRefuseBeforeDriver(t *testing.T) {
	db, driver, id := nativePostgresCoreFixture(t)
	key := record.NewKeyWithID(id, "1")
	if _, err := db.Get(context.Background(), key); !errors.Is(err, ErrNativePostgresOperationUnsupported) {
		t.Fatalf("Get error = %v", err)
	}
	if _, err := db.Exists(context.Background(), key); !errors.Is(err, ErrNativePostgresOperationUnsupported) {
		t.Fatalf("Exists error = %v", err)
	}
	if _, err := db.Apply(context.Background(), nil, ""); !errors.Is(err, ErrNativePostgresOperationUnsupported) {
		t.Fatalf("Apply error = %v", err)
	}
	if len(driver.seen) != 0 || driver.transactions != 0 {
		t.Fatalf("read-only API reached DAL: calls=%v transactions=%d", driver.seen, driver.transactions)
	}
}

func TestNativePostgresMappingMustMatchEncodedPhysicalSource(t *testing.T) {
	id, err := schema.NativePostgresCollectionID("sales data", "Order Details")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		id   string
		src  schema.NativeCollectionSource
	}{
		{"id mismatch", id + "x", schema.NativeCollectionSource{Schema: "sales data", Name: "Order Details"}},
		{"empty schema", id, schema.NativeCollectionSource{Name: "Order Details"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := nativePostgresMappings(&schema.Schemas{Collections: map[string]schema.Collection{
				tc.id: {Source: &tc.src, Fields: map[string]schema.Field{"Order ID": {Type: schema.TypeInteger}}},
			}})
			if err == nil {
				t.Fatal("invalid native mapping accepted")
			}
		})
	}
	// Two distinct IDs that claim one physical table are rejected as ambiguous.
	second := schema.NativeCollectionSource{Schema: "sales data", Name: "Order Details"}
	otherID, err := schema.NativePostgresCollectionID("other", "table")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = nativePostgresMappings(&schema.Schemas{Collections: map[string]schema.Collection{
		id:      {Source: &schema.NativeCollectionSource{Schema: "sales data", Name: "Order Details"}, Fields: map[string]schema.Field{"Order ID": {Type: schema.TypeInteger}}},
		otherID: {Source: &second, Fields: map[string]schema.Field{"Order ID": {Type: schema.TypeInteger}}},
	}})
	if err == nil {
		t.Fatal("duplicate physical mapping accepted")
	}
}

func TestNativePostgresMetadataDoesNotInjectRecordID(t *testing.T) {
	db, _, id := nativePostgresCoreFixture(t)
	if _, ok := db.schemaCollection(id).Fields["id"]; ok {
		t.Fatal("native schema contains an undeclared synthetic id")
	}
}
