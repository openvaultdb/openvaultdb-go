package joinexec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// The helpers of this file start with "sr" so that they cannot clash with the
// helpers of the tests around them.
//
// The rule of these tests: the server sorts a document, binds it, or refuses it. It
// never ignores part of it, and never reads a name as another thing than the document
// says. What checkScopes refuses without a field list (a name an earlier column
// carries, an alias that stands for an expression) and what it refuses with the lists
// (a name no source carries, in ORDER BY, and a name another source carries in ON) are
// tested here at the walk and through Execute, on real engines' lists.

var (
	srCount = dal.CountAs(dal.Star(), "x")
	srOne   = dal.NewConstant(1)
)

// srSingle builds a query of one source, A, with the clause the test puts on it.
func srSingle(build func(b dal.IQueryBuilder) dal.StructuredQuery) dal.StructuredQuery {
	return build(dal.From(exRef("", "A", "a")).NewQuery())
}

// srSupplied is a supplier in which A and B both carry id and x, and C carries k.
func srSupplied() *scSupplier {
	return &scSupplier{lists: map[string][]string{"A": {"id", "x"}, "B": {"id", "x"}, "C": {"k"}}}
}

// DALgo's aggregation reads an unqualified field of a column first as the column of
// the select list that carries that name, and as a field only when none does. So a
// column field that an earlier column carries as its alias, or selects under that
// name from a source, is read as that column and not as the field it says.
func TestAColumnFieldThatAnEarlierColumnCarriesAsItsAliasIsRefusedInAQueryThatAggregates(t *testing.T) {
	later := dal.Column{Expression: scX, Alias: "t"}
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
	}{
		"one source: the count is named after the grouped field": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(srCount, later)
		}), "columns[1]"},
		"two sources": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(srCount, later)
		}), "columns[1]"},
		"an operand of arithmetic in a later column": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(srCount, dal.Column{Expression: dal.Binary(scX, dal.Add, srOne), Alias: "next"})
		}), "columns[1].left"},
		"the later column is not the next one": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(srCount, dal.CountAs(dal.Star(), "y"), later)
		}), "columns[2]"},
		"the earlier column is a qualified field under that alias": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX, dal.NewFieldRef("a", "id")).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id"), Alias: "x"}, later)
		}), "columns[1]"},
		"the earlier column selects a qualified field under that name": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX, dal.NewFieldRef("b", "x")).SelectColumns(dal.Column{Expression: dal.NewFieldRef("b", "x")}, later)
		}), "columns[1]"},
		"the earlier column is a field qualified by another source than the only one": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX, dal.NewFieldRef("outer", "x")).SelectColumns(dal.Column{Expression: dal.NewFieldRef("outer", "x")}, later)
		}), "columns[1]"},
		"the earlier column is an unqualified field under another alias": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX, dal.NewFieldRef("", "y")).SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "y"), Alias: "x"}, later)
		}), "columns[1]"},
		"a query that aggregates, nested in one that does not": {func() dal.StructuredQuery {
			inner := srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery { return b.GroupBy(scX).SelectColumns(srCount, later) })
			return dal.From(exRef("", "C", "c")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
		}(), "where.query.columns[1]"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{} // no source supplies a list: the refusal needs none
			scope := scAsScope(t, checkScopes(context.Background(), tc.query, supplier.fields))
			if scope.Path != tc.path {
				t.Errorf("path = %q, want %q", scope.Path, tc.path)
			}
			for _, want := range []string{"unqualified field x", "earlier column", "rename the alias", "qualify the field"} {
				if !strings.Contains(scope.Message, want) {
					t.Errorf("message %q does not say %q", scope.Message, want)
				}
			}
			if len(supplier.asked) != 0 {
				t.Errorf("a source was asked for its fields: %v", supplier.asked)
			}
		})
	}
}

