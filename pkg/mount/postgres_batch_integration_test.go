package mount

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// perRelationVisibility hides the batched describer of the access connection, so
// that discovery takes the per-relation DescribeCollection path of the adapter.
type perRelationVisibility struct {
	inner nativePostgresSelectVisibility
}

func (v perRelationVisibility) SelectableRelations(ctx context.Context, schemaName string) (map[string]bool, error) {
	return v.inner.SelectableRelations(ctx, schemaName)
}

// The fixture covers what the batched reader must spell as DescribeCollection
// does: every mapped type family, sized and unsized numerics and strings, an
// array, an enum, a domain, a composite and a nullable primary key column order,
// a view, a materialized view (read from pg_attribute, not information_schema), a
// partitioned table, a mixed-case and a dotted relation name, and (for the
// reader) a relation the role may read only some columns of, which discovery
// leaves out on both paths.
const batchDiscoveryFixtureSQL = `
DROP SCHEMA IF EXISTS ovdb_batch_it CASCADE;
CREATE SCHEMA ovdb_batch_it;
SET search_path = ovdb_batch_it;
CREATE TYPE mood AS ENUM ('sad', 'ok');
CREATE DOMAIN positive_int AS integer CHECK (VALUE > 0);
CREATE TABLE "Order Lines" (
	order_id bigint NOT NULL, line smallint NOT NULL, sku varchar(20), qty positive_int,
	price numeric(10,2) NOT NULL, ratio numeric, weight real, total double precision,
	shipped boolean, at timestamptz, on_day date, at_local timestamp, t time, ttz timetz,
	blob bytea, tags text[], feeling mood, doc jsonb, id uuid, code char(3), note text,
	PRIMARY KEY (line, order_id));
CREATE TABLE "Sales.Customer" (id integer PRIMARY KEY, name text NOT NULL, secret text);
CREATE TABLE measurements (id bigint, at date NOT NULL, v numeric(6,3)) PARTITION BY RANGE (at);
CREATE TABLE measurements_2026 PARTITION OF measurements FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE VIEW line_totals AS SELECT order_id, sum(price) AS total, max(sku) AS sku FROM "Order Lines" GROUP BY order_id;
CREATE MATERIALIZED VIEW customer_names AS SELECT id, name::varchar(40) AS name, 1.50::numeric(4,2) AS fee, ARRAY[1,2] AS a, 'ok'::mood AS m, 7::positive_int AS p FROM "Sales.Customer";
`

func TestPostgresIntegration_NativeBatchedDiscoveryMatchesPerRelationDescribe(t *testing.T) {
	dsn := os.Getenv("OVDB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OVDB_TEST_POSTGRES_DSN is not set: no PostgreSQL server to test against")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA IF EXISTS ovdb_batch_it CASCADE")
		_, _ = admin.Exec("DROP OWNED BY ovdb_batch_it_reader")
		_, _ = admin.Exec("DROP ROLE IF EXISTS ovdb_batch_it_reader")
		_ = admin.Close()
	})
	if _, err := admin.ExecContext(ctx, batchDiscoveryFixtureSQL); err != nil {
		t.Fatalf("create the fixture: %v", err)
	}
	// The superuser sees every relation; the reader may read only two columns of
	// "Sales.Customer", which both paths therefore leave out.
	_, _ = admin.ExecContext(ctx, "DROP OWNED BY ovdb_batch_it_reader")
	_, _ = admin.ExecContext(ctx, "DROP ROLE IF EXISTS ovdb_batch_it_reader")
	for _, statement := range []string{
		"CREATE ROLE ovdb_batch_it_reader LOGIN PASSWORD 'reader'",
		"DO $$ BEGIN EXECUTE format('GRANT CONNECT ON DATABASE %I TO ovdb_batch_it_reader', current_database()); END $$",
		"GRANT USAGE ON SCHEMA ovdb_batch_it TO ovdb_batch_it_reader",
		`GRANT SELECT ON "Order Lines", line_totals, customer_names, measurements, measurements_2026 TO ovdb_batch_it_reader`,
		`GRANT SELECT (id, name) ON "Sales.Customer" TO ovdb_batch_it_reader`,
	} {
		if _, err := admin.ExecContext(ctx, "SET search_path = ovdb_batch_it; "+statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	readerDSN := dsnWithUser(t, dsn, "ovdb_batch_it_reader", "reader")
	for name, role := range map[string]struct {
		dsn            string
		customerListed bool
	}{"superuser": {dsn, true}, "column-restricted reader": {readerDSN, false}} {
		roleDSN := role.dsn
		t.Run(name, func(t *testing.T) {
			catalog, err := openPostgresDatabase(roleDSN, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{
				Placeholder: dalgo2sql.PlaceholderDollar, IdentifierCase: dalgo2sql.IdentifierCaseExact,
				ExactNumericValues: true, PreserveBinaryValues: true,
			}, dalgo2postgres.WithIdentifierMode(dalgo2postgres.IdentifierExact))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = catalog.Close() }()
			access, err := sqlOpenPostgresAccess(roleDSN)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = access.Close() }()
			if _, batched := access.(nativePostgresSchemaDescriber); !batched {
				t.Fatal("the access connection does not describe schemas in a batch")
			}
			batched, err := discoverNativePostgresSchemasExcluding(ctx, catalog, access, nil)
			if err != nil {
				t.Fatalf("batched discovery: %v", err)
			}
			perRelation, err := discoverNativePostgresSchemasExcluding(ctx, catalog, perRelationVisibility{access}, nil)
			if err != nil {
				t.Fatalf("per-relation discovery: %v", err)
			}
			if !reflect.DeepEqual(batched, perRelation) {
				for id, want := range perRelation.Collections {
					if got := batched.Collections[id]; !reflect.DeepEqual(got, want) {
						t.Errorf("%s:\n batched      %#v\n per-relation %#v", id, got, want)
					}
				}
				t.Fatalf("batched discovery found %d collections, per-relation %d", len(batched.Collections), len(perRelation.Collections))
			}
			for relation, want := range map[string]bool{"customer_names": true, "Order Lines": true, "line_totals": true, "measurements": true, "Sales.Customer": role.customerListed} {
				id, _ := schema.NativePostgresCollectionID("ovdb_batch_it", relation)
				if _, got := batched.Collections[id]; got != want {
					t.Errorf("relation %q discovered = %v, want %v", relation, got, want)
				}
			}
		})
	}
}

// dsnWithUser is the postgres:// URL dsn with another user and password.
func dsnWithUser(t *testing.T, dsn, user, password string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("OVDB_TEST_POSTGRES_DSN is not a URL: %v", err)
	}
	parsed.User = url.UserPassword(user, password)
	return parsed.String()
}
