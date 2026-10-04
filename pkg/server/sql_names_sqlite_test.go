package server_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// sqlNamesTables are the tables of the SQLite file the name tests run against.
// "Orders" and "Orders Status" share a prefix: a request for one must never be
// answered from, or write to, the other. The rest carry what a name can carry
// that a statement has to quote: a hyphen, a reserved word, a double quote and a
// backtick.
var sqlNamesTables = []struct{ table, marker string }{
	{"Orders", "row of Orders"},
	{"Orders Status", "row of Orders Status"},
	{"order-items", "row of order-items"},
	{"Order Details", "row of Order Details"},
	{"select", "row of select"},
	{`O"Brien`, `row of O"Brien`},
	{"My" + backtick + "Table", "row of My" + backtick + "Table"},
}

const backtick = "`"

// sqlNamesQuotedTable is a table whose name carries the quote characters of the
// quoted spelling of Order Details. It is not declared. The file holds it only
// where a test asks for it (startSQLNamesWithQuotedTable), with one row that no
// request for a declared collection may read or change.
var sqlNamesQuotedTable = struct{ table, marker string }{`"Order Details"`, "row of the table named with quotes"}

// sqlIdent writes name as a SQL identifier: in double quotes, a quote inside the
// name doubled.
func sqlIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

const sqlNamesManifest = `database: {id: dev, schema_mode: strict}
storage: {engine: sqlite, path: data.sqlite}
schemas:
  collections:
    Orders:
      fields: {id: {type: string}, name: {type: string}}
    Orders Status:
      fields: {id: {type: string}, name: {type: string}}
    order-items:
      fields: {id: {type: string}, name: {type: string}}
    '"Order Details"':
      fields: {id: {type: string}, name: {type: string}}
    select:
      fields: {id: {type: string}, name: {type: string}}
    '"O""Brien"':
      fields: {id: {type: string}, name: {type: string}}
    'My` + "`" + `Table':
      fields: {id: {type: string}, name: {type: string}}
`

// sqlNamesFixture is a real SQLite file served through the HTTP API.
type sqlNamesFixture struct {
	ts   *httptest.Server
	path string
}

// startSQLNames creates the tables (one row, id "1", each named for its table),
// mounts the manifest and serves it. The tables are created before the mount so
// that the test reads what a database that already exists looks like.
func startSQLNames(t *testing.T, opts ...server.Option) sqlNamesFixture {
	t.Helper()
	return startSQLNamesAs(t, "dev", opts...)
}

// startSQLNamesAs is startSQLNames with the database served under the given id
// (the manifest's own id is "dev").
func startSQLNamesAs(t *testing.T, id string, opts ...server.Option) sqlNamesFixture {
	t.Helper()
	return startSQLNamesFile(t, id, sqlNamesTables, opts...)
}

// startSQLNamesWithQuotedTable is startSQLNames over a file that also holds
// sqlNamesQuotedTable.
func startSQLNamesWithQuotedTable(t *testing.T, opts ...server.Option) sqlNamesFixture {
	t.Helper()
	tables := append(slices.Clone(sqlNamesTables), sqlNamesQuotedTable)
	return startSQLNamesFile(t, "dev", tables, opts...)
}