// What DALgo reads as the field it says stays as it was.
func TestAColumnFieldIsLeftAloneWhenNoEarlierColumnCarriesItAsItsAlias(t *testing.T) {
	qualified := dal.NewFieldRef("a", "x")
	for name, query := range map[string]dal.StructuredQuery{
		"the earlier column is the same field, unaliased": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(dal.Column{Expression: scX}, dal.Column{Expression: scX, Alias: "t"}, dal.CountAs(dal.Star(), "n"))
		}),
		"the earlier column is the same field under its own name": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(dal.Column{Expression: scX, Alias: "x"}, dal.Column{Expression: scX, Alias: "t"}, dal.CountAs(dal.Star(), "n"))
		}),
		"the earlier column is the same field, qualified by the only source": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(qualified).SelectColumns(dal.Column{Expression: qualified}, dal.Column{Expression: scX, Alias: "t"}, dal.CountAs(dal.Star(), "n"))
		}),
		"the earlier column is the same field, qualified by the only source, under its own name": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(qualified).SelectColumns(dal.Column{Expression: qualified, Alias: "x"}, dal.Column{Expression: scX, Alias: "t"}, dal.CountAs(dal.Star(), "n"))
		}),
		"the earlier column is the same field, qualified by the collection of a source with no alias": func() dal.StructuredQuery {
			field := dal.NewFieldRef("A", "x")
			return dal.From(exRef("", "A", "")).NewQuery().GroupBy(field).
				SelectColumns(dal.Column{Expression: field}, dal.Column{Expression: scX, Alias: "t"}, dal.CountAs(dal.Star(), "n"))
		}(),
		"the later column names its source": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(qualified).SelectColumns(srCount, dal.Column{Expression: qualified, Alias: "t"})
		}),
		"the alias comes after the field": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(dal.Column{Expression: scX, Alias: "t"}, srCount)
		}),
		"the alias is the name of another field": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(dal.CountAs(dal.Star(), "n"), dal.Column{Expression: scX, Alias: "t"})
		}),
		"the name is in the argument of an aggregate": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(dal.NewFieldRef("a", "id")).SelectColumns(srCount, dal.Column{Expression: dal.NewAggregate("SUM", false, scX), Alias: "s"})
		}),
		"the name is in GROUP BY and in a column that is the first": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(scX).SelectColumns(dal.Column{Expression: scX, Alias: "t"}, dal.CountAs(dal.Star(), "n"))
		}),
		"a query that does not aggregate reads no column as another": srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id"), Alias: "x"}, dal.Column{Expression: scX, Alias: "t"})
		}),
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{}
			if err := checkScopes(context.Background(), query, supplier.fields); err != nil {
				t.Fatalf("checkScopes: %v", err)
			}
		})
	}
}

func TestTheNameOfAnEarlierColumnIsClippedInTheRefusal(t *testing.T) {
	long := strings.Repeat("f", 300)
	field := dal.NewFieldRef("", long)
	q := srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.GroupBy(field).SelectColumns(dal.CountAs(dal.Star(), long), dal.Column{Expression: field, Alias: "t"})
	})
	scope := scAsScope(t, checkScopes(context.Background(), q, (&scSupplier{}).fields))
	if strings.Contains(scope.Message, long) || !strings.Contains(scope.Message, strings.Repeat("f", maxEchoLen)+"...") {
		t.Fatalf("message = %q", scope.Message)
	}
}

// Through Execute, on a mount that supplies its lists: the document that was answered
// with the count in the place of the field is refused before anything is read, one
// source or two.
func TestAnAggregateIsNeverAnsweredWithAnEarlierColumnInThePlaceOfAField(t *testing.T) {
	k := dal.NewFieldRef("", "k")
	q := dal.From(exRef("", "A", "a")).NewQuery().GroupBy(k).
		SelectColumns(dal.CountAs(dal.Star(), "k"), dal.Column{Expression: k, Alias: "kk"})
	rows, err := alRun(t, q)
	scope := scAsScope(t, err)
	if rows != nil || scope.Path != "columns[1]" || !strings.Contains(scope.Message, "unqualified field k") {
		t.Fatalf("got rows %v, %+v", rows, scope)
	}

	// Renamed, the same document is answered with the field.
	renamed := dal.From(exRef("", "A", "a")).NewQuery().GroupBy(k).
		SelectColumns(dal.CountAs(dal.Star(), "n"), dal.Column{Expression: k, Alias: "kk"})
	rows, err = alRun(t, renamed)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %v", rows)
	}
	for _, row := range rows {
		if row["n"] != 1.0 || (row["kk"] != 1.0 && row["kk"] != 2.0 && row["kk"] != 3.0) {
			t.Fatalf("row = %v: the group key must come back as kk", row)
		}
	}
}

