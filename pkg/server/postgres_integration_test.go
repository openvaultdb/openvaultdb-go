package server_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// The proof of the PostgreSQL mount against a real server. Every test here needs
// a PostgreSQL server and skips without one: the CI job .github/workflows/
// postgres-integration.yml starts a postgres container on each of two versions (17
// and 18), sets OVDB_TEST_POSTGRES_DSN (a postgres:// URL) and OVDB_TEST_POSTGRES_MAJOR
// (the version of the leg), and fails when one of these tests is skipped or does not
// report PASS. The helpers of this file all start with pgIT so
// they cannot clash with the others of the package.
//
// What the mount holds is the preview of queries (core.PreviewPostgresQueriesEnv):
// each test says whether the switch is on when it mounts.

const (
	pgITDSNEnv      = "OVDB_TEST_POSTGRES_DSN"
	pgITMountDSNEnv = "OVDB_TEST_POSTGRES_MOUNT_DSN" // the variable the manifest of the mount names
)

// pgITTables are the tables the tests create and drop. ovdb_canary is not declared
// by any manifest: nothing a caller sends may touch it.
const pgITTables = "customers, orders, people, ovdb_canary"

// pgITAdmin opens the connection the test looks at the server with, and leaves the
// database with the tables of the tests dropped and a canary table of one row.
func pgITAdmin(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(pgITDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set: no PostgreSQL server to test against", pgITDSNEnv)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open the administration connection: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP TABLE IF EXISTS " + pgITTables + " CASCADE")
		_ = admin.Close()
	})
	for _, statement := range []string{
		"DROP TABLE IF EXISTS " + pgITTables + " CASCADE",
		"CREATE TABLE ovdb_canary (id text PRIMARY KEY, note text)",
		"INSERT INTO ovdb_canary VALUES ('canary', 'untouched')",
	} {
		if _, err := admin.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return admin
}

// pgITCanary fails when the canary table is not what pgITAdmin left.
func pgITCanary(t *testing.T, admin *sql.DB) {
	t.Helper()
	var rows int
	var id, note string
	if err := admin.QueryRow("SELECT count(*) FROM ovdb_canary").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the canary table holds %d rows (%v), want 1", rows, err)
	}
	if err := admin.QueryRow("SELECT id, note FROM ovdb_canary").Scan(&id, &note); err != nil || id != "canary" || note != "untouched" {
		t.Fatalf("the canary row is %q, %q (%v)", id, note, err)
	}
}

