package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
	_ "modernc.org/sqlite"
)

// The whole path of a relational document: a real HTTP request, the real
// handler, the real executor and real SQLite files. The helpers of this file all
// start with relHTTP so they cannot clash with the helpers of the other test
// files of the package.

// relHTTPMount creates a SQLite file holding statements and mounts it as id,
// declaring every collection of collections with its fields (all strings: the
// driver reads columns of any type).
func relHTTPMount(t *testing.T, id, extraManifest string, collections map[string][]string, statements ...string) *core.Database {
	t.Helper()
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := storage.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	declaration := "database: {id: " + id + ", schema_mode: strict" + extraManifest + "}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n"
	for name, fields := range collections {
		declaration += "    " + name + ":\n      fields:\n"
		for _, field := range fields {
			declaration += "        " + field + ": {type: string}\n"
		}
	}
	path := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(path, []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// relHTTPChinook is the database the joins run in: customers and their invoices.
func relHTTPChinook(t *testing.T, extraManifest string) *core.Database {
	return relHTTPMount(t, "chinook", extraManifest,
		map[string][]string{"Customer": {"id", "name", "country"}, "Invoice": {"id", "customer_id", "total"}},
		`CREATE TABLE "Customer" ("id" TEXT PRIMARY KEY, "name" TEXT, "country" TEXT)`,
		`CREATE TABLE "Invoice" ("id" TEXT PRIMARY KEY, "customer_id" TEXT, "total" REAL)`,
		`INSERT INTO "Customer" VALUES ('c1', 'Ada', 'UK'), ('c2', 'Grace', 'US'), ('c3', 'Edsger', 'NL')`,
		`INSERT INTO "Invoice" VALUES ('i1', 'c1', 10), ('i2', 'c1', 20), ('i3', 'c2', 5), ('i4', 'c3', 7), ('i5', 'c3', 8)`,
	)
}

// relHTTPCountries is a second database: one row per country code.
func relHTTPCountries(t *testing.T, extraManifest string) *core.Database {
	return relHTTPMount(t, "countries", extraManifest,
		map[string][]string{"Country": {"code", "name", "region"}},
		`CREATE TABLE "Country" ("code" TEXT PRIMARY KEY, "name" TEXT, "region" TEXT)`,
		`INSERT INTO "Country" VALUES ('UK', 'United Kingdom', 'Europe'), ('US', 'United States', 'Americas'), ('NL', 'Netherlands', 'Europe')`,
	)
}

// relHTTPResponse is one answered request.
type relHTTPResponse struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func relHTTPDo(t *testing.T, host, method, path, token, body string, headers map[string]string) relHTTPResponse {
	t.Helper()
	req, err := http.NewRequest(method, host+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := relHTTPResponse{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (r relHTTPResponse) errorField(field string) string {
	detail, _ := r.body["error"].(map[string]any)
	value, _ := detail[field].(string)
	return value
}

func (r relHTTPResponse) rows(t *testing.T) []map[string]any {
	t.Helper()
	records, _ := r.body["records"].([]any)
	rows := make([]map[string]any, len(records))
	for i, rec := range records {
		entry, _ := rec.(map[string]any)
		rows[i], _ = entry["data"].(map[string]any)
		if _, hasKey := entry["key"]; hasKey && r.body["columns"] != nil {
			t.Fatalf("a relational record carries a key: %v", entry)
		}
	}
	return rows
}

func (r relHTTPResponse) execution(t *testing.T) map[string]any {
	t.Helper()
	execution, ok := r.body["execution"].(map[string]any)
	if !ok {
		t.Fatalf("no execution block: %s", r.raw)
	}
	return execution
}

func (r relHTTPResponse) columns() []string {
	var columns []string
	list, _ := r.body["columns"].([]any)
	for _, c := range list {
		columns = append(columns, c.(string))
	}
	return columns
}

// Documents. relHTTPInvoiceJoin joins invoices to their customers inside one
// database; the others read two.
const (
	relHTTPInvoiceJoin = `from:
  name: Invoice
  alias: i
  joins:
    - type: inner
      from: {name: Customer, alias: c}
      on:
        - {left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}
orderBy:
  - {field: id, source: i}
columns:
  - {field: id, source: i}
  - {field: name, source: c, as: customer_name}
  - {field: total, source: i, as: total}
`
	relHTTPRevenueByCountry = `from:
  name: Invoice
  alias: i
  joins:
    - type: inner
      from: {name: Customer, alias: c}
      on:
        - {left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}
groupBy:
  - {field: country, source: c}
orderBy:
  - {field: country, source: c}
columns:
  - {field: country, source: c}
  - {aggregate: {function: sum, args: [{field: total, source: i}]}, as: revenue}
`
	// relHTTPCustomerRegions joins a customer of chinook to its country in the
	// countries database. Every source names its database.
	relHTTPCustomerRegions = `from:
  database: chinook
  name: Customer
  alias: c
  joins:
    - type: inner
      from: {database: countries, name: Country, alias: k}
      on:
        - {left: {field: country, source: c}, op: '==', right: {field: code, source: k}}
orderBy:
  - {field: name, source: c}
columns:
  - {field: name, source: c, as: customer}
  - {field: name, source: k, as: country}
  - {field: region, source: k}
`
	// relHTTPInvoicesOfCustomersInEurope reads Invoice and, in a subquery, Customer.
	relHTTPInvoicesWithACustomer = `from:
  database: chinook
  name: Invoice
  alias: i
where:
  exists:
    query:
      from: {database: chinook, name: Customer, alias: c}
      where: {op: '==', left: {field: id, source: c}, right: {field: customer_id, source: i}}
orderBy:
  - {field: id, source: i}
columns:
  - {field: id, source: i}
`
)

func relHTTPPost(t *testing.T, host, path, token, doc string) relHTTPResponse {
	t.Helper()
	return relHTTPDo(t, host, http.MethodPost, path, token, doc, nil)
}

func relHTTPNames(rows []map[string]any, field string) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row[field]
	}
	return out
}

func TestRelationalDTQLOverHTTPAcceptance(t *testing.T) {
	chinook, countries := relHTTPChinook(t, ""), relHTTPCountries(t, "")
	service := server.New("test", map[string]*core.Database{"chinook": chinook, "countries": countries})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()

	t.Run("a join on the per-database endpoint has ordered columns and the route", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", relHTTPInvoiceJoin)
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if got := resp.columns(); !reflect.DeepEqual(got, []string{"id", "customer_name", "total"}) {
			t.Fatalf("columns = %v", got)
		}
		rows := resp.rows(t)
		if len(rows) != 5 || rows[0]["id"] != "i1" || rows[0]["customer_name"] != "Ada" || rows[4]["customer_name"] != "Edsger" {
			t.Fatalf("rows = %v", rows)
		}
		execution := resp.execution(t)
		if execution["route"] != "database" || execution["rowsReturned"] != float64(5) {
			t.Fatalf("execution = %v", execution)
		}
		if cc := resp.header.Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("Cache-Control = %q", cc)
		}
	})
	t.Run("an aggregate over a join runs on the database route", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", relHTTPRevenueByCountry)
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		rows := resp.rows(t)
		if len(rows) != 3 || rows[0]["country"] != "NL" || rows[0]["revenue"] != float64(15) || rows[2]["country"] != "US" || rows[2]["revenue"] != float64(5) {
			t.Fatalf("rows = %v", rows)
		}
		if resp.execution(t)["route"] != "database" {
			t.Fatalf("execution = %v", resp.execution(t))
		}
	})
	t.Run("a second database is a 400 on the per-database endpoint and rows on /v1/dtql", func(t *testing.T) {
		there := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", relHTTPCustomerRegions)
		if there.status != http.StatusBadRequest || !strings.Contains(there.errorField("message"), "/v1/dtql") || !strings.Contains(there.errorField("message"), "countries") {
			t.Fatalf("per-database endpoint: status %d: %s", there.status, there.raw)
		}
		here := relHTTPPost(t, host.URL, "/v1/dtql", "", relHTTPCustomerRegions)
		if here.status != http.StatusOK {
			t.Fatalf("/v1/dtql: status %d: %s", here.status, here.raw)
		}
		rows := here.rows(t)
		if len(rows) != 3 || rows[0]["customer"] != "Ada" || rows[0]["country"] != "United Kingdom" || rows[2]["region"] != "Americas" && rows[2]["customer"] != "Grace" {
			t.Fatalf("rows = %v", rows)
		}
		if got := here.columns(); !reflect.DeepEqual(got, []string{"customer", "country", "region"}) {
			t.Fatalf("columns = %v", got)
		}
		execution := here.execution(t)
		sources, _ := execution["sources"].([]any)
		if execution["route"] != "in-memory" || len(sources) != 2 {
			t.Fatalf("execution = %v", execution)
		}
		first := sources[0].(map[string]any)
		if first["database"] != "chinook" || first["collection"] != "Customer" || first["rows"] != float64(3) {
			t.Fatalf("first source = %v", first)
		}
	})
	t.Run("the GET form of /v1/dtql", func(t *testing.T) {
		resp := relHTTPDo(t, host.URL, http.MethodGet, "/v1/dtql?q="+url.QueryEscape(relHTTPCustomerRegions), "", "", nil)
		if resp.status != http.StatusOK || len(resp.rows(t)) != 3 {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		bad := relHTTPDo(t, host.URL, http.MethodGet, "/v1/dtql?x=1", "", "", nil)
		if bad.status != http.StatusBadRequest {
			t.Fatalf("status %d: %s", bad.status, bad.raw)
		}
		long := relHTTPDo(t, host.URL, http.MethodGet, "/v1/dtql?q="+strings.Repeat("a", 9<<10), "", "", nil)
		if long.status != http.StatusRequestURITooLong {
			t.Fatalf("status %d: %s", long.status, long.raw)
		}
	})
	t.Run("parameters are bound", func(t *testing.T) {
		body := `{"query": "from: {database: chinook, name: Customer, alias: c}\nwhere: {op: '==', left: {field: country, source: c}, right: {param: country}}\ncolumns: [{field: name, source: c}]\n", "parameters": {"country": "UK"}}`
		resp := relHTTPDo(t, host.URL, http.MethodPost, "/v1/dtql", "", body, map[string]string{"Content-Type": "application/json"})
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if got := relHTTPNames(resp.rows(t), "name"); !reflect.DeepEqual(got, []any{"Ada"}) {
			t.Fatalf("rows = %v", got)
		}
		unbound := relHTTPDo(t, host.URL, http.MethodPost, "/v1/dtql", "", `{"query": "from: {database: chinook, name: Customer}\nwhere: {op: '==', left: {field: id}, right: {param: x}}\n"}`, map[string]string{"Content-Type": "application/json"})
		if unbound.status != http.StatusBadRequest || unbound.errorField("code") != "invalid_dtql" {
			t.Fatalf("status %d: %s", unbound.status, unbound.raw)
		}
	})
	t.Run("a single-collection document returns exactly today's body", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", "from: {name: Customer}\norderBy: [{field: id}]\n")
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if len(resp.body) != 1 {
			t.Fatalf("body has more than records: %s", resp.raw)
		}
		records, _ := resp.body["records"].([]any)
		first, _ := records[0].(map[string]any)
		data, _ := first["data"].(map[string]any)
		if len(records) != 3 || first["key"] != "Customer/c1" || data["name"] != "Ada" {
			t.Fatalf("records = %v", records)
		}
	})
	t.Run("a one-source document that names its own database is read as one that names none", func(t *testing.T) {
		const bare = "from: {name: Customer}\norderBy: [{field: id}]\ncolumns: [{field: name}]\n"
		want := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", bare)
		if want.status != http.StatusOK || want.body["columns"] != nil || want.body["execution"] != nil {
			t.Fatalf("status %d: %s", want.status, want.raw)
		}
		for name, doc := range map[string]string{
			"flow form":            "from: {database: chinook, name: Customer}\norderBy: [{field: id}]\ncolumns: [{field: name}]\n",
			"block form":           "from:\n  database: chinook\n  name: Customer\norderBy: [{field: id}]\ncolumns: [{field: name}]\n",
			"the database last":    "from: {name: Customer, database: chinook}\norderBy: [{field: id}]\ncolumns: [{field: name}]\n",
			"with the default one": "from: {database: chinook, schema: main, name: Customer}\norderBy: [{field: id}]\ncolumns: [{field: name}]\n",
		} {
			got := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", doc)
			if got.status != want.status || got.raw != want.raw {
				t.Errorf("%s: status %d: %s\nwant the body of the document that names no database: %s", name, got.status, got.raw, want.raw)
			}
		}
		records, _ := want.body["records"].([]any)
		if first, _ := records[0].(map[string]any); len(records) != 3 || first["key"] != "Customer/c1" {
			t.Fatalf("records = %v, want keys", records)
		}
	})
	t.Run("the same document on /v1/dtql is relational, without keys", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/dtql", "", "from: {database: chinook, name: Customer}\norderBy: [{field: id}]\ncolumns: [{field: name}]\n")
		if resp.status != http.StatusOK || resp.body["columns"] == nil || resp.body["execution"] == nil {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if got := relHTTPNames(resp.rows(t), "name"); !reflect.DeepEqual(got, []any{"Ada", "Grace", "Edsger"}) {
			t.Fatalf("rows = %v", got)
		}
	})
	t.Run("a document that names its own database and has another relational feature is relational", func(t *testing.T) {
		for name, doc := range map[string]string{
			"an alias":             "from: {database: chinook, name: Customer, alias: c}\norderBy: [{field: id, source: c}]\ncolumns: [{field: name, source: c}]\n",
			"a column alias":       "from: {database: chinook, name: Customer}\norderBy: [{field: id}]\ncolumns: [{field: name, as: customer}]\n",
			"a subquery":           "from: {database: chinook, name: Invoice}\nwhere: {exists: {query: {from: {database: chinook, name: Customer}}}}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n",
			"an aggregate":         "from: {database: chinook, name: Customer}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\n",
			"a second source":      "from: {database: chinook, name: Invoice, alias: i, joins: [{from: {database: chinook, name: Customer, alias: c}, on: [{left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}]}]}\ncolumns: [{field: id, source: i}]\n",
			"a null test":          "from: {database: chinook, name: Customer}\nwhere: {isNotNull: {field: country}}\ncolumns: [{field: id}]\n",
			"the other's database": "from: {database: chinook, name: Customer}\nwhere: {exists: {query: {from: {database: countries, name: Country}}}}\ncolumns: [{field: id}]\n",
		} {
			resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", doc)
			if name == "the other's database" {
				if resp.status != http.StatusBadRequest || !strings.Contains(resp.errorField("message"), "/v1/dtql") {
					t.Errorf("%s: status %d: %s", name, resp.status, resp.raw)
				}
				continue
			}
			if resp.status != http.StatusOK || resp.body["columns"] == nil || resp.body["execution"] == nil {
				t.Errorf("%s: status %d: %s", name, resp.status, resp.raw)
			}
		}
	})
	t.Run("a document whose only relational feature is a subquery is answered in memory, as on /v1/dtql", func(t *testing.T) {
		// One query has one answer: the root unaliased or aliased, the sources
		// naming the database or not, on either endpoint.
		want := []any{"i1", "i2", "i3", "i4", "i5"}
		for name, call := range map[string]struct{ path, doc string }{
			"unaliased root":          {"/v1/databases/chinook/dtql", "from: {name: Invoice}\nwhere: {exists: {query: {from: {name: Customer}}}}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n"},
			"aliased root":            {"/v1/databases/chinook/dtql", "from: {name: Invoice, alias: i}\nwhere: {exists: {query: {from: {name: Customer}}}}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n"},
			"sources naming the base": {"/v1/databases/chinook/dtql", "from: {database: chinook, name: Invoice}\nwhere: {exists: {query: {from: {database: chinook, name: Customer}}}}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n"},
			"/v1/dtql":                {"/v1/dtql", "from: {database: chinook, name: Invoice}\nwhere: {exists: {query: {from: {database: chinook, name: Customer}}}}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n"},
		} {
			resp := relHTTPPost(t, host.URL, call.path, "", call.doc)
			if resp.status != http.StatusOK || resp.execution(t)["route"] != "in-memory" || !reflect.DeepEqual(relHTTPNames(resp.rows(t), "id"), want) {
				t.Errorf("%s: status %d: %s", name, resp.status, resp.raw)
			}
		}
	})
	t.Run("the paging headers are refused on a relational document", func(t *testing.T) {
		for _, header := range []string{"OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close"} {
			for _, path := range []string{"/v1/databases/chinook/dtql", "/v1/dtql"} {
				doc := relHTTPInvoiceJoin
				if path == "/v1/dtql" {
					doc = relHTTPCustomerRegions
				}
				resp := relHTTPDo(t, host.URL, http.MethodPost, path, "", doc, map[string]string{header: "10"})
				if resp.status != http.StatusUnprocessableEntity || resp.errorField("code") != "snapshot_unsupported" {
					t.Fatalf("%s on %s: status %d: %s", header, path, resp.status, resp.raw)
				}
			}
		}
	})
	t.Run("/v1/dtql needs a database on every source", func(t *testing.T) {
		for name, doc := range map[string]string{
			"no database":        "from: {name: Customer}\n",
			"join without one":   strings.Replace(relHTTPCustomerRegions, "database: countries, ", "", 1),
			"invalid id":         "from: {database: 'a b', name: Customer}\n",
			"schema":             "from: {database: chinook, schema: main, name: Customer}\n",
			"scan":               "from: {database: chinook, name: Customer, scan: {limit: 5, orderBy: [{field: id}]}}\n",
			"cursor":             "from: {database: chinook, name: Customer}\nstartFrom: x\n",
			"money":              "from: {database: chinook, name: Customer}\nmoney: {minorUnitScale: 2, divisionScale: 2, rounding: halfEven}\n",
			"limit too large":    "from: {database: chinook, name: Customer}\nlimit: 1001\n",
			"offset too large":   "from: {database: chinook, name: Customer}\noffset: 10001\n",
			"a document of YAML": "- not a document\n",
		} {
			resp := relHTTPPost(t, host.URL, "/v1/dtql", "", doc)
			if resp.status != http.StatusBadRequest {
				t.Errorf("%s: status %d: %s", name, resp.status, resp.raw)
			}
		}
		if resp := relHTTPPost(t, host.URL, "/v1/dtql", "", ""); resp.status != http.StatusBadRequest {
			t.Errorf("empty body: status %d", resp.status)
		}
	})
	t.Run("a well-formed database that is not mounted is a 404", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/dtql", "", "from: {database: nowhere, name: Customer}\n")
		if resp.status != http.StatusNotFound || resp.errorField("code") != "not_found" {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
	t.Run("a column the database does not know is a 400", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", strings.Replace(relHTTPInvoiceJoin, "{field: total, source: i, as: total}", "{field: nosuchcolumn, source: i}", 1))
		if resp.status != http.StatusBadRequest {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
}

// On the existing per-database path a root source may carry the engine's default
// schema, which is treated as carrying none; any other schema, a scan and a
// money configuration are refused.
func TestSingleCollectionDocumentAndTheDefaultSchemaOverHTTP(t *testing.T) {
	chinook := relHTTPChinook(t, "")
	service := server.New("test", map[string]*core.Database{"chinook": chinook})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	post := func(doc string) relHTTPResponse {
		return relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", doc)
	}
	plain := post("from: {name: Customer}\norderBy: [{field: id}]\n")
	if plain.status != http.StatusOK {
		t.Fatalf("status %d: %s", plain.status, plain.raw)
	}
	for name, doc := range map[string]string{
		"block form": "from:\n  schema: main\n  name: Customer\norderBy: [{field: id}]\n",
		"flow form":  "from: {schema: main, name: Customer}\norderBy: [{field: id}]\n",
		"quoted":     "from: {schema: \"main\", name: Customer}\norderBy: [{field: id}]\n",
	} {
		t.Run("default schema, "+name, func(t *testing.T) {
			resp := post(doc)
			if resp.status != http.StatusOK || resp.raw != plain.raw {
				t.Fatalf("status %d: %s\nwant the body of the unqualified document: %s", resp.status, resp.raw, plain.raw)
			}
		})
	}
	for name, doc := range map[string]string{
		"another schema":                 "from: {schema: other, name: Customer}\n",
		"the default schema in capitals": "from: {schema: MAIN, name: Customer}\n",
		"two schemas":                    "from: {schema: main, name: Customer, schema: other}\n",
		"a scan":                         "from: {name: Customer, scan: {limit: 5, orderBy: [{field: id}]}}\n",
		"money":                          "from: {name: Customer}\nmoney: {minorUnitScale: 2, divisionScale: 2, rounding: halfEven}\n",
		"a cursor":                       "from: {name: Customer}\nstartFrom: x\n",
		"a limit above 1000":             "from: {name: Customer}\nlimit: 1001\n",
		"a database of another mount":    "from: {database: other, name: Customer}\n",
		"a join with a schema":           "from: {name: Invoice, alias: i, joins: [{from: {schema: main, name: Customer, alias: c}, on: [{left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}]}]}\n",
	} {
		t.Run("refused, "+name, func(t *testing.T) {
			resp := post(doc)
			if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" {
				t.Fatalf("status %d, want a 400 invalid_dtql: %s", resp.status, resp.raw)
			}
		})
	}
	// The default schema of the other mounts is none: only SQLite has one.
	if resp := relHTTPPost(t, host.URL, "/v1/dtql", "", "from: {database: chinook, schema: main, name: Customer}\n"); resp.status != http.StatusBadRequest {
		t.Fatalf("/v1/dtql takes no schema: status %d: %s", resp.status, resp.raw)
	}
}

// A grant names one database, or one database and one collection. The handler
// checks every source of the document before anything is read.
func TestRelationalDTQLGrantsOverHTTP(t *testing.T) {
	dir := t.TempDir()
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	grant := func(token, database string, capabilities ...auth.Capability) {
		t.Helper()
		if err := store.CreateGrant(&auth.Grant{DatabaseID: database, Capabilities: capabilities}, token); err != nil {
			t.Fatal(err)
		}
	}
	read := auth.Capability{Action: auth.CapRecordsRead}
	grant("tok-chinook", "chinook", read)
	grant("tok-invoice-only", "chinook", auth.Capability{Action: auth.CapRecordsRead, Collection: "Invoice"})
	grant("tok-countries", "countries", read)
	grant("tok-server", "", read)
	grant("tok-write-only", "chinook", auth.Capability{Action: auth.CapRecordsWrite})

	chinook, countries := relHTTPChinook(t, ""), relHTTPCountries(t, "")
	service := server.New("test", map[string]*core.Database{"chinook": chinook, "countries": countries},
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}))
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()

	t.Run("an owner token gets rows", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/dtql", ownerToken, relHTTPCustomerRegions)
		if resp.status != http.StatusOK || len(resp.rows(t)) != 3 {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		resp = relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", ownerToken, relHTTPInvoiceJoin)
		if resp.status != http.StatusOK || len(resp.rows(t)) != 5 {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
	t.Run("a server-level grant reads both databases", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/dtql", "tok-server", relHTTPCustomerRegions)
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
	t.Run("no token is a 401", func(t *testing.T) {
		if resp := relHTTPPost(t, host.URL, "/v1/dtql", "", relHTTPCustomerRegions); resp.status != http.StatusUnauthorized {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
	t.Run("a token for one database joining two gets a 403 that names the second", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/dtql", "tok-chinook", relHTTPCustomerRegions)
		if resp.status != http.StatusForbidden || resp.errorField("code") != "forbidden" || !strings.Contains(resp.errorField("message"), "countries") {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if strings.Contains(resp.raw, "Ada") || resp.body["records"] != nil {
			t.Fatalf("a refused request returned rows: %s", resp.raw)
		}
		// The same token reads its own database.
		own := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "tok-chinook", relHTTPInvoiceJoin)
		if own.status != http.StatusOK || len(own.rows(t)) != 5 {
			t.Fatalf("own database: status %d: %s", own.status, own.raw)
		}
		// And the token of the second database names the first.
		other := relHTTPPost(t, host.URL, "/v1/dtql", "tok-countries", relHTTPCustomerRegions)
		if other.status != http.StatusForbidden || !strings.Contains(other.errorField("message"), "chinook") {
			t.Fatalf("status %d: %s", other.status, other.raw)
		}
	})
	t.Run("a grant for another capability reads nothing", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "tok-write-only", relHTTPInvoiceJoin)
		if resp.status != http.StatusForbidden {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
	t.Run("a token scoped to one collection is refused a join that reads another", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "tok-invoice-only", relHTTPInvoiceJoin)
		if resp.status != http.StatusForbidden || !strings.Contains(resp.errorField("message"), "Customer") {
			t.Fatalf("join: status %d: %s", resp.status, resp.raw)
		}
		// A subquery reads the other collection of the same database.
		sub := relHTTPPost(t, host.URL, "/v1/dtql", "tok-invoice-only", relHTTPInvoicesWithACustomer)
		if sub.status != http.StatusForbidden || !strings.Contains(sub.errorField("message"), "Customer") {
			t.Fatalf("subquery: status %d: %s", sub.status, sub.raw)
		}
		// The same token reads the one collection it is granted.
		own := relHTTPPost(t, host.URL, "/v1/dtql", "tok-invoice-only", "from: {database: chinook, name: Invoice}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n")
		if own.status != http.StatusOK || len(own.rows(t)) != 5 {
			t.Fatalf("own collection: status %d: %s", own.status, own.raw)
		}
		// The owner's subquery document returns the invoices that have a customer.
		owner := relHTTPPost(t, host.URL, "/v1/dtql", ownerToken, relHTTPInvoicesWithACustomer)
		if owner.status != http.StatusOK || len(owner.rows(t)) != 5 {
			t.Fatalf("owner: status %d: %s", owner.status, owner.raw)
		}
	})
	t.Run("a collection that the document hides is checked wherever it is", func(t *testing.T) {
		// Every document reads Customer in a place a check of the root source or of the
		// joins at the first level would not look, or under a name that is not its own.
		// The token is granted Invoice only, so each is a 403 that names Customer and
		// returns no row; the owner gets rows for the same document, so none of them is
		// refused for another reason.
		for name, doc := range map[string]string{
			"derived source":  "from:\n  query:\n    as: d\n    from: {database: chinook, name: Customer}\n    columns: [{field: id}]\n",
			"scalar subquery": "from: {database: chinook, name: Invoice}\ncolumns:\n  - field: id\n  - query: {as: n, from: {database: chinook, name: Customer}, columns: [{aggregate: {function: count, args: [{star: true}]}}]}\n",
			"a join nested in the relation tree of a join": `from:
  database: chinook
  name: Invoice
  alias: i
  joins:
    - type: inner
      from:
        database: chinook
        name: Invoice
        alias: j
        joins:
          - type: inner
            from: {database: chinook, name: Customer, alias: c}
            on:
              - {left: {field: customer_id, source: j}, op: '==', right: {field: id, source: c}}
      on:
        - {left: {field: id, source: i}, op: '==', right: {field: id, source: j}}
columns:
  - {field: id, source: i}
`,
			"a subquery in ORDER BY":                             "from: {database: chinook, name: Invoice}\norderBy:\n  - query: {as: n, from: {database: chinook, name: Customer}, columns: [{aggregate: {function: count, args: [{star: true}]}}]}\ncolumns: [{field: id}]\n",
			"a query as the operand of a comparison":             "from: {database: chinook, name: Invoice}\nwhere: {op: '>', left: {field: total}, right: {query: {as: m, from: {database: chinook, name: Customer}, columns: [{aggregate: {function: count, args: [{star: true}]}}]}}}\ncolumns: [{field: id}]\n",
			"an alias equal to the granted collection":           "from: {database: chinook, name: Customer, alias: Invoice}\ncolumns: [{field: id, source: Invoice}]\n",
			"a derived source aliased as the granted collection": "from:\n  query:\n    as: Invoice\n    from: {database: chinook, name: Customer}\n    columns: [{field: id}]\n",
		} {
			resp := relHTTPPost(t, host.URL, "/v1/dtql", "tok-invoice-only", doc)
			if resp.status != http.StatusForbidden || resp.errorField("code") != "forbidden" || !strings.Contains(resp.errorField("message"), "Customer") {
				t.Errorf("%s: status %d, want a 403 that names Customer: %s", name, resp.status, resp.raw)
			}
			if resp.body["records"] != nil || strings.Contains(resp.raw, "c1") {
				t.Errorf("%s: a refused request returned rows: %s", name, resp.raw)
			}
			owner := relHTTPPost(t, host.URL, "/v1/dtql", ownerToken, doc)
			if owner.status != http.StatusOK || len(owner.rows(t)) == 0 {
				t.Errorf("%s: the owner's status %d, want rows: %s", name, owner.status, owner.raw)
			}
		}
	})
}

// With auth off every caller is an owner: the same document returns rows.
func TestRelationalDTQLWithAuthOffReturnsRows(t *testing.T) {
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, ""), "countries": relHTTPCountries(t, "")})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	resp := relHTTPPost(t, host.URL, "/v1/dtql", "", relHTTPInvoicesWithACustomer)
	if resp.status != http.StatusOK || len(resp.rows(t)) != 5 || resp.execution(t)["route"] != "in-memory" {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
}

// A request counts as in flight on every database it reads, so unmounting waits
// for it.
func TestRelationalDTQLLeasesEveryDatabaseItReads(t *testing.T) {
	chinook, countries := relHTTPChinook(t, ""), relHTTPCountries(t, "")
	service := server.New("test", map[string]*core.Database{"chinook": chinook, "countries": countries})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	if resp := relHTTPPost(t, host.URL, "/v1/dtql", "", relHTTPCustomerRegions); resp.status != http.StatusOK {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
	if err := service.Unmount("countries"); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	// A database that left the server is a 404 for the next request.
	if resp := relHTTPPost(t, host.URL, "/v1/dtql", "", relHTTPCustomerRegions); resp.status != http.StatusNotFound {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
	_ = chinook
}

// Public caching: the headers of a GET on a read-only server with no auth and no
// policy follow the smallest cache_ttl of the databases read.
func TestRelationalDTQLCacheHeaders(t *testing.T) {
	chinook := relHTTPChinook(t, ", cache_ttl: 120s")
	countries := relHTTPCountries(t, ", cache_ttl: 60s")
	service := server.New("test", map[string]*core.Database{"chinook": chinook, "countries": countries}, server.WithReadOnly(true))
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	get := func(path, doc string) relHTTPResponse {
		return relHTTPDo(t, host.URL, http.MethodGet, path+"?q="+url.QueryEscape(doc), "", "", nil)
	}
	resp := get("/v1/dtql", relHTTPCustomerRegions)
	if resp.status != http.StatusOK || resp.header.Get("Cache-Control") != "public, max-age=60, s-maxage=60" {
		t.Fatalf("status %d, Cache-Control %q: %s", resp.status, resp.header.Get("Cache-Control"), resp.raw)
	}
	if !strings.Contains(resp.header.Get("Vary"), "OVDB-Page-Size") {
		t.Fatalf("Vary = %q", resp.header.Get("Vary"))
	}
	resp = get("/v1/databases/chinook/dtql", relHTTPInvoiceJoin)
	if resp.status != http.StatusOK || resp.header.Get("Cache-Control") != "public, max-age=120, s-maxage=120" {
		t.Fatalf("status %d, Cache-Control %q: %s", resp.status, resp.header.Get("Cache-Control"), resp.raw)
	}
	// A POST is never cached.
	if post := relHTTPPost(t, host.URL, "/v1/dtql", "", relHTTPCustomerRegions); post.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("POST Cache-Control = %q", post.header.Get("Cache-Control"))
	}
	// An error is never cached either.
	if bad := get("/v1/dtql", "from: {name: Customer}\n"); bad.status != http.StatusBadRequest || bad.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, Cache-Control %q", bad.status, bad.header.Get("Cache-Control"))
	}
	// Without a cache_ttl on one of the databases nothing is cached.
	plain := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, ""), "countries": countries}, server.WithReadOnly(true))
	defer plain.CloseSnapshots()
	plainHost := httptest.NewServer(plain.Handler())
	defer plainHost.Close()
	if resp := relHTTPDo(t, plainHost.URL, http.MethodGet, "/v1/dtql?q="+url.QueryEscape(relHTTPCustomerRegions), "", "", nil); resp.status != http.StatusOK || resp.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, Cache-Control %q", resp.status, resp.header.Get("Cache-Control"))
	}
	// A server with auth on caches nothing.
	secured := server.New("test", map[string]*core.Database{"chinook": chinook, "countries": countries}, server.WithReadOnly(true), server.WithAuth(&auth.Config{OwnerToken: ownerToken}))
	defer secured.CloseSnapshots()
	securedHost := httptest.NewServer(secured.Handler())
	defer securedHost.Close()
	if resp := relHTTPDo(t, securedHost.URL, http.MethodGet, "/v1/dtql?q="+url.QueryEscape(relHTTPCustomerRegions), ownerToken, "", nil); resp.status != http.StatusOK || resp.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, Cache-Control %q", resp.status, resp.header.Get("Cache-Control"))
	}
}