// DALgo's streaming plan of a flat join of two sources reads an unqualified operand of ON
// from the first source, with no check. An ON field is held to the rule of an
// aggregation's columns on every plan: it is a field of the first source or is refused,
// so no plan answers a name that only the second source carries as no row at all.
func TestAnUnqualifiedFieldInOnIsRefusedWhenOnlyAnotherSourceCarriesIt(t *testing.T) {
	onOf := func(on ...dal.Condition) dal.StructuredQuery {
		a, b := exRef("", "A", "a"), exRef("", "B", "b")
		return dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, append([]dal.Condition{exKeyEquals(a, b)}, on...)...)).NewQuery().
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
	}
	amount := func(path string) dal.Condition { return dal.NewComparison(dal.NewFieldRef("", path), dal.Equal, srOne) }
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		lists map[string][]string
		want  string // "" is no refusal
		path  string
	}{
		"the second source carries it": {onOf(amount("only_b")), map[string][]string{"A": {"id", "k"}, "B": {"id", "k", "only_b"}},
			"unqualified field only_b", "from.joins[0].on[1].left"},
		"the second source carries it, on the right": {onOf(dal.NewComparison(srOne, dal.Equal, dal.NewFieldRef("", "only_b"))),
			map[string][]string{"A": {"id", "k"}, "B": {"id", "k", "only_b"}}, "unqualified field only_b", "from.joins[0].on[1].right"},
		"it is the first of two conditions": {onOf(), map[string][]string{"A": {"id", "k"}, "B": {"id", "k"}}, "", ""},
		"the first source carries it":       {onOf(amount("only_a")), map[string][]string{"A": {"id", "k", "only_a"}, "B": {"id", "k"}}, "", ""},
		"both carry it":                     {onOf(amount("both")), map[string][]string{"A": {"id", "k", "both"}, "B": {"id", "k", "both"}}, "ambiguous unqualified field both", "from.joins[0].on[1].left"},
		"no source carries it":              {onOf(amount("none")), map[string][]string{"A": {"id", "k"}, "B": {"id", "k"}}, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: tc.lists}
			err := checkScopes(context.Background(), tc.query, supplier.fields)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkScopes: %v", err)
				}
				return
			}
			scope := scAsScope(t, err)
			if scope.Path != tc.path || !strings.Contains(scope.Message, tc.want) {
				t.Fatalf("got %+v, want %q at %s", scope, tc.want, tc.path)
			}
			if !strings.Contains(tc.want, "ambiguous") && (!strings.Contains(scope.Message, "qualify") || !strings.Contains(scope.Message, "join condition") || strings.Contains(scope.Message, "ambiguous")) {
				t.Fatalf("message = %q, want the first source rule of a join condition", scope.Message)
			}
		})
	}
}

