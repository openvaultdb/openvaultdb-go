package joinexec

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// The helpers of this file start with "sa" so that they cannot clash with the
// helpers of the tests around them.

// DALgo's aggregation evaluates a GROUP BY expression and the argument of an aggregate
// from the first source of the query and from no other, whichever source carries the
// name: a field that only another source carries is read as null there, or, when a row of
// the first source holds a key of that name that its list does not, as that key. So in a
// query of several sources that aggregates, an unqualified field outside WHERE and ON
// is a field of the first source or is refused. (An ON condition is held to the same rule
// since OJ-16, and is tested with the refusals of scope_refusals_test.go.)

var (
	saID  = dal.NewFieldRef("a", "id")
	saSum = dal.NewAggregate("SUM", false, scX)
	saOne = dal.NewConstant(1)
)

// saOnlyB is the lists in which only B carries x, and saOnlyA the lists in which only A
// does. Both sources carry the key.
var (
	saOnlyB = map[string][]string{"A": {"id"}, "B": {"id", "x"}}
	saOnlyA = map[string][]string{"A": {"id", "x"}, "B": {"id"}}
	saNone  = map[string][]string{"A": {"id"}, "B": {"id"}}
)

// saCount is a column that makes a query aggregate without naming a field.
func saCount() dal.Column { return dal.CountAs(dal.Star(), "n") }

// saAggregating builds a query of A and B that aggregates by the key of A, with the clause
// the test puts on it.
func saAggregating(build func(b dal.IQueryBuilder) dal.StructuredQuery) dal.StructuredQuery {
	return scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery { return build(b.GroupBy(saID)) })
}

func TestAnUnqualifiedFieldOnlyAnotherSourceCarriesIsRefusedWhereAnAggregationReadsItFromTheFirst(t *testing.T) {
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
	}{
		"GROUP BY": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(saCount())
		}), "groupBy[0]"},
		"an operand of arithmetic in GROUP BY": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(dal.Binary(saID, dal.Add, scX)).SelectColumns(saCount())
		}), "groupBy[0].right"},
		"the argument of an aggregate in a column": {saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: saSum, Alias: "s"})
		}), "columns[0].args[0]"},
		"the argument of an aggregate in HAVING": {saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Having(dal.NewComparison(saSum, dal.GreaterThen, saOne)).SelectColumns(saCount())
		}), "having.left.args[0]"},
		"the argument of an aggregate in ORDER BY": {saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(saSum)).SelectColumns(saCount())
		}), "orderBy[0].args[0]"},
		"a group key in a column": {saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: scX}, saCount())
		}), "columns[0]"},
		"an operand of arithmetic over a group key in a column": {saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: dal.Binary(scX, dal.Add, saOne), Alias: "next"}, saCount())
		}), "columns[0].left"},
		"a name in HAVING that no column has as its alias": {saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Having(dal.NewComparison(scX, dal.GreaterThen, saOne)).SelectColumns(saCount())
		}), "having.left"},
		"a name in ORDER BY that no column has as its alias": {saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(scX)).SelectColumns(saCount())
		}), "orderBy[0]"},
		"a query that aggregates without GROUP BY": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: saSum, Alias: "s"})
		}), "columns[0].args[0]"},
		"a query that aggregates, nested in one that does not": {func() dal.StructuredQuery {
			inner := saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
				return b.SelectColumns(dal.Column{Expression: saSum, Alias: "s"})
			})
			return dal.From(exRef("", "C", "c")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
		}(), "where.query.columns[0].args[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: map[string][]string{"A": {"id"}, "B": {"id", "x"}, "C": {"k"}}}
			scope := scAsScope(t, checkScopes(context.Background(), tc.query, supplier.fields))
			if scope.Path != tc.path {
				t.Errorf("path = %q, want %q", scope.Path, tc.path)
			}
			for _, want := range []string{"unqualified field x", "qualify"} {
				if !strings.Contains(scope.Message, want) {
					t.Errorf("message %q does not say %q", scope.Message, want)
				}
			}
			if strings.Contains(scope.Message, "ambiguous") {
				t.Errorf("message %q calls the field ambiguous, and one source carries it", scope.Message)
			}
		})
	}
}

// What DALgo reads correctly stays as it was: a name the first source carries, a name no
// source carries (DALgo refuses it as unavailable), and every clause that is not an
// aggregation's.
func TestAnUnqualifiedFieldOfAnAggregateIsLeftAloneWhenTheFirstSourceCarriesItOrNoSourceDoes(t *testing.T) {
	groupByX := func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.GroupBy(scX).Having(dal.NewComparison(saSum, dal.GreaterThen, saOne)).OrderBy(dal.Ascending(saSum)).
			SelectColumns(dal.Column{Expression: scX}, dal.Column{Expression: saSum, Alias: "s"})
	}
	for name, tc := range map[string]struct {
		lists map[string][]string
		query dal.StructuredQuery
	}{
		"the first source carries it, in every clause of the aggregation": {saOnlyA, scJoin(groupByX)},
		"no source carries it, which DALgo refuses as unavailable":        {saNone, scJoin(groupByX)},
		"another source carries it, in WHERE, which DALgo binds to its carrier": {saOnlyB, saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(scX, dal.Equal, saOne)).SelectColumns(saCount())
		})},
		// A name only another source carries, in ON, is refused since OJ-16 on every plan, an
		// aggregating query included: see TestAnUnqualifiedFieldInOnIsRefusedWhenOnlyAnotherSourceCarriesIt.
		"another source carries it, in a query that does not aggregate": {saOnlyB, scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(scX)).SelectColumns(dal.Column{Expression: scX})
		})},
		"another source carries it, qualified": {saOnlyB, saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: dal.NewAggregate("SUM", false, dal.NewFieldRef("b", "x")), Alias: "s"})
		})},
		"another source carries it, as the alias of a column, in HAVING and ORDER BY": {saOnlyB, saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Having(dal.NewComparison(scX, dal.GreaterThen, saOne)).OrderBy(dal.Ascending(scX)).
				SelectColumns(dal.Column{Expression: dal.NewAggregate("SUM", false, dal.NewFieldRef("b", "x")), Alias: "x"})
		})},
		"another source carries it, in a query of one source": {saOnlyB, dal.From(exRef("", "B", "b")).NewQuery().GroupBy(scX).SelectColumns(dal.Column{Expression: scX}, saCount())},
		"an aggregating level nested in a level that reads the name from another source, and names no field": {saOnlyB, func() dal.StructuredQuery {
			inner := saAggregating(func(b dal.IQueryBuilder) dal.StructuredQuery { return b.SelectColumns(saCount()) })
			return dal.From(exRef("", "A", "a")).Join(dal.NewJoinedSource(exRef("", "B", "b"), dal.JoinInner, exKeyEquals(exRef("", "A", "a"), exRef("", "B", "b")))).NewQuery().
				Where(dal.NewExistsCondition(inner)).OrderBy(dal.Ascending(scX)).SelectColumns(dal.Column{Expression: scX})
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: tc.lists}
			if err := checkScopes(context.Background(), tc.query, supplier.fields); err != nil {
				t.Fatalf("checkScopes: %v", err)
			}
		})
	}
}

