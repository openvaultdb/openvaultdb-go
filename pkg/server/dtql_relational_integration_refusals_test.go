package server_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The rule of the in-memory route: the server sorts a relational document, binds it or
// refuses it, and never ignores part of it. The documents below were answered with a
// status 200 and a wrong value or order, or with a 500 that blamed the server for the
// document, before the refusals of this file. They run over HTTP on real SQLite files and
// on a local inGitDB directory, on both endpoints, on the dataset of the field tests.
// The helpers of this file all start with relIntRefusals so they cannot clash with the
// others of the package.

// relIntRefusalsRefused requires the 400 invalid_dtql of a refusal of the document that
// starts with the category and path given, names the name and says what to do about it.
func relIntRefusalsRefused(t *testing.T, resp relHTTPResponse, prefix, name string, says ...string) {
	t.Helper()
	message := resp.errorField("message")
	if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || resp.body["records"] != nil || resp.body["execution"] != nil {
		t.Fatalf("status %d, want a 400 invalid_dtql with no rows: %s", resp.status, resp.raw)
	}
	if !strings.HasPrefix(message, prefix) || !strings.Contains(message, name) {
		t.Fatalf("the message does not start with %q and name %q: %s", prefix, name, resp.raw)
	}
	for _, want := range says {
		if !strings.Contains(message, want) {
			t.Fatalf("the message does not say %q: %s", want, resp.raw)
		}
	}
}

// A column field that an earlier column carries as its alias is read by DALgo's
// aggregation as that column: the count came back as the grouped field, with a status 200.
// One source or two, the document is refused with the way out; renamed, or qualified, the
// same document is answered with the field.
func TestAColumnFieldThatAnEarlierColumnCarriesAsItsAliasIsRefusedOverHTTP(t *testing.T) {
	base := relIntFieldsServer(t)
	const (
		oneSource = "from: {name: Invoice, alias: i}\n"
		twoSource = "from: {name: Invoice, alias: i, joins: [{type: inner, from: {name: Customer, alias: c}, " +
			"on: [{left: {field: CustomerId, source: i}, op: '==', right: {field: CustomerId, source: c}}]}]}\n"
	)
	for name, doc := range map[string]string{
		"one source": oneSource + relIntFieldsAnyCustomer + "groupBy: [{field: CustomerId}]\n" +
			"columns:\n  - {aggregate: {function: count, args: [{star: true}]}, as: CustomerId}\n  - {field: CustomerId, as: Customer}\n",
		// Only the invoices carry Total, and they are the first source.
		"two sources": twoSource + relIntFieldsAnyCustomer + "groupBy: [{field: Total}]\n" +
			"columns:\n  - {aggregate: {function: count, args: [{star: true}]}, as: Total}\n  - {field: Total, as: Spent}\n",
	} {
		t.Run(name, func(t *testing.T) {
			field := "CustomerId"
			if strings.Contains(doc, "Total") {
				field = "Total"
			}
			relIntFieldsEachRoute(t, base, doc, func(t *testing.T, resp relHTTPResponse) {
				relIntRefusalsRefused(t, resp, "scope at columns[1]: ", "unqualified field "+field, "rename the alias", "qualify the field")
			})
		})
	}

	t.Run("an earlier column that is the same field, qualified by the only source, is not a refusal", func(t *testing.T) {
		// What the later column reads as the earlier one is the field it says.
		doc := oneSource + relIntFieldsAnyCustomer + "groupBy: [{field: CustomerId, source: i}, {field: CustomerId}]\norderBy: [{field: CustomerId, source: i}]\n" +
			"columns:\n  - {field: CustomerId, source: i}\n  - {field: CustomerId, as: Customer}\n" +
			"  - {aggregate: {function: count, args: [{star: true}]}, as: Invoices}\n"
		relIntFieldsEachRoute(t, base, doc, func(t *testing.T, resp relHTTPResponse) {
			relIntRowsAre(t, resp, []map[string]any{
				{"CustomerId": float64(1), "Customer": float64(1), "Invoices": float64(2)},
				{"CustomerId": float64(2), "Customer": float64(2), "Invoices": float64(1)},
				{"CustomerId": float64(3), "Customer": float64(3), "Invoices": float64(1)},
			})
		})
	})

	t.Run("renamed, the same document is answered with the field", func(t *testing.T) {
		doc := oneSource + relIntFieldsAnyCustomer + "groupBy: [{field: CustomerId}]\norderBy: [{field: Customer}]\n" +
			"columns:\n  - {aggregate: {function: count, args: [{star: true}]}, as: Invoices}\n  - {field: CustomerId, as: Customer}\n"
		relIntFieldsEachRoute(t, base, doc, func(t *testing.T, resp relHTTPResponse) {
			relIntRowsAre(t, resp, []map[string]any{
				{"Invoices": float64(2), "Customer": float64(1)},
				{"Invoices": float64(1), "Customer": float64(2)},
				{"Invoices": float64(1), "Customer": float64(3)},
			})
		})
	})
}