// The same document through Execute, on two mounts that name their database: the flat
// join of two sources that DALgo's streaming plan runs. Before the check an ON field only
// the ledger carries was read from the customer row and the join answered no row, with no
// error.
func TestAnOnFieldOnlyTheSecondMountCarriesIsNeverAnsweredAsNoRow(t *testing.T) {
	flat := func(on dal.Condition) dal.StructuredQuery {
		a, b := exRef("one", "A", "a"), exRef("two", "B", "b")
		return dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, exKeyEquals(a, b), on)).NewQuery().
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name")})
	}
	run := func(on dal.Condition) (Result, []*exSource, error) {
		customers, ledger := scTwoMounts([]string{"id", "k", "name"}, []string{"id", "k", "amount"})
		res, err := exRun(t, flat(on), "", newExRegistry(customers, ledger), exAllow, Limits{})
		return res, []*exSource{customers, ledger}, err
	}
	equals := func(name string, value any) dal.Condition {
		return dal.NewComparison(dal.NewFieldRef("", name), dal.Equal, dal.NewConstant(value))
	}

	t.Run("a name only the second mount carries is refused, before anything is read", func(t *testing.T) {
		res, mounts, err := run(equals("amount", 42))
		scope := scAsScope(t, err)
		if len(res.Records) != 0 || scope.Path != "from.joins[0].on[1].left" || !strings.Contains(scope.Message, "unqualified field amount") {
			t.Fatalf("got %d rows and %+v", len(res.Records), scope)
		}
		for _, mount := range mounts {
			if seen := mount.exec.seen(); len(seen) != 0 {
				t.Fatalf("mount %s was read before the refusal", mount.id)
			}
		}
	})
	t.Run("the same field qualified is read from its source", func(t *testing.T) {
		res, _, err := run(dal.NewComparison(dal.NewFieldRef("b", "amount"), dal.Equal, dal.NewConstant(42)))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if got, want := exAsRows(t, res.Records), []map[string]any{{"name": "ada"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
	t.Run("a name only the first mount carries is read from it", func(t *testing.T) {
		res, _, err := run(equals("name", "ada"))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if got, want := exAsRows(t, res.Records), []map[string]any{{"name": "ada"}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
	})
}

// ORDER BY of a query that does not aggregate. The name of a column of the select list
// is the alias of a field of a source, or of an expression. DALgo, and the executor of a
// mount that is handed the document whole, read the name as a field of a source, so the
// document is sorted by the field the column selects before either sees it, as a SQL
// database sorts it; an expression is not a field, and is refused with the way out.
func TestAnOrderByNameThatIsTheAliasOfAFieldIsNotLookedAtAndAnAliasOfAnExpressionIsRefused(t *testing.T) {
	label := dal.NewFieldRef("", "label")
	aliased := func(expression dal.Expression) dal.Column { return dal.Column{Expression: expression, Alias: "label"} }
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		lists map[string][]string // nil: no source supplies a list
		path  string              // "" is no refusal
	}{
		"the alias of a qualified field": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(label)).SelectColumns(aliased(dal.NewFieldRef("a", "x")))
		}), map[string][]string{"A": {"id", "x"}}, ""},
		"the alias of an unqualified field, in a query of two sources": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Descending(label)).SelectColumns(aliased(dal.NewFieldRef("", "only_a")))
		}), map[string][]string{"A": {"id", "only_a"}, "B": {"id"}}, ""},
		"the alias of an expression": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(label)).SelectColumns(aliased(dal.Binary(dal.NewFieldRef("a", "x"), dal.Multiply, srOne)))
		}), map[string][]string{"A": {"id", "x"}}, "orderBy[0]"},
		"the alias of an expression, with no list at all": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(label)).SelectColumns(aliased(dal.Binary(dal.NewFieldRef("a", "x"), dal.Multiply, srOne)))
		}), nil, "orderBy[0]"},
		"the alias of a constant": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(label)).SelectColumns(aliased(srOne))
		}), map[string][]string{"A": {"id", "x"}}, "orderBy[0]"},
		"the result name of a scalar subquery": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			inner := dal.From(exRef("", "C", "c")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
			return b.OrderBy(dal.Ascending(label)).SelectColumns(dal.Column{Expression: dal.NewQueryExpression(inner, "label")})
		}), map[string][]string{"A": {"id", "x"}, "C": {"k"}}, "orderBy[0]"},
		"the alias of an expression, in a query nested in one that does not name it": {func() dal.StructuredQuery {
			inner := srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
				return b.OrderBy(dal.Ascending(label)).SelectColumns(aliased(srOne))
			})
			return dal.From(exRef("", "C", "c")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
		}(), map[string][]string{"A": {"id"}, "C": {"k"}}, "where.query.orderBy[0]"},
		"a name of a qualified field is a field of its source": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("a", "label"))).SelectColumns(aliased(srOne))
		}), map[string][]string{"A": {"id", "label"}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: tc.lists}
			err := checkScopes(context.Background(), tc.query, supplier.fields)
			if tc.path == "" {
				if err != nil {
					t.Fatalf("checkScopes: %v", err)
				}
				return
			}
			scope := scAsScope(t, err)
			if scope.Path != tc.path {
				t.Errorf("path = %q, want %q", scope.Path, tc.path)
			}
			for _, want := range []string{"label", "order by the fields of"} {
				if !strings.Contains(scope.Message, want) {
					t.Errorf("message %q does not say %q", scope.Message, want)
				}
			}
			if len(supplier.asked) != 0 {
				t.Errorf("a source was asked for its fields: %v", supplier.asked)
			}
		})
	}
}