// The name that two lists carry is ambiguous whoever the first source is, and the refusal
// comes at the first field in document order that DALgo cannot read from the right source,
// whichever of the two rules finds it.
func TestTheFirstRefusedFieldInDocumentOrderIsTheOneNamedInAnAggregate(t *testing.T) {
	only := func(name string) dal.Expression { return dal.NewFieldRef("", name) }
	query := func(group dal.Expression, columns ...dal.Column) dal.StructuredQuery {
		return scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery { return b.GroupBy(group).SelectColumns(columns...) })
	}
	lists := map[string][]string{"A": {"id", "both"}, "B": {"both", "onlyb"}}
	for name, tc := range map[string]struct {
		query   dal.StructuredQuery
		path    string
		message string
	}{
		"a name two lists carry, before a name only the other source carries": {
			query(only("both"), dal.Column{Expression: only("onlyb")}), "groupBy[0]", "ambiguous unqualified field both"},
		"a name only the other source carries, before a name two lists carry": {
			query(only("onlyb"), dal.Column{Expression: only("both")}), "groupBy[0]", "unqualified field onlyb"},
		"a name two lists carry, after one the first source carries": {
			query(only("id"), dal.Column{Expression: only("id")}, dal.Column{Expression: only("both")}), "columns[1]", "ambiguous unqualified field both"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: lists}
			scope := scAsScope(t, checkScopes(context.Background(), tc.query, supplier.fields))
			if scope.Path != tc.path || !strings.Contains(scope.Message, tc.message) {
				t.Fatalf("got %+v, want %q at %s", scope, tc.message, tc.path)
			}
			if !reflect.DeepEqual(supplier.asked, []string{"A", "B"}) {
				t.Fatalf("asked %v, want both sources", supplier.asked)
			}
		})
	}
}

// A source that supplies no list is refused first, as it was, with the way out.
func TestAMissingListIsRefusedBeforeTheFirstSourceRuleOfAnAggregate(t *testing.T) {
	q := scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery { return b.GroupBy(scX).SelectColumns(saCount()) })
	scope := scAsScope(t, checkScopes(context.Background(), q, scHalf().fields))
	if scope.Path != "groupBy[0]" || !strings.Contains(scope.Message, "no field list") {
		t.Fatalf("got %+v", scope)
	}
}

// The refusal over a built query: Execute answers a 400-class scope error before any
// source is read, and the same query with the name qualified, or with the first source
// as its only carrier, is answered from the right source.
func TestAnAggregateOfSeveralSourcesIsNeverAnsweredFromTheFirstSourceForAFieldItDoesNotCarry(t *testing.T) {
	// A is the customer-like source (id, k, name), B the ledger-like source (id, k, amount).
	build := func(amount dal.Expression) dal.StructuredQuery {
		a, b := exRef("one", "A", "a"), exRef("two", "B", "b")
		return dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, exKeyEquals(a, b))).NewQuery().
			Where(dal.NewExistsCondition(exPlain("one", "A"))).GroupBy(dal.NewFieldRef("a", "name")).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name")}, dal.Column{Expression: dal.NewAggregate("SUM", false, amount), Alias: "total"})
	}
	run := func(amount dal.Expression) (Result, []*exSource, error) {
		customers, ledger := scTwoMounts([]string{"id", "k", "name"}, []string{"id", "k", "amount"})
		res, err := exRun(t, build(amount), "", newExRegistry(customers, ledger), exAllow, Limits{})
		return res, []*exSource{customers, ledger}, err
	}

	t.Run("the argument only the second source carries is refused before anything is read", func(t *testing.T) {
		_, mounts, err := run(dal.NewFieldRef("", "amount"))
		scope := scAsScope(t, err)
		if scope.Path != "columns[1].args[0]" || !strings.Contains(scope.Message, "qualify") {
			t.Fatalf("got %+v", scope)
		}
		for _, mount := range mounts {
			if seen := mount.exec.seen(); len(seen) != 0 {
				t.Fatalf("mount %s was read before the refusal", mount.id)
			}
		}
	})
	t.Run("the argument qualified by its source is read from it", func(t *testing.T) {
		res, _, err := run(dal.NewFieldRef("b", "amount"))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if got, want := exAsRows(t, res.Records), []map[string]any{{"name": "ada", "total": 42.0}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
}
