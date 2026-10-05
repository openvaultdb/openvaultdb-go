package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// What the field lists the mounts supply (OJ-14) must not take away from a document
// that was answered before they were supplied, over HTTP, on real mounts: an
// aggregate that orders by the alias of its own column, a nested path of an object
// field, and a refusal inside a derived source. The helpers of this file all start
// with relIntFields so they cannot clash with the others of the package.

// relIntFieldsSales is the dataset of the aggregate documents: three customers, and
// invoices of which one has no total.
func relIntFieldsSales() relIntSet {
	n := func(v string) json.Number { return json.Number(v) }
	return relIntSet{
		tables: map[string][]string{
			"Customer": {"CustomerId", "FirstName", "Country"},
			"Invoice":  {"InvoiceId", "CustomerId", "Total"},
		},
		rows: map[string][]map[string]any{
			"Customer": {
				{"CustomerId": n("1"), "FirstName": "Ada", "Country": "IE"},
				{"CustomerId": n("2"), "FirstName": "Bea", "Country": "US"},
				{"CustomerId": n("3"), "FirstName": "Cy", "Country": "IE"},
			},
			"Invoice": {
				{"InvoiceId": n("10"), "CustomerId": n("1"), "Total": n("100")},
				{"InvoiceId": n("11"), "CustomerId": n("1"), "Total": nil},
				{"InvoiceId": n("12"), "CustomerId": n("2"), "Total": n("200")},
				{"InvoiceId": n("13"), "CustomerId": n("3"), "Total": n("50")},
			},
		},
	}
}

// relIntFieldsServer serves the sales dataset in a SQLite mount and in a strict
// inGitDB mount, which both supply the fields of their collections.
func relIntFieldsServer(t *testing.T) string {
	t.Helper()
	set := relIntFieldsSales()
	mounts := map[string]*core.Database{}
	for _, engine := range relIntEngines {
		mounts[engine.id] = engine.mount(t, engine.id, "", set)
	}
	return relIntServe(t, mounts)
}

// relIntFieldsEachRoute posts doc to both engines on both endpoints and hands each
// answer to check. The document names no database: the per-database endpoint reads it
// as it is and /v1/dtql gets the database written into every source.
func relIntFieldsEachRoute(t *testing.T, base, doc string, check func(t *testing.T, resp relHTTPResponse)) {
	t.Helper()
	for _, engine := range relIntEngines {
		for _, endpoint := range []struct{ name, path, database string }{
			{"per-database endpoint", "/v1/databases/" + engine.id + "/dtql", ""},
			{"/v1/dtql", "/v1/dtql", engine.id},
		} {
			t.Run(engine.name+", "+endpoint.name, func(t *testing.T) {
				check(t, relHTTPPost(t, base, endpoint.path, "", string(relIntRewrite(t, []byte(doc), endpoint.database))))
			})
		}
	}
}

const (
	relIntFieldsSpent = "from: {name: Invoice, alias: i}\n" +
		"groupBy: [{field: CustomerId, source: i}]\n"
	relIntFieldsSpentColumns = "columns:\n  - {field: CustomerId, source: i}\n" +
		"  - {aggregate: {function: sum, args: [{field: Total, source: i}]}, as: TotalSpent}\n"
	// relIntFieldsAnyCustomer is a test that is true for every row: it puts a document
	// on the evaluation of DALgo on a SQLite mount too, which would answer an aggregate
	// of one source itself.
	relIntFieldsAnyCustomer = "where: {exists: {query: {from: {name: Customer, alias: c}}}}\n"
)