// A name inside the arithmetic of an ORDER BY is a field of a source, as a SQL database
// reads it: only an ORDER BY expression that is the bare name reads the alias of a column
// (SQLite: `select a as b from t order by b` sorts by the alias, `order by b*1` by the
// table's own b). So inside arithmetic the alias of a field does not hide a field of the
// source that has the name, and the alias of an expression is not refused for what it is: the
// name is looked at as a field of a source like any other.
func TestAnAliasInsideOrderByArithmeticIsLookedAtAsAFieldOfASource(t *testing.T) {
	label := dal.NewFieldRef("", "label")
	plusOne := func(left dal.Expression) dal.Expression { return dal.Binary(left, dal.Add, srOne) }
	onField := func(order dal.Expression) dal.StructuredQuery {
		return srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(order)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x"), Alias: "label"})
		})
	}
	onExpression := func(order dal.Expression) dal.StructuredQuery {
		return srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(order)).SelectColumns(dal.Column{Expression: srOne, Alias: "label"})
		})
	}
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		lists map[string][]string
		path  string // "" is no refusal
	}{
		"the alias of a field, which the source carries as a field too": {onField(plusOne(label)), map[string][]string{"A": {"id", "x", "label"}}, ""},
		"the alias of an expression, which the source carries as a field too": {onExpression(dal.Binary(srOne, dal.Add, label)),
			map[string][]string{"A": {"id", "label"}}, ""},
		"the alias of a field, which the source does not carry": {onField(plusOne(label)), map[string][]string{"A": {"id", "x"}}, "orderBy[0].left"},
		"the alias of an expression, which the source does not carry": {onExpression(dal.Binary(srOne, dal.Add, label)),
			map[string][]string{"A": {"id", "x"}}, "orderBy[0].right"},
		"the alias of a field, where no list says it is unknown": {onField(plusOne(label)), nil, ""},
		"the bare alias of a field is still the column":          {onField(label), map[string][]string{"A": {"id", "x"}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkScopes(context.Background(), tc.query, (&scSupplier{lists: tc.lists}).fields)
			if tc.path == "" {
				if err != nil {
					t.Fatalf("checkScopes: %v", err)
				}
				return
			}
			scope := scAsScope2(t, err, "shape")
			if scope.Path != tc.path || !strings.Contains(scope.Message, `unknown field "label"`) || !strings.Contains(scope.Message, "alias") {
				t.Fatalf("got %+v, want an unknown field label at %s that says what an alias is for", scope, tc.path)
			}
		})
	}
}

// The name of an ORDER BY that no source of the query carries, and that no column
// carries as its alias, is refused before anything is read: not ignored, as a mount that
// is handed the document whole ignores a field it does not know.
func TestAnOrderByNameNoSourceCarriesIsRefusedWhenEverySourceSuppliesAList(t *testing.T) {
	unknown := dal.NewFieldRef("", "nope")
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
		asked []string
	}{
		"one source": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(unknown)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), "orderBy[0]", []string{"A"}},
		"one source, the second ordering": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("", "x")), dal.Descending(unknown)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), "orderBy[1]", []string{"A"}},
		"an operand of arithmetic": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.Binary(unknown, dal.Add, srOne))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), "orderBy[0].left", []string{"A"}},
		"two sources": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(unknown)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
		}), "orderBy[0]", []string{"A", "B"}},
		"a qualified name": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("a", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), "orderBy[0]", []string{"A"}},
		"a qualified name of the second source, which is the only one asked": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("b", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
		}), "orderBy[0]", []string{"B"}},
		"a qualified operand of arithmetic": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.Binary(srOne, dal.Add, dal.NewFieldRef("a", "nope")))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), "orderBy[0].right", []string{"A"}},
		"a source with no alias is qualified by its collection": {
			dal.From(exRef("", "A", "")).NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("A", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("A", "x")}),
			"orderBy[0]", []string{"A"}},
		"an unqualified name that follows a qualified one that is known": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("a", "x")), dal.Ascending(unknown)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), "orderBy[1]", []string{"A"}},
		"a query nested in a clause": {func() dal.StructuredQuery {
			inner := srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
				return b.OrderBy(dal.Ascending(unknown)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
			})
			return dal.From(exRef("", "C", "c")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
		}(), "where.query.orderBy[0]", []string{"A"}},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := srSupplied()
			scope := scAsScope2(t, checkScopes(context.Background(), tc.query, supplier.fields), "shape")
			if scope.Path != tc.path || !strings.Contains(scope.Message, "unknown field") || !strings.Contains(scope.Message, `"nope"`) {
				t.Fatalf("got %+v, want an unknown field at %s", scope, tc.path)
			}
			if !reflect.DeepEqual(supplier.asked, tc.asked) {
				t.Fatalf("asked %v, want %v", supplier.asked, tc.asked)
			}
		})
	}
}