// relHTTPPolicy is an access policy for the crm database that lets the reader
// role read customers whose country is IE.
const relHTTPPolicyFile = `apiVersion: dtql.org/access/v1
kind: AccessPolicy
metadata:
  name: readers
  visibility: public
target:
  database: crm
composition: dalgo-hierarchical-v1
default: deny
ruleSets:
  reader:
    - path: /customers
      rules:
        - id: read-irish
          effect: allow
          operations: [query, get]
          where:
            op: "=="
            left: {field: country}
            right: {value: IE}
          fields: [id, name, country]
    - path: /orders
      rules:
        - id: read-orders
          effect: allow
          operations: [query, get]
          fields: [id, customer_id, total]
bindings:
  roles:
    reader: [reader]
`

// relHTTPProtected mounts a SQLite database named crm with access policies:
// customers carry a row filter, orders none.
func relHTTPProtected(t *testing.T) *core.Database {
	t.Helper()
	dir := t.TempDir()
	storage, err := sql.Open("sqlite", filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE "customers" ("id" TEXT PRIMARY KEY, "name" TEXT, "country" TEXT)`,
		`CREATE TABLE "orders" ("id" TEXT PRIMARY KEY, "customer_id" TEXT, "total" REAL)`,
		`INSERT INTO "customers" VALUES ('c1', 'Ada', 'IE'), ('c2', 'Grace', 'US'), ('c3', 'Edsger', 'IE')`,
		`INSERT INTO "orders" VALUES ('o1', 'c1', 10), ('o2', 'c2', 20), ('o3', 'c3', 30), ('o4', 'c1', 40)`,
	} {
		if _, err := storage.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := "database: {id: crm, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    customers:\n      fields:\n        id: {type: string}\n        name: {type: string}\n        country: {type: string}\n    orders:\n      fields:\n        id: {type: string}\n        customer_id: {type: string}\n        total: {type: number}\nacl: {enabled: true, policies: [readers.yaml]}\n"
	aclWriteFile(t, filepath.Join(dir, "readers.yaml"), relHTTPPolicyFile)
	aclWriteFile(t, filepath.Join(dir, "db.yaml"), manifest)
	db, err := mount.File(filepath.Join(dir, "db.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A relational document is not run on a mount with access policies: a join, a
// grouping and a subquery are each a 422 authorization_unsupported on both
// endpoints, and nothing is read for them. The mount itself refuses a joined read
// transaction, which is the one way a whole joined query could be handed to its
// driver. A document of one plain collection is answered as it always was, through
// the policy, with keys; a scan clause is refused by the classifier.
func TestRelationalDTQLOnAPolicyProtectedMountIsRefusedBeforeItsDriver(t *testing.T) {
	crm := relHTTPProtected(t)
	service := server.New("test", map[string]*core.Database{"crm": crm},
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken}))
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()

	const (
		join = "from: {%[1]sname: orders, alias: o, joins: [{from: {%[1]sname: customers, alias: c}, on: [{left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}]}]}\n" +
			"orderBy: [{field: id, source: o}]\ncolumns: [{field: id, source: o, as: order_id}, {field: name, source: c, as: customer}]\n"
		count = "from: {%[1]sname: orders, alias: o, joins: [{from: {%[1]sname: customers, alias: c}, on: [{left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}]}]}\n" +
			"columns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\n"
		subquery = "from: {%[1]sname: orders}\nwhere: {exists: {query: {from: {%[1]sname: customers}}}}\n"
	)
	for _, endpoint := range []struct{ name, path, database string }{
		{"per-database endpoint", "/v1/databases/crm/dtql", ""},
		{"/v1/dtql", "/v1/dtql", "database: crm, "},
	} {
		for shape, format := range map[string]string{"a join": join, "a grouped count": count, "a subquery": subquery} {
			t.Run(endpoint.name+", "+shape, func(t *testing.T) {
				resp := relHTTPPost(t, host.URL, endpoint.path, ownerToken, fmt.Sprintf(format, endpoint.database))
				if resp.status != http.StatusUnprocessableEntity || resp.errorField("code") != "authorization_unsupported" || resp.body["records"] != nil {
					t.Fatalf("status %d, want a 422 authorization_unsupported with no rows: %s", resp.status, resp.raw)
				}
			})
		}
	}
	ran := false
	if err := crm.ReadTx(context.Background(), func(dal.QueryExecutor) error { ran = true; return nil }); !errors.Is(err, core.ErrProtectedReadTx) || ran {
		t.Fatalf("ReadTx on a protected mount = %v (ran %v), want core.ErrProtectedReadTx and no run", err, ran)
	}
	t.Run("a document of one plain collection is read through the policy, with keys", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/databases/crm/dtql", ownerToken, "from: {name: customers}\norderBy: [{field: id}]\n")
		records, _ := resp.body["records"].([]any)
		if resp.status != http.StatusOK || len(records) != 2 || resp.body["columns"] != nil {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		if first, _ := records[0].(map[string]any); first["key"] != "customers/c1" {
			t.Fatalf("records = %v", records)
		}
	})
	t.Run("a scan clause is refused by the classifier", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/databases/crm/dtql", ownerToken, "from: {name: customers, scan: {limit: 1, orderBy: [{field: id}]}}\ncolumns: [{field: id}]\n")
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(resp.errorField("message"), "scan") {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
}

// relHTTPBig is a database with one collection of 1001 rows, one more than a
// response holds.
func relHTTPBig(t *testing.T) *core.Database {
	return relHTTPMount(t, "big", "",
		map[string][]string{"N": {"id", "k"}},
		`CREATE TABLE "N" ("id" TEXT PRIMARY KEY, "k" TEXT)`,
		`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 1001) INSERT INTO "N" SELECT printf('%04d', n), 'k' FROM seq`,
	)
}

// The refusals of the classifier for a document that the single-collection
// validator accepts are client errors that give the classifier's reason, on both
// endpoints.
func TestClassifierRefusalsOfADocumentTheSingleCollectionValidatorAcceptsAreClientErrors(t *testing.T) {
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, "")})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	nested := func(levels int, name string) string {
		doc := "from: {name: " + name + "}\n"
		for i := 0; i < levels; i++ {
			doc = "from: {name: " + name + "}\nwhere:\n  exists:\n    query:\n" + indentRelHTTP(doc, 6)
		}
		return doc
	}
	for _, tc := range []struct {
		name, doc, reason string
	}{
		{"money", "from: {name: Customer}\nmoney: {minorUnitScale: 2, divisionScale: 2, rounding: halfEven}\n", "money"},
		{"subqueries nested five deep", nested(5, "Customer"), "subqueries nest at most"},
		{"a scan bound on a nested source", "from: {name: Customer}\nwhere:\n  exists:\n    query:\n      from: {name: Invoice, scan: {limit: 3, orderBy: [{field: id}]}}\n", "scan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The premise: the validator of the single-collection path accepts the
			// document, so before the classifier it ran (money) or failed closed at
			// the adapter (the subquery shapes).
			if _, _, err := core.ParseDTQL([]byte(tc.doc)); err != nil {
				t.Fatalf("ParseDTQL refuses the document, so it is not the case under test: %v", err)
			}
			doc := tc.doc
			for _, path := range []string{"/v1/databases/chinook/dtql", "/v1/dtql"} {
				if path == "/v1/dtql" {
					doc = strings.ReplaceAll(tc.doc, "from: {name: ", "from: {database: chinook, name: ")
				}
				resp := relHTTPPost(t, host.URL, path, "", doc)
				if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(resp.errorField("message"), tc.reason) {
					t.Fatalf("%s: status %d, want a 400 invalid_dtql naming %q: %s", path, resp.status, tc.reason, resp.raw)
				}
			}
		})
	}
}

func indentRelHTTP(text string, spaces int) string {
	pad := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = pad + line
	}
	return strings.Join(lines, "\n") + "\n"
}

// What the executor reports reaches the caller with the status the handler maps
// it to: DALgo's own shape errors on the in-memory route, a null test, the
// strict field-name rule, a column the database does not know.
func TestRelationalDTQLStatusesOverHTTP(t *testing.T) {
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, ""), "countries": relHTTPCountries(t, "")})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()

	t.Run("an alias DALgo does not know is a 400 on the in-memory route", func(t *testing.T) {
		doc := strings.Replace(relHTTPCustomerRegions, "{field: name, source: k, as: country}", "{field: name, source: nowhere, as: country}", 1)
		resp := relHTTPPost(t, host.URL, "/v1/dtql", "", doc)
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
	t.Run("a null test runs and returns the rows that match", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/dtql", "", "from: {database: chinook, name: Customer}\nwhere: {isNotNull: {field: country}}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n")
		if resp.status != http.StatusOK || len(resp.rows(t)) != 3 {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		none := relHTTPPost(t, host.URL, "/v1/dtql", "", "from: {database: chinook, name: Customer}\nwhere: {isNull: {field: country}}\ncolumns: [{field: id}]\n")
		if none.status != http.StatusOK || len(none.rows(t)) != 0 {
			t.Fatalf("status %d: %s", none.status, none.raw)
		}
	})
	t.Run("a field name that only the wider quoted-name rule accepts is refused", func(t *testing.T) {
		// The walk of the executor holds the strict rule, whatever the engine: a
		// column named with a space is a plain query on the single-collection path and
		// a 400 on a relational document.
		resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", "from: {name: Customer, alias: c}\ncolumns: [{field: 'first name', source: c}]\n")
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(resp.errorField("message"), "field name") {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
	t.Run("a column the database does not know is a 400 that names it", func(t *testing.T) {
		resp := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", "from: {name: Customer, alias: c}\ncolumns: [{field: nosuchcolumn, source: c}]\n")
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(resp.errorField("message"), "nosuchcolumn") {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
}

// A query that runs out of its budget is a 422 that names the bound and the
// limit and returns no rows.
func TestRelationalDTQLBudgetErrorsOverHTTP(t *testing.T) {
	chinook, countries, big := relHTTPChinook(t, ""), relHTTPCountries(t, ""), relHTTPBig(t)
	service := server.New("test", map[string]*core.Database{"chinook": chinook, "countries": countries, "big": big},
		server.WithQueryLimits(server.QueryLimits{MaxSourceRows: 2}))
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	check := func(resp relHTTPResponse, name string, limit float64, route string) {
		t.Helper()
		detail, _ := resp.body["error"].(map[string]any)
		budget, _ := detail["budget"].(map[string]any)
		if resp.status != http.StatusUnprocessableEntity || resp.errorField("code") != "query_budget_exceeded" || budget["name"] != name || budget["limit"] != limit || budget["route"] != route {
			t.Fatalf("status %d, want a 422 for %s: %s", resp.status, name, resp.raw)
		}
		if detail["hint"] == "" || resp.body["records"] != nil || resp.body["execution"] != nil {
			t.Fatalf("a budget refusal has a hint and no rows: %s", resp.raw)
		}
	}
	// Three customers read from a source limited to two rows.
	check(relHTTPPost(t, host.URL, "/v1/dtql", "", relHTTPCustomerRegions), "source_rows", 2, "in-memory")
	// A result of 1001 rows is refused whole, not cut to 1000.
	check(relHTTPPost(t, host.URL, "/v1/databases/big/dtql", "", "from: {name: N, alias: n}\norderBy: [{field: id, source: n}]\n"), "response_rows", 1000, "database")
}

// The request timeout is a 504 on both routes, and the server answers the next
// request.
func TestRelationalDTQLTimeoutIsA504OverHTTP(t *testing.T) {
	chinook, countries := relHTTPChinook(t, ""), relHTTPCountries(t, "")
	service := server.New("test", map[string]*core.Database{"chinook": chinook, "countries": countries},
		server.WithQueryLimits(server.QueryLimits{Timeout: 1})) // one nanosecond: over before the first read
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	for name, tc := range map[string]struct{ path, doc string }{
		"database route":  {"/v1/databases/chinook/dtql", relHTTPInvoiceJoin},
		"in-memory route": {"/v1/dtql", relHTTPCustomerRegions},
	} {
		t.Run(name, func(t *testing.T) {
			resp := relHTTPPost(t, host.URL, tc.path, "", tc.doc)
			if resp.status != http.StatusGatewayTimeout || resp.errorField("code") != "query_timeout" {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
		})
	}
	if resp := relHTTPDo(t, host.URL, http.MethodGet, "/v1/status", "", "", nil); resp.status != http.StatusOK {
		t.Fatalf("the server does not answer after a timeout: %d", resp.status)
	}
}