// relIntRefusalsByName orders the customers by the alias of the column that selects their
// first name.
const relIntRefusalsByName = "from: {name: Customer, alias: c}\norderBy: [{field: name, desc: true}]\n" +
	"columns: [{field: FirstName, source: c, as: name}]\n"

// ORDER BY the alias of a column that is a field of a source, in a query that does not
// aggregate: the answer is sorted by that field, as the database route of SQLite sorts
// it, on every route and both endpoints. It was a 400 on the per-database endpoint of an
// inGitDB mount, and an answer in the order the records were read on /v1/dtql.
func TestOrderByTheAliasOfAFieldSortsTheAnswerOnEveryRouteAndEndpoint(t *testing.T) {
	base := relIntFieldsServer(t)
	byName := []map[string]any{{"name": "Cy"}, {"name": "Bea"}, {"name": "Ada"}}
	for name, tc := range map[string]struct {
		doc  string
		want []map[string]any
	}{
		"the alias of a qualified field": {relIntRefusalsByName, byName},
		"the alias of an unqualified field": {"from: {name: Customer}\norderBy: [{field: name, desc: true}]\n" +
			"columns: [{field: FirstName, as: name}]\n", byName},
		"with a test that puts the document in memory on SQLite": {"from: {name: Customer, alias: c}\n" + relIntFieldsAnyCustomerOf("x") +
			"orderBy: [{field: name, desc: true}]\ncolumns: [{field: FirstName, source: c, as: name}]\n", byName},
		"two sources": {"from: {name: Invoice, alias: i, joins: [{type: inner, from: {name: Customer, alias: c}, " +
			"on: [{left: {field: CustomerId, source: i}, op: '==', right: {field: CustomerId, source: c}}]}]}\n" +
			"where: {exists: {query: {from: {name: Customer, alias: x}}}}\n" +
			"orderBy: [{field: who}, {field: InvoiceId, source: i}]\n" +
			"columns: [{field: FirstName, source: c, as: who}, {field: InvoiceId, source: i}]\n",
			[]map[string]any{
				{"who": "Ada", "InvoiceId": float64(10)}, {"who": "Ada", "InvoiceId": float64(11)},
				{"who": "Bea", "InvoiceId": float64(12)}, {"who": "Cy", "InvoiceId": float64(13)},
			}},
		"the alias of a key column": {"from: {name: Customer, alias: c}\norderBy: [{field: first, desc: true}]\n" +
			"columns: [{field: CustomerId, source: c, as: first}]\n",
			[]map[string]any{{"first": float64(3)}, {"first": float64(2)}, {"first": float64(1)}}},
		"in a derived source, where a limit shows the order": {"from:\n  query:\n    as: top\n    from: {name: Customer, alias: c}\n" +
			"    orderBy: [{field: name, desc: true}]\n    limit: 2\n    columns: [{field: FirstName, source: c, as: name}]\n" +
			"columns: [{field: name, source: top}]\n", byName[:2]},
	} {
		t.Run(name, func(t *testing.T) {
			relIntFieldsEachRoute(t, base, tc.doc, func(t *testing.T, resp relHTTPResponse) {
				relIntRowsAre(t, resp, tc.want)
			})
		})
	}
}

// relIntFieldsAnyCustomerOf is relIntFieldsAnyCustomer over a source of the given alias,
// which a document that already uses c as an alias needs.
func relIntFieldsAnyCustomerOf(alias string) string {
	return "where: {exists: {query: {from: {name: Customer, alias: " + alias + "}}}}\n"
}

