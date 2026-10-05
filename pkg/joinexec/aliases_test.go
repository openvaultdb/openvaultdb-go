package joinexec

import (
	"context"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
)

// The helpers of this file start with "al" so that they cannot clash with the
// helpers of the tests around them.

// alMount is a mount of the document engine whose collection A holds three rows
// (id, k and name, each the same number: a1, a2, a3), with the field lists a strict
// manifest supplies. DALgo checks every field of a query against such a list.
func alMount() *exSource {
	mount := exMount("one", "ingitdb", false, map[string][]record.Record{"A": exRows("A", "a", 3)})
	mount.exec.fields = []string{"id", "k", "name"}
	return mount
}

var (
	alName  = dal.NewFieldRef("a", "name")
	alK     = dal.NewFieldRef("a", "k")
	alTotal = dal.NewFieldRef("", "total")
	alSum   = dal.NewAggregate("SUM", false, alK)
)

// alPerName is a query that aggregates: the sum of k of each name, ordered by the
// alias of that sum. The extra builds more of it.
func alPerName(extra func(b dal.IQueryBuilder) dal.IQueryBuilder) dal.StructuredQuery {
	b := dal.From(exRef("", "A", "a")).NewQuery().GroupBy(alName)
	if extra != nil {
		b = extra(b)
	}
	return b.SelectColumns(dal.Column{Expression: alName}, dal.Column{Expression: alSum, Alias: "total"})
}

// alOrderedPerName orders alPerName by its alias, largest first.
func alOrderedPerName() dal.StructuredQuery {
	return alPerName(func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.OrderBy(dal.Descending(alTotal)) })
}

// alScalar is a scalar query over the same rows: the sum of all of k, which an
// ORDER BY names by alias. Its value is 6.
func alScalar() dal.StructuredQuery {
	return dal.From(exRef("", "A", "x")).NewQuery().OrderBy(dal.Ascending(alTotal)).
		SelectColumns(dal.Column{Expression: dal.NewAggregate("SUM", false, dal.NewFieldRef("x", "k")), Alias: "total"})
}

// alRun executes q on the mount with the lists supplied, as a document that names no
// database runs.
func alRun(t *testing.T, q dal.StructuredQuery) ([]map[string]any, error) {
	t.Helper()
	res, err := exRun(t, q, "one", newExRegistry(alMount()), exAllow, Limits{})
	if err != nil {
		return nil, err
	}
	// The rows in the order the answer has them, which is what an ORDER BY is for, and
	// every number as a float.
	rows := make([]map[string]any, len(res.Records))
	for i, rec := range res.Records {
		rows[i] = map[string]any{}
		for key, value := range rec.Data().(map[string]any) {
			switch n := value.(type) {
			case int:
				value = float64(n)
			case int64:
				value = float64(n)
			}
			rows[i][key] = value
		}
	}
	return rows, nil
}

