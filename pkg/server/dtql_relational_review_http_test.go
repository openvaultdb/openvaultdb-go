package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// Whole-path tests of the shapes a relational document can take that the first
// tests of the endpoint did not select: columns that leave out the key, columns
// that share an output name, a protected database that is asked for a collection
// it does not declare, the spelling of a collection, names that are too long to
// repeat, and the answer to a column no database knows. The helpers of this file
// start with relRev so they cannot clash with the helpers of the other test files
// of the package.

// relRevDocument joins Invoice (alias i) to Customer (alias c) in the database
// chinook and selects columns (a YAML list), then tail (more top-level keys). A
// document for /v1/dtql names the database of every source (qualified); one for
// the per-database endpoint names none.
func relRevDocument(qualified bool, columns, tail string) string {
	root, joined := "  name: Invoice\n", "{name: Customer, alias: c}"
	if qualified {
		root, joined = "  database: chinook\n  name: Invoice\n", "{database: chinook, name: Customer, alias: c}"
	}
	return "from:\n" + root + "  alias: i\n  joins:\n    - type: inner\n      from: " + joined + "\n" +
		"      on:\n        - {left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}\n" +
		"columns:\n" + columns + tail
}

// relRevEndpoints are the two routes a document of the join reaches: the
// per-database endpoint, with sources that name no database, and /v1/dtql, with
// sources that name it.
var relRevEndpoints = []struct {
	name      string
	path      string
	qualified bool
}{
	{"per-database endpoint", "/v1/databases/chinook/dtql", false},
	{"/v1/dtql", "/v1/dtql", true},
}

// A join that selects no unaliased column named like the registered key of its
// collections is answered, whichever way the database or the executor reads it.
func TestAJoinThatSelectsNoUnaliasedKeyColumnIsAnsweredOverHTTP(t *testing.T) {
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, ""), "countries": relHTTPCountries(t, "")})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	const orderByID = "orderBy:\n  - {field: id, source: i}\n"
	for _, endpoint := range relRevEndpoints {
		for _, tc := range []struct {
			name    string
			columns string
			tail    string
			want    []string
			rows    int
			first   map[string]any
		}{
			{
				"columns that leave out the key",
				"  - {field: name, source: c}\n  - {field: total, source: i}\n", orderByID,
				[]string{"name", "total"}, 5, map[string]any{"name": "Ada", "total": float64(10)},
			},
			{
				"the key of the first source under an alias",
				"  - {field: id, source: i, as: invoice_id}\n  - {field: name, source: c}\n", orderByID,
				[]string{"invoice_id", "name"}, 5, map[string]any{"invoice_id": "i1", "name": "Ada"},
			},
			{
				"the key under an alias, with an order and a limit",
				"  - {field: id, source: i, as: invoice_id}\n  - {field: name, source: c}\n", orderByID + "limit: 2\n",
				[]string{"invoice_id", "name"}, 2, map[string]any{"invoice_id": "i1", "name": "Ada"},
			},
			{
				"the key of the first source with no alias",
				"  - {field: id, source: i}\n  - {field: name, source: c}\n", orderByID,
				[]string{"id", "name"}, 5, map[string]any{"id": "i1", "name": "Ada"},
			},
			{
				"the key of the joined source with no alias",
				"  - {field: name, source: c}\n  - {field: id, source: c}\n", orderByID,
				[]string{"name", "id"}, 5, map[string]any{"name": "Ada", "id": "c1"},
			},
		} {
			t.Run(endpoint.name+", "+tc.name, func(t *testing.T) {
				resp := relHTTPPost(t, host.URL, endpoint.path, "", relRevDocument(endpoint.qualified, tc.columns, tc.tail))
				if resp.status != http.StatusOK {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
				if got := resp.columns(); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("columns = %v, want %v", got, tc.want)
				}
				rows := resp.rows(t)
				if len(rows) != tc.rows || !reflect.DeepEqual(rows[0], tc.first) {
					t.Fatalf("rows = %v, want %d rows starting with %v", rows, tc.rows, tc.first)
				}
				if route := resp.execution(t)["route"]; route != "database" && route != "in-memory" {
					t.Fatalf("execution = %v", resp.execution(t))
				}
			})
		}
	}
}