// ORDER BY the alias of a column that is not a field (arithmetic, a function) is read by
// a SQL database as the column, and by DALgo and a mount as a field of a source. A
// document that one SQL database runs whole is sorted by the expression by the database;
// every other document is refused with the way out, and no mount is handed a name it
// would ignore. (The SQLite adapter compiles no computed column of a document of one
// source, so the first kind is not shown here.)
func TestOrderByTheAliasOfAnExpressionIsRefusedWhenTheDocumentIsNotRunWholeByASQLDatabase(t *testing.T) {
	base := relIntFieldsServer(t)
	const (
		order   = "orderBy: [{field: neg}]\n"
		columns = "columns:\n  - {binary: {op: '*', left: {field: InvoiceId, source: i}, right: {value: -1}}, as: neg}\n  - {field: InvoiceId, source: i}\n"
	)
	t.Run("in memory, on both engines and both endpoints", func(t *testing.T) {
		relIntFieldsEachRoute(t, base, "from: {name: Invoice, alias: i}\n"+relIntFieldsAnyCustomer+order+columns, func(t *testing.T, resp relHTTPResponse) {
			relIntRefusalsRefused(t, resp, "scope at orderBy[0]: ", "neg", "order by the fields of")
		})
	})
	t.Run("handed whole to an inGitDB mount it is refused, as the mount cannot sort by it", func(t *testing.T) {
		for _, tc := range []struct{ path, doc string }{
			{"/v1/databases/gitdb/dtql", "from: {name: Invoice, alias: i}\n" + order + columns},
			{"/v1/dtql", "from: {database: gitdb, name: Invoice, alias: i}\n" + order + columns},
		} {
			relIntRefusalsRefused(t, relHTTPPost(t, base, tc.path, "", tc.doc), "scope at orderBy[0]: ", "neg", "order by the fields of")
		}
	})
}

// An ORDER BY name that is neither the alias of a column nor a field of a source that
// supplies its fields is refused before anything is read. A document of one source that
// names its database is handed whole to the mount's executor, which ignores a field it
// does not know: the answer was a 200 in the order the records were read.
func TestOrderByANameNoSourceCarriesIsRefusedBeforeAnythingIsRead(t *testing.T) {
	base := relIntFieldsServer(t)
	const (
		plain   = "from: {name: Customer, alias: c}\norderBy: [{field: nope}]\ncolumns: [{field: FirstName, source: c}]\n"
		inMemry = "from: {name: Customer, alias: c}\n" + "where: {exists: {query: {from: {name: Invoice, alias: x}}}}\n" +
			"orderBy: [{field: nope}]\ncolumns: [{field: FirstName, source: c}]\n"
	)
	t.Run("a document of one source: the database route of SQLite says it has no such column", func(t *testing.T) {
		relIntFieldsEachRoute(t, base, plain, func(t *testing.T, resp relHTTPResponse) {
			if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || resp.body["records"] != nil {
				t.Fatalf("status %d, want a 400 invalid_dtql: %s", resp.status, resp.raw)
			}
		})
	})
	t.Run("handed whole to the inGitDB mount, on both endpoints", func(t *testing.T) {
		for _, tc := range []struct{ path, doc string }{
			{"/v1/databases/gitdb/dtql", plain},
			{"/v1/dtql", strings.Replace(plain, "from: {name: Customer", "from: {database: gitdb, name: Customer", 1)},
		} {
			relIntRefusalsRefused(t, relHTTPPost(t, base, tc.path, "", tc.doc), "shape at orderBy[0]: ", `unknown field "nope"`)
		}
	})
	t.Run("a name the source of its qualifier does not carry is refused the same way", func(t *testing.T) {
		// The mount's executor reads the name of the field and finds nothing, so a name that
		// was qualified was ignored too: the answer was a 200 in the order the records were read.
		const qualified = "from: {name: Customer, alias: c}\norderBy: [{field: nope, source: c}]\ncolumns: [{field: FirstName, source: c}]\n"
		relIntFieldsEachRoute(t, base, qualified, func(t *testing.T, resp relHTTPResponse) {
			if resp.status != http.StatusBadRequest || resp.errorField("code") != "invalid_dtql" || resp.body["records"] != nil {
				t.Fatalf("status %d, want a 400 invalid_dtql: %s", resp.status, resp.raw)
			}
		})
		for _, tc := range []struct{ path, doc string }{
			{"/v1/databases/gitdb/dtql", qualified},
			{"/v1/dtql", strings.Replace(qualified, "from: {name: Customer", "from: {database: gitdb, name: Customer", 1)},
		} {
			relIntRefusalsRefused(t, relHTTPPost(t, base, tc.path, "", tc.doc), "shape at orderBy[0]: ", `unknown field "nope"`)
		}
		t.Run("the key, which no list carries, is not refused", func(t *testing.T) {
			resp := relHTTPPost(t, base, "/v1/dtql", "", "from: {database: gitdb, name: Customer, alias: c}\norderBy: [{field: $id, source: c, desc: true}]\ncolumns: [{field: FirstName, source: c}]\n")
			relIntRowsAre(t, resp, []map[string]any{{"FirstName": "Cy"}, {"FirstName": "Bea"}, {"FirstName": "Ada"}})
		})
	})
	t.Run("the key of a strict inGitDB mount is not a field its manifest declares", func(t *testing.T) {
		// The mount's executor reads no key from the data of a record, so ordering by id
		// left the answer in the order the records were read.
		doc := strings.Replace(strings.Replace(plain, "field: nope", "field: id", 1), "from: {name: Customer", "from: {database: gitdb, name: Customer", 1)
		relIntRefusalsRefused(t, relHTTPPost(t, base, "/v1/dtql", "", doc), "shape at orderBy[0]: ", `unknown field "id"`)
	})
	t.Run("in memory, on both engines and both endpoints", func(t *testing.T) {
		relIntFieldsEachRoute(t, base, inMemry, func(t *testing.T, resp relHTTPResponse) {
			relIntRefusalsRefused(t, resp, "shape at orderBy[0]: ", `unknown field "nope"`)
		})
	})
}