// A query that aggregates names an aggregate column by its alias in HAVING and ORDER
// BY, wherever the query stands in a document: DALgo's aggregation reads the alias, and
// its join evaluation, which checks every field against the lists the mounts supply,
// does not know it, so the alias is replaced by the column before DALgo sees the query.
func TestTheAliasOfAnAggregateColumnIsNamedInOrderByAndHavingWhereverTheQueryStands(t *testing.T) {
	d := func(alias string) dal.QuerySource { return dal.NewQuerySource(alOrderedPerName(), alias) }
	byName := func(alias string) dal.Condition {
		return dal.NewComparison(alName, dal.Equal, dal.NewFieldRef(alias, "name"))
	}
	threeRows := []map[string]any{{"name": "a3", "total": 3.0}, {"name": "a2", "total": 2.0}, {"name": "a1", "total": 1.0}}
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		want  []map[string]any
	}{
		"the query itself, ORDER BY": {alOrderedPerName(), threeRows},
		"the query itself, HAVING": {alPerName(func(b dal.IQueryBuilder) dal.IQueryBuilder {
			return b.Having(dal.NewComparison(alTotal, dal.GreaterThen, dal.NewConstant(1))).OrderBy(dal.Descending(alName))
		}), []map[string]any{{"name": "a3", "total": 3.0}, {"name": "a2", "total": 2.0}}},
		"HAVING with a group of conditions and an arithmetic expression of the alias": {alPerName(func(b dal.IQueryBuilder) dal.IQueryBuilder {
			return b.Having(dal.NewGroupCondition(dal.And,
				dal.NewComparison(dal.Binary(alTotal, dal.Multiply, dal.NewConstant(10)), dal.GreaterThen, dal.NewConstant(10)),
				dal.NewIsNotNullCondition(alTotal),
				dal.NewComparison(alSum, dal.LessThen, dal.NewConstant(3)))).OrderBy(dal.Descending(alName))
		}), []map[string]any{{"name": "a2", "total": 2.0}}},
		"HAVING a null test of the alias": {alPerName(func(b dal.IQueryBuilder) dal.IQueryBuilder {
			return b.Having(dal.NewIsNullCondition(alTotal))
		}), []map[string]any{}},
		"ORDER BY an arithmetic expression of the alias": {alPerName(func(b dal.IQueryBuilder) dal.IQueryBuilder {
			return b.OrderBy(dal.Ascending(dal.Binary(alTotal, dal.Multiply, dal.NewConstant(-1))))
		}), threeRows},
		"a derived source in the base position": {
			dal.From(d("d")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "name")}, dal.Column{Expression: dal.NewFieldRef("d", "total")}), threeRows},
		"a derived source joined to a collection": {
			dal.From(exRef("", "A", "a")).Join(dal.NewJoinedSource(d("d"), dal.JoinInner, byName("d"))).NewQuery().
				OrderBy(dal.Descending(dal.NewFieldRef("a", "k"))).
				SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name"), Alias: "n"}, dal.Column{Expression: dal.NewFieldRef("d", "total"), Alias: "t"}),
			[]map[string]any{{"n": "a3", "t": 3.0}, {"n": "a2", "t": 2.0}, {"n": "a1", "t": 1.0}}},
		"a derived source in a join tree, with a hint": {
			dal.From(exRef("", "A", "a")).Join(dal.NewJoinedFrom(dal.From(d("d")), dal.JoinLeft, byName("d")).WithAlgorithms(dal.JoinAlgorithmNestedLoop)).NewQuery().
				OrderBy(dal.Descending(dal.NewFieldRef("a", "k"))).
				SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name"), Alias: "n"}, dal.Column{Expression: dal.NewFieldRef("d", "total"), Alias: "t"}),
			[]map[string]any{{"n": "a3", "t": 3.0}, {"n": "a2", "t": 2.0}, {"n": "a1", "t": 1.0}}},
		"an EXISTS test": {
			dal.From(exRef("", "A", "o")).NewQuery().Where(dal.NewExistsCondition(alOrderedPerName())).
				OrderBy(dal.Ascending(dal.NewFieldRef("o", "k"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "name")}),
			[]map[string]any{{"name": "a1"}, {"name": "a2"}, {"name": "a3"}}},
		"a NOT EXISTS test in a group of conditions": {
			dal.From(exRef("", "A", "o")).NewQuery().
				Where(dal.NewGroupCondition(dal.Or, dal.NewNotExistsCondition(alOrderedPerName()), dal.NewComparison(dal.NewFieldRef("o", "k"), dal.Equal, dal.NewConstant(2)))).
				SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "name")}),
			[]map[string]any{{"name": "a2"}}},
		"a scalar subquery as a column, in arithmetic": {
			dal.From(exRef("", "A", "o")).NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("o", "k"))).
				SelectColumns(dal.Column{Expression: dal.Binary(dal.NewFieldRef("o", "k"), dal.Add, dal.NewQueryExpression(alScalar(), "sum")), Alias: "shifted"}),
			[]map[string]any{{"shifted": 7.0}, {"shifted": 8.0}, {"shifted": 9.0}}},
		"a scalar subquery as the operand of a comparison": {
			dal.From(exRef("", "A", "o")).NewQuery().Where(dal.NewComparison(dal.NewFieldRef("o", "k"), dal.LessThen, dal.NewQueryExpression(alScalar(), "sum"))).
				OrderBy(dal.Ascending(dal.NewFieldRef("o", "k"))).SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "name")}),
			[]map[string]any{{"name": "a1"}, {"name": "a2"}, {"name": "a3"}}},
		"a scalar subquery as the operand of a null test": {
			dal.From(exRef("", "A", "o")).NewQuery().
				Where(dal.NewGroupCondition(dal.And, dal.NewIsNotNullCondition(dal.NewQueryExpression(alScalar(), "sum")), dal.NewIsNullCondition(dal.NewQueryExpression(alScalar(), "sum")))).
				SelectColumns(dal.Column{Expression: dal.NewFieldRef("o", "name")}),
			[]map[string]any{}},
		"an EXISTS test in an ON condition": {
			dal.From(exRef("", "A", "a")).Join(dal.NewJoinedSource(exRef("", "A", "b"), dal.JoinInner, exKeyEquals(exRef("", "A", "a"), exRef("", "A", "b")), dal.NewExistsCondition(alOrderedPerName()))).NewQuery().
				OrderBy(dal.Ascending(dal.NewFieldRef("a", "k"))).
				SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "name"), Alias: "n"}),
			[]map[string]any{{"n": "a1"}, {"n": "a2"}, {"n": "a3"}}},
		"a derived source inside a derived source": {
			dal.From(dal.NewQuerySource(dal.From(d("d")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "name")}, dal.Column{Expression: dal.NewFieldRef("d", "total")}), "e")).NewQuery().
				SelectColumns(dal.Column{Expression: dal.NewFieldRef("e", "name")}, dal.Column{Expression: dal.NewFieldRef("e", "total")}),
			threeRows},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := alRun(t, tc.query)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
		})
	}
}