// What a refusal of the name would take away from a document that is read right stays.
func TestAnOrderByNameIsLeftAloneWhenASourceCarriesItOrNoListCanSayItIsUnknown(t *testing.T) {
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		lists map[string][]string
		asked []string
	}{
		"a field of the source": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(scX)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
		}), map[string][]string{"A": {"id", "x"}}, []string{"A"}},
		"a field only the second source carries": {scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("", "only_b"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
		}), map[string][]string{"A": {"id"}, "B": {"id", "only_b"}}, []string{"A", "B"}},
		"the key pseudo-field of the document engines": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("", "$id"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), map[string][]string{"A": {"x"}}, []string{"A"}},
		"a source that supplies no list, which the mount decides": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), nil, []string{"A"}},
		"a derived source, which supplies no list": {
			dal.From(dal.NewQuerySource(srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
				return b.SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
			}), "d")).NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "x")}),
			map[string][]string{"A": {"x"}}, nil},
		"a qualified name that the source carries": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("a", "x"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), map[string][]string{"A": {"x"}}, []string{"A"}},
		"the key pseudo-field, qualified": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("a", "$id"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), map[string][]string{"A": {"x"}}, nil},
		"a qualified name of a source that supplies no list, which the mount decides": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("a", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), nil, []string{"A"}},
		"a qualified name of a derived source, which supplies no list": {
			dal.From(dal.NewQuerySource(srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
				return b.SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
			}), "d")).NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("d", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "x")}),
			map[string][]string{"A": {"x"}}, nil},
		"a qualified name of a query outside the one that orders, which DALgo binds to its source": {func() dal.StructuredQuery {
			inner := srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
				return b.OrderBy(dal.Ascending(dal.NewFieldRef("c", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
			})
			return dal.From(exRef("", "C", "c")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
		}(), map[string][]string{"A": {"x"}, "C": {"k"}}, nil},
		"a qualified name of a query that aggregates, which DALgo checks itself": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(dal.NewFieldRef("a", "x")).OrderBy(dal.Ascending(dal.NewFieldRef("a", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")}, dal.CountAs(dal.Star(), "n"))
		}), map[string][]string{"A": {"x"}}, nil},
		"a name of the clause WHERE, which is not ordered by": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(dal.NewFieldRef("", "nope"), dal.Equal, srOne)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), map[string][]string{"A": {"x"}}, nil},
		"a name of WHERE beside a name of ORDER BY that the source carries": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(dal.NewFieldRef("", "nope"), dal.Equal, srOne)).OrderBy(dal.Ascending(scX)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
		}), map[string][]string{"A": {"x"}}, []string{"A"}},
		"a query that aggregates, which DALgo checks itself": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(dal.NewFieldRef("a", "x")).OrderBy(dal.Ascending(dal.NewFieldRef("", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")}, dal.CountAs(dal.Star(), "n"))
		}), map[string][]string{"A": {"x"}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: tc.lists}
			if err := checkScopes(context.Background(), tc.query, supplier.fields); err != nil {
				t.Fatalf("checkScopes: %v", err)
			}
			if !reflect.DeepEqual(supplier.asked, tc.asked) {
				t.Fatalf("asked %v, want %v", supplier.asked, tc.asked)
			}
		})
	}
}

func TestAnOrderByNameTwoSourcesCarryStaysAmbiguousAndALongUnknownNameIsClipped(t *testing.T) {
	q := scJoin(func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.OrderBy(dal.Ascending(scX)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
	})
	scope := scAsScope(t, checkScopes(context.Background(), q, srSupplied().fields))
	if scope.Path != "orderBy[0]" || scope.Message != "ambiguous unqualified field x" {
		t.Fatalf("got %+v", scope)
	}

	long := strings.Repeat("f", 300)
	q = srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
		return b.OrderBy(dal.Ascending(dal.NewFieldRef("", long))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "x")})
	})
	scope = scAsScope2(t, checkScopes(context.Background(), q, srSupplied().fields), "shape")
	if strings.Contains(scope.Message, long) || !strings.Contains(scope.Message, strings.Repeat("f", maxEchoLen)+"...") {
		t.Fatalf("message = %q", scope.Message)
	}
}

// scAsScope2 is scAsScope for a refusal of another category.
func scAsScope2(t *testing.T, err error, category string) *dal.QueryValidationError {
	t.Helper()
	var refused *dal.QueryValidationError
	if !errors.As(err, &refused) || refused.Category != category {
		t.Fatalf("want a %s error, got %T: %v", category, err, err)
	}
	return refused
}