// pgITStatementsSince counts the sessions of the application that started a
// statement after at, by the server's own clock. It is what the server says of
// itself, not what the code under test says it did.
func pgITStatementsSince(t *testing.T, admin *sql.DB, application string, at time.Time) int {
	t.Helper()
	var n int
	if err := admin.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND query_start > $2", application, at).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pgITSessions counts the sessions the server holds for the application.
func pgITSessions(t *testing.T, admin *sql.DB, application string) int {
	t.Helper()
	var n int
	if err := admin.QueryRow("SELECT count(*) FROM pg_stat_activity WHERE application_name = $1", application).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func pgITNow(t *testing.T, admin *sql.DB) time.Time {
	t.Helper()
	var now time.Time
	if err := admin.QueryRow("SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// pgITSchemas is the declaration both databases of the tests share.
const pgITSchemas = `schemas:
  collections:
    customers:
      fields:
        name: {type: string}
        country: {type: string}
    orders:
      fields:
        customer_id: {type: string}
        total: {type: integer}
        status: {type: string}
    people:
      fields:
        FirstName: {type: string}
        city: {type: string}
`

// pgITMount mounts the PostgreSQL database as id, its sessions named application on
// the server, with the preview switch on or off.
func pgITMount(t *testing.T, id, application string, switchOn bool) *core.Database {
	t.Helper()
	u, err := url.Parse(os.Getenv(pgITDSNEnv))
	if err != nil {
		t.Fatalf("%s is not a URL: %v", pgITDSNEnv, err)
	}
	query := u.Query()
	query.Set("application_name", application)
	u.RawQuery = query.Encode()
	t.Setenv(pgITMountDSNEnv, u.String())
	if switchOn {
		t.Setenv(core.PreviewPostgresQueriesEnv, "1")
	} else {
		t.Setenv(core.PreviewPostgresQueriesEnv, "")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: " + id + ", schema_mode: strict}\nstorage: {engine: postgres, postgres: {dsn_env: " + pgITMountDSNEnv + "}}\n" + pgITSchemas
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(path)
	if err != nil {
		t.Fatalf("mount the PostgreSQL database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// pgITLite mounts a SQLite database of the same collections, as id.
func pgITLite(t *testing.T, id string) *core.Database {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: " + id + ", schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\n" + pgITSchemas
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// pgITLog is what a server under test logs. A handler may still be writing a record
// when the response it sent is read, so it is safe for concurrent use.
type pgITLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *pgITLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *pgITLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// pgITServe serves the mounts, with PostgreSQL among the engines that join.
func pgITServe(t *testing.T, mounts map[string]*core.Database) string {
	t.Helper()
	base, _ := pgITServeLogged(t, mounts)
	return base
}

// pgITServeLogged is pgITServe, and returns what the server logs.
func pgITServeLogged(t *testing.T, mounts map[string]*core.Database) (string, *pgITLog) {
	t.Helper()
	logs := &pgITLog{}
	service := server.New("test", mounts,
		server.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))),
		server.WithQueryLimits(server.QueryLimits{JoinEngines: []string{"sqlite", "postgres"}}))
	t.Cleanup(service.CloseSnapshots)
	host := httptest.NewServer(service.Handler())
	t.Cleanup(host.Close)
	return host.URL, logs
}

// pgITRows are the rows both databases hold: four customers, five orders, and two
// people (under the keys of two customers, and with a field whose name has capitals).
var pgITRows = []struct{ collection, id, data string }{
	{"customers", "c1", `{"name":"Ada","country":"IE"}`},
	{"customers", "c2", `{"name":"Bob","country":"US"}`},
	{"customers", "c3", `{"name":"Cy","country":"IE"}`},
	{"customers", "c4", `{"name":"Dee","country":"GB"}`},
	{"orders", "o1", `{"customer_id":"c1","total":100,"status":"paid"}`},
	{"orders", "o2", `{"customer_id":"c1","total":250,"status":"paid"}`},
	{"orders", "o3", `{"customer_id":"c2","total":75,"status":"open"}`},
	{"orders", "o4", `{"customer_id":"c3","total":300,"status":"paid"}`},
	{"orders", "o5", `{"customer_id":"c3","total":20,"status":"open"}`},
	{"people", "c1", `{"FirstName":"Ada","city":"Dublin"}`},
	{"people", "c2", `{"FirstName":"Bob","city":"Austin"}`},
}

// pgITSeed writes the rows to database id by key, over HTTP.
func pgITSeed(t *testing.T, base, id string) {
	t.Helper()
	for _, row := range pgITRows {
		resp := relHTTPDo(t, base, http.MethodPut, "/v1/databases/"+id+"/records/"+row.collection+"/"+row.id, "", `{"data":`+row.data+`}`, nil)
		if resp.status != http.StatusNoContent {
			t.Fatalf("PUT %s/%s into %s: status %d: %s", row.collection, row.id, id, resp.status, resp.raw)
		}
	}
}

// pgITBoth sends the same request to the PostgreSQL database and to the SQLite one
// and requires both to answer the same: the status, and the part of the body that
// is the answer (the records, and for a relational document the columns too). It
// returns the PostgreSQL answer.
func pgITBoth(t *testing.T, base, method, path, litePath, body, liteBody string) relHTTPResponse {
	t.Helper()
	pg := relHTTPDo(t, base, method, path, "", body, nil)
	lite := relHTTPDo(t, base, method, litePath, "", liteBody, nil)
	if pg.status != http.StatusOK || lite.status != http.StatusOK {
		t.Fatalf("status: PostgreSQL %d (%s), SQLite %d (%s)", pg.status, pg.raw, lite.status, lite.raw)
	}
	for _, field := range []string{"records", "columns"} {
		if !reflect.DeepEqual(pg.body[field], lite.body[field]) {
			t.Fatalf("%s differ\nPostgreSQL: %s\nSQLite:     %s", field, pg.raw, lite.raw)
		}
	}
	if strings.Contains(pg.raw, "__dalgo_record_id") {
		t.Fatalf("a record of the answer carries the placeholder key of an undeclared source: %s", pg.raw)
	}
	return pg
}

// pgITRecordNames are the names of the records of a wire or database answer, in
// order.
func pgITRecordNames(t *testing.T, resp relHTTPResponse) []string {
	t.Helper()
	records, _ := resp.body["records"].([]any)
	names := make([]string, len(records))
	for i, rec := range records {
		data, _ := rec.(map[string]any)["data"].(map[string]any)
		name, _ := data["name"].(string)
		names[i] = name
	}
	return names
}

// pgITKeys are the keys of the records of a wire or database answer, in order.
func pgITKeys(resp relHTTPResponse) []string {
	records, _ := resp.body["records"].([]any)
	keys := make([]string, len(records))
	for i, rec := range records {
		keys[i], _ = rec.(map[string]any)["key"].(string)
	}
	return keys
}

// TestPostgresIntegration_WritesAndSingleCollectionQueries: records are written to a
// PostgreSQL mount by key, read back, and queried with filters, order and limit on
// /query and on the per-database /dtql; each answer is the answer a SQLite mount
// holding the same rows gives, and a record carries the key it was written under.
func TestPostgresIntegration_WritesAndSingleCollectionQueries(t *testing.T) {
	admin := pgITAdmin(t)
	pg, lite := pgITMount(t, "pg", "ovdb-it-single", true), pgITLite(t, "lite")
	base := pgITServe(t, map[string]*core.Database{"pg": pg, "lite": lite})
	pgITSeed(t, base, "pg")
	pgITSeed(t, base, "lite")

	got := relHTTPDo(t, base, http.MethodGet, "/v1/databases/pg/records/customers/c1", "", "", nil)
	if got.status != http.StatusOK || !strings.Contains(got.raw, `"name":"Ada"`) || !strings.Contains(got.raw, `"country":"IE"`) {
		t.Fatalf("GET customers/c1: status %d: %s", got.status, got.raw)
	}
	// A row of a table the mount did not read through the API is the row the write made.
	var name string
	if err := admin.QueryRow("SELECT name FROM customers WHERE id = 'c3'").Scan(&name); err != nil || name != "Cy" {
		t.Fatalf("customers/c3 holds %q (%v)", name, err)
	}

	for _, c := range []struct {
		name     string
		wire     string
		wantKeys []string
	}{
		{"a filter and an order", `{"collection":"customers","where":[{"field":"country","op":"==","value":"IE"}],"orderBy":[{"field":"name"}]}`, []string{"customers/c1", "customers/c3"}},
		{"a comparison, a descending order and a limit", `{"collection":"orders","where":[{"field":"total","op":">","value":50}],"orderBy":[{"field":"total","desc":true}],"limit":3}`, []string{"orders/o4", "orders/o2", "orders/o1"}},
		{"a membership test", `{"collection":"customers","where":[{"field":"name","op":"in","value":["Bob","Dee","Zed"]}],"orderBy":[{"field":"name","desc":true}]}`, []string{"customers/c4", "customers/c2"}},
		{"two filters", `{"collection":"orders","where":[{"field":"status","op":"==","value":"paid"},{"field":"total","op":"<=","value":250}],"orderBy":[{"field":"total"}]}`, []string{"orders/o1", "orders/o2"}},
		{"keys only", `{"collection":"customers","keysOnly":true}`, []string{"customers/c1", "customers/c2", "customers/c3", "customers/c4"}},
	} {
		t.Run("/query: "+c.name, func(t *testing.T) {
			resp := pgITBoth(t, base, http.MethodPost, "/v1/databases/pg/query", "/v1/databases/lite/query", c.wire, c.wire)
			if keys := pgITKeys(resp); !reflect.DeepEqual(keys, c.wantKeys) {
				t.Fatalf("keys = %v, want %v", keys, c.wantKeys)
			}
		})
	}
	for _, c := range []struct {
		name      string
		doc       string
		wantNames []string
	}{
		{"a filter, an order and a limit", "from: {name: customers}\nwhere: {op: '==', left: {field: country}, right: {value: IE}}\norderBy: [{field: name, desc: true}]\nlimit: 1\n", []string{"Cy"}},
		{"two conditions", "from: {name: customers}\nwhere:\n  and:\n    - {op: '>=', left: {field: country}, right: {value: IE}}\n    - {op: '<', left: {field: name}, right: {value: Dee}}\norderBy: [{field: name}]\n", []string{"Ada", "Bob", "Cy"}},
	} {
		t.Run("/dtql: "+c.name, func(t *testing.T) {
			resp := pgITBoth(t, base, http.MethodPost, "/v1/databases/pg/dtql", "/v1/databases/lite/dtql", c.doc, c.doc)
			if names := pgITRecordNames(t, resp); !reflect.DeepEqual(names, c.wantNames) {
				t.Fatalf("names = %v, want %v", names, c.wantNames)
			}
		})
	}
	pgITCanary(t, admin)
}

// TestPostgresIntegration_NativeReadOnlyCatalogAndFilteredRows exercises the
// additive native catalog profile against PostgreSQL itself. It uses a
// schema-qualified relation with mixed case and spaces, and confirms the API
// exposes its original names while querying only through a catalog-resolved
// logical collection ID.
func TestPostgresIntegration_NativeReadOnlyCatalogAndFilteredRows(t *testing.T) {
	admin := pgITAdmin(t)
	const physicalSchema = "ovdb Native"
	const physicalTable = "Order Details"
	if _, err := admin.Exec(`CREATE SCHEMA "ovdb Native"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA IF EXISTS "ovdb Native" CASCADE`) })
	for _, statement := range []string{
		`CREATE SEQUENCE "ovdb Native"."side effect seq"`,
		`CREATE FUNCTION "ovdb Native"."mutating read"() RETURNS bigint
			LANGUAGE SQL VOLATILE AS $$ SELECT nextval('"ovdb Native"."side effect seq"') $$`,
		`CREATE VIEW "ovdb Native"."Mutation Attempt" AS
			SELECT "ovdb Native"."mutating read"() AS "Next Value"`,
		`CREATE TABLE "ovdb Native"."Order Details" (
			"Order ID" bigint PRIMARY KEY,
			"Total Amount" numeric(24,6) NOT NULL,
			"Created On" date NOT NULL,
			"Status Name" text NOT NULL,
			"Payload Data" jsonb NOT NULL,
			"Binary Data" bytea NOT NULL,
			"Time Value" time,
			"Time TZ Value" timetz,
			"Timestamp Value" timestamp,
			"Timestamp TZ Value" timestamptz
		)`,
		`CREATE TABLE "ovdb Native"."Order Events" (
			"Event Name" text NOT NULL,
			"Description Text" text NOT NULL
		)`,
		`INSERT INTO "ovdb Native"."Order Details" ("Order ID", "Total Amount", "Created On", "Status Name", "Payload Data", "Binary Data") VALUES (
			9007199254740993,
			123456789012345678.120000,
			DATE '2025-03-04',
			'keep',
			'{"n":9007199254740993}'::jsonb,
			decode('00ff10', 'hex')
		)`,
		`INSERT INTO "ovdb Native"."Order Details" ("Order ID", "Total Amount", "Created On", "Status Name", "Payload Data", "Binary Data") VALUES (
			9007199254740994,
			1.000000,
			DATE '2025-03-05',
			'skip',
			'{"n":1}'::jsonb,
			decode('01', 'hex')
		)`,
		`INSERT INTO "ovdb Native"."Order Events" VALUES ('open', 'same'), ('open', 'same')`,
	} {
		if _, err := admin.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	u, err := url.Parse(os.Getenv(pgITDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("application_name", "ovdb-it-native-read")
	u.RawQuery = query.Encode()
	t.Setenv(pgITMountDSNEnv, u.String())
	dir := t.TempDir()
	path := filepath.Join(dir, "native.yaml")
	manifest := "database: {id: native, schema_mode: strict}\nstorage: {engine: postgres, postgres: {dsn_env: " + pgITMountDSNEnv + ", read_only: true}}\n"
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	native, err := mount.File(path)
	if err != nil {
		t.Fatalf("mount native read-only PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = native.Close() })
	if !native.CanQuery() || !native.ReadOnly() {
		t.Fatalf("native capabilities: query=%v readonly=%v", native.CanQuery(), native.ReadOnly())
	}
	id, err := schema.NativePostgresCollectionID(physicalSchema, physicalTable)
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := schema.NativePostgresCollectionID(physicalSchema, "Order Events")
	if err != nil {
		t.Fatal(err)
	}
	mutationViewID, err := schema.NativePostgresCollectionID(physicalSchema, "Mutation Attempt")
	if err != nil {
		t.Fatal(err)
	}
	canaryID, err := schema.NativePostgresCollectionID("public", "ovdb_canary")
	if err != nil {
		t.Fatal(err)
	}
	collections, err := native.Collections(t.Context())
	if err != nil || len(collections) != 4 {
		t.Fatalf("collections = %q, %v; want the two native tables, side-effect view, and public canary", collections, err)
	}
	collectionIDs := make(map[string]bool, len(collections))
	for _, collectionID := range collections {
		collectionIDs[collectionID] = true
	}
	for _, wantID := range []string{id, eventID, mutationViewID, canaryID} {
		if !collectionIDs[wantID] {
			t.Errorf("discovered collections %q do not include %q", collections, wantID)
		}
	}

	base := pgITServe(t, map[string]*core.Database{"native": native})
	metadata := relHTTPDo(t, base, http.MethodGet, "/v1/databases/native", "", "", nil)
	if metadata.status != http.StatusOK {
		t.Fatalf("GET native metadata: %d %s", metadata.status, metadata.raw)
	}
	var described struct {
		Collections  []string        `json:"collections"`
		Capabilities map[string]bool `json:"capabilities"`
		Schemas      struct {
			Collections map[string]struct {
				Source struct {
					Schema string `json:"schema"`
					Name   string `json:"name"`
				} `json:"source"`
				Fields map[string]struct {
					NativeType string `json:"nativeType"`
					PrimaryKey bool   `json:"primaryKey"`
				} `json:"fields"`
			} `json:"collections"`
		} `json:"schemas"`
	}
	if err := json.Unmarshal([]byte(metadata.raw), &described); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(described.Collections, collections) || described.Capabilities["read"] || !described.Capabilities["query"] || described.Capabilities["write"] {
		t.Fatalf("native metadata capabilities/IDs = %+v, collections=%q", described.Capabilities, described.Collections)
	}
	collection := described.Schemas.Collections[id]
	if collection.Source.Schema != physicalSchema || collection.Source.Name != physicalTable || collection.Fields["Order ID"].NativeType != "bigint" || !collection.Fields["Order ID"].PrimaryKey {
		t.Fatalf("native schema metadata = %+v", collection)
	}
	for field, wantType := range map[string]string{
		"Created On":         "date",
		"Time Value":         "time without time zone",
		"Time TZ Value":      "time with time zone",
		"Timestamp Value":    "timestamp without time zone",
		"Timestamp TZ Value": "timestamp with time zone",
	} {
		if got := collection.Fields[field].NativeType; got != wantType {
			t.Errorf("native type for %q = %q, want %q", field, got, wantType)
		}
	}
	eventCollection := described.Schemas.Collections[eventID]
	if eventCollection.Source.Schema != physicalSchema || eventCollection.Source.Name != "Order Events" || len(eventCollection.Fields) != 2 {
		t.Fatalf("native keyless schema metadata = %+v", eventCollection)
	}
	for field, metadata := range eventCollection.Fields {
		if metadata.PrimaryKey {
			t.Errorf("keyless relation field %q unexpectedly has primary-key metadata", field)
		}
	}

	doc := "from: {schema: 'ovdb Native', name: 'Order Details'}\nwhere: {op: '==', left: {field: 'Status Name'}, right: {value: keep}}\nlimit: 5\n"
	read := relHTTPPost(t, base, "/v1/databases/native/dtql", "", doc)
	if read.status != http.StatusOK {
		t.Fatalf("filtered native read: %d %s", read.status, read.raw)
	}
	var result struct {
		Records []struct {
			Data map[string]json.RawMessage `json:"data"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(read.raw), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("filtered records = %d, want one: %s", len(result.Records), read.raw)
	}
	data := result.Records[0].Data
	for field, want := range map[string]string{
		"Order ID":     "9007199254740993",
		"Total Amount": `"123456789012345678.120000"`,
		"Created On":   `"2025-03-04"`,
		"Payload Data": `{"n":9007199254740993}`,
		"Binary Data":  `"AP8Q"`,
	} {
		if got := string(data[field]); got != want {
			t.Errorf("%s = %s, want %s", field, got, want)
		}
	}
	keylessDoc := "from: {schema: 'ovdb Native', name: 'Order Events'}\nwhere: {op: '==', left: {field: 'Event Name'}, right: {value: open}}\nlimit: 5\n"
	keylessRead := relHTTPPost(t, base, "/v1/databases/native/dtql", "", keylessDoc)
	if keylessRead.status != http.StatusOK {
		t.Fatalf("bounded keyless read: %d %s", keylessRead.status, keylessRead.raw)
	}
	var keylessResult struct {
		Records []struct {
			Data map[string]json.RawMessage `json:"data"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(keylessRead.raw), &keylessResult); err != nil {
		t.Fatal(err)
	}
	if len(keylessResult.Records) != 2 {
		t.Fatalf("keyless query returned %d rows, want both duplicate rows: %s", len(keylessResult.Records), keylessRead.raw)
	}
	var beforeValue int64
	var beforeCalled bool
	if err := admin.QueryRow(`SELECT last_value, is_called FROM "ovdb Native"."side effect seq"`).Scan(&beforeValue, &beforeCalled); err != nil {
		t.Fatal(err)
	}
	mutationRead := relHTTPPost(t, base, "/v1/databases/native/dtql", "", "from: {schema: 'ovdb Native', name: 'Mutation Attempt'}\nlimit: 1\n")
	if mutationRead.status < http.StatusBadRequest {
		t.Fatalf("volatile view unexpectedly succeeded in native read-only mount: %d %s", mutationRead.status, mutationRead.raw)
	}
	var afterValue int64
	var afterCalled bool
	if err := admin.QueryRow(`SELECT last_value, is_called FROM "ovdb Native"."side effect seq"`).Scan(&afterValue, &afterCalled); err != nil {
		t.Fatal(err)
	}
	if beforeValue != afterValue || beforeCalled != afterCalled {
		t.Fatalf("volatile view changed sequence state: before=(%d,%v) after=(%d,%v)", beforeValue, beforeCalled, afterValue, afterCalled)
	}
	const restrictedRole = `"ovdb native insert reader"`
	if _, err := admin.Exec(`CREATE ROLE ` + restrictedRole + ` LOGIN PASSWORD 'test-only-password'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP OWNED BY ` + restrictedRole); err != nil {
			t.Errorf("drop privileges owned by test role: %v", err)
		}
		if _, err := admin.Exec(`DROP ROLE IF EXISTS ` + restrictedRole); err != nil {
			t.Errorf("drop test role: %v", err)
		}
	})
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA "ovdb Native" TO ` + restrictedRole,
		`GRANT SELECT ON "ovdb Native"."Order Events" TO ` + restrictedRole,
		`GRANT INSERT ON "ovdb Native"."Order Details" TO ` + restrictedRole,
	} {
		if _, err := admin.Exec(grant); err != nil {
			t.Fatal(err)
		}
	}
	restrictedURL, err := url.Parse(os.Getenv(pgITDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	restrictedURL.User = url.UserPassword("ovdb native insert reader", "test-only-password")
	restrictedQuery := restrictedURL.Query()
	restrictedQuery.Set("application_name", "ovdb-it-select-visibility")
	restrictedURL.RawQuery = restrictedQuery.Encode()
	t.Setenv(pgITMountDSNEnv, restrictedURL.String())
	restricted, err := mount.File(path)
	if err != nil {
		t.Fatalf("mount SELECT-restricted role: %v", err)
	}
	t.Cleanup(func() { _ = restricted.Close() })
	restrictedCollections, err := restricted.Collections(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restrictedCollections, []string{eventID}) {
		t.Fatalf("SELECT-restricted role collections = %q, want only %q", restrictedCollections, eventID)
	}
	write := relHTTPDo(t, base, http.MethodPost, "/v1/databases/native/records/"+url.PathEscape(id)+"/new", "", `{"Status Name":"write"}`, map[string]string{"Content-Type": "application/json"})
	if write.status < http.StatusBadRequest {
		t.Fatalf("native keyed write unexpectedly succeeded: %d %s", write.status, write.raw)
	}
	pgITCanary(t, admin)
}

// The documents of the relational tests. DB names the database of every source.
const (
	pgITJoin = `from:
  database: DB
  name: orders
  alias: o
  joins:
    - type: inner
      from: {database: DB, name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
orderBy:
  - {field: id, source: o}
columns:
  - {field: id, source: o, as: order_id}
  - {field: name, source: c, as: customer}
  - {field: total, source: o}
`
	pgITGroupHaving = `from: {database: DB, name: orders, alias: o}
groupBy: [{field: customer_id, source: o}]
having:
  op: '>'
  left: {aggregate: {function: sum, args: [{field: total, source: o}]}}
  right: {value: 100}
orderBy: [{field: customer_id, source: o}]
columns:
  - {field: customer_id, source: o}
  - {aggregate: {function: count, args: [{star: true}]}, as: orders}
  - {aggregate: {function: sum, args: [{field: total, source: o}]}, as: revenue}
`
	pgITJoinGroupHaving = `from:
  database: DB
  name: orders
  alias: o
  joins:
    - type: inner
      from: {database: DB, name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
groupBy: [{field: country, source: c}]
having:
  op: '>='
  left: {aggregate: {function: count, args: [{star: true}]}}
  right: {value: 2}
orderBy: [{field: country, source: c}]
columns:
  - {field: country, source: c}
  - {aggregate: {function: count, args: [{star: true}]}, as: orders}
  - {aggregate: {function: sum, args: [{field: total, source: o}]}, as: revenue}
`
	pgITExists = `from: {database: DB, name: customers, alias: c}
where:
  exists:
    query:
      from: {database: DB, name: orders, alias: e}
      where:
        and:
          - {op: '==', left: {field: customer_id, source: e}, right: {field: id, source: c}}
          - {op: '>', left: {field: total, source: e}, right: {value: 200}}
orderBy: [{field: id, source: c}]
columns:
  - {field: name, source: c}
`
	pgITIn = `from: {database: DB, name: customers, alias: c}
where:
  op: In
  left: {field: id, source: c}
  right:
    query:
      from: {database: DB, name: orders, alias: p}
      where: {op: '==', left: {field: status, source: p}, right: {value: open}}
      columns: [{field: customer_id, source: p}]
orderBy: [{field: name, source: c}]
columns:
  - {field: name, source: c}
  - {field: country, source: c}
`
)

func pgITDoc(doc, database string) string { return strings.ReplaceAll(doc, "DB", database) }

// pgITRoute reads execution.route of a relational answer.
func pgITRoute(resp relHTTPResponse) string {
	execution, _ := resp.body["execution"].(map[string]any)
	route, _ := execution["route"].(string)
	return route
}

// TestPostgresIntegration_RelationalDocumentsAnswerAsSQLiteDoes: a join, a GROUP BY
// with HAVING, a join under a GROUP BY with HAVING, and two subqueries are
// answered on /v1/dtql by a PostgreSQL mount as by a SQLite mount holding the same
// rows. A document of one database without a subquery runs in the database (the
// route the answer reports); one with a subquery runs in the engine of the server.
func TestPostgresIntegration_RelationalDocumentsAnswerAsSQLiteDoes(t *testing.T) {
	admin := pgITAdmin(t)
	pg, lite := pgITMount(t, "pg", "ovdb-it-relational", true), pgITLite(t, "lite")
	base := pgITServe(t, map[string]*core.Database{"pg": pg, "lite": lite})
	pgITSeed(t, base, "pg")
	pgITSeed(t, base, "lite")

	for _, c := range []struct {
		name  string
		doc   string
		route string
		want  []map[string]any
	}{
		{"a join", pgITJoin, "database", []map[string]any{
			{"order_id": "o1", "customer": "Ada", "total": float64(100)},
			{"order_id": "o2", "customer": "Ada", "total": float64(250)},
			{"order_id": "o3", "customer": "Bob", "total": float64(75)},
			{"order_id": "o4", "customer": "Cy", "total": float64(300)},
			{"order_id": "o5", "customer": "Cy", "total": float64(20)},
		}},
		{"a GROUP BY with HAVING", pgITGroupHaving, "database", []map[string]any{
			{"customer_id": "c1", "orders": float64(2), "revenue": float64(350)},
			{"customer_id": "c3", "orders": float64(2), "revenue": float64(320)},
		}},
		{"a join under a GROUP BY with HAVING", pgITJoinGroupHaving, "database", []map[string]any{
			{"country": "IE", "orders": float64(4), "revenue": float64(670)},
		}},
		{"an EXISTS subquery", pgITExists, "in-memory", []map[string]any{{"name": "Ada"}, {"name": "Cy"}}},
		{"an IN subquery", pgITIn, "in-memory", []map[string]any{{"name": "Bob", "country": "US"}, {"name": "Cy", "country": "IE"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp := pgITBoth(t, base, http.MethodPost, "/v1/dtql", "/v1/dtql", pgITDoc(c.doc, "pg"), pgITDoc(c.doc, "lite"))
			relIntRowsAre(t, resp, c.want)
			if route := pgITRoute(resp); route != c.route {
				t.Errorf("route = %q, want %q", route, c.route)
			}
		})
	}
	pgITCanary(t, admin)
}

// TestPostgresIntegration_CrossDatabaseJoin: a join between a PostgreSQL mount and a
// SQLite mount is answered as the same join between two SQLite mounts is.
func TestPostgresIntegration_CrossDatabaseJoin(t *testing.T) {
	admin := pgITAdmin(t)
	pg, lite, other := pgITMount(t, "pg", "ovdb-it-cross", true), pgITLite(t, "lite"), pgITLite(t, "other")
	base := pgITServe(t, map[string]*core.Database{"pg": pg, "lite": lite, "other": other})
	pgITSeed(t, base, "pg")
	pgITSeed(t, base, "lite")
	pgITSeed(t, base, "other")

	const doc = `from:
  database: ORDERS
  name: orders
  alias: o
  joins:
    - type: inner
      from: {database: CUSTOMERS, name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
orderBy:
  - {field: id, source: o}
columns:
  - {field: id, source: o, as: order_id}
  - {field: name, source: c, as: customer}
  - {field: total, source: o}
`
	across := func(orders, customers string) string {
		return strings.NewReplacer("ORDERS", orders, "CUSTOMERS", customers).Replace(doc)
	}
	for name, c := range map[string]struct{ orders, customers string }{
		"orders in PostgreSQL, customers in SQLite": {"pg", "lite"},
		"orders in SQLite, customers in PostgreSQL": {"lite", "pg"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := pgITBoth(t, base, http.MethodPost, "/v1/dtql", "/v1/dtql", across(c.orders, c.customers), across("lite", "other"))
			if route := pgITRoute(resp); route != "in-memory" {
				t.Errorf("route = %q, want in-memory", route)
			}
			if rows := resp.rows(t); len(rows) != 5 || rows[0]["customer"] != "Ada" {
				t.Fatalf("rows = %v", rows)
			}
		})
	}
	pgITCanary(t, admin)
}

// pgITValueProbes are texts that would end a string, a statement or a line of a
// statement if they were written into SQL text.
var pgITValueProbes = []string{
	`x'`,
	`x''`,
	`x'; DROP TABLE ovdb_canary; --`,
	`x"; DROP TABLE ovdb_canary; --`,
	`x'); DELETE FROM ovdb_canary; --`,
	`x'; UPDATE ovdb_canary SET note = 'changed'; SELECT 'y`,
	`/* comment */ x`,
	`x -- comment`,
	`\`,
	`%`,
}

// pgITFieldProbes are names that would end a quoted name, a statement or a line if
// they were written into SQL text.
var pgITFieldProbes = []string{
	`name"`,
	`name""`,
	`name; DROP TABLE ovdb_canary`,
	`name'; DROP TABLE ovdb_canary; --`,
	`name--`,
	`name/**/`,
	`"name"`,
	`name) OR (1=1`,
}

// pgITCollectionProbes are collection names of the same kinds, and the name of a
// table of the database that no manifest declares.
var pgITCollectionProbes = []string{
	`customers"`,
	`customers""`,
	`customers; DROP TABLE ovdb_canary`,
	`customers"; DROP TABLE ovdb_canary; --`,
	`customers--`,
	`ovdb_canary`,
	`"customers"`,
	`CUSTOMERS`,
}

// TestPostgresIntegration_ProbesAreRowsAnEmptyResultOrARefusal: a quote, a doubled
// quote, a semicolon that starts a second statement and a comment marker, in a
// value, in a field name and in a collection name, on every route that takes one:
// each is a record that is stored and found as it was written, an empty result or a
// refusal (a 4xx), and a canary table that no manifest declares is what it was.
//
// A value against a text field is bound, so it finds its record or nothing. A value that
// does not fit its field (a word against the integer field total, a number or a boolean
// against the text field name), a field the table does not have, a dotted name and a
// qualifier no source has are the caller's mistakes, and each is a 400 invalid_dtql on
// every route, never a 500: the server logs no error for any probe of the test.
func TestPostgresIntegration_ProbesAreRowsAnEmptyResultOrARefusal(t *testing.T) {
	admin := pgITAdmin(t)
	pg := pgITMount(t, "pg", "ovdb-it-probes", true)
	base, logs := pgITServeLogged(t, map[string]*core.Database{"pg": pg})
	pgITSeed(t, base, "pg")

	// A value is bound: a record written with it is found by it, exactly, and by
	// nothing else.
	for i, probe := range pgITValueProbes {
		encoded, err := json.Marshal(probe)
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("probe%d", i)
		if resp := relHTTPDo(t, base, http.MethodPut, "/v1/databases/pg/records/customers/"+id, "", `{"data":{"name":`+string(encoded)+`,"country":"ZZ"}}`, nil); resp.status != http.StatusNoContent {
			t.Fatalf("PUT of the value %q: status %d: %s", probe, resp.status, resp.raw)
		}
		wire := relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":"customers","where":[{"field":"name","op":"==","value":`+string(encoded)+`}]}`, nil)
		if wire.status != http.StatusOK || !reflect.DeepEqual(pgITKeys(wire), []string{"customers/" + id}) {
			t.Errorf("/query by the value %q: status %d: %s", probe, wire.status, wire.raw)
		}
		dtql := relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: customers}\nwhere: {op: '==', left: {field: name}, right: {value: "+string(encoded)+"}}\n", nil)
		if dtql.status != http.StatusOK || !reflect.DeepEqual(pgITKeys(dtql), []string{"customers/" + id}) {
			t.Errorf("/dtql by the value %q: status %d: %s", probe, dtql.status, dtql.raw)
		}
		relational := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: customers, alias: c}\nwhere: {op: '==', left: {field: name, source: c}, right: {value: "+string(encoded)+"}}\ncolumns: [{field: id, source: c}]\n", nil)
		if relational.status != http.StatusOK || len(relational.rows(t)) != 1 {
			t.Errorf("/v1/dtql by the value %q: status %d: %s", probe, relational.status, relational.raw)
		}
		if got := relHTTPDo(t, base, http.MethodGet, "/v1/databases/pg/records/customers/"+id, "", "", nil); !strings.Contains(got.raw, string(encoded)) {
			t.Errorf("the record written with the value %q reads back as %s", probe, got.raw)
		}
	}

	// A value that does not fit its field is the caller's mistake: a 400, on every route.
	mistakes := 0
	mistake := func(what string, resp relHTTPResponse) {
		t.Helper()
		mistakes++
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" {
			t.Errorf("%s: status %d, want 400 invalid_dtql: %s", what, resp.status, resp.raw)
		}
	}
	against := func(collection, alias, field, encoded string) {
		t.Helper()
		what := fmt.Sprintf("the value %s against %s.%s", encoded, collection, field)
		mistake("/query, "+what, relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":"`+collection+`","where":[{"field":"`+field+`","op":"==","value":`+encoded+`}]}`, nil))
		mistake("/query, in, "+what, relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":"`+collection+`","where":[{"field":"`+field+`","op":"in","value":[`+encoded+`]}]}`, nil))
		mistake("/dtql, "+what, relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: "+collection+"}\nwhere: {op: '==', left: {field: "+field+"}, right: {value: "+encoded+"}}\n", nil))
		mistake("/v1/dtql, "+what, relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: "+collection+", alias: "+alias+"}\nwhere: {op: '==', left: {field: "+field+", source: "+alias+"}, right: {value: "+encoded+"}}\ncolumns: [{field: id, source: "+alias+"}]\n", nil))
	}
	for _, probe := range pgITValueProbes {
		encoded, _ := json.Marshal(probe)
		against("orders", "o", "total", string(encoded))
	}
	for _, encoded := range []string{"5", "1.5", "true"} {
		against("customers", "c", "name", encoded)
	}
	// A field the table does not have, a dotted name and a qualifier no source has.
	mistake("/query, an undeclared field in where", relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":"customers","where":[{"field":"nosuch","op":"==","value":"x"}]}`, nil))
	mistake("/query, an undeclared field in orderBy", relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":"customers","orderBy":[{"field":"nosuch"}]}`, nil))
	mistake("/query, a dotted name", relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":"customers","where":[{"field":"name.first","op":"==","value":"x"}]}`, nil))
	mistake("/dtql, an undeclared field in where", relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: customers}\nwhere: {op: '==', left: {field: nosuch}, right: {value: x}}\n", nil))
	mistake("/dtql, an undeclared field in orderBy", relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: customers}\norderBy: [{field: nosuch}]\n", nil))
	mistake("/dtql, an undeclared column", relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: customers}\ncolumns: [{field: nosuch}]\n", nil))
	mistake("/v1/dtql, an undeclared field", relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: customers, alias: c}\nwhere: {op: '==', left: {field: nosuch, source: c}, right: {value: x}}\n", nil))
	mistake("/v1/dtql, an undeclared column", relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: customers, alias: c}\ncolumns: [{field: nosuch, source: c}]\n", nil))
	mistake("/v1/dtql, a qualifier no source has", relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: customers, alias: c}\nwhere: {op: '==', left: {field: name, source: x}, right: {value: x}}\n", nil))
	mistake("/v1/dtql, a qualifier no source has, in a join", relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", pgITDoc(pgITJoin, "pg")+"where: {op: '==', left: {field: name, source: x}, right: {value: x}}\n", nil))
	mistake("/v1/dtql, an undeclared field of a joined source", relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", strings.Replace(pgITDoc(pgITJoin, "pg"), "{field: name, source: c, as: customer}", "{field: nosuch, source: c, as: customer}", 1), nil))
	// A name that a column carries as its alias is read as the alias only in HAVING and ORDER BY,
	// outside an aggregate, and in the spelling the column wrote: anywhere else it is a name no
	// column has, which the server refuses before the driver (and which the adapter would fail as
	// a plain error). An alias over the 63 bytes of a name in PostgreSQL is refused the same way.
	mistake("/v1/dtql, a name that is only an alias, in where", relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: customers}\ncolumns: [{field: name, as: x}]\nwhere: {op: '==', left: {field: x}, right: {value: Ada}}\n", nil))
	mistake("/v1/dtql, a name that is only an alias, in group by", relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: customers}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\ngroupBy: [{field: n}]\n", nil))
	mistake("/v1/dtql, a name that is only an alias, in an aggregate", relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: customers}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\nhaving: {op: '>', left: {aggregate: {function: sum, args: [{field: n}]}}, right: {value: 1}}\n", nil))
	mistake("/dtql, a name that is only an alias, in where", relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: customers}\ncolumns: [{field: name, as: x}]\nwhere: {op: '==', left: {field: x}, right: {value: Ada}}\n", nil))
	mistake("/v1/dtql, a source alias over the length of a name", relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: customers, alias: "+strings.Repeat("a", 64)+"}\ncolumns: [{field: id, source: "+strings.Repeat("a", 64)+"}]\n", nil))
	if want := (len(pgITValueProbes)+3)*4 + 16; mistakes != want {
		t.Errorf("%d probes of a mistake were made, want %d", mistakes, want)
	}

	// A name is refused or finds nothing: it is never part of a statement.
	refused := 0
	check := func(what string, resp relHTTPResponse) {
		t.Helper()
		switch {
		case resp.status >= 400 && resp.status < 500:
			refused++
		case resp.status == http.StatusOK:
			if records, _ := resp.body["records"].([]any); len(records) != 0 && !strings.Contains(what, "value") {
				t.Errorf("%s: a name that no column has found rows: %s", what, resp.raw)
			}
		default:
			t.Errorf("%s: status %d, want an answer or a refusal: %s", what, resp.status, resp.raw)
		}
	}
	for _, field := range pgITFieldProbes {
		encoded, _ := json.Marshal(field)
		check("/query where on the field "+field, relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":"customers","where":[{"field":`+string(encoded)+`,"op":"==","value":"x"}]}`, nil))
		check("/query orderBy the field "+field, relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":"customers","orderBy":[{"field":`+string(encoded)+`}]}`, nil))
		check("/dtql where on the field "+field, relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: customers}\nwhere: {op: '==', left: {field: "+string(encoded)+"}, right: {value: x}}\n", nil))
		check("/dtql column "+field, relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: customers}\ncolumns: [{field: "+string(encoded)+"}]\n", nil))
		check("/v1/dtql column "+field, relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: customers, alias: c}\ncolumns: [{field: "+string(encoded)+", source: c}]\n", nil))
		check("PUT of the field "+field, relHTTPDo(t, base, http.MethodPut, "/v1/databases/pg/records/customers/fieldprobe", "", `{"data":{`+string(encoded)+`:"x"}}`, nil))
	}
	for _, collection := range pgITCollectionProbes {
		encoded, _ := json.Marshal(collection)
		check("/query on the collection "+collection, relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":`+string(encoded)+`}`, nil))
		check("/dtql on the collection "+collection, relHTTPDo(t, base, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: "+string(encoded)+"}\n", nil))
		check("/v1/dtql on the collection "+collection, relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", "from: {database: pg, name: "+string(encoded)+"}\n", nil))
		check("GET of a record of the collection "+collection, relHTTPDo(t, base, http.MethodGet, "/v1/databases/pg/records/"+url.PathEscape(collection)+"/x", "", "", nil))
		check("PUT of a record of the collection "+collection, relHTTPDo(t, base, http.MethodPut, "/v1/databases/pg/records/"+url.PathEscape(collection)+"/x", "", `{"data":{"name":"x"}}`, nil))
	}
	// The probes of a name were refused: a check that every name found nothing would
	// pass for a server that answered 200 to all of them.
	if want := len(pgITFieldProbes)*6 + len(pgITCollectionProbes)*5; refused < want/2 {
		t.Errorf("only %d of %d probes of a name were refused", refused, want)
	}

	// Not one of the probes was a fault of the server: it logs no error record.
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("the server logged an error while it answered the probes:\n%s", logs)
	}

	pgITCanary(t, admin)
	var customers, orders int
	if err := admin.QueryRow("SELECT (SELECT count(*) FROM customers), (SELECT count(*) FROM orders)").Scan(&customers, &orders); err != nil {
		t.Fatal(err)
	}
	if customers != 4+len(pgITValueProbes) || orders != 5 {
		t.Errorf("the tables hold %d customers and %d orders, want %d and 5", customers, orders, 4+len(pgITValueProbes))
	}
}

// pgITBulkJoin joins the orders of the mount to their customers and keeps the few orders whose
// total is at least 12999, every source naming its database.
const pgITBulkJoin = `from:
  database: pg
  name: orders
  alias: o
  joins:
    - type: inner
      from: {database: pg, name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
where: {op: '>=', left: {field: total, source: o}, right: {value: 12999}}
orderBy:
  - {field: total, source: o}
columns:
  - {field: id, source: o, as: order_id}
  - {field: name, source: c, as: customer}
  - {field: total, source: o}
`

// pgITBulkMismatchedJoin is the join above on two columns of types the server cannot equate (an
// integer and a text), which the compiler of the adapter declines: DALgo reads the tables
// and joins them itself.
const pgITBulkMismatchedJoin = `from:
  database: pg
  name: orders
  alias: o
  joins:
    - type: inner
      from: {database: pg, name: customers, alias: c}
      on:
        - {left: {field: total, source: o}, op: '==', right: {field: id, source: c}}
columns:
  - {field: id, source: o, as: order_id}
  - {field: name, source: c, as: customer}
`

// TestPostgresIntegration_AJoinOfOneDatabaseRunsInTheDatabase: a join of two collections of
// the PostgreSQL mount, with the database named on each source as /v1/dtql takes it, is run by
// the server as one statement. The orders table holds 12,005 rows, more than the 10,000 that
// DALgo reads of a table when it joins the rows itself in the transaction, and the join
// answers the two orders its filter keeps: it passes only if the filter reaches the server. A
// join the compiler of the adapter declines (two columns of types it cannot equate) is read
// by DALgo and is bound by it: the answer is a 422 query_budget_exceeded, and the server logs
// no error.
func TestPostgresIntegration_AJoinOfOneDatabaseRunsInTheDatabase(t *testing.T) {
	admin := pgITAdmin(t)
	pg := pgITMount(t, "pg", "ovdb-it-bulk", true)
	base, logs := pgITServeLogged(t, map[string]*core.Database{"pg": pg})
	pgITSeed(t, base, "pg")
	if _, err := admin.Exec(`INSERT INTO orders (id, customer_id, total, status)
		SELECT 'g' || i, 'c' || (i % 4 + 1), 1000 + i, 'bulk' FROM generate_series(1, 12000) AS i`); err != nil {
		t.Fatalf("seed the bulk of orders: %v", err)
	}

	t.Run("one statement over 12,005 rows", func(t *testing.T) {
		resp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", pgITBulkJoin, nil)
		relIntRowsAre(t, resp, []map[string]any{
			{"order_id": "g11999", "customer": "Dee", "total": float64(12999)},
			{"order_id": "g12000", "customer": "Ada", "total": float64(13000)},
		})
		if route := pgITRoute(resp); route != "database" {
			t.Errorf("route = %q, want database", route)
		}
	})
	t.Run("a join the adapter declines is bound by DALgo", func(t *testing.T) {
		resp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", pgITBulkMismatchedJoin, nil)
		if resp.status != http.StatusUnprocessableEntity || resp.errorField("code") != "query_budget_exceeded" {
			t.Fatalf("status %d, want 422 query_budget_exceeded: %s", resp.status, resp.raw)
		}
	})
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("the server logged an error while it answered:\n%s", logs)
	}
	pgITCanary(t, admin)
}

// pgITNameDocs are documents that select a field of people under a spelling of its name
// (SPELLING), alone in one database and joined to the customers of another.
const (
	pgITNameDoc = `from: {database: DB, name: people, alias: p}
where: {op: '==', left: {field: SPELLING, source: p}, right: {value: Ada}}
columns:
  - {field: id, source: p}
  - {field: SPELLING, source: p}
`
	pgITNameJoinDoc = `from:
  database: PEOPLE
  name: people
  alias: p
  joins:
    - type: inner
      from: {database: CUSTOMERS, name: customers, alias: c}
      on:
        - {left: {field: id, source: p}, op: '==', right: {field: id, source: c}}
orderBy:
  - {field: id, source: p}
columns:
  - {field: SPELLING, source: p, as: first}
  - {field: name, source: c, as: customer}
`
)

// TestPostgresIntegration_MixedCaseFieldNames: a manifest that declares a field with
// capitals in its name (FirstName) holds it in a PostgreSQL table folded to lower case.
// A route that reads one collection answers as a SQLite mount does, under the declared
// name, whichever spelling it was found by; a document that runs in the database finds it
// by either spelling and labels the column as the document wrote it. A join across databases reads the names the server holds,
// which are lower case: it answers the lower-case spelling, and refuses the declared one
// (400), where a SQLite mount answers the declared spelling and refuses the lower-case
// one. The limit is stated in docs/api.md.
func TestPostgresIntegration_MixedCaseFieldNames(t *testing.T) {
	admin := pgITAdmin(t)
	pg, lite, other := pgITMount(t, "pg", "ovdb-it-mixed", true), pgITLite(t, "lite"), pgITLite(t, "other")
	base := pgITServe(t, map[string]*core.Database{"pg": pg, "lite": lite, "other": other})
	for _, id := range []string{"pg", "lite", "other"} {
		pgITSeed(t, base, id)
	}

	for _, spelling := range []string{"FirstName", "firstname"} {
		wire := `{"collection":"people","where":[{"field":"` + spelling + `","op":"==","value":"Ada"}],"orderBy":[{"field":"` + spelling + `"}]}`
		t.Run("/query, "+spelling, func(t *testing.T) {
			resp := pgITBoth(t, base, http.MethodPost, "/v1/databases/pg/query", "/v1/databases/lite/query", wire, wire)
			if keys := pgITKeys(resp); !reflect.DeepEqual(keys, []string{"people/c1"}) || !strings.Contains(resp.raw, `"FirstName":"Ada"`) {
				t.Fatalf("keys = %v, the answer is %s, want people/c1 under the declared name", keys, resp.raw)
			}
		})
		doc := "from: {name: people}\nwhere: {op: '==', left: {field: " + spelling + "}, right: {value: Ada}}\n"
		t.Run("/dtql, "+spelling, func(t *testing.T) {
			resp := pgITBoth(t, base, http.MethodPost, "/v1/databases/pg/dtql", "/v1/databases/lite/dtql", doc, doc)
			if keys := pgITKeys(resp); !reflect.DeepEqual(keys, []string{"people/c1"}) || !strings.Contains(resp.raw, `"FirstName":"Ada"`) {
				t.Fatalf("keys = %v, the answer is %s, want people/c1 under the declared name", keys, resp.raw)
			}
		})
		t.Run("/v1/dtql in one database, "+spelling, func(t *testing.T) {
			doc := strings.ReplaceAll(pgITNameDoc, "SPELLING", spelling)
			var resp relHTTPResponse
			if spelling == "FirstName" {
				resp = pgITBoth(t, base, http.MethodPost, "/v1/dtql", "/v1/dtql", pgITDoc(doc, "pg"), pgITDoc(doc, "lite"))
			} else {
				// A SQLite mount reads the folded spelling as the column it is and answers it under
				// the name it was declared with, listing both in its columns. The PostgreSQL mount
				// labels the column as the document wrote it, so it is not compared with it.
				resp = relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", pgITDoc(doc, "pg"), nil)
				if got := resp.columns(); !reflect.DeepEqual(got, []string{"id", spelling}) {
					t.Errorf("columns = %v, want [id %s]", got, spelling)
				}
			}
			relIntRowsAre(t, resp, []map[string]any{{"id": "c1", spelling: "Ada"}})
			if route := pgITRoute(resp); route != "database" {
				t.Errorf("route = %q, want database", route)
			}
		})
	}

	across := func(spelling, people, customers string) string {
		return strings.NewReplacer("SPELLING", spelling, "PEOPLE", people, "CUSTOMERS", customers).Replace(pgITNameJoinDoc)
	}
	// A join of two collections of one database runs in the database, which finds the field by
	// either spelling and labels the column as the document wrote it, as a document of one source does.
	for _, spelling := range []string{"FirstName", "firstname"} {
		t.Run("a join in one database, "+spelling, func(t *testing.T) {
			resp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", across(spelling, "pg", "pg"), nil)
			relIntRowsAre(t, resp, []map[string]any{{"first": "Ada", "customer": "Ada"}, {"first": "Bob", "customer": "Bob"}})
			if route := pgITRoute(resp); route != "database" {
				t.Errorf("route = %q, want database", route)
			}
			if got := resp.columns(); !reflect.DeepEqual(got, []string{"first", "customer"}) {
				t.Errorf("columns = %v, want [first customer]", got)
			}
			if spelling == "FirstName" {
				lite := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", across(spelling, "lite", "lite"), nil)
				if !reflect.DeepEqual(resp.body["records"], lite.body["records"]) {
					t.Fatalf("PostgreSQL: %s\nSQLite:     %s", resp.raw, lite.raw)
				}
			}
		})
	}
	t.Run("a join across databases, the folded spelling", func(t *testing.T) {
		pgResp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", across("firstname", "pg", "lite"), nil)
		liteResp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", across("FirstName", "lite", "other"), nil)
		if pgResp.status != http.StatusOK || liteResp.status != http.StatusOK {
			t.Fatalf("status: PostgreSQL %d (%s), SQLite %d (%s)", pgResp.status, pgResp.raw, liteResp.status, liteResp.raw)
		}
		if !reflect.DeepEqual(pgResp.body["records"], liteResp.body["records"]) || pgITRoute(pgResp) != "in-memory" {
			t.Fatalf("PostgreSQL: %s\nSQLite:     %s", pgResp.raw, liteResp.raw)
		}
		relIntRowsAre(t, pgResp, []map[string]any{{"first": "Ada", "customer": "Ada"}, {"first": "Bob", "customer": "Bob"}})
	})
	t.Run("a join across databases, the declared spelling", func(t *testing.T) {
		resp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", across("FirstName", "pg", "lite"), nil)
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" {
			t.Fatalf("status %d, want 400 invalid_dtql: %s", resp.status, resp.raw)
		}
	})
	// A document with a subquery runs in the engine of this server, over the field list the
	// adapter reads from the catalog, which is lower case, as a join across databases does:
	// the folded spelling answers and the declared one is a 400. The subquery is the
	// part of the document that names the field.
	subquery := func(spelling string) string {
		return "from: {database: pg, name: people, alias: p}\n" +
			"where:\n  exists:\n    query:\n      from: {database: pg, name: people, alias: x}\n" +
			"      where: {op: '==', left: {field: " + spelling + ", source: x}, right: {value: Ada}}\n" +
			"orderBy:\n  - {field: id, source: p}\ncolumns:\n  - {field: id, source: p}\n"
	}
	t.Run("a document with a subquery, the folded spelling", func(t *testing.T) {
		resp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", subquery("firstname"), nil)
		relIntRowsAre(t, resp, []map[string]any{{"id": "c1"}, {"id": "c2"}})
		if route := pgITRoute(resp); route != "in-memory" {
			t.Errorf("route = %q, want in-memory: a document with a subquery runs in the engine of the server", route)
		}
	})
	t.Run("a document with a subquery, the declared spelling", func(t *testing.T) {
		resp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", subquery("FirstName"), nil)
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" {
			t.Fatalf("status %d, want 400 invalid_dtql: %s", resp.status, resp.raw)
		}
	})
	// A scalar subquery of one source is evaluated by DALgo row by row, and DALgo checks no
	// field of it: the declared spelling, in its WHERE or in its column, is read as a null
	// and the document is answered (200), where the same spelling in the query of an EXISTS
	// is a 400. The folded spelling answers the value.
	scalar := func(where, selected string) string {
		return "from: {database: pg, name: people, alias: p}\norderBy:\n  - {field: id, source: p}\n" +
			"columns:\n  - {field: id, source: p}\n  - query:\n      as: s\n      from: {database: pg, name: people, alias: x}\n" +
			"      where: {op: '==', left: {field: " + where + ", source: x}, right: {value: Ada}}\n" +
			"      columns: [{field: " + selected + ", source: x}]\n"
	}
	for _, c := range []struct {
		name, where, selected string
		want                  any
	}{
		{"the folded spelling", "firstname", "city", "Dublin"},
		{"the declared spelling in its WHERE", "FirstName", "city", nil},
		{"the declared spelling in its column", "firstname", "FirstName", nil},
	} {
		t.Run("a scalar subquery of one source, "+c.name, func(t *testing.T) {
			resp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", scalar(c.where, c.selected), nil)
			relIntRowsAre(t, resp, []map[string]any{{"id": "c1", "s": c.want}, {"id": "c2", "s": c.want}})
			if route := pgITRoute(resp); route != "in-memory" {
				t.Errorf("route = %q, want in-memory: a document with a subquery runs in the engine of the server", route)
			}
		})
	}
	pgITCanary(t, admin)
}

// TestPostgresIntegration_WithoutTheSwitchEveryQueryRouteRefusesAndNoStatementIsSent:
// a PostgreSQL mount that read the switch off answers every route that takes a
// structured query with the 501 it always gave, and the server is sent no statement
// for any of them (its own list of sessions says so). Key reads and writes still
// work. The same check, made with the switch on, does see the statements: it is the
// control of the observation.
func TestPostgresIntegration_WithoutTheSwitchEveryQueryRouteRefusesAndNoStatementIsSent(t *testing.T) {
	admin := pgITAdmin(t)
	off, lite := pgITMount(t, "pg", "ovdb-it-off", false), pgITLite(t, "lite")
	base := pgITServe(t, map[string]*core.Database{"pg": off, "lite": lite})
	if pgITSessions(t, admin, "ovdb-it-off") == 0 {
		t.Fatal("the server holds no session of the mount: the observation below would prove nothing")
	}

	routes := []struct{ name, method, path, body string }{
		{"wire query, POST", http.MethodPost, "/v1/databases/pg/query", `{"collection":"customers"}`},
		{"wire query, GET", http.MethodGet, "/v1/databases/pg/query?q=" + url.QueryEscape(`{"collection":"customers"}`), ""},
		{"wire query, keys only", http.MethodPost, "/v1/databases/pg/query", `{"collection":"customers","keysOnly":true}`},
		{"DTQL of the database, POST", http.MethodPost, "/v1/databases/pg/dtql", "from: {name: customers}\n"},
		{"DTQL of the database, GET", http.MethodGet, "/v1/databases/pg/dtql?q=" + url.QueryEscape("from: {name: customers}\n"), ""},
		{"DTQL of the database, a snapshot", http.MethodPost, "/v1/databases/pg/dtql", "from: {name: customers}\n"},
		{"relational document, alone", http.MethodPost, "/v1/dtql", "from: {database: pg, name: customers}\n"},
		{"relational document, joined", http.MethodPost, "/v1/dtql", pgITDoc(pgITJoin, "pg")},
		{"relational document, joined to SQLite", http.MethodPost, "/v1/dtql", strings.Replace(pgITDoc(pgITJoin, "pg"), "database: pg, name: customers", "database: lite, name: customers", 1)},
	}
	at := pgITNow(t, admin)
	for _, route := range routes {
		headers := map[string]string(nil)
		if strings.Contains(route.name, "snapshot") {
			headers = map[string]string{"OVDB-Page-Size": "2"}
		}
		resp := relHTTPDo(t, base, route.method, route.path, "", route.body, headers)
		if resp.status != http.StatusNotImplemented || resp.errorField("code") != "query_unsupported" {
			t.Errorf("%s: status %d, want 501 query_unsupported: %s", route.name, resp.status, resp.raw)
		}
	}
	if n := pgITStatementsSince(t, admin, "ovdb-it-off", at); n != 0 {
		t.Errorf("%d sessions of the mount started a statement while every route refused", n)
	}

	// The control: with the switch on the same observation sees the statements.
	on := pgITMount(t, "pgon", "ovdb-it-on", true)
	onBase := pgITServe(t, map[string]*core.Database{"pgon": on})
	at = pgITNow(t, admin)
	if resp := relHTTPDo(t, onBase, http.MethodPost, "/v1/databases/pgon/query", "", `{"collection":"customers"}`, nil); resp.status != http.StatusOK {
		t.Fatalf("a query of the mount that read the switch on: status %d: %s", resp.status, resp.raw)
	}
	if n := pgITStatementsSince(t, admin, "ovdb-it-on", at); n == 0 {
		t.Error("the server reports no statement for a query that was answered: the observation does not see statements")
	}

	// Key reads and writes answer as they always did, with the switch off.
	if resp := relHTTPDo(t, base, http.MethodPut, "/v1/databases/pg/records/customers/k1", "", `{"data":{"name":"Key","country":"IE"}}`, nil); resp.status != http.StatusNoContent {
		t.Fatalf("PUT: status %d: %s", resp.status, resp.raw)
	}
	if resp := relHTTPDo(t, base, http.MethodGet, "/v1/databases/pg/records/customers/k1", "", "", nil); resp.status != http.StatusOK || !strings.Contains(resp.raw, `"name":"Key"`) {
		t.Fatalf("GET: status %d: %s", resp.status, resp.raw)
	}
	pgITCanary(t, admin)
}

// pgITMajorEnv names the major version of the server the job runs against (17 or 18).
// The job sets it from its matrix, so a leg that met another server fails.
const pgITMajorEnv = "OVDB_TEST_POSTGRES_MAJOR"

// pgITServerMajor is the major version of the server the administration connection
// reached.
func pgITServerMajor(t *testing.T, admin *sql.DB) int {
	t.Helper()
	var number int
	if err := admin.QueryRow("SELECT current_setting('server_version_num')::int").Scan(&number); err != nil {
		t.Fatal(err)
	}
	return number / 10000
}

// TestPostgresIntegration_ServerIsTheVersionTheLegNames: the job runs these tests on two
// server versions, and a row that holds on one only (the ordering over a column whose
// not-null constraint is not validated shows only on PostgreSQL 18) proves nothing if a
// leg met the wrong server. The leg names its version in OVDB_TEST_POSTGRES_MAJOR.
func TestPostgresIntegration_ServerIsTheVersionTheLegNames(t *testing.T) {
	admin := pgITAdmin(t)
	want := os.Getenv(pgITMajorEnv)
	if want == "" {
		t.Skipf("%s is not set: the job names the version of its leg there", pgITMajorEnv)
	}
	if got := fmt.Sprint(pgITServerMajor(t, admin)); got != want {
		t.Fatalf("the server is PostgreSQL %s, the leg names %s", got, want)
	}
}

// TestPostgresIntegration_OrderOverAColumnWithAnUnvalidatedNotNullConstraint: since
// PostgreSQL 18 a column can be marked not null by a constraint added NOT VALID while rows
// that were there hold NULL, and the catalog then says the column is not null. DALgo's rule
// puts a NULL first in an ascending order and last in a descending one, and the compiler
// writes the NULLS clause that makes PostgreSQL do so unless the column cannot hold one;
// before dalgo2sql v0.26.7 it left the clause out for such a column and PostgreSQL put the
// NULL last in an ascending order and first in a descending one. The rows asserted are the
// ones a SQLite mount holding the same rows answers, with a limit, in both directions, on
// /query, on /dtql and on a relational document. On PostgreSQL 17, which has no such
// constraint, the same table holds the column as nullable and the rows are the same; the row
// shows the fix only on 18, which the job runs beside 17.
func TestPostgresIntegration_OrderOverAColumnWithAnUnvalidatedNotNullConstraint(t *testing.T) {
	admin := pgITAdmin(t)
	pg, lite := pgITMount(t, "pg", "ovdb-it-order", true), pgITLite(t, "lite")
	base := pgITServe(t, map[string]*core.Database{"pg": pg, "lite": lite})
	for _, id := range []string{"pg", "lite"} {
		pgITSeed(t, base, id)
		// An order with no total: a NULL in the column, before any constraint covers it.
		if resp := relHTTPDo(t, base, http.MethodPut, "/v1/databases/"+id+"/records/orders/o6", "", `{"data":{"customer_id":"c9","status":"draft"}}`, nil); resp.status != http.StatusNoContent {
			t.Fatalf("PUT orders/o6 into %s: status %d: %s", id, resp.status, resp.raw)
		}
	}
	var holdsNull int
	if err := admin.QueryRow("SELECT count(*) FROM orders WHERE total IS NULL").Scan(&holdsNull); err != nil || holdsNull != 1 {
		t.Fatalf("the table holds %d rows with no total (%v), want 1", holdsNull, err)
	}
	if major := pgITServerMajor(t, admin); major >= 18 {
		if _, err := admin.Exec("ALTER TABLE orders ADD CONSTRAINT orders_total_not_null NOT NULL total NOT VALID"); err != nil {
			t.Fatalf("add the not-null constraint NOT VALID: %v", err)
		}
		// The condition that shows the fault is in place: the catalog says the column is
		// not null, and the constraint that says so is not validated.
		var notNull, unvalidated bool
		if err := admin.QueryRow(`SELECT a.attnotnull,
			EXISTS (SELECT 1 FROM pg_catalog.pg_constraint c WHERE c.conrelid = a.attrelid AND c.contype = 'n' AND a.attnum = ANY (c.conkey) AND NOT c.convalidated)
			FROM pg_catalog.pg_attribute a WHERE a.attrelid = 'orders'::regclass AND a.attname = 'total'`).Scan(&notNull, &unvalidated); err != nil {
			t.Fatal(err)
		}
		if !notNull || !unvalidated {
			t.Fatalf("on PostgreSQL %d the column is attnotnull=%v with an unvalidated constraint=%v: the row would show nothing", major, notNull, unvalidated)
		}
	}

	ascending := []string{"orders/o6", "orders/o5", "orders/o3", "orders/o1", "orders/o2", "orders/o4"}
	descending := []string{"orders/o4", "orders/o2", "orders/o1", "orders/o3", "orders/o5", "orders/o6"}
	for _, direction := range []struct {
		name string
		desc bool
		keys []string
	}{{"ascending", false, ascending}, {"descending", true, descending}} {
		for _, limit := range []int{2, 6} {
			want := direction.keys[:limit]
			wire := fmt.Sprintf(`{"collection":"orders","orderBy":[{"field":"total","desc":%v}],"limit":%d}`, direction.desc, limit)
			doc := fmt.Sprintf("from: {name: orders}\norderBy: [{field: total, desc: %v}]\nlimit: %d\n", direction.desc, limit)
			t.Run(fmt.Sprintf("/query, %s, limit %d", direction.name, limit), func(t *testing.T) {
				resp := pgITBoth(t, base, http.MethodPost, "/v1/databases/pg/query", "/v1/databases/lite/query", wire, wire)
				if keys := pgITKeys(resp); !reflect.DeepEqual(keys, want) {
					t.Fatalf("keys = %v, want %v", keys, want)
				}
			})
			t.Run(fmt.Sprintf("/dtql, %s, limit %d", direction.name, limit), func(t *testing.T) {
				resp := pgITBoth(t, base, http.MethodPost, "/v1/databases/pg/dtql", "/v1/databases/lite/dtql", doc, doc)
				if keys := pgITKeys(resp); !reflect.DeepEqual(keys, want) {
					t.Fatalf("keys = %v, want %v", keys, want)
				}
			})
			t.Run(fmt.Sprintf("relational document, %s, limit %d", direction.name, limit), func(t *testing.T) {
				relational := fmt.Sprintf("from: {database: DB, name: orders, alias: o}\ncolumns:\n  - {field: id, source: o}\n  - {field: total, source: o}\norderBy: [{field: total, source: o, desc: %v}]\nlimit: %d\n", direction.desc, limit)
				resp := pgITBoth(t, base, http.MethodPost, "/v1/dtql", "/v1/dtql", pgITDoc(relational, "pg"), pgITDoc(relational, "lite"))
				rows := resp.rows(t)
				if len(rows) != len(want) {
					t.Fatalf("rows = %v, want %d rows", rows, len(want))
				}
				for i, key := range want {
					if got, _ := rows[i]["id"].(string); got != strings.TrimPrefix(key, "orders/") {
						t.Fatalf("rows = %v, want the orders %v in this order", rows, want)
					}
				}
			})
		}
	}
	pgITCanary(t, admin)
}

// TestPostgresIntegration_NamesOver63BytesAreA400AndNoStatementIsSent: PostgreSQL keeps 63
// bytes of a name and cuts the rest, so a name of 64 bytes would address the table or
// the column named by its first 63. Each route that takes a name (a key, a write, a
// query, a relational document) answers 400 with the code of its kind and the message
// that gives the limit, and the server's own list of sessions shows that no statement
// was started for any of them. The control, a request the mount answers, does show a
// statement, so the observation sees statements.
func TestPostgresIntegration_NamesOver63BytesAreA400AndNoStatementIsSent(t *testing.T) {
	admin := pgITAdmin(t)
	pg, lite := pgITMount(t, "pg", "ovdb-it-names", true), pgITLite(t, "lite")
	base := pgITServe(t, map[string]*core.Database{"pg": pg, "lite": lite})
	long := strings.Repeat("n", 64)
	routes := []struct{ name, method, path, body, code string }{
		{"key read, a collection", http.MethodGet, "/v1/databases/pg/records/" + long + "/k1", "", "invalid_key"},
		{"key write, a collection", http.MethodPut, "/v1/databases/pg/records/" + long + "/k1", `{"data":{"name":"x"}}`, "invalid_key"},
		{"key write, a field", http.MethodPut, "/v1/databases/pg/records/customers/k1", `{"data":{"` + long + `":"x"}}`, "bad_request"},
		{"key update, a field", http.MethodPatch, "/v1/databases/pg/records/customers/c1", `{"updates":[{"fieldName":"` + long + `","value":"x"}]}`, "bad_request"},
		{"key delete, a collection", http.MethodDelete, "/v1/databases/pg/records/" + long + "/k1", "", "invalid_key"},
		{"batch, a field", http.MethodPost, "/v1/databases/pg/batch", `{"ops":[{"op":"set","key":"customers/k1","data":{"` + long + `":"x"}}]}`, "bad_request"},
		{"wire query, a collection", http.MethodPost, "/v1/databases/pg/query", `{"collection":"` + long + `"}`, "invalid_key"},
		{"wire query, a field", http.MethodPost, "/v1/databases/pg/query", `{"collection":"customers","where":[{"field":"` + long + `","op":"==","value":"x"}]}`, "invalid_dtql"},
		{"DTQL of the database, a collection", http.MethodPost, "/v1/databases/pg/dtql", "from: {name: " + long + "}\n", "invalid_key"},
		{"DTQL of the database, a field", http.MethodPost, "/v1/databases/pg/dtql", "from: {name: customers}\norderBy: [{field: " + long + "}]\n", "invalid_dtql"},
		{"relational document, a collection", http.MethodPost, "/v1/dtql", "from: {database: pg, name: " + long + "}\n", "invalid_key"},
		{"relational document, a field", http.MethodPost, "/v1/dtql", "from: {database: pg, name: customers, alias: c}\nwhere: {op: '==', left: {field: " + long + ", source: c}, right: {value: x}}\n", "invalid_dtql"},
	}
	// The mount must hold a session, or "no statement" would say nothing.
	if pgITSessions(t, admin, "ovdb-it-names") == 0 {
		t.Fatal("the server holds no session of the mount: the observation below would prove nothing")
	}
	at := pgITNow(t, admin)
	for _, route := range routes {
		resp := relHTTPDo(t, base, route.method, route.path, "", route.body, nil)
		if resp.status != http.StatusBadRequest || resp.errorField("code") != route.code {
			t.Errorf("%s: status %d, want 400 %s: %s", route.name, resp.status, route.code, resp.raw)
			continue
		}
		if message := resp.errorField("message"); !strings.Contains(message, "63 bytes") || strings.Contains(resp.raw, long) {
			t.Errorf("%s: the message must give the limit and not the name: %s", route.name, resp.raw)
		}
	}
	if n := pgITStatementsSince(t, admin, "ovdb-it-names", at); n != 0 {
		t.Errorf("%d sessions of the mount started a statement while every route refused a long name", n)
	}

	// A document the server evaluates itself (a join across databases, a document with a
	// subquery) hands the mount a plain scan, which holds no field name: the length is not
	// looked at, the field list is read from the catalog, and a name that no column has is an
	// unknown field (400 invalid_dtql). It is not written into a statement, but the list is
	// one, so these are not part of the observation above.
	for _, c := range []struct{ name, doc string }{
		{"a join across databases", strings.Replace(pgITDoc(pgITJoin, "pg"), "database: pg, name: customers", "database: lite, name: customers", 1) +
			"where: {op: '==', left: {field: " + long + ", source: o}, right: {value: x}}\n"},
		{"a document with a subquery", "from: {database: pg, name: customers, alias: c}\n" +
			"where: {exists: {query: {from: {database: pg, name: orders, alias: o}, where: {op: '==', left: {field: " + long + ", source: o}, right: {value: x}}}}}\n"},
	} {
		resp := relHTTPDo(t, base, http.MethodPost, "/v1/dtql", "", c.doc, nil)
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" {
			t.Errorf("%s, a field of 64 bytes: status %d, want 400 invalid_dtql: %s", c.name, resp.status, resp.raw)
			continue
		}
		// The status and the code are the database route's too; what tells this answer from
		// that one is that it does not give the limit.
		if strings.Contains(resp.errorField("message"), "63 bytes") {
			t.Errorf("%s, a field of 64 bytes: the message gives the limit, which the database route's does and this one does not: %s", c.name, resp.raw)
		}
	}

	// The control: a request the mount answers is seen as a statement.
	at = pgITNow(t, admin)
	if resp := relHTTPDo(t, base, http.MethodPut, "/v1/databases/pg/records/customers/k1", "", `{"data":{"name":"Key"}}`, nil); resp.status != http.StatusNoContent {
		t.Fatalf("PUT: status %d: %s", resp.status, resp.raw)
	}
	if n := pgITStatementsSince(t, admin, "ovdb-it-names", at); n == 0 {
		t.Error("the server reports no statement for a write that was answered: the observation does not see statements")
	}
	pgITCanary(t, admin)
}

// TestPostgresIntegration_ManifestNamesAreCheckedBeforeProvisioning: a declared
// collection or field the adapter cannot keep is refused while loading the
// manifest, before a session opens or an earlier collection is created. Both a
// 64-byte name and a name outside the adapter's identifier rule are covered.
// A manifest with collection and field names of exactly 63 bytes mounts and
// answers a write and read through the server.
func TestPostgresIntegration_ManifestNamesAreCheckedBeforeProvisioning(t *testing.T) {
	admin := pgITAdmin(t)
	const application = "ovdb-it-manifest-names"
	u, err := url.Parse(os.Getenv(pgITDSNEnv))
	if err != nil {
		t.Fatalf("%s is not a URL: %v", pgITDSNEnv, err)
	}
	q := u.Query()
	q.Set("application_name", application)
	u.RawQuery = q.Encode()
	t.Setenv(pgITMountDSNEnv, u.String())
	manifestText := func(collection, field string) string {
		return fmt.Sprintf("database: {id: pg, schema_mode: strict}\nstorage: {engine: postgres, postgres: {dsn_env: %s}}\nschemas:\n  collections:\n    customers: {fields: {name: {type: string}}}\n    %q: {fields: {%q: {type: string}}}\n", pgITMountDSNEnv, collection, field)
	}
	load := func(t *testing.T, collection, field string) (*core.Database, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "db.yaml")
		if err := os.WriteFile(path, []byte(manifestText(collection, field)), 0o600); err != nil {
			t.Fatal(err)
		}
		return mount.File(path)
	}
	long := strings.Repeat("n", 64)
	for _, tc := range []struct{ name, collection, field string }{
		{"collection of 64 bytes", long, "f"},
		{"field of 64 bytes", "orders", long},
		{"collection outside identifier rule", "order details", "f"},
		{"field outside identifier rule", "orders", "a-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := pgITNow(t, admin)
			db, err := load(t, tc.collection, tc.field)
			if db != nil {
				_ = db.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "cannot be mounted on PostgreSQL") || !strings.Contains(err.Error(), "schemas.collections") {
				t.Fatalf("mount = %v, want a manifest entry and the name rule", err)
			}
			if n := pgITSessions(t, admin, application); n != 0 {
				t.Errorf("%d sessions opened for a refused manifest", n)
			}
			if n := pgITStatementsSince(t, admin, application, at); n != 0 {
				t.Errorf("%d sessions started a statement for a refused manifest", n)
			}
			var table sql.NullString
			if err := admin.QueryRow("SELECT to_regclass('customers')::text").Scan(&table); err != nil || table.Valid {
				t.Errorf("an earlier collection was created: %q (%v)", table.String, err)
			}
		})
	}

	atLimit := strings.Repeat("n", 63)
	fieldAtLimit := strings.Repeat("f", 63)
	t.Cleanup(func() { _, _ = admin.Exec("DROP TABLE IF EXISTS " + atLimit + " CASCADE") })
	at := pgITNow(t, admin)
	db, err := load(t, atLimit, fieldAtLimit)
	if err != nil {
		t.Fatalf("mount at the 63-byte limit: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if n := pgITStatementsSince(t, admin, application, at); n == 0 {
		t.Error("the server reports no provisioning statement for the accepted manifest")
	}
	base := pgITServe(t, map[string]*core.Database{"pg": db})
	path := "/v1/databases/pg/records/" + atLimit + "/k1"
	if resp := relHTTPDo(t, base, http.MethodPut, path, "", `{"data":{"`+fieldAtLimit+`":"accepted"}}`, nil); resp.status != http.StatusNoContent {
		t.Fatalf("PUT at the 63-byte limit: status %d: %s", resp.status, resp.raw)
	}
	if resp := relHTTPDo(t, base, http.MethodGet, path, "", "", nil); resp.status != http.StatusOK || !strings.Contains(resp.raw, `"`+fieldAtLimit+`":"accepted"`) {
		t.Fatalf("GET at the 63-byte limit: status %d: %s", resp.status, resp.raw)
	}
	pgITCanary(t, admin)
}
