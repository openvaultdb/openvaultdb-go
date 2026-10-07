package mount

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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

type excludedRelationCatalogFixture struct {
	mu        sync.Mutex
	described []schema.NativeCollectionSource
}

func (f *excludedRelationCatalogFixture) ListSchemas(context.Context) ([]string, error) {
	return []string{"demodb", "other"}, nil
}

func (*excludedRelationCatalogFixture) ListSchemaCollections(_ context.Context, schemaName string) ([]dal.CollectionRef, error) {
	return []dal.CollectionRef{
		dal.NewQualifiedRootCollectionRef(schemaName, "_import_manifest", ""),
		dal.NewQualifiedRootCollectionRef(schemaName, "Artist", ""),
	}, nil
}

func (*excludedRelationCatalogFixture) SelectableRelations(_ context.Context, schemaName string) (map[string]bool, error) {
	return map[string]bool{"_import_manifest": true, "Artist": true}, nil
}

func (f *excludedRelationCatalogFixture) DescribeCollection(_ context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.described = append(f.described, schema.NativeCollectionSource{Schema: ref.Schema(), Name: ref.Name()})
	return &dbschema.CollectionDef{Name: ref.Name(), Fields: []dbschema.FieldDef{{Name: "Name", Type: dbschema.String}}}, nil
}

func TestNativePostgresRelationExclusionIsQualifiedAndAppliedBeforeDescription(t *testing.T) {
	catalog := &excludedRelationCatalogFixture{}
	all, err := discoverNativePostgresSchemas(context.Background(), catalog, catalog)
	if err != nil {
		t.Fatal(err)
	}
	manifestID, _ := schema.NativePostgresCollectionID("demodb", "_import_manifest")
	otherManifestID, _ := schema.NativePostgresCollectionID("other", "_import_manifest")
	if _, exists := all.Collections[manifestID]; !exists {
		t.Fatal("default discovery must continue to expose every selectable relation")
	}
	catalog.described = nil
	exclusions := map[schema.NativeCollectionSource]struct{}{{Schema: "demodb", Name: "_import_manifest"}: {}}
	filtered, err := discoverNativePostgresSchemasExcluding(context.Background(), catalog, catalog, exclusions)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := filtered.Collections[manifestID]; exists {
		t.Fatal("excluded relation appeared in the discovered schema")
	}
	if _, exists := filtered.Collections[otherManifestID]; !exists {
		t.Fatal("exclusion must match the exact schema and relation pair")
	}
	for _, described := range catalog.described {
		if described == (schema.NativeCollectionSource{Schema: "demodb", Name: "_import_manifest"}) {
			t.Fatal("excluded relation was described by the provider catalog")
		}
	}
}

type concurrentNativeCatalogFixture struct {
	refs      []dal.CollectionRef
	started   chan string
	release   <-chan struct{}
	failName  string
	failErr   error
	failGate  <-chan struct{}
	mu        sync.Mutex
	active    int
	maxActive int
	cancelled int
}

func newConcurrentNativeCatalogFixture(count int, release <-chan struct{}) *concurrentNativeCatalogFixture {
	f := &concurrentNativeCatalogFixture{started: make(chan string, count), release: release}
	for i := range count {
		f.refs = append(f.refs, dal.NewQualifiedRootCollectionRef("sales", fmt.Sprintf("relation-%02d", i), ""))
	}
	return f
}

func (f *concurrentNativeCatalogFixture) ListSchemas(context.Context) ([]string, error) {
	return []string{"sales"}, nil
}

func (f *concurrentNativeCatalogFixture) ListSchemaCollections(context.Context, string) ([]dal.CollectionRef, error) {
	return f.refs, nil
}

func (f *concurrentNativeCatalogFixture) SelectableRelations(context.Context, string) (map[string]bool, error) {
	selectable := make(map[string]bool, len(f.refs))
	for _, ref := range f.refs {
		selectable[ref.Name()] = true
	}
	return selectable, nil
}