// Through Execute, on a mount that supplies its lists and is handed the document whole
// (one source that names its database): the mount sorts by the field the alias stands
// for, receives no name it does not know, and is never read for a name nobody carries.
func TestADocumentHandedWholeToAMountIsSortedByTheFieldOfAnAliasOrRefused(t *testing.T) {
	handed := func(build func(b dal.IQueryBuilder) dal.StructuredQuery) dal.StructuredQuery {
		return build(dal.From(exRef("one", "A", "a")).NewQuery())
	}
	run := func(q dal.StructuredQuery) (Result, *exSource, error) {
		mount := alMount()
		res, err := exRun(t, q, "", newExRegistry(mount), exAllow, Limits{})
		return res, mount, err
	}

	t.Run("the mount receives the field and not the alias", func(t *testing.T) {
		_, mount, err := run(handed(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Descending(dal.NewFieldRef("", "label"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name"), Alias: "label"})
		}))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		seen := mount.exec.seen()
		if len(seen) != 1 {
			t.Fatalf("the mount was read %d times, want once", len(seen))
		}
		order := seen[0].(dal.StructuredQuery).OrderBy()
		field, ok := order[0].Expression().(dal.FieldRef)
		if len(order) != 1 || !ok || field.Source() != "a" || field.Name() != "name" || !order[0].Descending() {
			t.Fatalf("the mount was asked to order by %v", order)
		}
	})
	t.Run("an ordering that is not a field is sorted by DALgo: the mount is asked for a plain read", func(t *testing.T) {
		// A mount's executor skips an ordering that is not a field, and answers in the order
		// it reads the records: the document is not handed to it whole.
		for name, tc := range map[string]struct {
			order []dal.OrderExpression
			want  []any
		}{
			"arithmetic": {[]dal.OrderExpression{dal.Ascending(dal.Binary(dal.NewFieldRef("a", "k"), dal.Multiply, dal.NewConstant(-1)))}, []any{"a3", "a2", "a1"}},
			"arithmetic after a plain field": {[]dal.OrderExpression{dal.Ascending(dal.NewFieldRef("a", "name")),
				dal.Ascending(dal.Binary(dal.NewFieldRef("", "k"), dal.Multiply, dal.NewConstant(-1)))}, []any{"a1", "a2", "a3"}},
		} {
			res, mount, err := run(handed(func(b dal.IQueryBuilder) dal.StructuredQuery {
				return b.OrderBy(tc.order...).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name")})
			}))
			if err != nil {
				t.Fatalf("%s: Execute: %v", name, err)
			}
			seen := mount.exec.seen()
			if len(seen) != 1 || len(seen[0].(dal.StructuredQuery).OrderBy()) != 0 {
				t.Fatalf("%s: the mount was asked %v, want one read with no ordering", name, seen)
			}
			var names []any
			for _, rec := range res.Records {
				names = append(names, rec.Data().(map[string]any)["name"])
			}
			if !reflect.DeepEqual(names, tc.want) {
				t.Fatalf("%s: names = %v, want %v", name, names, tc.want)
			}
		}
	})
	t.Run("a name no source carries is refused and nothing is read", func(t *testing.T) {
		_, mount, err := run(handed(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("", "nope"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name")})
		}))
		scope := scAsScope2(t, err, "shape")
		if scope.Path != "orderBy[0]" || len(mount.exec.seen()) != 0 {
			t.Fatalf("got %+v, and the mount was read %d times", scope, len(mount.exec.seen()))
		}
	})
	t.Run("the alias of an expression is refused and nothing is read", func(t *testing.T) {
		_, mount, err := run(handed(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.OrderBy(dal.Ascending(dal.NewFieldRef("", "label"))).
				SelectColumns(dal.Column{Expression: dal.Binary(dal.NewFieldRef("a", "k"), dal.Multiply, dal.NewConstant(-1)), Alias: "label"})
		}))
		scope := scAsScope(t, err)
		if scope.Path != "orderBy[0]" || len(mount.exec.seen()) != 0 {
			t.Fatalf("got %+v, and the mount was read %d times", scope, len(mount.exec.seen()))
		}
	})
}