// Two columns of one query that carry the same output name are a 400 on both
// routes and on both endpoints, before anything is read: the answer never holds
// fewer columns than the document selected.
func TestTwoColumnsWithOneOutputNameAreRefusedOverHTTP(t *testing.T) {
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, ""), "countries": relHTTPCountries(t, "")})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	for _, endpoint := range relRevEndpoints {
		for name, columns := range map[string]string{
			"the same field of two sources":                      "  - {field: id, source: i}\n  - {field: id, source: c}\n",
			"two aliases of one name":                            "  - {field: total, source: i, as: a}\n  - {field: name, source: c, as: a}\n",
			"an alias equal to the field name of another column": "  - {field: id, source: i}\n  - {field: name, source: c, as: id}\n",
		} {
			t.Run(endpoint.name+", "+name, func(t *testing.T) {
				resp := relHTTPPost(t, host.URL, endpoint.path, "", relRevDocument(endpoint.qualified, columns, ""))
				if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(resp.errorField("message"), "duplicate output name") {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
				if resp.body["records"] != nil || resp.body["columns"] != nil {
					t.Fatalf("a refused document returned a result: %s", resp.raw)
				}
			})
		}
	}
	t.Run("a join across two databases", func(t *testing.T) {
		doc := strings.Replace(relHTTPCustomerRegions, "{field: name, source: k, as: country}", "{field: name, source: k}", 1)
		doc = strings.Replace(doc, "{field: name, source: c, as: customer}", "{field: name, source: c}", 1)
		resp := relHTTPPost(t, host.URL, "/v1/dtql", "", doc)
		if resp.status != http.StatusBadRequest || !strings.Contains(resp.errorField("message"), "duplicate output name") {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
	t.Run("a derived source", func(t *testing.T) {
		doc := "from:\n  query:\n    as: d\n    from: {database: chinook, name: Customer}\n    columns: [{field: id}, {field: name, as: id}]\ncolumns: [{field: id, source: d}]\n"
		resp := relHTTPPost(t, host.URL, "/v1/dtql", "", doc)
		if resp.status != http.StatusBadRequest || !strings.Contains(resp.errorField("message"), "duplicate output name") {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
}

// The same documents with distinct output names are answered: one field under
// two aliases, and a name that a derived source and the query around it share.
func TestColumnsWhoseOutputNamesDifferAreAnsweredOverHTTP(t *testing.T) {
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, "")})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	resp := relHTTPPost(t, host.URL, "/v1/dtql", "", "from:\n  query:\n    as: d\n    from: {database: chinook, name: Customer}\n    columns: [{field: id}, {field: name}]\norderBy: [{field: id, source: d}]\ncolumns: [{field: id, source: d}, {field: name, source: d, as: first}, {field: name, source: d, as: second}]\n")
	if resp.status != http.StatusOK {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
	if got := resp.columns(); !reflect.DeepEqual(got, []string{"id", "first", "second"}) {
		t.Fatalf("columns = %v", got)
	}
}

// relRevProtectedServer serves the policy-protected crm database to a principal
// that holds role: the policy of relHTTPProtected lets the role reader read
// customers and orders (customers only in part) and gives every other role
// nothing.
func relRevProtectedServer(t *testing.T, role string) *httptest.Server {
	t.Helper()
	service := server.New("test", map[string]*core.Database{"crm": relHTTPProtected(t)},
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{role}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken}))
	t.Cleanup(service.CloseSnapshots)
	host := httptest.NewServer(service.Handler())
	t.Cleanup(host.Close)
	return host
}

// A database with access policies answers a collection it does not declare as it
// answers one the policy does not allow: the same status and the same body, so
// the answer says nothing about which collections the database declares.
func TestAnUndeclaredCollectionOfAProtectedDatabaseIsAnsweredAsADenial(t *testing.T) {
	host := relRevProtectedServer(t, "nobody")
	for _, endpoint := range []struct{ name, path string }{
		{"per-database endpoint", "/v1/databases/crm/dtql"},
		{"/v1/dtql", "/v1/dtql"},
	} {
		for _, tc := range []struct {
			name              string
			declared, missing string
			headers           map[string]string
		}{
			{"as the root source", "from: {database: crm, name: customers}\n", "from: {database: crm, name: ghost}\n", nil},
			{
				"in a subquery",
				"from: {database: crm, name: orders}\nwhere: {exists: {query: {from: {database: crm, name: customers}}}}\n",
				"from: {database: crm, name: orders}\nwhere: {exists: {query: {from: {database: crm, name: ghost}}}}\n", nil,
			},
			{
				"in a join",
				"from: {database: crm, name: orders, alias: o, joins: [{from: {database: crm, name: customers, alias: c}, on: [{left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}]}]}\n",
				"from: {database: crm, name: orders, alias: o, joins: [{from: {database: crm, name: ghost, alias: c}, on: [{left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}]}]}\n", nil,
			},
			{"with a paging header", "from: {database: crm, name: customers}\n", "from: {database: crm, name: ghost}\n", map[string]string{"OVDB-Page-Size": "10"}},
			{"with a spelling the database does not declare", "from: {database: crm, name: customers}\n", "from: {database: crm, name: '\"customers\"'}\n", nil},
		} {
			t.Run(endpoint.name+", "+tc.name, func(t *testing.T) {
				declared := relHTTPDo(t, host.URL, http.MethodPost, endpoint.path, ownerToken, tc.declared, tc.headers)
				missing := relHTTPDo(t, host.URL, http.MethodPost, endpoint.path, ownerToken, tc.missing, tc.headers)
				if declared.status/100 != 4 || declared.status != missing.status || declared.raw != missing.raw {
					t.Fatalf("declared collection: %d %s\nundeclared collection: %d %s\nwant one answer for both", declared.status, declared.raw, missing.status, missing.raw)
				}
				if strings.Contains(missing.raw, "ghost") {
					t.Fatalf("the answer repeats the name: %s", missing.raw)
				}
			})
		}
	}
	t.Run("a role the policy allows reads what the policy allows, and an undeclared collection is still a denial", func(t *testing.T) {
		host := relRevProtectedServer(t, "reader")
		ok := relHTTPDo(t, host.URL, http.MethodPost, "/v1/dtql", ownerToken, "from: {database: crm, name: orders}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n", nil)
		if ok.status != http.StatusOK || len(ok.rows(t)) != 4 {
			t.Fatalf("status %d: %s", ok.status, ok.raw)
		}
		// A subquery that reads what the policy allows is answered in memory on
		// /v1/dtql, every read passing the policy; the per-database endpoint keeps the
		// 422 of the single-collection path for a document of this shape.
		evaluated := relHTTPDo(t, host.URL, http.MethodPost, "/v1/dtql", ownerToken, "from: {database: crm, name: orders}\nwhere: {exists: {query: {from: {database: crm, name: customers}}}}\norderBy: [{field: id}]\ncolumns: [{field: id}]\n", nil)
		if evaluated.status != http.StatusOK || len(evaluated.rows(t)) != 4 || evaluated.execution(t)["route"] != "in-memory" {
			t.Fatalf("a subquery of what the policy allows: status %d: %s", evaluated.status, evaluated.raw)
		}
		perDatabase := relHTTPDo(t, host.URL, http.MethodPost, "/v1/databases/crm/dtql", ownerToken, "from: {name: orders}\nwhere: {exists: {query: {from: {name: customers}}}}\n", nil)
		if perDatabase.status != http.StatusUnprocessableEntity || perDatabase.errorField("code") != "authorization_unsupported" {
			t.Fatalf("the same document on the per-database endpoint: status %d: %s", perDatabase.status, perDatabase.raw)
		}
		for name, doc := range map[string]string{
			"as the root source": "from: {database: crm, name: ghost}\n",
			// The executor loads the fields of the joined source before it reads a row.
			"in a join":                       "from: {database: crm, name: orders, alias: o, joins: [{from: {database: crm, name: ghost, alias: g}, on: [{left: {field: customer_id, source: o}, op: '==', right: {field: id, source: g}}]}]}\ncolumns: [{field: id, source: o}]\n",
			"in a subquery that is evaluated": "from: {database: crm, name: orders}\nwhere: {exists: {query: {from: {database: crm, name: ghost}}}}\n",
		} {
			missing := relHTTPDo(t, host.URL, http.MethodPost, "/v1/dtql", ownerToken, doc, nil)
			if missing.status != http.StatusForbidden || missing.errorField("code") != "ACCESS_DENIED" || strings.Contains(missing.raw, "ghost") {
				t.Fatalf("%s: status %d: %s", name, missing.status, missing.raw)
			}
		}
	})
}

// relRevElapsed matches the time a request took, the one part of a successful
// answer that differs between two runs of the same request.
var relRevElapsed = regexp.MustCompile(`"elapsedMs":\d+`)

// A database with access policies answers the shapes of a document that do not
// reach a policy decision exactly as it answers them for a collection it does not
// declare: the answer comes where a policy denial would, at the read, and no
// answer before the read tells a declared collection from an undeclared one. The
// three shapes: a document refused for its columns before anything is read, a
// document refused for a field name only the classifier accepts, and a subquery
// that is never evaluated because the condition before it is false. Each is sent
// with a declared collection and with an undeclared one in the place the document
// reads, for a principal the policy gives nothing and for one that may read.
func TestAProtectedDatabaseAnswersADeclaredAndAnUndeclaredCollectionAlike(t *testing.T) {
	const (
		repeated = "columns: [{field: id}, {field: name, as: id}]\n"
		spaced   = "columns: [{field: 'zip code'}]\n"
		never    = "from: {database: crm, name: orders}\nwhere: {and: [{op: '==', left: {field: id}, right: {value: none}}, {exists: {query: {from: {database: crm, name: %s}}}}]}\n"
	)
	root := func(name, tail string) string { return "from: {database: crm, name: " + name + "}\n" + tail }
	for _, role := range []struct {
		name string
		// never is the status of the subquery that is never evaluated: a principal
		// the policy gives nothing is denied the collection the document reads first.
		never int
	}{{"nobody", http.StatusForbidden}, {"reader", http.StatusOK}} {
		host := relRevProtectedServer(t, role.name)
		for _, endpoint := range []struct{ name, path string }{
			{"per-database endpoint", "/v1/databases/crm/dtql"},
			{"/v1/dtql", "/v1/dtql"},
		} {
			for _, tc := range []struct {
				name              string
				declared, missing string
				status            int
			}{
				{"two columns of one output name", root("customers", repeated), root("ghost", repeated), http.StatusBadRequest},
				{"a field name only the classifier accepts", root("customers", spaced), root("ghost", spaced), http.StatusBadRequest},
				{"a subquery behind a false condition", fmt.Sprintf(never, "customers"), fmt.Sprintf(never, "ghost"), role.never},
			} {
				t.Run(role.name+", "+endpoint.name+", "+tc.name, func(t *testing.T) {
					declared := relHTTPDo(t, host.URL, http.MethodPost, endpoint.path, ownerToken, tc.declared, nil)
					missing := relHTTPDo(t, host.URL, http.MethodPost, endpoint.path, ownerToken, tc.missing, nil)
					if declared.status != tc.status || missing.status != tc.status {
						t.Fatalf("declared collection: %d %s\nundeclared collection: %d %s\nwant %d for both", declared.status, declared.raw, missing.status, missing.raw, tc.status)
					}
					if got, want := relRevElapsed.ReplaceAllString(missing.raw, `"elapsedMs":0`), relRevElapsed.ReplaceAllString(declared.raw, `"elapsedMs":0`); got != want {
						t.Fatalf("declared collection: %s\nundeclared collection: %s\nwant one answer for both", declared.raw, missing.raw)
					}
					if strings.Contains(missing.raw, "ghost") {
						t.Fatalf("the answer repeats the name: %s", missing.raw)
					}
				})
			}
		}
	}
}

// A database without access policies keeps saying which collection it does not
// declare, and a collection that the document spells another way than its
// canonical name is not read: the table of the quote characters is not the table
// the manifest declares.
func TestARelationalDocumentReadsACollectionUnderItsCanonicalNameOnly(t *testing.T) {
	f := startSQLNamesWithQuotedTable(t)
	const publicName, quotedName = "Order Details", `"Order Details"`
	for _, endpoint := range []struct{ name, path string }{
		{"per-database endpoint", "/v1/databases/dev/dtql"},
		{"/v1/dtql", "/v1/dtql"},
	} {
		post := func(doc string) relHTTPResponse {
			return relHTTPPost(t, f.ts.URL, endpoint.path, "", doc)
		}
		t.Run(endpoint.name+", the canonical name", func(t *testing.T) {
			resp := post("from: {database: dev, name: '" + publicName + "'}\n")
			if resp.status != http.StatusOK || !strings.Contains(resp.raw, "row of Order Details") || strings.Contains(resp.raw, sqlNamesQuotedTable.marker) {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
		})
		for name, doc := range map[string]string{
			"as the root source": "from: {database: dev, name: '" + quotedName + "'}\n",
			"in a subquery":      "from: {database: dev, name: '" + publicName + "'}\nwhere: {exists: {query: {from: {database: dev, name: '" + quotedName + "'}}}}\n",
			"in a join": "from: {database: dev, name: '" + publicName + "', alias: a, joins: [{from: {database: dev, name: '" + quotedName + "', alias: b}, " +
				"on: [{left: {field: id, source: a}, op: '==', right: {field: id, source: b}}]}]}\n",
		} {
			t.Run(endpoint.name+", the quoted spelling "+name, func(t *testing.T) {
				resp := post(doc)
				if resp.status != http.StatusNotFound || resp.errorField("code") != "not_found" || strings.Contains(resp.raw, sqlNamesQuotedTable.marker) {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
			})
		}
		t.Run(endpoint.name+", a collection the database does not declare", func(t *testing.T) {
			resp := post("from: {database: dev, name: ghost}\n")
			if resp.status != http.StatusNotFound || resp.errorField("code") != "not_found" {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
		})
	}
	f.quotedTableUntouched(t)
}

// A name that came with the request is repeated in an error at no more than 1 KiB
// of text, whichever refusal it comes with, on both endpoints.
func TestRefusalsOfADocumentRepeatNoMoreThanABoundedTextOverHTTP(t *testing.T) {
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, "")})
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	long := strings.Repeat("n", 6000)
	jsonHeader := map[string]string{"Content-Type": "application/json"}
	for _, endpoint := range []string{"/v1/databases/chinook/dtql", "/v1/dtql"} {
		for _, tc := range []struct {
			name    string
			body    string
			headers map[string]string
			status  int
			code    string
		}{
			{"a database id that is not valid", "from: {database: '" + long + " x', name: Customer}\n", nil, http.StatusBadRequest, "invalid_dtql"},
			{"a collection name with a control character", "from: {database: chinook, name: \"" + long + "\\u0001\"}\n", nil, http.StatusBadRequest, ""},
			{"a join type that is not known", "from: {database: chinook, name: Customer, alias: c, joins: [{type: " + long + ", from: {database: chinook, name: Invoice, alias: i}, on: [{left: {field: id, source: c}, op: '==', right: {field: customer_id, source: i}}]}]}\n", nil, http.StatusBadRequest, "invalid_dtql"},
			{
				"a parameter that is not bound",
				`{"query": "from: {database: chinook, name: Customer}\nwhere: {op: '==', left: {field: id}, right: {param: ` + long + `}}\n"}`,
				jsonHeader, http.StatusBadRequest, "invalid_dtql",
			},
		} {
			t.Run(endpoint+", "+tc.name, func(t *testing.T) {
				resp := relHTTPDo(t, host.URL, http.MethodPost, endpoint, "", tc.body, tc.headers)
				if resp.status != tc.status || (tc.code != "" && resp.errorField("code") != tc.code) {
					t.Fatalf("status %d code %q, want %d %q: %.300s", resp.status, resp.errorField("code"), tc.status, tc.code, resp.raw)
				}
				if len(resp.raw) > 2048 {
					t.Fatalf("the body is %d bytes, want under 2 KiB", len(resp.raw))
				}
			})
		}
	}
	t.Run("the GET form, with a parameter of a name that is not supported", func(t *testing.T) {
		resp := relHTTPDo(t, host.URL, http.MethodGet, "/v1/dtql?"+strings.Repeat("p", 6000)+"=1", "", "", nil)
		if resp.status != http.StatusBadRequest || len(resp.raw) > 2048 {
			t.Fatalf("status %d, %d bytes", resp.status, len(resp.raw))
		}
	})
}

// A column that the database does not know is a 400 when the database runs the
// whole document and a null when the executor reads the sources and joins them
// itself; a correlated subquery reads its inner collection once for every row of
// the outer one, and every read counts against the source budget.
func TestAnUnknownColumnAndACorrelatedSubqueryOverHTTP(t *testing.T) {
	chinook, countries := relHTTPChinook(t, ""), relHTTPCountries(t, "")
	t.Run("an unknown column", func(t *testing.T) {
		service := server.New("test", map[string]*core.Database{"chinook": chinook, "countries": countries})
		defer service.CloseSnapshots()
		host := httptest.NewServer(service.Handler())
		defer host.Close()
		database := relHTTPPost(t, host.URL, "/v1/databases/chinook/dtql", "", relRevDocument(false, "  - {field: id, source: i}\n  - {field: nosuch, source: c}\n", ""))
		if database.status != http.StatusBadRequest || !strings.Contains(database.errorField("message"), "nosuch") {
			t.Fatalf("database route: status %d: %s", database.status, database.raw)
		}
		doc := strings.Replace(relHTTPCustomerRegions, "{field: region, source: k}", "{field: nosuch, source: k}", 1)
		memory := relHTTPPost(t, host.URL, "/v1/dtql", "", doc)
		if memory.status != http.StatusOK || memory.execution(t)["route"] != "in-memory" {
			t.Fatalf("in-memory route: status %d: %s", memory.status, memory.raw)
		}
		rows := memory.rows(t)
		if len(rows) != 3 {
			t.Fatalf("rows = %v", rows)
		}
		for _, row := range rows {
			if value, present := row["nosuch"]; !present || value != nil {
				t.Fatalf("row = %v, want a null for the column no database knows", row)
			}
		}
	})
	t.Run("a correlated subquery", func(t *testing.T) {
		// Invoices that have a customer: for each of the five invoices the customers are
		// read again, up to the first that matches.
		doc := relHTTPInvoicesWithACustomer
		open := server.New("test", map[string]*core.Database{"chinook": chinook})
		defer open.CloseSnapshots()
		openHost := httptest.NewServer(open.Handler())
		defer openHost.Close()
		resp := relHTTPPost(t, openHost.URL, "/v1/dtql", "", doc)
		if resp.status != http.StatusOK || len(resp.rows(t)) != 5 {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
		reads := map[string]float64{}
		for _, source := range resp.execution(t)["sources"].([]any) {
			entry := source.(map[string]any)
			reads[entry["collection"].(string)], _ = entry["rows"].(float64)
		}
		// Customer holds three rows and is read ten times over.
		if reads["Invoice"] != 5 || reads["Customer"] != 10 {
			t.Fatalf("rows read per collection = %v, want 5 invoices and 10 customer reads", reads)
		}
		// One pass over each collection reads 5 + 3 rows, so a budget of 10 would pass
		// it; the 15 rows the document reads do not fit.
		small := server.New("test", map[string]*core.Database{"chinook": chinook}, server.WithQueryLimits(server.QueryLimits{MaxSourceRows: 10}))
		defer small.CloseSnapshots()
		smallHost := httptest.NewServer(small.Handler())
		defer smallHost.Close()
		refused := relHTTPPost(t, smallHost.URL, "/v1/dtql", "", doc)
		if refused.status != http.StatusUnprocessableEntity || refused.errorField("code") != "query_budget_exceeded" {
			t.Fatalf("status %d: %s", refused.status, refused.raw)
		}
	})
}

// A document that still holds a parameter when it reaches the executor, which a
// YAML body does (only a JSON body binds parameters), is a mistake of the caller:
// a 400 that logs nothing, on the route DALgo's streaming join takes (no ORDER BY)
// and on the other, in the document and inside a derived source or a subquery. The
// same document sent as JSON, with the parameter bound, is answered.
func TestAParameterNoBinderReplacedIsARefusalOverHTTP(t *testing.T) {
	var logs bytes.Buffer
	service := server.New("test", map[string]*core.Database{"chinook": relHTTPChinook(t, ""), "countries": relHTTPCountries(t, "")},
		server.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	defer service.CloseSnapshots()
	host := httptest.NewServer(service.Handler())
	defer host.Close()
	const join = `from:
  database: chinook
  name: Customer
  alias: c
  joins:
    - type: inner
      from: {database: countries, name: Country, alias: k}
      on:
        - {left: {field: country, source: c}, op: '==', right: {field: code, source: k}}
`
	const (
		where   = "where: {op: '==', left: {field: name, source: c}, right: {param: n}}\n"
		order   = "orderBy:\n  - {field: name, source: c}\n"
		columns = "columns:\n  - {field: name, source: c, as: customer}\n  - {field: name, source: k, as: country}\n"
		derived = `from:
  query:
    as: d
    from:
      database: chinook
      name: Customer
      alias: c
      joins:
        - type: inner
          from: {database: countries, name: Country, alias: k}
          on:
            - {left: {field: country, source: c}, op: '==', right: {field: code, source: k}}
    where: {op: '==', left: {field: name, source: c}, right: {param: n}}
    columns:
      - {field: name, source: c, as: customer}
columns:
  - {field: customer, source: d}
`
		subquery = join + "where:\n  exists:\n    query:\n      from: {database: chinook, name: Invoice, alias: i}\n      where: {op: '==', left: {field: total, source: i}, right: {param: n}}\n" + columns
	)
	for _, tc := range []struct{ name, doc string }{
		{"a join without ORDER BY", join + where + columns},
		{"a join with ORDER BY", join + where + order + columns},
		{"a derived source", derived},
		{"a subquery", subquery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := relHTTPPost(t, host.URL, "/v1/dtql", "", tc.doc)
			if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(resp.errorField("message"), `"n"`) {
				t.Fatalf("status %d, want a 400 invalid_dtql that names the parameter: %s", resp.status, resp.raw)
			}
			if logs.Len() != 0 {
				t.Fatalf("a mistake of the caller was logged: %s", logs.String())
			}
		})
	}
	t.Run("the same join, with the parameter bound by a JSON body", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{"query": join + where + order + columns, "parameters": map[string]any{"n": "Ada"}})
		if err != nil {
			t.Fatal(err)
		}
		resp := relHTTPDo(t, host.URL, http.MethodPost, "/v1/dtql", "", string(body), map[string]string{"Content-Type": "application/json"})
		if resp.status != http.StatusOK {
			t.Fatalf("status %d: %s", resp.status, resp.raw)
		}
	})
}
