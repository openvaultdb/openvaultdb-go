package joinexec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// The helpers of this file start with "sc" so that they cannot clash with the
// helpers of the tests around them.

// scSupplier answers the field lists of sources by collection name, as the
// executors DALgo reads through do: a collection it has no list for has none. It
// remembers every source it was asked for.
type scSupplier struct {
	lists map[string][]string
	errs  map[string]error
	asked []string
}

func (s *scSupplier) fields(_ context.Context, source dal.RecordsetSource) ([]string, error) {
	s.asked = append(s.asked, source.Name())
	if err := s.errs[source.Name()]; err != nil {
		return nil, err
	}
	return s.lists[source.Name()], nil
}

// scLists is a supplier for A and B in which A has a list and B has none.
func scHalf() *scSupplier { return &scSupplier{lists: map[string][]string{"A": {"id", "x"}}} }

// scAsScope returns err as the scope error checkScopes refuses with.
func scAsScope(t *testing.T, err error) *dal.QueryValidationError {
	t.Helper()
	var scope *dal.QueryValidationError
	if !errors.As(err, &scope) || scope.Category != "scope" {
		t.Fatalf("want a scope error, got %T: %v", err, err)
	}
	return scope
}

// scJoin joins A and B on their key and runs build over the builder, so a test puts
// one clause of its own on a query of two sources.
func scJoin(build func(b dal.IQueryBuilder) dal.StructuredQuery) dal.StructuredQuery {
	a, b := exRef("", "A", "a"), exRef("", "B", "b")
	return build(dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, exKeyEquals(a, b))).NewQuery())
}

var scX = dal.NewFieldRef("", "x")

// Every clause that can hold a field is read, and the path says which.
func TestAnUnqualifiedFieldOfAQueryOfSeveralSourcesIsRefusedWhereverItStandsWhenASourceSuppliesNoFields(t *testing.T) {
	one := dal.NewConstant(1)
	column := func(e dal.Expression) dal.Column { return dal.Column{Expression: e, Alias: "out"} }
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
	}{
		"a column": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery { return b.SelectColumns(dal.Column{Expression: scX}) }), "columns[0]"},
		"an operand of arithmetic in a column": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(column(dal.Binary(dal.NewFieldRef("a", "id"), dal.Add, scX)))
		}), "columns[0].right"},
		"an argument of an aggregate": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(column(dal.NewAggregate("SUM", false, scX)))
		}), "columns[0].args[0]"},
		"the left of a comparison in WHERE": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(scX, dal.Equal, one)).SelectIntoRecord(nil)
		}), "where.left"},
		"the right of a comparison in WHERE": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(one, dal.Equal, scX)).SelectIntoRecord(nil)
		}), "where.right"},
		"a condition of a group in WHERE": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewGroupCondition(dal.And, dal.NewComparison(one, dal.Equal, one), dal.NewComparison(scX, dal.Equal, one))).SelectIntoRecord(nil)
		}), "where.conditions[1].left"},
		"the operand of a null test": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewIsNullCondition(scX)).SelectIntoRecord(nil)
		}), "where.operand"},
		"GROUP BY": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(dal.CountAs(dal.Star(), "n"))
		}), "groupBy[0]"},
		"HAVING": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(dal.NewFieldRef("a", "id")).Having(dal.NewComparison(scX, dal.GreaterThen, one)).SelectColumns(dal.CountAs(dal.Star(), "n"))
		}), "having.left"},
		"ORDER BY": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(scX)).SelectIntoRecord(nil)
		}), "orderBy[0]"},
		"the ON condition of a join": {func() dal.StructuredQuery {
			a, b := exRef("", "A", "a"), exRef("", "B", "b")
			return dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, dal.NewComparison(scX, dal.Equal, dal.NewFieldRef("b", "k")))).NewQuery().SelectIntoRecord(nil)
		}(), "from.joins[0].on[0].left"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := scHalf()
			err := checkScopes(context.Background(), tc.query, supplier.fields)
			scope := scAsScope(t, err)
			if scope.Path != tc.path {
				t.Errorf("path = %q, want %q", scope.Path, tc.path)
			}
			for _, want := range []string{"x", "qualify"} {
				if !strings.Contains(scope.Message, want) {
					t.Errorf("message %q does not say %q", scope.Message, want)
				}
			}
		})
	}
}

