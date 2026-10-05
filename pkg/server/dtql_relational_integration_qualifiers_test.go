package server_test

import (
	"net/http"
	"strings"
	"testing"
)

// The rule of the in-memory route: the server sorts a relational document, binds it or
// refuses it, and never ignores part of it. The documents below were answered with a status
// 200 and a wrong order before the refusals of this file, or are the divergences that a
// mount's own executor still has and the documentation states. They run over HTTP on real
// SQLite files and on a local inGitDB directory, on both endpoints. The helpers of this file
// all start with relIntQualifiers so they cannot clash with the others of the package.

// relIntQualifiersGit posts doc to the inGitDB mount on both endpoints: the per-database
// endpoint reads it as it is (DALgo evaluates it), and /v1/dtql gets the database written
// into the one source, so the mount is handed a plain document whole.
func relIntQualifiersGit(t *testing.T, base, doc string, check func(t *testing.T, resp relHTTPResponse)) {
	t.Helper()
	for _, endpoint := range []struct{ name, path, database string }{
		{"per-database endpoint", "/v1/databases/gitdb/dtql", ""},
		{"/v1/dtql, which hands the mount the document whole", "/v1/dtql", "gitdb"},
	} {
		t.Run(endpoint.name, func(t *testing.T) {
			check(t, relHTTPPost(t, base, endpoint.path, "", string(relIntRewrite(t, []byte(doc), endpoint.database))))
		})
	}
}

// A qualifier that names no source of the query is refused on every route and both
// endpoints, in the words of DALgo, before anything is read. A document of one source that
// names its database is handed whole to the mount, whose executor reads the name of a field
// and nothing else: the qualifier of an ORDER BY field was ignored, with a status 200, where
// the per-database endpoint refused the same document.
func TestAQualifierThatNamesNoSourceOfTheQueryIsRefusedOnBothEndpoints(t *testing.T) {
	base := relIntFieldsServer(t)
	const (
		from    = "from: {name: Customer, alias: c}\n"
		columns = "columns: [{field: FirstName, source: c}]\n"
	)
	for name, tc := range map[string]struct{ doc, path, alias string }{
		"ORDER BY a field the source does not have": {from + "orderBy: [{field: nope, source: zzz}]\n" + columns, "orderBy[0].source", "zzz"},
		"ORDER BY a field the source has":           {from + "orderBy: [{field: FirstName, source: zzz, desc: true}]\n" + columns, "orderBy[0].source", "zzz"},
		"ORDER BY the collection of a source that has an alias": {from + "orderBy: [{field: FirstName, source: Customer, desc: true}]\n" + columns,
			"orderBy[0].source", "Customer"},
		"the second ordering": {from + "orderBy: [{field: CustomerId, source: c}, {field: nope, source: zzz}]\n" + columns, "orderBy[1].source", "zzz"},
		"an operand of arithmetic": {from + "orderBy: [{binary: {op: '*', left: {field: CustomerId, source: zzz}, right: {value: -1}}}]\n" + columns,
			"orderBy[0].left.source", "zzz"},
		// The same hole in the clauses that a mount reads by the name of the field too.
		"WHERE":    {from + "where: {op: '==', left: {field: FirstName, source: zzz}, right: {value: Ada}}\n" + columns, "where.left.source", "zzz"},
		"a column": {from + "columns: [{field: FirstName, source: zzz}]\n", "columns[0].source", "zzz"},
	} {
		t.Run(name, func(t *testing.T) {
			var answers []string
			relIntQualifiersGit(t, base, tc.doc, func(t *testing.T, resp relHTTPResponse) {
				relIntRefusalsRefused(t, resp, "query_scope at "+tc.path+": ", `unknown alias "`+tc.alias+`"`)
				answers = append(answers, resp.raw)
			})
			if len(answers) == 2 && answers[0] != answers[1] {
				t.Fatalf("the two endpoints answer differently:\n%s\n%s", answers[0], answers[1])
			}
		})
	}

	t.Run("in memory on both engines and both endpoints", func(t *testing.T) {
		doc := from + relIntFieldsAnyCustomerOf("x") + "orderBy: [{field: FirstName, source: zzz}]\n" + columns
		relIntFieldsEachRoute(t, base, doc, func(t *testing.T, resp relHTTPResponse) {
			relIntRefusalsRefused(t, resp, "", `unknown alias "zzz"`)
		})
	})

	t.Run("a qualifier that names the source is answered", func(t *testing.T) {
		doc := from + "orderBy: [{field: FirstName, source: c, desc: true}]\n" + columns
		relIntQualifiersGit(t, base, doc, func(t *testing.T, resp relHTTPResponse) {
			relIntRowsAre(t, resp, []map[string]any{{"FirstName": "Cy"}, {"FirstName": "Bea"}, {"FirstName": "Ada"}})
		})
	})

	t.Run("a long qualifier is repeated clipped, as every refusal of DALgo is", func(t *testing.T) {
		// The classifier refuses a qualifier of more than 256 bytes; this is the longest it lets by.
		long := strings.Repeat("q", 256)
		resp := relHTTPPost(t, base, "/v1/dtql", "", "from: {database: gitdb, name: Customer, alias: c}\norderBy: [{field: FirstName, source: "+long+"}]\n"+columns)
		relIntRefusalsRefused(t, resp, "query_scope at orderBy[0].source: ", "unknown alias")
		if strings.Contains(resp.raw, long) || len(resp.errorField("message")) > 300 {
			t.Fatalf("the refusal repeats the qualifier: %s", resp.raw)
		}
	})
}