// A document that names no alias in HAVING and ORDER BY reaches DALgo as it is built,
// and so does one whose names are fields of a source.
func TestADocumentThatNamesNoAliasIsNotRebuilt(t *testing.T) {
	other := dal.NewFieldRef("", "k")
	for name, q := range map[string]dal.StructuredQuery{
		"a plain document":                     exPlain("", "A"),
		"a qualified name":                     alPerName(func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.OrderBy(dal.Ascending(alName)) }),
		"an unqualified name that is no alias": alPerName(func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.OrderBy(dal.Ascending(other)) }),
		"an alias in the argument of an aggregate": alPerName(func(b dal.IQueryBuilder) dal.IQueryBuilder {
			return b.OrderBy(dal.Ascending(dal.NewAggregate("MAX", false, alTotal)))
		}),
		"an alias of a query that does not aggregate": dal.From(exRef("", "A", "a")).NewQuery().OrderBy(dal.Ascending(alTotal)).
			SelectColumns(dal.Column{Expression: alK, Alias: "total"}),
		"a column that has no alias and a wildcard": dal.From(exRef("", "A", "a")).NewQuery().GroupBy(alName).OrderBy(dal.Ascending(alName)).
			SelectColumns(dal.Column{Expression: alName}, dal.Column{Wildcard: &dal.WildcardProjection{Source: "a"}}, dal.CountAs(dal.Star(), "n")),
		"nested queries that name none": dal.From(dal.NewQuerySource(exPlain("", "A"), "d")).NewQuery().
			Where(dal.NewGroupCondition(dal.And, dal.NewExistsCondition(exPlain("", "A")), dal.NewIsNullCondition(dal.NewQueryExpression(exPlain("", "A"), "s")),
				dal.NewComparison(dal.Binary(alK, dal.Add, dal.NewConstant(1)), dal.Equal, dal.NewQueryExpression(exPlain("", "A"), "s")))).
			SelectColumns(dal.Column{Expression: dal.NewAggregate("SUM", false, dal.NewQueryExpression(exPlain("", "A"), "s")), Alias: "n"}),
	} {
		t.Run(name, func(t *testing.T) {
			if got := resolveAliases(q); !reflect.DeepEqual(got, q) {
				t.Fatalf("the query was rebuilt: %T", got)
			}
		})
	}
}

