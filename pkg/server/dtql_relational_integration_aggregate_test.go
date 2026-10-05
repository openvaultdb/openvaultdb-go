package server_test

import (
	"strings"
	"testing"
)

// An unqualified field of an aggregating query of several sources, over HTTP, on real
// mounts. DALgo's aggregation evaluates a GROUP BY expression and the argument of an
// aggregate from the first source of the query and from no other, whichever source
// carries the name, so a name that another source carries must not be answered from the
// first (OJ-14). The documents carry an EXISTS test, which puts them on the in-memory
// evaluation that binds unqualified fields (a join document without a subquery is
// refused by the DTQL parser before it reaches the engine). The helpers of this file all
// start with relIntAggregate so they cannot clash with the others of the package.

const (
	// relIntAggregateInvoiceFirst reads the invoices first and joins the customers to them.
	relIntAggregateInvoiceFirst = "from: {name: Invoice, alias: i, joins: [{type: inner, from: {name: Customer, alias: c}, " +
		"on: [{left: {field: CustomerId, source: i}, op: '==', right: {field: CustomerId, source: c}}]}]}\n" +
		"where: {exists: {query: {from: {name: Customer, alias: x}}}}\n"
	// relIntAggregateCustomerFirst reads the customers first and joins the invoices to them.
	relIntAggregateCustomerFirst = "from: {name: Customer, alias: c, joins: [{type: inner, from: {name: Invoice, alias: i}, " +
		"on: [{left: {field: CustomerId, source: c}, op: '==', right: {field: CustomerId, source: i}}]}]}\n" +
		"where: {exists: {query: {from: {name: Customer, alias: x}}}}\n"
	relIntAggregateRevenue = "{aggregate: {function: sum, args: [{field: Total, source: i}]}, as: Revenue}"
)

// relIntAggregateRefused requires the 400 invalid_dtql of a scope refusal at path that
// names the field and says what the caller can do about it.
func relIntAggregateRefused(t *testing.T, resp relHTTPResponse, path, field string) {
	t.Helper()
	relIntScopeRefused(t, resp, field, "scope at "+path+":", "qualify")
}

// The defect: the first source of the query was read for a name that only another source
// carries, and the answer was a 200 with a null group and the sum of every invoice.
func TestAnUnqualifiedFieldOnlyAnotherSourceCarriesIsNotReadFromTheFirstInAnAggregate(t *testing.T) {
	base := relIntFieldsServer(t)
	for name, tc := range map[string]struct {
		doc   string
		path  string
		field string
	}{
		"GROUP BY": {
			relIntAggregateInvoiceFirst + "groupBy: [{field: Country}]\n" +
				"columns: [{field: Country}, " + relIntAggregateRevenue + "]\n",
			"groupBy[0]", "Country",
		},
		"the argument of an aggregate in a column": {
			relIntAggregateCustomerFirst + "groupBy: [{field: Country, source: c}]\n" +
				"columns: [{field: Country, source: c}, {aggregate: {function: sum, args: [{field: Total}]}, as: Revenue}]\n",
			"columns[1].args[0]", "Total",
		},
		"the argument of an aggregate in HAVING": {
			relIntAggregateCustomerFirst + "groupBy: [{field: Country, source: c}]\n" +
				"having: {op: '>', left: {aggregate: {function: sum, args: [{field: Total}]}}, right: {value: 10}}\n" +
				"columns: [{field: Country, source: c}, {aggregate: {function: count, args: [{star: true}]}, as: Invoices}]\n",
			"having.left.args[0]", "Total",
		},
		"the argument of an aggregate in ORDER BY": {
			relIntAggregateCustomerFirst + "groupBy: [{field: Country, source: c}]\n" +
				"orderBy: [{aggregate: {function: sum, args: [{field: Total}]}}]\n" +
				"columns: [{field: Country, source: c}, {aggregate: {function: count, args: [{star: true}]}, as: Invoices}]\n",
			"orderBy[0].args[0]", "Total",
		},
	} {
		t.Run(name, func(t *testing.T) {
			relIntFieldsEachRoute(t, base, tc.doc, func(t *testing.T, resp relHTTPResponse) {
				relIntAggregateRefused(t, resp, tc.path, tc.field)
			})
		})
	}
}

// The way out the refusal gives, and the names that are read from the right source
// anyway, are answered as SQL answers them.
func TestAnAggregateOfSeveralSourcesIsAnsweredWhenEveryFieldIsQualifiedOrOnlyTheFirstSourceCarriesIt(t *testing.T) {
	base := relIntFieldsServer(t)
	byCountry := []map[string]any{{"Country": "IE", "Revenue": float64(150)}, {"Country": "US", "Revenue": float64(200)}}
	for name, doc := range map[string]string{
		"every field qualified": relIntAggregateInvoiceFirst + "groupBy: [{field: Country, source: c}]\n" +
			"orderBy: [{field: Country, source: c}]\n" +
			"columns: [{field: Country, source: c}, " + relIntAggregateRevenue + "]\n",
		"the argument unqualified, and only the first source carries it": relIntAggregateInvoiceFirst +
			"groupBy: [{field: Country, source: c}]\norderBy: [{field: Country, source: c}]\n" +
			"columns: [{field: Country, source: c}, {aggregate: {function: sum, args: [{field: Total}]}, as: Revenue}]\n",
		"a name only the other source carries, unqualified in WHERE, which DALgo binds to its carrier": strings.Replace(relIntAggregateInvoiceFirst,
			"where: {exists: {query: {from: {name: Customer, alias: x}}}}\n",
			"where:\n  and:\n    - {op: '>=', left: {field: Country}, right: {value: A}}\n    - exists: {query: {from: {name: Customer, alias: x}}}\n", 1) +
			"groupBy: [{field: Country, source: c}]\norderBy: [{field: Country, source: c}]\n" +
			"columns: [{field: Country, source: c}, " + relIntAggregateRevenue + "]\n",
	} {
		t.Run(name, func(t *testing.T) {
			relIntFieldsEachRoute(t, base, doc, func(t *testing.T, resp relHTTPResponse) {
				relIntRowsAre(t, resp, byCountry)
			})
		})
	}
}
