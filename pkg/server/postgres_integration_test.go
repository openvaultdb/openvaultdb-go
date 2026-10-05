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
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// The proof of the PostgreSQL mount against a real server. Every test here needs
// a PostgreSQL server and skips without one: the CI job .github/workflows/
// postgres-integration.yml starts a postgres:17 container, sets
// OVDB_TEST_POSTGRES_DSN (a postgres:// URL) and fails when one of these tests is
// skipped or does not report PASS. The helpers of this file all start with pgIT so
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
	if want := (len(pgITValueProbes)+3)*4 + 11; mistakes != want {
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
// A route that reads one collection, and a document that runs in the database, find it
// by the declared spelling and by the folded one, and answer as a SQLite mount does,
// under the declared name. A join across databases reads the names the server holds,
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
			resp := pgITBoth(t, base, http.MethodPost, "/v1/dtql", "/v1/dtql", pgITDoc(doc, "pg"), pgITDoc(doc, "lite"))
			relIntRowsAre(t, resp, []map[string]any{{"id": "c1", spelling: "Ada"}})
			if route := pgITRoute(resp); route != "database" {
				t.Errorf("route = %q, want database", route)
			}
		})
	}

	across := func(spelling, people, customers string) string {
		return strings.NewReplacer("SPELLING", spelling, "PEOPLE", people, "CUSTOMERS", customers).Replace(pgITNameJoinDoc)
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