func startSQLNamesFile(t *testing.T, id string, tables []struct{ table, marker string }, opts ...server.Option) sqlNamesFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range tables {
		for _, statement := range []string{
			fmt.Sprintf(`CREATE TABLE %s (id TEXT PRIMARY KEY, name TEXT)`, sqlIdent(c.table)),
			fmt.Sprintf(`INSERT INTO %s VALUES ('1', '%s')`, sqlIdent(c.table), strings.ReplaceAll(c.marker, "'", "''")),
		} {
			if _, err := raw.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(manifestPath, []byte(sqlNamesManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := server.New("test", map[string]*core.Database{id: db}, opts...)
	t.Cleanup(service.CloseSnapshots)
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	return sqlNamesFixture{ts: ts, path: path}
}

// rows returns the "id=name" pairs of table in id order, read straight from the
// file.
func (f sqlNamesFixture) rows(t *testing.T, table string) string {
	t.Helper()
	raw, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	found, err := raw.Query(fmt.Sprintf(`SELECT id, name FROM %s ORDER BY id`, sqlIdent(table)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = found.Close() }()
	var out []string
	for found.Next() {
		var id, name sql.NullString
		if err := found.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		out = append(out, id.String+"="+name.String)
	}
	return strings.Join(out, ",")
}

// tableCount is the number of tables of the file that carry name.
func (f sqlNamesFixture) tableCount(t *testing.T, name string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// untouched asserts that every table except the named ones still holds its one
// seeded row.
func (f sqlNamesFixture) untouched(t *testing.T, except ...string) {
	t.Helper()
	for _, c := range sqlNamesTables {
		skip := false
		for _, name := range except {
			skip = skip || name == c.table
		}
		if skip {
			continue
		}
		if got, want := f.rows(t, c.table), "1="+c.marker; got != want {
			t.Errorf("table %q holds %q, want %q", c.table, got, want)
		}
	}
}

func (f sqlNamesFixture) call(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	return send(t, f.ts, guardCall{method: method, path: path, body: body})
}

// sqlNamesSpellings are the forms a caller can send for each table: the plain
// name, and for the manifest key that is a quoted SQL identifier the quoted form
// as well.
var sqlNamesSpellings = []struct{ label, collection, table string }{
	{"plain", "Orders", "Orders"},
	{"name with a space that extends another name", "Orders Status", "Orders Status"},
	{"name with a hyphen", "order-items", "order-items"},
	{"name with a space, plain form", "Order Details", "Order Details"},
	{"name with a space, quoted form", `"Order Details"`, "Order Details"},
	{"reserved word", "select", "select"},
	{"name with a double quote, plain form", `O"Brien`, `O"Brien`},
	{"name with a double quote, quoted form", `"O""Brien"`, `O"Brien`},
	{"name with a backtick", "My" + backtick + "Table", "My" + backtick + "Table"},
}

const sqlNamesRecords = "/v1/databases/dev/records/"

func TestSQLiteKeyReadsAddressTheirOwnTable(t *testing.T) {
	f := startSQLNames(t)
	for _, c := range sqlNamesSpellings {
		t.Run(c.label, func(t *testing.T) {
			want := "row of " + c.table
			key := url.PathEscape(c.collection) + "/1"
			for _, route := range []struct{ name, method, path string }{
				{"GET", "GET", sqlNamesRecords + key},
				{"GET read?key", "GET", "/v1/databases/dev/read?key=" + url.QueryEscape(key)},
			} {
				status, body := f.call(t, route.method, route.path, "")
				data, _ := body["data"].(map[string]any)
				if status != http.StatusOK || data["name"] != want {
					t.Errorf("%s: status %d body %v, want the %q", route.name, status, body, want)
				}
			}
			if status, _ := f.call(t, "HEAD", sqlNamesRecords+key, ""); status != http.StatusOK {
				t.Errorf("HEAD: status %d", status)
			}
			if status, _ := f.call(t, "HEAD", sqlNamesRecords+url.PathEscape(c.collection)+"/absent", ""); status != http.StatusNotFound {
				t.Errorf("HEAD of an absent id: status %d", status)
			}
			if status, _ := f.call(t, "GET", sqlNamesRecords+url.PathEscape(c.collection)+"/absent", ""); status != http.StatusNotFound {
				t.Errorf("GET of an absent id: status %d", status)
			}
		})
	}
}

func TestSQLiteKeyWritesAddressTheirOwnTable(t *testing.T) {
	for _, c := range sqlNamesSpellings {
		t.Run(c.label, func(t *testing.T) {
			f := startSQLNames(t)
			collection := url.PathEscape(c.collection)
			steps := []struct {
				name, method, path, body string
				status                   int
				rows                     string
			}{
				{"PUT an existing row", "PUT", sqlNamesRecords + collection + "/1", `{"data":{"name":"put"}}`, 204, "1=put"},
				{"PUT a new row", "PUT", sqlNamesRecords + collection + "/2", `{"data":{"name":"new"}}`, 204, "1=put,2=new"},
				{"POST a new row", "POST", sqlNamesRecords + collection + "/3", `{"data":{"name":"posted"}}`, 201, "1=put,2=new,3=posted"},
				{"POST an existing row", "POST", sqlNamesRecords + collection + "/3", `{"data":{"name":"again"}}`, 409, "1=put,2=new,3=posted"},
				{"PATCH", "PATCH", sqlNamesRecords + collection + "/2", `{"updates":[{"fieldName":"name","value":"patched"}]}`, 204, "1=put,2=patched,3=posted"},
				{"DELETE", "DELETE", sqlNamesRecords + collection + "/1", "", 204, "2=patched,3=posted"},
				{"batch", "POST", "/v1/databases/dev/batch", `{"ops":[{"op":"set","key":"` + collection + `/4","data":{"name":"batched"}},{"op":"delete","key":"` + collection + `/3"}]}`, 200, "2=patched,4=batched"},
			}
			for _, step := range steps {
				if status, body := f.call(t, step.method, step.path, step.body); status != step.status {
					t.Fatalf("%s: status %d, want %d: %v", step.name, status, step.status, body)
				}
				if got := f.rows(t, c.table); got != step.rows {
					t.Fatalf("%s: table %q holds %q, want %q", step.name, c.table, got, step.rows)
				}
				f.untouched(t, c.table)
			}
		})
	}
}

// TestMountDoesNotCreateATableNamedWithQuotes: a manifest key that is a quoted
// SQL identifier provisions the table of its public name, not a second table
// whose name carries the quote characters.
func TestMountDoesNotCreateATableNamedWithQuotes(t *testing.T) {
	f := startSQLNames(t)
	if n := f.tableCount(t, "Order Details"); n != 1 {
		t.Errorf("tables named Order Details: %d", n)
	}
	if n := f.tableCount(t, `"Order Details"`); n != 0 {
		t.Errorf("tables named with the quote characters: %d", n)
	}
}

// TestSQLiteUndeclaredAndUnsafeNamesStayRefused: the new spellings do not widen
// what the guard lets through.
func TestSQLiteUndeclaredAndUnsafeNamesStayRefused(t *testing.T) {
	f := startSQLNames(t)
	for _, collection := range []string{
		"Orders Status;x",
		`"Orders"`,        // a plain declaration stays literal
		`"Orders Status"`, // likewise
		`Order Details"`,
		`"Order Details`,
		"Order",
		"order items",
	} {
		key := url.PathEscape(collection) + "/1"
		for _, route := range []struct{ method, path, body string }{
			{"GET", sqlNamesRecords + key, ""},
			{"HEAD", sqlNamesRecords + key, ""},
			{"PUT", sqlNamesRecords + key, `{"data":{"name":"x"}}`},
			{"PATCH", sqlNamesRecords + key, `{"updates":[{"fieldName":"name","value":"x"}]}`},
			{"DELETE", sqlNamesRecords + key, ""},
		} {
			if status, body := f.call(t, route.method, route.path, route.body); status != http.StatusNotFound {
				t.Errorf("%s %q: status %d: %v", route.method, collection, status, body)
			}
		}
	}
	f.untouched(t)
}

// TestCapabilityOfACollectionHoldsUnderEverySpelling: a grant scoped to a
// collection is checked against the collection a key designates, not the
// spelling it was written in. A grant on Order Details, or on its quoted form,
// holds for a key in either form; a grant on another collection holds for
// neither.
func TestCapabilityOfACollectionHoldsUnderEverySpelling(t *testing.T) {
	for _, c := range []struct {
		grant   string
		allowed bool
	}{
		{"Order Details", true},
		{`"Order Details"`, true},
		{"Orders", false},
		{"Orders Status", false},
	} {
		t.Run("grant on "+c.grant, func(t *testing.T) {
			store, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			f := startSQLNames(t, server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}))
			token := connectFlow(t, f.ts, "records:read:"+c.grant+",records:write:"+c.grant+",records:delete:"+c.grant)
			for _, spelling := range []string{"Order Details", `"Order Details"`} {
				collection := url.PathEscape(spelling)
				row := func(id string) string { return collection + "/" + id }
				want := func(allowed int) int {
					if c.allowed {
						return allowed
					}
					return http.StatusForbidden
				}
				for _, step := range []struct {
					name, method, path, body string
					status                   int
				}{
					{"GET", "GET", sqlNamesRecords + row("1"), "", want(http.StatusOK)},
					{"GET read?key", "GET", "/v1/databases/dev/read?key=" + url.QueryEscape(row("1")), "", want(http.StatusOK)},
					{"HEAD", "HEAD", sqlNamesRecords + row("1"), "", want(http.StatusOK)},
					{"PUT", "PUT", sqlNamesRecords + row("2"), `{"data":{"name":"put"}}`, want(http.StatusNoContent)},
					{"PATCH", "PATCH", sqlNamesRecords + row("2"), `{"updates":[{"fieldName":"name","value":"patched"}]}`, want(http.StatusNoContent)},
					{"batch", "POST", "/v1/databases/dev/batch", `{"ops":[{"op":"set","key":"` + row("3") + `","data":{"name":"batched"}}]}`, want(http.StatusOK)},
					{"DELETE", "DELETE", sqlNamesRecords + row("2"), "", want(http.StatusNoContent)},
					{"DELETE after batch", "DELETE", sqlNamesRecords + row("3"), "", want(http.StatusNoContent)},
				} {
					if status, _ := request(t, f.ts, step.method, step.path, token, step.body); status != step.status {
						t.Errorf("%s as %s: status %d, want %d", step.name, spelling, status, step.status)
					}
				}
			}
			f.untouched(t)
		})
	}
}

// TestCapabilityOfACollectionHoldsUnderEverySpellingWhenServedUnderAnotherID:
// the spellings of a collection come from the database the request reached, not
// from a lookup by id. A database the server serves under an id other than its
// manifest's follows the same rule as any other: a grant on Order Details covers
// a key written in either spelling, and a grant on another collection covers
// neither.
func TestCapabilityOfACollectionHoldsUnderEverySpellingWhenServedUnderAnotherID(t *testing.T) {
	for _, c := range []struct {
		grant string
		want  int
	}{
		{"Order Details", http.StatusOK},
		{`"Order Details"`, http.StatusOK},
		{"Orders", http.StatusForbidden},
	} {
		t.Run("grant on "+c.grant, func(t *testing.T) {
			store, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			// The grant names the database by its manifest id; the server serves it as
			// "alias".
			const token = "ovdb_test_alias_token"
			grantToken(t, store, "dev", token, auth.Capability{Action: auth.CapRecordsRead, Collection: c.grant})
			f := startSQLNamesAs(t, "alias", server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}))
			for _, spelling := range []string{"Order Details", `"Order Details"`} {
				path := "/v1/databases/alias/records/" + url.PathEscape(spelling) + "/1"
				if status, _ := request(t, f.ts, "GET", path, token, ""); status != c.want {
					t.Errorf("GET as %s: status %d, want %d", spelling, status, c.want)
				}
			}
		})
	}
}

// TestEmptyWritesOnARealSQLiteMount: a write that carries no field has nothing
// to put in a statement. The adapter cannot update a row with nothing to set
// (a set of no field on a row that exists, an update with no operation), so
// those are refused before it is called, with 400 bad_request and nothing
// written, a batch included. A write the adapter does carry out (a row with only
// its id is inserted) keeps answering success.
func TestEmptyWritesOnARealSQLiteMount(t *testing.T) {
	f := startSQLNames(t)
	const batch = "/v1/databases/dev/batch"
	for _, c := range []struct {
		name, method, path, body string
		status                   int
		rows                     string // the rows of Orders afterwards
	}{
		{"PUT empty data, existing row", "PUT", sqlNamesRecords + "Orders/1", `{"data":{}}`, 400, "1=row of Orders"},
		{"PUT empty data, new row", "PUT", sqlNamesRecords + "Orders/2", `{"data":{}}`, 204, "1=row of Orders,2="},
		{"PUT empty data, the row just created", "PUT", sqlNamesRecords + "Orders/2", `{"data":{}}`, 400, "1=row of Orders,2="},
		{"PUT data with only its id, existing row", "PUT", sqlNamesRecords + "Orders/1", `{"data":{"id":"1"}}`, 400, "1=row of Orders,2="},
		{"POST empty data, new row", "POST", sqlNamesRecords + "Orders/3", `{"data":{}}`, 201, "1=row of Orders,2=,3="},
		{"POST empty data, existing row", "POST", sqlNamesRecords + "Orders/1", `{"data":{}}`, 409, "1=row of Orders,2=,3="},
		{"PATCH empty operation list", "PATCH", sqlNamesRecords + "Orders/1", `{"updates":[]}`, 400, "1=row of Orders,2=,3="},
		{"PATCH empty operation", "PATCH", sqlNamesRecords + "Orders/1", `{"updates":[{}]}`, 400, "1=row of Orders,2=,3="},
		{"batch set, empty data, existing row", "POST", batch, `{"ops":[{"op":"set","key":"Orders/1","data":{}}]}`, 400, "1=row of Orders,2=,3="},
		{"batch set, no data, existing row", "POST", batch, `{"ops":[{"op":"set","key":"Orders/1"}]}`, 400, "1=row of Orders,2=,3="},
		{"batch insert, empty data", "POST", batch, `{"ops":[{"op":"insert","key":"Orders/4","data":{}}]}`, 200, "1=row of Orders,2=,3=,4="},
		{"batch update, empty operation list", "POST", batch, `{"ops":[{"op":"update","key":"Orders/1","updates":[]}]}`, 400, "1=row of Orders,2=,3=,4="},
		{"batch refused as a whole", "POST", batch, `{"ops":[{"op":"set","key":"Orders/1","data":{"name":"changed"}},{"op":"set","key":"Orders/2","data":{}}]}`, 400, "1=row of Orders,2=,3=,4="},
		{"batch that empties a row it inserted", "POST", batch, `{"ops":[{"op":"insert","key":"Orders/5","data":{"name":"x"}},{"op":"set","key":"Orders/5","data":{}}]}`, 400, "1=row of Orders,2=,3=,4="},
		{"batch that sets a row it deleted", "POST", batch, `{"ops":[{"op":"delete","key":"Orders/4"},{"op":"set","key":"Orders/4","data":{}}]}`, 200, "1=row of Orders,2=,3=,4="},
		{"batch that inserts one key twice with no data", "POST", batch, `{"ops":[{"op":"insert","key":"Orders/6"},{"op":"insert","key":"Orders/6"}]}`, 409, "1=row of Orders,2=,3=,4="},
		{"batch that updates a row it inserted with no data", "POST", batch, `{"ops":[{"op":"insert","key":"Orders/6"},{"op":"update","key":"Orders/6","updates":[{"fieldName":"name","value":"u"}]}]}`, 200, "1=row of Orders,2=,3=,4=,6=u"},
		{"batch that inserts a row it set with no data", "POST", batch, `{"ops":[{"op":"set","key":"Orders/7"},{"op":"insert","key":"Orders/7"}]}`, 409, "1=row of Orders,2=,3=,4=,6=u"},
		{"batch that updates a row it set with no data", "POST", batch, `{"ops":[{"op":"set","key":"Orders/7"},{"op":"update","key":"Orders/7","updates":[{"fieldName":"name","value":"v"}]}]}`, 200, "1=row of Orders,2=,3=,4=,6=u,7=v"},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.call(t, c.method, c.path, c.body)
			if status != c.status {
				t.Fatalf("status %d, want %d: %v", status, c.status, body)
			}
			if c.status == 400 && writeGuardErrorCode(body) != "bad_request" {
				t.Errorf("error code %v, want bad_request", writeGuardErrorCode(body))
			}
			if got := f.rows(t, "Orders"); got != c.rows {
				t.Errorf("Orders holds %q, want %q", got, c.rows)
			}
		})
	}
	f.untouched(t, "Orders")
}