// A name inside the arithmetic of an ORDER BY is a field of a source, as a SQL database
// reads it: only an ORDER BY expression that is the bare name reads a column's alias (SQLite
// sorts `select a as b ... order by b` by the alias and `order by b*1` by the table's own b).
// Here the column that selects InvoiceId is called Total, which the invoices carry too: the
// answer is sorted by the invoice's Total, with the one that has none last, as it was before
// the alias was replaced inside arithmetic.
func TestAnAliasInsideOrderByArithmeticDoesNotHideTheFieldOfTheSource(t *testing.T) {
	base := relIntFieldsServer(t)
	const doc = "from: {name: Invoice, alias: i}\n" + relIntFieldsAnyCustomer +
		"orderBy: [{binary: {op: '+', left: {field: Total}, right: {value: 0}}, desc: true}]\n" +
		"columns: [{field: InvoiceId, source: i, as: Total}]\n"
	relIntFieldsEachRoute(t, base, doc, func(t *testing.T, resp relHTTPResponse) {
		relIntRowsAre(t, resp, []map[string]any{{"Total": float64(12)}, {"Total": float64(10)}, {"Total": float64(13)}, {"Total": float64(11)}})
	})
}

// An ORDER BY expression that is not a plain field (arithmetic) is sorted on a document of
// one source that names its database too: the executor of an inGitDB mount that is handed
// such a document whole skips an ordering that is not a field, and answered in the order the
// records were read, with a 200. (The SQLite adapter compiles no computed ordering of a
// document of one source, and answers that one with a 422 of its own, which this change does
// not touch; in memory, with a test that puts the document there, it is sorted.)
func TestOrderByArithmeticSortsADocumentOfOneSourceOnEveryRouteAndEndpoint(t *testing.T) {
	base := relIntFieldsServer(t)
	ids := func(ids ...float64) []map[string]any {
		rows := make([]map[string]any, len(ids))
		for i, id := range ids {
			rows[i] = map[string]any{"InvoiceId": id}
		}
		return rows
	}
	for name, tc := range map[string]struct {
		order string
		want  []map[string]any
	}{
		"the source is qualified": {"{binary: {op: '*', left: {field: InvoiceId, source: i}, right: {value: -1}}}", ids(13, 12, 11, 10)},
		"the field is not":        {"{binary: {op: '*', left: {field: InvoiceId}, right: {value: -1}}}", ids(13, 12, 11, 10)},
		"beside a plain field": {"{field: CustomerId, source: i}, {binary: {op: '*', left: {field: InvoiceId, source: i}, right: {value: -1}}}",
			ids(11, 10, 12, 13)},
	} {
		doc := "from: {name: Invoice, alias: i}\n%sorderBy: [" + tc.order + "]\ncolumns: [{field: InvoiceId, source: i}]\n"
		t.Run(name, func(t *testing.T) {
			for _, endpoint := range []struct{ name, path, database string }{
				{"per-database endpoint", "/v1/databases/gitdb/dtql", ""},
				{"/v1/dtql, which hands the mount the document whole", "/v1/dtql", "gitdb"},
			} {
				t.Run("the inGitDB mount, "+endpoint.name, func(t *testing.T) {
					resp := relHTTPPost(t, base, endpoint.path, "", string(relIntRewrite(t, []byte(fmt.Sprintf(doc, "")), endpoint.database)))
					relIntRowsAre(t, resp, tc.want)
				})
			}
			t.Run("in memory, on both engines and both endpoints", func(t *testing.T) {
				relIntFieldsEachRoute(t, base, fmt.Sprintf(doc, relIntFieldsAnyCustomer), func(t *testing.T, resp relHTTPResponse) {
					relIntRowsAre(t, resp, tc.want)
				})
			})
		})
	}
}