// On the evaluation of DALgo, which sorts the joined rows itself, the alias of a field
// sorts by that field: the name used to be refused as an unavailable field.
func TestAnAliasOfAFieldSortsTheRowsDALgoOrders(t *testing.T) {
	anyRow := dal.NewExistsCondition(exPlain("", "A"))
	label := dal.NewFieldRef("", "label")
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		want  []map[string]any
	}{
		"a qualified field": {dal.From(exRef("", "A", "a")).NewQuery().Where(anyRow).OrderBy(dal.Descending(label)).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name"), Alias: "label"}),
			[]map[string]any{{"label": "a3"}, {"label": "a2"}, {"label": "a1"}}},
		"an unqualified field": {dal.From(exRef("", "A", "a")).NewQuery().Where(anyRow).OrderBy(dal.Descending(label)).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "name"), Alias: "label"}),
			[]map[string]any{{"label": "a3"}, {"label": "a2"}, {"label": "a1"}}},
		"in a derived source, with a limit that shows the order": {dal.From(dal.NewQuerySource(
			dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Descending(label)).Limit(2).SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name"), Alias: "label"}), "d")).NewQuery().
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "label")}),
			[]map[string]any{{"label": "a3"}, {"label": "a2"}}},
		"beside another ordering": {dal.From(exRef("", "A", "a")).NewQuery().Where(anyRow).OrderBy(dal.Ascending(dal.NewFieldRef("", "same")), dal.Descending(label)).
			SelectColumns(dal.Column{Expression: srOne, Alias: "same"}, dal.Column{Expression: dal.NewFieldRef("a", "name"), Alias: "label"}),
			nil},
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := alRun(t, tc.query)
			if tc.want == nil {
				// "same" is the alias of a constant: refused, with the way out.
				scope := scAsScope(t, err)
				if scope.Path != "orderBy[0]" || !strings.Contains(scope.Message, "same") {
					t.Fatalf("got %v, %+v", rows, scope)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !reflect.DeepEqual(rows, tc.want) {
				t.Fatalf("rows = %v, want %v", rows, tc.want)
			}
		})
	}
}

// resolveAliases replaces the name of an ORDER BY that a column carries as the alias of a
// field, in a query that does not aggregate, wherever the query stands; every other query
// reaches DALgo as it was built.
func TestTheOrderByAliasOfAFieldIsReplacedByTheFieldWhereverTheQueryStands(t *testing.T) {
	label := dal.NewFieldRef("", "label")
	name := dal.NewFieldRef("a", "name")
	labelled := dal.Column{Expression: name, Alias: "label"}
	inner := func() dal.StructuredQuery {
		return dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Descending(label)).SelectColumns(labelled)
	}
	orderOf := func(q dal.StructuredQuery) string {
		order := q.OrderBy()
		if len(order) != 1 {
			return "no order"
		}
		direction := "asc"
		if order[0].Descending() {
			direction = "desc"
		}
		return order[0].Expression().String() + " " + direction
	}
	t.Run("the query itself", func(t *testing.T) {
		if got := orderOf(resolveAliases(inner())); got != "a.name desc" {
			t.Fatalf("orders by %s", got)
		}
	})
	t.Run("a derived source, an EXISTS test and a scalar subquery", func(t *testing.T) {
		derived := dal.From(dal.NewQuerySource(inner(), "d")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "label")})
		got := resolveAliases(derived)
		source, ok := got.From().Base().(dal.QuerySource)
		if !ok || orderOf(source.Query()) != "a.name desc" {
			t.Fatalf("the derived source is %T: %v", got.From().Base(), got)
		}
		exists := dal.From(exRef("", "A", "o")).NewQuery().Where(dal.NewExistsCondition(inner())).SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "name")})
		condition, ok := resolveAliases(exists).Where().(dal.ExistsCondition)
		if !ok || orderOf(condition.Query()) != "a.name desc" {
			t.Fatalf("the EXISTS test is %T", resolveAliases(exists).Where())
		}
		scalar := dal.From(exRef("", "A", "o")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewQueryExpression(inner(), "s")})
		expression, ok := resolveAliases(scalar).Columns()[0].Expression.(dal.QueryExpression)
		if !ok || orderOf(expression.Query()) != "a.name desc" {
			t.Fatalf("the scalar subquery is %T", resolveAliases(scalar).Columns()[0].Expression)
		}
	})
	t.Run("what is not replaced reaches DALgo as it was built", func(t *testing.T) {
		for name, q := range map[string]dal.StructuredQuery{
			"a qualified name":              dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("a", "label"))).SelectColumns(labelled),
			"a name that no column carries": dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("", "k"))).SelectColumns(labelled),
			// A SQL database reads the alias only as the whole of the expression: inside
			// arithmetic the name is the field of the source that has it.
			"arithmetic over the alias":  dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Ascending(dal.Binary(label, dal.Add, srOne))).SelectColumns(labelled),
			"the alias of an expression": dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Ascending(label)).SelectColumns(dal.Column{Expression: srOne, Alias: "label"}),
			"a column without an alias":  dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("", "name"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("", "name")}),
			"a wildcard":                 dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Ascending(label)).SelectColumns(dal.Column{Wildcard: &dal.WildcardProjection{Source: "a"}}),
		} {
			if got := resolveAliases(q); !reflect.DeepEqual(got, q) {
				t.Fatalf("%s: the query was rebuilt: %v", name, got)
			}
		}
	})
}