// A numeric key with a missing value, in a document that an inGitDB mount is handed whole, is
// placed by the mount's own comparator: a record without the field comes after every number
// ascending, and first descending. SQLite and DALgo, which sort every other document, put it
// first ascending, so the two endpoints answer the same document in a different order and, with
// a limit, with different rows. The comparator is in dalgo2ingitdb, not in this repository
// (docs/api.md says so, "What the mount is handed whole"); this test pins the answer until the
// library orders a missing value before every value, and then it is to be changed with the
// sentence of the documentation.
func TestAMissingNumberIsPlacedByTheMountsOwnComparatorWhenTheDocumentIsHandedWholeToIt(t *testing.T) {
	base := relIntFieldsServer(t)
	byTotal := func(desc string) string {
		return "from: {name: Invoice, alias: i}\norderBy: [{field: t" + desc + "}]\n" +
			"columns: [{field: InvoiceId, source: i}, {field: Total, source: i, as: t}]\n"
	}
	row := func(id float64, total any) map[string]any { return map[string]any{"InvoiceId": id, "t": total} }
	for name, tc := range map[string]struct {
		doc              string
		database, handed []map[string]any
	}{
		"ascending": {byTotal(""),
			[]map[string]any{row(11, nil), row(13, float64(50)), row(10, float64(100)), row(12, float64(200))},
			[]map[string]any{row(13, float64(50)), row(10, float64(100)), row(12, float64(200)), row(11, nil)}},
		"descending": {byTotal(", desc: true"),
			[]map[string]any{row(12, float64(200)), row(10, float64(100)), row(13, float64(50)), row(11, nil)},
			[]map[string]any{row(11, nil), row(12, float64(200)), row(10, float64(100)), row(13, float64(50))}},
	} {
		t.Run(name, func(t *testing.T) {
			relIntRowsAre(t, relHTTPPost(t, base, "/v1/databases/gitdb/dtql", "", tc.doc), tc.database)
			relIntRowsAre(t, relHTTPPost(t, base, "/v1/dtql", "", string(relIntRewrite(t, []byte(tc.doc), "gitdb"))), tc.handed)
		})
	}
}

// A name inside the arithmetic of an ORDER BY that a column also carries as its alias is, in
// SQLite, the alias when the table has no column of that name. Where the one source supplies no
// field list (the partial database of the scope tests) the mount would read it as a field, and
// a record that has none sorts nothing: the document was answered in the order the records were
// read. It is refused, with the way out; the qualified field is answered.
func TestAnAliasInsideOrderByArithmeticOverASourceWithNoFieldListIsRefusedOverHTTP(t *testing.T) {
	base := relIntScopeServer(t)
	const from = "from: {database: notes, name: Note, alias: n}\n"
	resp := relHTTPPost(t, base, "/v1/dtql", "", from+"orderBy: [{binary: {op: '+', left: {field: T}, right: {value: 0}}}]\n"+
		"columns: [{field: mood, source: n, as: T}]\n")
	relIntRefusalsRefused(t, resp, "scope at orderBy[0].left: ", "T", "inside arithmetic", "no field list", "qualify the field with its source")

	t.Run("a name no column carries is left to the mount, as the documentation says", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/dtql", "", from+"orderBy: [{binary: {op: '+', left: {field: mood}, right: {value: 0}}}]\n"+
			"columns: [{field: mood, source: n, as: T}]\n")
		if resp.status != http.StatusOK {
			t.Fatalf("status %d, want 200: %s", resp.status, resp.raw)
		}
	})
	t.Run("the qualified field is answered", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/dtql", "", from+"orderBy: [{binary: {op: '+', left: {field: mood, source: n}, right: {value: 0}}}]\n"+
			"columns: [{field: mood, source: n, as: T}]\n")
		if resp.status != http.StatusOK {
			t.Fatalf("status %d, want 200: %s", resp.status, resp.raw)
		}
	})
}