// A source that supplies no field list (a partial or schemaless database) cannot say a
// name is unknown, and the mount decides: the document is not refused for it.
func TestOrderByANameOfASourceThatSuppliesNoFieldListIsLeftToTheMount(t *testing.T) {
	base := relIntScopeServer(t)
	resp := relHTTPPost(t, base, "/v1/dtql", "", "from: {database: notes, name: Note}\norderBy: [{field: mood}]\n")
	relIntRowsAre(t, resp, []map[string]any{{"mood": "busy", "person_id": "p2"}, {"mood": "calm", "person_id": "p1"}})
}

// A refusal of the document inside a derived source in the base position of a query is
// the refusal the same document gets anywhere else, and not a fault of the server: DALgo
// reports it as the text of a failed scan, which the server read as an error of its own.
// The wildcard of a source that has no field list is one; a derived source inside
// another is the same, and each answer names the path under the derived sources.
func TestAJoinPlanRefusalInsideADerivedSourceInTheBasePositionIsAClientError(t *testing.T) {
	base := relIntScopeServer(t)
	const (
		inner       = "from: {database: notes, name: Note, alias: x}\n    columns: [{wildcard: {source: x, exclude: [none]}}]\n"
		wildcardMsg = "wildcard expansion requires ordered schema metadata"
	)
	for name, tc := range map[string]struct{ doc, path string }{
		"a wildcard over a source with no field list": {
			"from:\n  query:\n    as: n\n    " + inner + "columns: [{field: mood, source: n}]\n",
			"join_plan at from.query.columns[0]: "},
		"in a derived source inside a derived source": {
			"from:\n  query:\n    as: m\n    from:\n      query:\n        as: n\n" +
				"        from: {database: notes, name: Note, alias: x}\n        columns: [{wildcard: {source: x, exclude: [none]}}]\n" +
				"    columns: [{field: mood, source: n}]\ncolumns: [{field: mood, source: m}]\n",
			"join_plan at from.query.from.query.columns[0]: "},
		"over a join inside the derived source": {
			"from:\n  query:\n    as: n\n    from: {database: notes, name: Note, alias: x, joins: [{type: inner, from: {database: crm, name: Person, alias: p}, " +
				"on: [{left: {field: person_id, source: x}, op: '==', right: {field: id, source: p}}]}]}\n" +
				"    columns: [{wildcard: {source: x, exclude: [none]}}]\ncolumns: [{field: mood, source: n}]\n",
			"join_plan at from.query.columns[0]: "},
	} {
		t.Run(name, func(t *testing.T) {
			resp := relHTTPPost(t, base, "/v1/dtql", "", tc.doc)
			relIntRefusalsRefused(t, resp, tc.path, wildcardMsg)
		})
	}

	t.Run("the same derived source on a join is answered the same way", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/dtql", "", "from: {database: crm, name: Person, alias: p, joins: [{type: inner, from: {query: {as: n, "+
			"from: {database: notes, name: Note, alias: x}, columns: [{wildcard: {source: x, exclude: [none]}}]}}, "+
			"on: [{left: {field: id, source: p}, op: '==', right: {field: person_id, source: n}}]}]}\ncolumns: [{field: mood, source: n}]\n")
		relIntRefusalsRefused(t, resp, "join_plan at columns[0]: ", wildcardMsg)
	})
}