func TestTheFirstUnqualifiedFieldInDocumentOrderIsTheOneNamed(t *testing.T) {
	q := scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.Where(dal.NewComparison(dal.NewFieldRef("", "inwhere"), dal.Equal, dal.NewConstant(1))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "incolumn")})
	})
	err := checkScopes(context.Background(), q, scHalf().fields)
	if scope := scAsScope(t, err); scope.Path != "where.left" || !strings.Contains(scope.Message, "inwhere") {
		t.Fatalf("got %+v", scope)
	}
}

// DALgo decides when it has every list and only one source carries the name: it binds
// the field the one source carries and refuses one that none does.
func TestTheDecisionIsLeftToDALgoWhenEverySourceSuppliesAFieldListAndOnlyOneCarriesTheName(t *testing.T) {
	q := scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery { return b.SelectColumns(dal.Column{Expression: scX}) })
	for name, lists := range map[string]map[string][]string{
		"the field is in the first source":  {"A": {"id", "x"}, "B": {"id", "k"}},
		"the field is in the other source":  {"A": {"id", "k"}, "B": {"id", "x"}},
		"the field is in neither source":    {"A": {"id"}, "B": {"k"}},
		"a list that is empty but supplied": {"A": {"x"}, "B": {}},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: lists}
			if err := checkScopes(context.Background(), q, supplier.fields); err != nil {
				t.Fatalf("checkScopes: %v", err)
			}
			if !reflect.DeepEqual(supplier.asked, []string{"A", "B"}) {
				t.Fatalf("asked %v, want both sources in document order", supplier.asked)
			}
		})
	}
}

func TestOnlyAnUnqualifiedFieldOfAQueryOfSeveralSourcesIsLookedAt(t *testing.T) {
	qualified := dal.NewFieldRef("a", "x")
	for name, query := range map[string]dal.StructuredQuery{
		// A field of an ORDER BY that names its source is looked at in the list of that source
		// (see scope_refusals_test.go), so it is not one of these.
		"fields that a source qualifies": scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(qualified, dal.Equal, dal.NewConstant(1))).SelectColumns(dal.Column{Expression: qualified})
		}),
		"the alias of a column in ORDER BY of an aggregating query": scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(qualified).OrderBy(dal.Descending(dal.NewFieldRef("", "n"))).SelectColumns(dal.CountAs(dal.Star(), "n"))
		}),
		"the alias of a column in HAVING": scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(qualified).Having(dal.NewComparison(dal.NewFieldRef("", "n"), dal.GreaterThen, dal.NewConstant(1))).SelectColumns(dal.CountAs(dal.Star(), "n"))
		}),
		"the alias of a column in an arithmetic expression of HAVING": scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(qualified).Having(dal.NewComparison(dal.Binary(dal.NewFieldRef("", "n"), dal.Add, dal.NewConstant(1)), dal.GreaterThen, dal.NewConstant(1))).
				SelectColumns(dal.CountAs(dal.Star(), "n"))
		}),
		"HAVING with no GROUP BY and no aggregate is a single group": scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Having(dal.NewComparison(dal.NewFieldRef("", "out"), dal.GreaterThen, dal.NewConstant(1))).SelectColumns(dal.Column{Expression: qualified, Alias: "out"})
		}),
		"a wildcard, a constant, a parameter and a star": scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(dal.NewParam("p"), dal.Equal, dal.NewConstant(1))).
				SelectColumns(dal.Column{Wildcard: &dal.WildcardProjection{Source: "a"}}, dal.Column{Expression: dal.NewConstant(1), Alias: "one"}, dal.CountAs(dal.Star(), "n"))
		}),
		"an unqualified field of a query of one source": dal.From(exRef("", "B", "")).NewQuery().Where(dal.WhereField("x", dal.Equal, 1)).SelectColumns(dal.Column{Expression: scX}),
	} {
		t.Run(name, func(t *testing.T) {
			supplier := scHalf()
			if err := checkScopes(context.Background(), query, supplier.fields); err != nil {
				t.Fatalf("checkScopes: %v", err)
			}
			if len(supplier.asked) != 0 {
				t.Fatalf("a source was asked for its fields: %v", supplier.asked)
			}
		})
	}
}