// DALgo does not know the key pseudo-field `$id` of the document engines. Where it evaluates
// the document it refused `$id` over a source with a field list (as unavailable) and read it
// as a null over one with none, so the ordering by it did nothing, with a status 200. Only a
// mount that is handed the document whole sorts by it: the answer there is unchanged, and an
// ORDER BY of it in a document DALgo evaluates is refused with the way out. Before this change
// a document that orders by the key and by arithmetic was handed whole to the mount, which
// sorted by the key and skipped the arithmetic (a status 200); the arithmetic now sorts, so the
// document must be answered by DALgo, which cannot sort by the key.
func TestTheKeyIsOrderedByOnlyWhereAMountIsHandedTheDocumentWholeAndRefusedElsewhere(t *testing.T) {
	const (
		says      = "order by a field"
		byKeyThen = "orderBy: [{field: $id, desc: true}, {binary: {op: '*', left: {field: InvoiceId, source: i}, right: {value: -1}}}]\n"
		invoices  = "from: {name: Invoice, alias: i}\n"
		invoiceId = "columns: [{field: InvoiceId, source: i}]\n"
	)
	base := relIntFieldsServer(t)
	t.Run("a strict mount that is handed a plain document whole sorts by the key", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/dtql", "", "from: {database: gitdb, name: Customer, alias: c}\norderBy: [{field: $id, desc: true}]\ncolumns: [{field: FirstName, source: c}]\n")
		relIntRowsAre(t, resp, []map[string]any{{"FirstName": "Cy"}, {"FirstName": "Bea"}, {"FirstName": "Ada"}})
	})
	t.Run("the key and arithmetic, over a source with a field list, on both endpoints", func(t *testing.T) {
		relIntQualifiersGit(t, base, invoices+byKeyThen+invoiceId, func(t *testing.T, resp relHTTPResponse) {
			relIntRefusalsRefused(t, resp, "scope at orderBy[0]: ", "$id", says)
		})
	})
	t.Run("the key on the per-database endpoint, which DALgo evaluates", func(t *testing.T) {
		resp := relHTTPPost(t, base, "/v1/databases/gitdb/dtql", "", invoices+"orderBy: [{field: $id}]\n"+invoiceId)
		relIntRefusalsRefused(t, resp, "scope at orderBy[0]: ", "$id", says)
	})
	t.Run("in memory, on both engines and both endpoints", func(t *testing.T) {
		relIntFieldsEachRoute(t, base, invoices+relIntFieldsAnyCustomer+"orderBy: [{field: $id, source: i}]\n"+invoiceId, func(t *testing.T, resp relHTTPResponse) {
			relIntRefusalsRefused(t, resp, "scope at orderBy[0]: ", "$id", says)
		})
	})

	t.Run("a source with no field list", func(t *testing.T) {
		notes := relIntScopeServer(t)
		const from = "from: {database: notes, name: Note, alias: n}\n"
		// Handed whole, the mount sorts by the key, as it did.
		relIntRowsAre(t, relHTTPPost(t, notes, "/v1/dtql", "", from+"orderBy: [{field: $id, desc: true}]\ncolumns: [{field: mood, source: n}]\n"),
			[]map[string]any{{"mood": "busy"}, {"mood": "calm"}})
		// Beside arithmetic it is evaluated by DALgo, which would read the key as a null and answer
		// in the order the records were read: it is refused instead.
		resp := relHTTPPost(t, notes, "/v1/dtql", "", from+"orderBy: [{field: $id, desc: true}, {binary: {op: '+', left: {field: mood, source: n}, right: {value: 0}}}]\n"+
			"columns: [{field: mood, source: n}]\n")
		relIntRefusalsRefused(t, resp, "scope at orderBy[0]: ", "$id", says)
		// So it is on the per-database endpoint, where DALgo evaluates the plain document too.
		resp = relHTTPPost(t, notes, "/v1/databases/notes/dtql", "", "from: {name: Note, alias: n}\norderBy: [{field: $id}]\ncolumns: [{field: mood, source: n}]\n")
		relIntRefusalsRefused(t, resp, "scope at orderBy[0]: ", "$id", says)
	})
}

// A relational document is not run on a database with access policies, so the document that
// Execute reads as a plain scan above the policy-checked executor (an ordering expression over
// one source) is never reached over HTTP: the endpoints refuse it first, for both mounts of
// examples/layered-acl, whatever the document orders by. (The row of joinexec.Execute is in
// pkg/joinexec.)
func TestAnOrderingExpressionOverADatabaseWithAccessPoliciesIsRefusedBeforeItIsRead(t *testing.T) {
	base := relIntLayeredServer(t)
	const doc = "from: {name: customers, alias: c}\n" +
		"orderBy: [{binary: {op: '*', left: {field: id, source: c}, right: {value: -1}}}]\ncolumns: [{field: name, source: c}]\n"
	for _, protected := range []string{"sqlite", "ingitdb"} {
		for _, endpoint := range []struct{ name, path, database string }{
			{"per-database endpoint", "/v1/databases/" + protected + "/dtql", ""},
			{"/v1/dtql", "/v1/dtql", protected},
		} {
			for _, caller := range relIntCallers {
				t.Run(protected+", "+endpoint.name+", "+caller.name, func(t *testing.T) {
					resp := relHTTPDo(t, base, http.MethodPost, endpoint.path, caller.token, string(relIntRewrite(t, []byte(doc), endpoint.database)), nil)
					if resp.status != http.StatusUnprocessableEntity || resp.errorField("code") != "authorization_unsupported" || resp.body["records"] != nil {
						t.Fatalf("status %d, want a 422 authorization_unsupported with no rows: %s", resp.status, resp.raw)
					}
				})
			}
		}
	}
}