// The query DALgo is given keeps everything about the query that was written but the
// names it resolved: the sources and the hints of their joins, the conditions, the
// grouping, the paging, and what a reader of it prints.
func TestTheQueryWithItsAliasesResolvedKeepsEverythingElse(t *testing.T) {
	a, b := exRef("", "A", "a"), exRef("", "A", "b")
	ordered := alOrderedPerName()
	original := dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinLeft, exKeyEquals(a, b)).WithAlgorithms(dal.JoinAlgorithmHash, dal.JoinAlgorithmNestedLoop)).
		Join(dal.NewJoinedSource(dal.NewQuerySource(ordered, "d"), dal.JoinInner, dal.NewComparison(dal.NewFieldRef("a", "name"), dal.Equal, dal.NewFieldRef("d", "name")))).
		NewQuery().
		Where(dal.NewComparison(alK, dal.GreaterThen, dal.NewConstant(0))).
		GroupBy(alName).
		Having(dal.NewComparison(alTotal, dal.GreaterThen, dal.NewConstant(0))).
		OrderBy(dal.Descending(alTotal)).
		Offset(1).Limit(2).
		SelectColumns(dal.Column{Expression: alName}, dal.Column{Expression: alSum, Alias: "total"})

	got := resolveAliases(original)
	if reflect.DeepEqual(got, original) {
		t.Fatal("the query was not rebuilt")
	}
	if got.Offset() != 1 || got.Limit() != 2 || !reflect.DeepEqual(got.GroupBy(), original.GroupBy()) || !reflect.DeepEqual(got.Columns(), original.Columns()) || !reflect.DeepEqual(got.Where(), original.Where()) {
		t.Fatalf("the rebuilt query changed what it had no alias in: %v", got)
	}
	// The alias is the aggregate, in HAVING and ORDER BY; the descending order stays.
	if comparison, ok := got.Having().(dal.Comparison); !ok || comparison.Left.String() != alSum.String() {
		t.Fatalf("having = %v", got.Having())
	}
	if order := got.OrderBy(); len(order) != 1 || order[0].Expression().String() != alSum.String() || !order[0].Descending() {
		t.Fatalf("orderBy = %v", order)
	}
	// The joins keep their order, their types, their hints and their conditions.
	joins := got.From().Joins()
	if len(joins) != 2 || joins[0].JoinType() != dal.JoinLeft || !reflect.DeepEqual(joins[0].Algorithms(), []dal.JoinAlgorithm{dal.JoinAlgorithmHash, dal.JoinAlgorithmNestedLoop}) ||
		joins[1].JoinType() != dal.JoinInner || joins[1].Algorithms() != nil || len(joins[0].On()) != 1 || len(joins[1].On()) != 1 {
		t.Fatalf("joins = %+v", joins)
	}
	// The derived source is the one that was written, with the alias of its own order resolved.
	derived, ok := joins[1].RecordsetSource.(dal.QuerySource)
	if !ok || derived.Alias() != "d" {
		t.Fatalf("the derived source is %T", joins[1].RecordsetSource)
	}
	if inner := derived.Query().OrderBy(); len(inner) != 1 || inner[0].Expression().String() != alSum.String() {
		t.Fatalf("the derived source orders by %v", inner)
	}
	// What a reader of the query prints is the query it holds, and a reader that asks
	// the query to run it hands an executor the rebuilt query, not the original.
	if got.String() != dal.QueryString(got) || got.String() == original.String() {
		t.Fatalf("String = %q", got.String())
	}
	probe := &alProbe{}
	if _, err := got.GetRecordsReader(context.Background(), probe); err != nil || !reflect.DeepEqual(probe.records, got) {
		t.Fatalf("GetRecordsReader handed %v, %v", probe.records, err)
	}
	if _, err := got.GetRecordsetReader(context.Background(), probe); err != nil || !reflect.DeepEqual(probe.recordset, got) {
		t.Fatalf("GetRecordsetReader handed %v, %v", probe.recordset, err)
	}
}

// alProbe is an executor that remembers the query it was asked to run.
type alProbe struct {
	records, recordset dal.Query
}

func (p *alProbe) ExecuteQueryToRecordsReader(_ context.Context, query dal.Query) (dal.RecordsReader, error) {
	p.records = query
	return nil, nil
}

func (p *alProbe) ExecuteQueryToRecordsetReader(_ context.Context, query dal.Query, _ ...recordset.Option) (dal.RecordsetReader, error) {
	p.recordset = query
	return nil, nil
}