// ORDER BY and HAVING name the alias of a column of an aggregating query: it is the
// column the query computed, not a field of a source.
func TestAnAggregateMayOrderAndFilterByTheAliasOfItsOwnColumn(t *testing.T) {
	base := relIntFieldsServer(t)
	byTotal := []map[string]any{
		{"CustomerId": float64(2), "TotalSpent": float64(200)},
		{"CustomerId": float64(1), "TotalSpent": float64(100)},
		{"CustomerId": float64(3), "TotalSpent": float64(50)},
	}
	for name, tc := range map[string]struct {
		doc  string
		want []map[string]any
	}{
		"ORDER BY the alias": {
			relIntFieldsSpent + "orderBy: [{field: TotalSpent, desc: true}]\n" + relIntFieldsSpentColumns,
			byTotal,
		},
		"ORDER BY the alias, with a test that puts the document in memory on SQLite": {
			relIntFieldsSpent + relIntFieldsAnyCustomer + "orderBy: [{field: TotalSpent, desc: true}]\n" + relIntFieldsSpentColumns,
			byTotal,
		},
		"HAVING the alias": {
			relIntFieldsSpent + "having: {op: '>', left: {field: TotalSpent}, right: {value: 60}}\n" +
				"orderBy: [{field: CustomerId, source: i}]\n" + relIntFieldsSpentColumns,
			[]map[string]any{{"CustomerId": float64(1), "TotalSpent": float64(100)}, {"CustomerId": float64(2), "TotalSpent": float64(200)}},
		},
		"HAVING the alias, with a test that puts the document in memory on SQLite": {
			relIntFieldsSpent + relIntFieldsAnyCustomer + "having: {op: '>', left: {field: TotalSpent}, right: {value: 60}}\n" +
				"orderBy: [{field: CustomerId, source: i}]\n" + relIntFieldsSpentColumns,
			[]map[string]any{{"CustomerId": float64(1), "TotalSpent": float64(100)}, {"CustomerId": float64(2), "TotalSpent": float64(200)}},
		},
		"ORDER BY and HAVING the alias of a join": {
			"from: {name: Invoice, alias: i, joins: [{type: inner, from: {name: Customer, alias: c}, " +
				"on: [{left: {field: CustomerId, source: i}, op: '==', right: {field: CustomerId, source: c}}]}]}\n" +
				// The parser keeps an unqualified field out of a join with no subquery, so the
				// document carries a test that is true of every row.
				"where: {exists: {query: {from: {name: Customer, alias: x}}}}\n" +
				"groupBy: [{field: Country, source: c}]\n" +
				"having: {op: '>', left: {field: Revenue}, right: {value: 10}}\n" +
				"orderBy: [{field: Revenue, desc: true}]\n" +
				"columns:\n  - {field: Country, source: c}\n  - {aggregate: {function: sum, args: [{field: Total, source: i}]}, as: Revenue}\n",
			[]map[string]any{{"Country": "US", "Revenue": float64(200)}, {"Country": "IE", "Revenue": float64(150)}},
		},
		"ORDER BY the alias of a key column": {
			relIntFieldsSpent + "orderBy: [{field: Customer, desc: true}]\n" +
				"columns:\n  - {field: CustomerId, source: i, as: Customer}\n  - {aggregate: {function: count, args: [{star: true}]}, as: Invoices}\n",
			[]map[string]any{
				{"Customer": float64(3), "Invoices": float64(1)},
				{"Customer": float64(2), "Invoices": float64(1)},
				{"Customer": float64(1), "Invoices": float64(2)},
			},
		},
		"ORDER BY the alias, in a derived source": {
			"from:\n  query:\n    as: spend\n    from: {name: Invoice, alias: i}\n    groupBy: [{field: CustomerId, source: i}]\n" +
				"    orderBy: [{field: TotalSpent, desc: true}]\n    limit: 2\n" +
				"    columns:\n      - {field: CustomerId, source: i}\n" +
				"      - {aggregate: {function: sum, args: [{field: Total, source: i}]}, as: TotalSpent}\n" +
				"columns:\n  - {field: CustomerId, source: spend}\n  - {field: TotalSpent, source: spend}\n",
			[]map[string]any{{"CustomerId": float64(2), "TotalSpent": float64(200)}, {"CustomerId": float64(1), "TotalSpent": float64(100)}},
		},
		"HAVING the alias, in an EXISTS test": {
			"from: {name: Customer, alias: c}\n" +
				"where:\n  exists:\n    query:\n      from: {name: Invoice, alias: i}\n" +
				"      where: {op: '==', left: {field: CustomerId, source: i}, right: {field: CustomerId, source: c}}\n" +
				"      groupBy: [{field: CustomerId, source: i}]\n" +
				"      having: {op: '>', left: {field: Spent}, right: {value: 150}}\n" +
				"      columns:\n        - {field: CustomerId, source: i}\n" +
				"        - {aggregate: {function: sum, args: [{field: Total, source: i}]}, as: Spent}\n" +
				"orderBy: [{field: CustomerId, source: c}]\ncolumns: [{field: FirstName, source: c}]\n",
			[]map[string]any{{"FirstName": "Bea"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			relIntFieldsEachRoute(t, base, tc.doc, func(t *testing.T, resp relHTTPResponse) {
				relIntRowsAre(t, resp, tc.want)
			})
		})
	}
}

// relIntFieldsPeople is a strict inGitDB mount whose Person declares an object field.
func relIntFieldsPeople(t *testing.T) *core.Database {
	t.Helper()
	db := relIntOpen(t, t.TempDir(), "database: {id: people, schema_mode: strict}\nstorage: {engine: ingitdb, path: data}\n"+
		"schemas:\n  collections:\n    Person:\n      fields:\n        name: {type: string}\n        address: {type: object}\n")
	for _, person := range []struct {
		id, name, city string
	}{{"p1", "Ada", "Leeds"}, {"p2", "Bea", "Cork"}} {
		key, err := core.ParseKey("Person", person.id)
		if err != nil {
			t.Fatal(err)
		}
		data := map[string]any{"name": person.name, "address": map[string]any{"city": person.city}}
		if _, err := db.Apply(context.Background(), []core.Op{{Op: "insert", Key: key, Data: data}}, "seed"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// A nested path of an object field is read as it was: the mount declares the field
// address, and the record holds what is inside it.
func TestANestedPathOfAnObjectFieldIsReadOnAStrictMount(t *testing.T) {
	base := relIntServe(t, map[string]*core.Database{"people": relIntFieldsPeople(t)})
	const anyone = "where: {exists: {query: {from: {name: Person, alias: x}}}}\n"
	for name, tc := range map[string]struct {
		doc  string
		want []map[string]any
	}{
		"a column": {
			"from: {name: Person, alias: p}\n" + anyone + "orderBy: [{field: name, source: p}]\n" +
				"columns: [{field: name, source: p}, {field: address.city, source: p, as: city}]\n",
			[]map[string]any{{"name": "Ada", "city": "Leeds"}, {"name": "Bea", "city": "Cork"}},
		},
		"WHERE": {
			"from: {name: Person, alias: p}\nwhere:\n  and:\n" +
				"    - {op: '==', left: {field: address.city, source: p}, right: {value: Leeds}}\n" +
				"    - exists: {query: {from: {name: Person, alias: x}}}\n" +
				"columns: [{field: name, source: p}]\n",
			[]map[string]any{{"name": "Ada"}},
		},
		"GROUP BY": {
			"from: {name: Person, alias: p}\ngroupBy: [{field: address.city, source: p}]\norderBy: [{field: address.city, source: p}]\n" +
				"columns:\n  - {field: address.city, source: p, as: city}\n  - {aggregate: {function: count, args: [{star: true}]}, as: people}\n",
			[]map[string]any{{"city": "Cork", "people": float64(1)}, {"city": "Leeds", "people": float64(1)}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, endpoint := range []struct{ name, path, database string }{
				{"per-database endpoint", "/v1/databases/people/dtql", ""},
				{"/v1/dtql", "/v1/dtql", "people"},
			} {
				t.Run(endpoint.name, func(t *testing.T) {
					relIntRowsAre(t, relHTTPPost(t, base, endpoint.path, "", string(relIntRewrite(t, []byte(tc.doc), endpoint.database))), tc.want)
				})
			}
		})
	}
}

// A name that a column of the select list carries is a field of a source everywhere
// but in HAVING and ORDER BY of an aggregating query (and in the ORDER BY of a query that
// does not aggregate, where it is replaced by the field its column selects: see
// TestOrderByTheAliasOfAFieldOfAMountWithNoFieldListIsSortedByThatField): in WHERE the name
// still has to be told apart, whichever source it belongs to, and is refused when a mount
// cannot say.
func TestAnAliasOfTheSelectListDoesNotHideAnUnqualifiedFieldOutsideAnAggregate(t *testing.T) {
	base := relIntScopeServer(t)
	const join = "from: {database: shop, name: Orders, alias: o, joins: [{type: inner, from: {database: notes, name: Note, alias: n}, " +
		"on: [{left: {field: person_id, source: o}, op: '==', right: {field: person_id, source: n}}]}]}\n"
	for name, doc := range map[string]string{
		"WHERE": join + "where:\n  and:\n    - {op: '==', left: {field: person_id}, right: {value: p1}}\n" +
			"    - exists: {query: {from: {database: shop, name: Orders, alias: x}}}\n" +
			"columns: [{field: mood, source: n, as: person_id}]\n",
		"an operand of a column": join + "where: {exists: {query: {from: {database: shop, name: Orders, alias: x}}}}\n" +
			"columns: [{field: mood, source: n, as: person_id}, {binary: {op: '+', left: {field: person_id}, right: {value: 1}}, as: other}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			relIntScopeRefused(t, relHTTPPost(t, base, "/v1/dtql", "", doc), "person_id", "qualify")
		})
	}
}

// A field that a derived source in the base position of a query is refused for is a
// 400 that says so, and not a fault of the server.
func TestARefusalInsideADerivedSourceInTheBasePositionIsAClientError(t *testing.T) {
	base := relIntFieldsServer(t)
	for name, tc := range map[string]struct{ doc, says string }{
		"a column no source has": {
			"from:\n  query:\n    as: recent\n    from: {name: Invoice, alias: i}\n    columns: [{field: Nope, source: i}]\n" +
				"columns: [{field: Nope, source: recent}]\n",
			`field "Nope" is unavailable`,
		},
		"a field two sources carry": {
			"from:\n  query:\n    as: joined\n    from: {name: Invoice, alias: i, joins: [{type: inner, from: {name: Customer, alias: c}, " +
				"on: [{left: {field: CustomerId, source: i}, op: '==', right: {field: CustomerId, source: c}}]}]}\n" +
				"    where: {exists: {query: {from: {name: Customer, alias: x}}}}\n" +
				"    columns: [{field: CustomerId}]\n" +
				"columns: [{field: CustomerId, source: joined}]\n",
			"ambiguous unqualified field CustomerId",
		},
	} {
		t.Run(name, func(t *testing.T) {
			relIntFieldsEachRoute(t, base, tc.doc, func(t *testing.T, resp relHTTPResponse) {
				message := resp.errorField("message")
				if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(message, tc.says) || resp.body["records"] != nil {
					t.Fatalf("status %d, want a 400 invalid_dtql that says %q: %s", resp.status, tc.says, resp.raw)
				}
				if !strings.Contains(message, "at from.query.columns[0]") {
					t.Fatalf("the message does not give the path under the derived source: %s", resp.raw)
				}
				if strings.Contains(message, "join_plan") || strings.Contains(message, "cannot scan") {
					t.Fatalf("the message repeats the wrapper of the engine: %s", resp.raw)
				}
			})
		})
	}
}

// What the field checks do not reach is pinned here, so that the sentences about them
// in the documentation stay as true as the code: a wildcard lists the fields of its source
// and a record that lacks an optional one has no key for it on DALgo's recursive plan and a
// null on its streaming plan, an unknown column inside an EXISTS test or a scalar subquery
// is read as a null, and an unqualified field is looked for in the sources of its own query
// only.
func TestWhatTheFieldChecksDoNotReachIsReadAsItAlwaysWas(t *testing.T) {
	// A strict mount whose records may lack an optional field: on the recursive plan the
	// wildcard lists the field and the record that lacks it has no key for it, not a null.
	db := relIntOpen(t, t.TempDir(), "database: {id: people, schema_mode: strict}\nstorage: {engine: ingitdb, path: data}\n"+
		"schemas:\n  collections:\n    Person:\n      fields:\n        name: {type: string}\n        nick: {type: string}\n")
	for _, person := range []struct {
		id, name string
		nick     any
	}{{"p1", "Ada", "ada"}, {"p2", "Bea", nil}} {
		key, err := core.ParseKey("Person", person.id)
		if err != nil {
			t.Fatal(err)
		}
		data := map[string]any{"name": person.name}
		if person.nick != nil {
			data["nick"] = person.nick
		}
		if _, err := db.Apply(context.Background(), []core.Op{{Op: "insert", Key: key, Data: data}}, "seed"); err != nil {
			t.Fatal(err)
		}
	}
	people := relIntServe(t, map[string]*core.Database{"people": db})
	wildcard := relHTTPPost(t, people, "/v1/databases/people/dtql", "",
		"from: {name: Person, alias: p}\nwhere: {exists: {query: {from: {name: Person, alias: x}}}}\norderBy: [{field: name, source: p}]\n"+
			"columns: [{wildcard: {source: p, exclude: [none]}}]\n")
	relIntRowsAre(t, wildcard, []map[string]any{{"name": "Ada", "nick": "ada"}, {"name": "Bea"}})
	if got := wildcard.columns(); !reflect.DeepEqual(got, []string{"name", "nick"}) {
		t.Fatalf("columns = %v", got)
	}

	// The streaming plan is DALgo's for a flat join of two sources that each name their
	// database, with no subquery, no ORDER BY and no aggregation: it projects the wildcard
	// from the list the mount supplied, and writes a null for the field a record lacks.
	streamed := relHTTPPost(t, people, "/v1/dtql", "",
		"from: {database: people, name: Person, alias: p, joins: [{type: inner, from: {database: people, name: Person, alias: q}, "+
			"on: [{left: {field: name, source: p}, op: '==', right: {field: name, source: q}}]}]}\n"+
			"columns: [{wildcard: {source: p, exclude: [none]}}]\n")
	rows := streamed.rows(t)
	sort.Slice(rows, func(i, j int) bool { return rows[i]["name"].(string) < rows[j]["name"].(string) })
	if want := []map[string]any{{"name": "Ada", "nick": "ada"}, {"name": "Bea", "nick": nil}}; streamed.status != http.StatusOK || !reflect.DeepEqual(rows, want) {
		t.Fatalf("the streaming plan answered status %d, rows %v, want %v: %s", streamed.status, rows, want, streamed.raw)
	}

	sales := relIntFieldsServer(t)
	const customers = "from: {name: Customer, alias: c}\norderBy: [{field: CustomerId, source: c}]\n"
	oneInvoice := "from: {name: Invoice, alias: i}, where: {op: '==', left: {field: InvoiceId, source: i}, right: {value: 10}}"
	for name, tc := range map[string]struct {
		doc  string
		want []map[string]any
	}{
		"an unknown column in an EXISTS test": {
			customers + "where: {exists: {query: {from: {name: Invoice, alias: i}, columns: [{field: Nope, source: i}]}}}\ncolumns: [{field: FirstName, source: c}]\n",
			[]map[string]any{{"FirstName": "Ada"}, {"FirstName": "Bea"}, {"FirstName": "Cy"}},
		},
		"an unknown column in a scalar subquery": {
			customers + "columns: [{field: FirstName, source: c}, {query: {as: s, " + oneInvoice + ", columns: [{field: Nope, source: i}]}}]\n",
			[]map[string]any{{"FirstName": "Ada", "s": nil}, {"FirstName": "Bea", "s": nil}, {"FirstName": "Cy", "s": nil}},
		},
		"a field of the outer query, unqualified, in a subquery": {
			customers + "columns: [{field: FirstName, source: c}, {query: {as: s, " + oneInvoice + ", columns: [{field: FirstName}]}}]\n",
			[]map[string]any{{"FirstName": "Ada", "s": nil}, {"FirstName": "Bea", "s": nil}, {"FirstName": "Cy", "s": nil}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			relIntRowsAre(t, relHTTPPost(t, sales, "/v1/databases/gitdb/dtql", "", tc.doc), tc.want)
		})
	}
}

// DALgo's aggregation reads the unqualified field of a column's expression as the column
// that is named so, so the name that stands for a column is not replaced by an expression
// that names another column: the answer is a refusal of the name, never an order by the
// other column. (The SQL engine of a SQLite database answers the same document on the
// database route; a test that puts it in memory, like the one below, does not.)
func TestAnAliasWhoseExpressionNamesAnotherColumnIsRefusedInsteadOfBeingReadAsIt(t *testing.T) {
	base := relIntFieldsServer(t)
	doc := "from: {name: Invoice, alias: i}\n" + relIntFieldsAnyCustomer +
		"groupBy: [{field: CustomerId}]\norderBy: [{field: customer, desc: true}]\n" +
		"columns:\n  - {field: CustomerId, as: customer}\n  - {aggregate: {function: count, args: [{star: true}]}, as: CustomerId}\n"
	relIntFieldsEachRoute(t, base, doc, func(t *testing.T, resp relHTTPResponse) {
		message := resp.errorField("message")
		if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || !strings.Contains(message, "customer") || resp.body["records"] != nil {
			t.Fatalf("status %d, want a 400 invalid_dtql that names customer: %s", resp.status, resp.raw)
		}
	})
}

// ORDER BY the alias of a column of a query that does not aggregate is sorted by the field
// the column selects, as one SQLite database sorts it when it runs the whole document: on
// the database route, and in memory on a source that supplies its fields (it was refused as
// an unavailable field on the per-database endpoint, and answered in the order the records
// were read on /v1/dtql, before the name was replaced by the field).
func TestOrderByTheAliasOfAColumnOfAQueryThatDoesNotAggregateIsSortedByTheFieldItSelects(t *testing.T) {
	base := relIntFieldsServer(t)
	doc := "from: {name: Customer, alias: c}\norderBy: [{field: name, desc: true}]\ncolumns: [{field: FirstName, source: c, as: name}]\n"
	for _, tc := range []struct {
		name, path, database string
		route                string
	}{
		{"sqlite, per-database endpoint", "/v1/databases/litedb/dtql", "", "database"},
		{"sqlite, /v1/dtql", "/v1/dtql", "litedb", "database"},
		{"ingitdb, per-database endpoint", "/v1/databases/gitdb/dtql", "", "in-memory"},
		{"ingitdb, /v1/dtql", "/v1/dtql", "gitdb", "in-memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := relHTTPPost(t, base, tc.path, "", string(relIntRewrite(t, []byte(doc), tc.database)))
			relIntRowsAre(t, resp, []map[string]any{{"name": "Cy"}, {"name": "Bea"}, {"name": "Ada"}})
			if got := resp.execution(t)["route"]; got != tc.route {
				t.Fatalf("route = %v, want %s", got, tc.route)
			}
		})
	}
}

// A mount that supplies no field list is sorted by the field the alias selects as well: the
// document used to be refused for an unqualified field that no list could place.
func TestOrderByTheAliasOfAFieldOfAMountWithNoFieldListIsSortedByThatField(t *testing.T) {
	base := relIntScopeServer(t)
	const join = "from: {database: shop, name: Orders, alias: o, joins: [{type: inner, from: {database: notes, name: Note, alias: n}, " +
		"on: [{left: {field: person_id, source: o}, op: '==', right: {field: person_id, source: n}}]}]}\n"
	resp := relHTTPPost(t, base, "/v1/dtql", "", join+"where: {exists: {query: {from: {database: shop, name: Orders, alias: x}}}}\n"+
		"orderBy: [{field: person_id}]\ncolumns: [{field: mood, source: n, as: person_id}]\n")
	relIntRowsAre(t, resp, []map[string]any{{"person_id": "busy"}, {"person_id": "calm"}})
}