// A name that a column of the select list carries as its alias is read as that column
// only where DALgo reads it so: in HAVING and ORDER BY of a query that aggregates, outside
// the argument of an aggregate. Everywhere else DALgo reads a field of a source, so the
// name is an unqualified field like any other and is refused when a source supplies no
// list, however a column of the select list is called.
func TestAnAliasOfTheSelectListIsAFieldOfASourceWhereDALgoReadsItSo(t *testing.T) {
	out, one := dal.NewFieldRef("", "out"), dal.NewConstant(1)
	qualified := dal.NewFieldRef("a", "x")
	aliased := dal.Column{Expression: qualified, Alias: "out"}
	counted := dal.CountAs(dal.Star(), "total")
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
	}{
		"WHERE": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(out, dal.Equal, one)).SelectColumns(aliased)
		}), "where.left"},
		"the ON condition of a join": {func() dal.StructuredQuery {
			a, b := exRef("", "A", "a"), exRef("", "B", "b")
			return dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, dal.NewComparison(out, dal.Equal, dal.NewFieldRef("b", "k")))).NewQuery().SelectColumns(aliased)
		}(), "from.joins[0].on[0].left"},
		"GROUP BY": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(out).SelectColumns(aliased, counted)
		}), "groupBy[0]"},
		"an operand of a column": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(aliased, dal.Column{Expression: dal.Binary(out, dal.Add, one), Alias: "next"})
		}), "columns[1].left"},
		// ORDER BY of a query that does not aggregate is not a row of this table since OJ-16:
		// the name of a column that selects a field is replaced by the field (see
		// scope_refusals_test.go), and the name of a column that is an expression is refused
		// whatever the sources supply.
		"an argument of an aggregate in HAVING": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(qualified).Having(dal.NewComparison(dal.NewAggregate("SUM", false, out), dal.GreaterThen, one)).SelectColumns(aliased, counted)
		}), "having.left.args[0]"},
		"an argument of an aggregate in ORDER BY": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(qualified).OrderBy(dal.Ascending(dal.NewAggregate("SUM", false, out))).SelectColumns(aliased, counted)
		}), "orderBy[0].args[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: map[string][]string{"A": {"id", "x"}, "C": {"k"}}}
			err := checkScopes(context.Background(), tc.query, supplier.fields)
			if scope := scAsScope(t, err); scope.Path != tc.path || !strings.Contains(scope.Message, "out") || !strings.Contains(scope.Message, "qualify") {
				t.Fatalf("got %+v, want a refusal at %s that names the field", scope, tc.path)
			}
		})
	}
}