func (f *concurrentNativeCatalogFixture) DescribeCollection(ctx context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	f.started <- ref.Name()
	if ref.Name() == f.failName {
		if f.failGate != nil {
			select {
			case <-f.failGate:
			case <-ctx.Done():
				f.mu.Lock()
				f.cancelled++
				f.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		return nil, f.failErr
	}
	select {
	case <-f.release:
		return &dbschema.CollectionDef{Name: ref.Name(), Fields: []dbschema.FieldDef{{Name: "Name", Type: dbschema.String}}}, nil
	case <-ctx.Done():
		f.mu.Lock()
		f.cancelled++
		f.mu.Unlock()
		return nil, ctx.Err()
	}
}

func TestDiscoverNativePostgresDescriptionsUseBoundedConcurrencyAndPreserveInventory(t *testing.T) {
	const relationCount = nativePostgresDescribeConcurrency + 3
	release := make(chan struct{})
	catalog := newConcurrentNativeCatalogFixture(relationCount, release)
	type result struct {
		schemas *schema.Schemas
		err     error
	}
	done := make(chan result, 1)
	go func() {
		got, err := discoverNativePostgresSchemas(context.Background(), catalog, catalog)
		done <- result{schemas: got, err: err}
	}()

	for range nativePostgresDescribeConcurrency {
		select {
		case <-catalog.started:
		case <-time.After(time.Second):
			t.Fatal("discovery did not start the configured number of parallel descriptions")
		}
	}
	select {
	case relation := <-catalog.started:
		t.Fatalf("description concurrency exceeded %d; started %s", nativePostgresDescribeConcurrency, relation)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)

	var got result
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("discovery did not finish after releasing descriptions")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	catalog.mu.Lock()
	maxActive := catalog.maxActive
	catalog.mu.Unlock()
	if maxActive != nativePostgresDescribeConcurrency {
		t.Fatalf("maximum concurrent descriptions = %d, want %d", maxActive, nativePostgresDescribeConcurrency)
	}
	if len(got.schemas.Collections) != relationCount {
		t.Fatalf("discovered collection count = %d, want %d", len(got.schemas.Collections), relationCount)
	}
	for _, ref := range catalog.refs {
		id, err := schema.NativePostgresCollectionID(ref.Schema(), ref.Name())
		if err != nil {
			t.Fatal(err)
		}
		collection, ok := got.schemas.Collections[id]
		if !ok || collection.Source == nil || collection.Source.Schema != ref.Schema() || collection.Source.Name != ref.Name() {
			t.Errorf("relation %q.%q was not returned in its original position mapping: %+v", ref.Schema(), ref.Name(), collection)
		}
	}
}

func TestDiscoverNativePostgresDescriptionFailureCancelsWorkersAndPreservesCause(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	failGate := make(chan struct{})
	const databaseError = "private-driver-error-marker"
	cause := errors.New(databaseError)
	catalog := newConcurrentNativeCatalogFixture(nativePostgresDescribeConcurrency*2, release)
	catalog.failName = catalog.refs[nativePostgresDescribeConcurrency-1].Name()
	catalog.failErr = cause
	catalog.failGate = failGate
	type result struct {
		schemas *schema.Schemas
		err     error
	}
	done := make(chan result, 1)
	go func() {
		got, err := discoverNativePostgresSchemas(context.Background(), catalog, catalog)
		done <- result{schemas: got, err: err}
	}()
	for range nativePostgresDescribeConcurrency {
		select {
		case <-catalog.started:
		case <-time.After(time.Second):
			t.Fatal("discovery did not start the configured workers before failure")
		}
	}
	close(failGate)
	var got result
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("discovery did not return after canceling workers")
	}
	if got.err == nil || got.schemas != nil {
		t.Fatalf("discovery = %v, %v; want no partial catalogue and an error", got.schemas, got.err)
	}
	if !errors.Is(got.err, cause) {
		t.Fatalf("discovery error does not preserve the provider cause: %v", got.err)
	}
	if errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("provider error was mislabeled as a deadline: %v", got.err)
	}
	if strings.Contains(got.err.Error(), databaseError) || !strings.Contains(got.err.Error(), `"sales"."relation-03"`) {
		t.Fatalf("discovery error is not safely contextualized: %v", got.err)
	}
	catalog.mu.Lock()
	cancelled, maxActive := catalog.cancelled, catalog.maxActive
	catalog.mu.Unlock()
	if cancelled == 0 {
		t.Fatal("an error did not cancel in-flight relation descriptions")
	}
	if maxActive > nativePostgresDescribeConcurrency {
		t.Fatalf("maximum concurrent descriptions = %d, exceeds bound %d", maxActive, nativePostgresDescribeConcurrency)
	}
}

func TestDiscoverNativePostgresDeadlineErrorPreservesErrorsIs(t *testing.T) {
	release := make(chan struct{})
	catalog := newConcurrentNativeCatalogFixture(1, release)
	catalog.failName = catalog.refs[0].Name()
	catalog.failErr = context.DeadlineExceeded
	_, err := discoverNativePostgresSchemas(context.Background(), catalog, catalog)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("discovery deadline error = %v, want errors.Is(context.DeadlineExceeded)", err)
	}
}

func TestDiscoverNativePostgresDeadlineRemainsDiscoverable(t *testing.T) {
	release := make(chan struct{})
	catalog := newConcurrentNativeCatalogFixture(nativePostgresDescribeConcurrency, release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := discoverNativePostgresSchemas(ctx, catalog, catalog)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery error = %v, want errors.Is(context.Canceled)", err)
	}
}

func TestNativePostgresExclusionsAreValidatedBeforeOpening(t *testing.T) {
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "samples"},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{DSNEnv: "TEST_EXCLUSION_DSN", ReadOnly: true}},
	}
	called := false
	open := func(string, dal.Schema, dalgo2sql.DbOptions, ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
		called = true
		return nil, errStopOpening
	}
	for name, exclusions := range map[string][]schema.NativeCollectionSource{
		"invalid name":   {{Schema: "public", Name: "bad\x00name"}},
		"duplicate pair": {{Schema: "public", Name: "hidden"}, {Schema: "public", Name: "hidden"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := openPostgresWithExclusions(m, open, exclusions)
			if err == nil {
				t.Fatal("expected exclusions to be rejected")
			}
		})
	}
	notReadOnly := &manifest.Manifest{
		Database: manifest.Database{ID: "samples"},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{DSNEnv: "TEST_EXCLUSION_DSN"}},
	}
	if _, _, err := openPostgresWithExclusions(notReadOnly, open, []schema.NativeCollectionSource{{Schema: "public", Name: "hidden"}}); err == nil {
		t.Fatal("expected relation exclusions on a non-native mount to be rejected")
	}
	if called {
		t.Fatal("invalid exclusions reached the PostgreSQL opener")
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
		if options.IdentifierCase != dalgo2sql.IdentifierCaseExact || !options.ExactNumericValues || !options.PreserveBinaryValues {
			t.Errorf("DbOptions = %+v, want exact identifiers, exact numerics, and preserved BYTEA values", options)
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