// With every list supplied, a name that two lists carry is refused here, as DALgo
// refuses it on the evaluation that checks it: DALgo's streaming plan of a flat join of
// two sources binds an unqualified field to the first source without a check, and the
// DTQL parser, which keeps such a field out of a join without a subquery, is not what
// Execute may depend on, since it takes any built query.
func TestAnUnqualifiedFieldTwoSuppliedListsCarryIsRefusedAsAmbiguous(t *testing.T) {
	lists := map[string][]string{"A": {"id", "x", "only_a"}, "B": {"x", "only_b"}}
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
	}{
		"a column": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery { return b.SelectColumns(dal.Column{Expression: scX}) }), "columns[0]"},
		"WHERE": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(scX, dal.Equal, dal.NewConstant(1))).SelectIntoRecord(nil)
		}), "where.left"},
		"the first of several fields, in document order": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(dal.NewFieldRef("", "only_a"), dal.Equal, dal.NewConstant(1))).
				SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "only_b")}, dal.Column{Expression: scX})
		}), "columns[1]"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: lists}
			err := checkScopes(context.Background(), tc.query, supplier.fields)
			scope := scAsScope(t, err)
			if scope.Path != tc.path || scope.Message != "ambiguous unqualified field x" {
				t.Fatalf("got %+v, want ambiguous unqualified field x at %s", scope, tc.path)
			}
			if !reflect.DeepEqual(supplier.asked, []string{"A", "B"}) {
				t.Fatalf("asked %v, want both sources", supplier.asked)
			}
		})
	}

	t.Run("a nested query is read against its own sources", func(t *testing.T) {
		c, d := exRef("", "C", "c"), exRef("", "D", "d")
		inner := dal.From(c).Join(dal.NewJoinedSource(d, dal.JoinInner, exKeyEquals(c, d))).NewQuery().SelectColumns(dal.Column{Expression: scX})
		query := dal.From(exRef("", "A", "a")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
		supplier := &scSupplier{lists: map[string][]string{"A": {"x"}, "C": {"x"}, "D": {"x"}}}
		scope := scAsScope(t, checkScopes(context.Background(), query, supplier.fields))
		if scope.Path != "where.query.columns[0]" || scope.Message != "ambiguous unqualified field x" {
			t.Fatalf("got %+v", scope)
		}
	})

	t.Run("a missing list is refused first, with the way out", func(t *testing.T) {
		supplier := &scSupplier{lists: map[string][]string{"A": {"x"}}}
		q := scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery { return b.SelectColumns(dal.Column{Expression: scX}) })
		scope := scAsScope(t, checkScopes(context.Background(), q, supplier.fields))
		if strings.Contains(scope.Message, "ambiguous") || !strings.Contains(scope.Message, "qualify") {
			t.Fatalf("got %+v", scope)
		}
	})

	t.Run("the name is clipped", func(t *testing.T) {
		long := strings.Repeat("f", 300)
		q := scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: dal.NewFieldRef("", long)})
		})
		supplier := &scSupplier{lists: map[string][]string{"A": {long}, "B": {long}}}
		scope := scAsScope(t, checkScopes(context.Background(), q, supplier.fields))
		if strings.Contains(scope.Message, long) || !strings.Contains(scope.Message, strings.Repeat("f", maxEchoLen)+"...") {
			t.Fatalf("message = %q", scope.Message)
		}
	})
}

// A query inside a clause is a level of its own: its fields are read against its
// own sources, whatever the sources around it supply.
func TestAnUnqualifiedFieldOfAQueryNestedInAClauseIsReadAgainstItsOwnSources(t *testing.T) {
	inner := func(extra func(b dal.IQueryBuilder) dal.IQueryBuilder) dal.StructuredQuery {
		c, d := exRef("", "C", "c"), exRef("", "D", "d")
		b := dal.From(c).Join(dal.NewJoinedSource(d, dal.JoinInner, exKeyEquals(c, d))).NewQuery()
		return extra(b).SelectColumns(dal.Column{Expression: scX})
	}
	noop := func(b dal.IQueryBuilder) dal.IQueryBuilder { return b }
	rootOf := func(where dal.Condition, columns ...dal.Column) dal.StructuredQuery {
		var b dal.IQueryBuilder = dal.From(exRef("", "A", "a")).NewQuery()
		if where != nil {
			b = b.Where(where)
		}
		return b.SelectColumns(append(columns, dal.Column{Expression: dal.NewFieldRef("a", "id")})...)
	}
	supplied := map[string][]string{"A": {"id"}, "C": {"id", "x"}} // D supplies none
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
	}{
		"an EXISTS test":    {rootOf(dal.NewExistsCondition(inner(noop))), "where.query.columns[0]"},
		"a scalar subquery": {rootOf(nil, dal.Column{Expression: dal.NewQueryExpression(inner(noop), "sub")}), "columns[0].query.columns[0]"},
		"a derived source":  {dal.From(dal.NewQuerySource(inner(noop), "q")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("q", "x")}), "from.query.columns[0]"},
		"a derived source on a join": {func() dal.StructuredQuery {
			a := exRef("", "A", "a")
			return dal.From(a).Join(dal.NewJoinedSource(dal.NewQuerySource(inner(noop), "q"), dal.JoinInner,
				dal.NewComparison(dal.NewFieldRef("a", "k"), dal.Equal, dal.NewFieldRef("q", "x")))).NewQuery().
				SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
		}(), "from.joins[0].from.query.columns[0]"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: supplied}
			err := checkScopes(context.Background(), tc.query, supplier.fields)
			if scope := scAsScope(t, err); scope.Path != tc.path {
				t.Fatalf("path = %q, want %q", scope.Path, tc.path)
			}
		})
	}

	t.Run("the sources around a nested query do not matter", func(t *testing.T) {
		// The root has two sources and no unqualified field; the nested query has two
		// sources that both supply a list.
		supplier := &scSupplier{lists: map[string][]string{"C": {"x"}, "D": {"k"}}}
		query := rootOf(dal.NewExistsCondition(inner(noop)))
		if err := checkScopes(context.Background(), query, supplier.fields); err != nil {
			t.Fatalf("checkScopes: %v", err)
		}
		if !reflect.DeepEqual(supplier.asked, []string{"C", "D"}) {
			t.Fatalf("asked %v, want the sources of the nested query only", supplier.asked)
		}
	})
}

// A derived source supplies no list, and DALgo does not ask for one: it runs the
// query of the source.
func TestADerivedSourceSuppliesNoFieldList(t *testing.T) {
	derived := dal.NewQuerySource(dal.From(exRef("", "B", "")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "k")}), "d")
	q := dal.From(exRef("", "A", "a")).Join(dal.NewJoinedSource(derived, dal.JoinInner,
		dal.NewComparison(dal.NewFieldRef("a", "k"), dal.Equal, dal.NewFieldRef("d", "k")))).NewQuery().
		SelectColumns(dal.Column{Expression: scX})
	supplier := &scSupplier{lists: map[string][]string{"A": {"id", "x"}, "B": {"k"}}}
	err := checkScopes(context.Background(), q, supplier.fields)
	scope := scAsScope(t, err)
	if scope.Path != "columns[0]" {
		t.Fatalf("got %+v", scope)
	}
	if !reflect.DeepEqual(supplier.asked, []string{"A"}) {
		t.Fatalf("asked %v: the derived source is not asked for a list", supplier.asked)
	}
}

func TestAFailureToSupplyAFieldListIsReturnedAsItIs(t *testing.T) {
	q := scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery { return b.SelectColumns(dal.Column{Expression: scX}) })
	supplier := &scSupplier{lists: map[string][]string{"A": {"x"}}, errs: map[string]error{"B": errExBoom}}
	if err := checkScopes(context.Background(), q, supplier.fields); err != errExBoom {
		t.Fatalf("got %v, want the failure of the supplier itself", err)
	}
}

func TestAFailureToSupplyTheFieldListOfAQualifiedOrderByNameIsReturnedAsItIs(t *testing.T) {
	q := scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.OrderBy(dal.Ascending(dal.NewFieldRef("b", "x"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
	})
	supplier := &scSupplier{lists: map[string][]string{"A": {"x"}}, errs: map[string]error{"B": errExBoom}}
	if err := checkScopes(context.Background(), q, supplier.fields); err != errExBoom {
		t.Fatalf("got %v, want the failure of the supplier itself", err)
	}
}

func TestTheFieldNamedInARefusalIsClipped(t *testing.T) {
	long := strings.Repeat("f", 300)
	q := scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.SelectColumns(dal.Column{Expression: dal.NewFieldRef("", long)})
	})
	err := checkScopes(context.Background(), q, scHalf().fields)
	scope := scAsScope(t, err)
	if strings.Contains(scope.Message, long) || !strings.Contains(scope.Message, strings.Repeat("f", maxEchoLen)+"...") {
		t.Fatalf("message = %q", scope.Message)
	}
}

// scRows builds rows that carry the given fields of one collection.
func scRows(collection string, rows ...map[string]any) []record.Record {
	out := make([]record.Record, len(rows))
	for i, data := range rows {
		out[i] = record.NewRecordWithData(record.NewKeyWithID(collection, i+1), data)
	}
	return out
}

// scTwoMounts holds a customer table and a ledger table in two mounts. Both rows
// carry the field k, and only the ledger carries amount.
func scTwoMounts(customerFields, ledgerFields []string) (customers, ledger *exSource) {
	customers = exMount("one", "sqlite", false, map[string][]record.Record{"A": scRows("A", map[string]any{"id": 1, "k": 7, "name": "ada"})})
	ledger = exMount("two", "sqlite", false, map[string][]record.Record{"B": scRows("B", map[string]any{"id": 9, "k": 7, "amount": 42})})
	customers.exec.fields, ledger.exec.fields = customerFields, ledgerFields
	return customers, ledger
}

// scAcross joins A of "one" to B of "two" on k and selects the given columns, with
// an EXISTS test that puts the document on the recursive evaluation of DALgo, the
// one that binds unqualified fields.
func scAcross(columns ...dal.Column) dal.StructuredQuery {
	a, b := exRef("one", "A", "a"), exRef("two", "B", "b")
	return dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, exKeyEquals(a, b))).NewQuery().
		Where(dal.NewExistsCondition(exPlain("one", "A"))).SelectColumns(columns...)
}

// The defect: with no field lists DALgo bound an unqualified name that two sources
// carry to the first source and answered with its values. The mounts supply their
// lists, so DALgo refuses the name as ambiguous; and a mount that cannot supply
// one is refused before anything is read, never answered from the wrong source.
func TestAnUnqualifiedFieldBothSourcesCarryIsRefusedAndNeverReadFromTheFirst(t *testing.T) {
	both := dal.Column{Expression: dal.NewFieldRef("", "k")}
	t.Run("both mounts supply their fields", func(t *testing.T) {
		customers, ledger := scTwoMounts([]string{"id", "k", "name"}, []string{"id", "k", "amount"})
		_, err := exRun(t, scAcross(both), "", newExRegistry(customers, ledger), exAllow, Limits{})
		scope := scAsScope(t, err)
		if scope.Path != "columns[0]" || !strings.Contains(scope.Message, "ambiguous unqualified field k") {
			t.Fatalf("got %+v", scope)
		}
	})
	t.Run("one mount supplies no fields", func(t *testing.T) {
		customers, ledger := scTwoMounts([]string{"id", "k", "name"}, nil)
		_, err := exRun(t, scAcross(both), "", newExRegistry(customers, ledger), exAllow, Limits{})
		scope := scAsScope(t, err)
		if scope.Path != "columns[0]" || !strings.Contains(scope.Message, "qualify") || strings.Contains(scope.Message, "ambiguous") {
			t.Fatalf("got %+v", scope)
		}
		for _, mount := range []*exSource{customers, ledger} {
			if seen := mount.exec.seen(); len(seen) != 0 {
				t.Fatalf("mount %s was read %d times before the refusal", mount.id, len(seen))
			}
		}
	})
}

// DALgo's streaming plan of a flat join of two sources (no subquery, no ORDER BY) binds
// an unqualified field to the first source and checks it against that source's list
// alone. The DTQL parser keeps such a field out of the document, but Execute takes a
// built query: a name both mounts carry is refused here, and a name only the other
// mount carries is never read from the first source.
func TestAnUnqualifiedFieldOfAFlatJoinIsNeverReadFromTheWrongSource(t *testing.T) {
	flat := func(name string) dal.StructuredQuery {
		a, b := exRef("one", "A", "a"), exRef("two", "B", "b")
		return dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, exKeyEquals(a, b))).NewQuery().
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("", name)})
	}
	run := func(name string) (Result, []*exSource, error) {
		customers, ledger := scTwoMounts([]string{"id", "k", "name"}, []string{"id", "k", "amount"})
		res, err := exRun(t, flat(name), "", newExRegistry(customers, ledger), exAllow, Limits{})
		return res, []*exSource{customers, ledger}, err
	}

	t.Run("a name both carry is ambiguous", func(t *testing.T) {
		_, mounts, err := run("k")
		scope := scAsScope(t, err)
		if scope.Path != "columns[0]" || scope.Message != "ambiguous unqualified field k" {
			t.Fatalf("got %+v", scope)
		}
		for _, mount := range mounts {
			if seen := mount.exec.seen(); len(seen) != 0 {
				t.Fatalf("mount %s was read before the refusal", mount.id)
			}
		}
	})
	t.Run("a name only the other mount carries is refused, not read from the first", func(t *testing.T) {
		res, _, err := run("amount")
		if err == nil || len(res.Records) != 0 {
			t.Fatalf("got %d rows and %v, want a refusal", len(res.Records), err)
		}
	})
	t.Run("a name only the first mount carries is read from it", func(t *testing.T) {
		res, _, err := run("name")
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if got, want := exAsRows(t, res.Records), []map[string]any{{"name": "ada"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
}

func TestAnUnqualifiedFieldOnlyOneSourceCarriesIsBoundToThatSource(t *testing.T) {
	customers, ledger := scTwoMounts([]string{"id", "k", "name"}, []string{"id", "k", "amount"})
	res, err := exRun(t, scAcross(dal.Column{Expression: dal.NewFieldRef("", "amount")}, dal.Column{Expression: dal.NewFieldRef("a", "name")}),
		"", newExRegistry(customers, ledger), exAllow, Limits{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := exAsRows(t, res.Records), []map[string]any{{"amount": 42.0, "name": "ada"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

func TestTheSameDocumentWithTheFieldQualifiedReadsWhateverTheMountsSupply(t *testing.T) {
	for name, tc := range map[string]struct{ customers, ledger []string }{
		"both supply":      {[]string{"id", "k", "name"}, []string{"id", "k", "amount"}},
		"one supplies":     {[]string{"id", "k", "name"}, nil},
		"neither supplies": {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			customers, ledger := scTwoMounts(tc.customers, tc.ledger)
			res, err := exRun(t, scAcross(dal.Column{Expression: dal.NewFieldRef("b", "k"), Alias: "key"}), "", newExRegistry(customers, ledger), exAllow, Limits{})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got, want := exAsRows(t, res.Records), []map[string]any{{"key": 7.0}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("rows = %v, want %v", got, want)
			}
		})
	}
}

func TestAFailureOfAMountToSupplyItsFieldsEndsTheRequestWithTheErrorOfTheSource(t *testing.T) {
	customers, ledger := scTwoMounts([]string{"id", "k", "name"}, []string{"id", "k", "amount"})
	ledger.exec.fieldsErr = errExBoom
	_, err := exRun(t, scAcross(dal.Column{Expression: dal.NewFieldRef("", "amount")}), "", newExRegistry(customers, ledger), exAllow, Limits{})
	if !errors.Is(err, errExBoom) {
		t.Fatalf("got %v, want the failure of the source", err)
	}
	if source := exAsSourceError(t, err); source.Database != "two" || source.Collection != "B" {
		t.Fatalf("source error = %+v", source)
	}
	for _, mount := range []*exSource{customers, ledger} {
		if seen := mount.exec.seen(); len(seen) != 0 {
			t.Fatalf("mount %s was read before the failure", mount.id)
		}
	}
}
