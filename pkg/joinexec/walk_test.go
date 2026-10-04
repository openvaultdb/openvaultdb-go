package joinexec

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// Shapes no DTQL document can produce. The walk must refuse them rather than
// pass them through: it is the check that every source a document reads is one
// that was authorised.
type exUnknownExpression struct{}

func (exUnknownExpression) String() string { return "unknown" }

type exUnknownCondition struct{}

func (exUnknownCondition) String() string { return "unknown" }

type exUnknownSource struct{ dal.RecordsetSource }

// exShapeQuery overrides the members a hand-built query cannot otherwise set.
type exShapeQuery struct {
	dal.StructuredQuery
	from    dal.FromSource
	hasFrom bool
	orderBy []dal.OrderExpression
	groupBy []dal.Expression
	having  dal.Condition
}

func (q exShapeQuery) From() dal.FromSource {
	if q.hasFrom {
		return q.from
	}
	return q.StructuredQuery.From()
}
func (q exShapeQuery) OrderBy() []dal.OrderExpression { return q.orderBy }
func (q exShapeQuery) GroupBy() []dal.Expression      { return q.groupBy }
func (q exShapeQuery) Having() dal.Condition          { return q.having }

type exNilBase struct{ dal.FromSource }

func (exNilBase) Base() dal.RecordsetSource { return nil }

// exCyclic is a query whose scalar subquery is the query itself.
type exCyclic struct {
	dal.StructuredQuery
	columns []dal.Column
}

func (q *exCyclic) Columns() []dal.Column { return q.columns }

func exCyclicQuery() dal.StructuredQuery {
	q := &exCyclic{StructuredQuery: exPlain("", "a")}
	q.columns = []dal.Column{{Expression: dal.NewQueryExpression(q, "again")}}
	return q
}

func exBase() dal.StructuredQuery { return exPlain("", "a") }

func exWithColumn(expr dal.Expression) dal.StructuredQuery {
	return dal.WithColumns(exBase(), []dal.Column{{Expression: expr}})
}

func exWithWhere(cond dal.Condition) dal.StructuredQuery {
	return dal.WithWhere(exBase(), cond)
}

func TestInspectListsEverySourceInDocumentOrder(t *testing.T) {
	a := exRef("one", "A", "a")
	b := exRef("two", "B", "b")
	c := exRef("two", "C", "c")
	derived := dal.NewQuerySource(exPlain("three", "D"), "d")
	nested := dal.From(b)
	nested.Join(dal.NewJoinedSource(c, dal.JoinLeft, exKeyEquals(b, c)))
	from := dal.From(a)
	from.Join(dal.NewJoinedFrom(nested, dal.JoinInner, exKeyEquals(a, b)))
	from.Join(dal.NewJoinedSource(derived, dal.JoinInner, dal.NewComparison(dal.NewFieldRef("a", "k"), dal.Equal, dal.NewFieldRef("d", "k"))))
	q := from.NewQuery().
		Where(dal.NewExistsCondition(exPlain("four", "E"))).
		SelectColumns(
			dal.Column{Expression: dal.NewFieldRef("a", "id"), Alias: "aid"},
			dal.Column{Expression: dal.NewQueryExpression(exPlain("five", "F"), "f")},
		)
	doc, err := inspect(q)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	want := []walkedSource{
		{database: "one", collection: "A"},
		{database: "two", collection: "B"},
		{database: "two", collection: "C"},
		{database: "three", collection: "D"},
		{database: "four", collection: "E"},
		{database: "five", collection: "F"},
	}
	if !reflect.DeepEqual(doc.sources, want) {
		t.Fatalf("sources = %+v, want %+v", doc.sources, want)
	}
	if !doc.hasSubquery {
		t.Fatal("a derived source, an EXISTS and a scalar subquery must set hasSubquery")
	}
}

func TestInspectFindsScanBoundsAndSubqueriesNowhereElse(t *testing.T) {
	plain, err := inspect(exJoin(exRef("one", "A", "a"), exRef("two", "B", "b"), true))
	if err != nil || plain.hasSubquery || plain.sources[0].scan || plain.sources[1].scan {
		t.Fatalf("a plain join: %+v, %v", plain, err)
	}
	scanned := dal.NewDatabaseCollectionRef("one", "", "A", "a").WithScan(5, dal.AscendingField("id"))
	limited := dal.NewDatabaseCollectionRef("one", "", "A", "a").WithScan(5)
	ordered := dal.NewDatabaseCollectionRef("one", "", "A", "a").WithScan(0, dal.AscendingField("id"))
	for name, ref := range map[string]dal.CollectionRef{"limit and order": scanned, "limit": limited, "order": ordered} {
		doc, err := inspect(dal.From(ref).NewQuery().SelectIntoRecord(nil))
		if err != nil || len(doc.sources) != 1 || !doc.sources[0].scan {
			t.Fatalf("%s: %+v, %v", name, doc, err)
		}
	}
}

func TestInspectRefusesShapesNoDocumentProduces(t *testing.T) {
	field := dal.NewFieldRef("", "id")
	parent := record.NewKeyWithID("p", "1")
	joined := func(src dal.RecordsetSource) dal.StructuredQuery {
		from := dal.From(exRef("", "a", ""))
		from.Join(dal.NewJoinedSource(src, dal.JoinInner, dal.NewComparison(field, dal.Equal, field)))
		return from.NewQuery().SelectIntoRecord(nil)
	}
	pointerDerived := dal.NewQuerySource(exPlain("", "b"), "d")
	pointerRef := exRef("", "a", "")
	for _, tc := range []struct {
		name  string
		query dal.StructuredQuery
		want  string
	}{
		{"nil query", nil, "a query is required"},
		{"nil from", exShapeQuery{StructuredQuery: exBase(), hasFrom: true}, "a source is required"},
		{"nil base", exShapeQuery{StructuredQuery: exBase(), hasFrom: true, from: exNilBase{dal.From(exRef("", "a", ""))}}, "a source is required"},
		{"nested collection", dal.From(dal.NewCollectionRef("kids", "", parent)).NewQuery().SelectIntoRecord(nil), "only plain root collections"},
		{"schema-qualified collection", dal.From(dal.NewQualifiedRootCollectionRef("main", "a", "")).NewQuery().SelectIntoRecord(nil), "only plain root collections"},
		{"collection group", dal.From(dal.NewCollectionGroupRef("g", "")).NewQuery().SelectIntoRecord(nil), "unsupported source"},
		{"unknown source", dal.From(exUnknownSource{exRef("", "x", "")}).NewQuery().SelectIntoRecord(nil), "unsupported source"},
		{"pointer to a collection", dal.From(&pointerRef).NewQuery().SelectIntoRecord(nil), "unsupported source"},
		{"pointer to a derived source", dal.From(&pointerDerived).NewQuery().SelectIntoRecord(nil), "unsupported source"},
		{"joined collection group", joined(dal.NewCollectionGroupRef("g", "")), "unsupported source"},
		{"join without a source", func() dal.StructuredQuery {
			from := dal.From(exRef("", "a", ""))
			from.Join(dal.NewNestedJoinedSource(nil, dal.JoinInner))
			return from.NewQuery().SelectIntoRecord(nil)
		}(), "a join needs a source"},
		{"unknown column expression", exWithColumn(exUnknownExpression{}), "unsupported expression"},
		{"column without an expression", dal.WithColumns(exBase(), []dal.Column{{}}), "a column needs an expression"},
		{"pointer constant", exWithColumn(&dal.Constant{Value: 1}), "unsupported expression"},
		{"unknown where condition", exWithWhere(exUnknownCondition{}), "unsupported condition"},
		{"pointer comparison", exWithWhere(&dal.Comparison{Operator: dal.Equal, Left: field, Right: field}), "unsupported condition"},
		{"nil operand", exWithWhere(dal.NewComparison(nil, dal.Equal, field)), "unsupported expression"},
		{"nil condition in a group", exWithWhere(dal.NewGroupCondition(dal.And, nil)), "a condition is required"},
		{"unknown condition in an or group", exWithWhere(dal.NewGroupCondition(dal.Or, dal.NewComparison(field, dal.Equal, field), exUnknownCondition{})), "unsupported condition"},
		{"nil is-null operand", exWithWhere(dal.NewIsNullCondition(nil)), "unsupported expression"},
		{"nil exists query", exWithWhere(dal.NewExistsCondition(nil)), "a query is required"},
		{"unknown binary left", exWithColumn(dal.Binary(exUnknownExpression{}, dal.Add, field)), "unsupported expression"},
		{"unknown binary right", exWithColumn(dal.Binary(field, dal.Add, exUnknownExpression{})), "unsupported expression"},
		{"unknown aggregate argument", exWithColumn(dal.NewAggregate("sum", false, field, exUnknownExpression{})), "unsupported expression"},
		{"nil scalar subquery", exWithColumn(dal.NewQueryExpression(nil, "q")), "a query is required"},
		{"unknown group by", exShapeQuery{StructuredQuery: exBase(), groupBy: []dal.Expression{exUnknownExpression{}}}, "unsupported expression"},
		{"unknown order by", exShapeQuery{StructuredQuery: exBase(), orderBy: []dal.OrderExpression{dal.Ascending(exUnknownExpression{})}}, "unsupported expression"},
		{"nil order by entry", exShapeQuery{StructuredQuery: exBase(), orderBy: []dal.OrderExpression{nil}}, "an ordering expression is required"},
		{"unknown having", exShapeQuery{StructuredQuery: exBase(), having: exUnknownCondition{}}, "unsupported condition"},
		{"unknown join condition", func() dal.StructuredQuery {
			from := dal.From(exRef("", "a", ""))
			from.Join(dal.NewJoinedSource(exRef("", "b", ""), dal.JoinInner, exUnknownCondition{}))
			return from.NewQuery().SelectIntoRecord(nil)
		}(), "unsupported condition"},
		{"nil derived query", dal.From(dal.NewQuerySource(nil, "d")).NewQuery().SelectIntoRecord(nil), "a query is required"},
		{"scan order that is not an expression", dal.From(exRef("", "a", "").WithScan(1, nil)).NewQuery().SelectIntoRecord(nil), "an ordering expression is required"},
		{"scan order with an unknown expression", dal.From(exRef("", "a", "").WithScan(1, dal.Ascending(exUnknownExpression{}))).NewQuery().SelectIntoRecord(nil), "unsupported expression"},
		{"cycle through scalar subqueries", exCyclicQuery(), "subqueries nest too deep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := inspect(tc.query)
			if !errors.Is(err, ErrInvalidDocument) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalidDocument containing %q", err, tc.want)
			}
			if !reflect.DeepEqual(doc, document{}) {
				t.Fatalf("a refused document returned a walk: %+v", doc)
			}
		})
	}
}

func TestInspectRefusesAJoinTreeNestedTooDeep(t *testing.T) {
	from := dal.From(exRef("", "leaf", ""))
	for i := 0; i <= maxWalkDepth; i++ {
		outer := dal.From(exRef("", "a", ""))
		outer.Join(dal.NewJoinedFrom(from, dal.JoinInner))
		from = outer
	}
	_, err := inspect(from.NewQuery().SelectIntoRecord(nil))
	if !errors.Is(err, ErrInvalidDocument) || !strings.Contains(err.Error(), "nest too deep") {
		t.Fatalf("err = %v", err)
	}
}

func TestInspectChecksEveryNameTheDocumentCarries(t *testing.T) {
	a := exRef("", "a", "a")
	for _, tc := range []struct {
		name  string
		query dal.StructuredQuery
		want  string
	}{
		{"field with a quote", exWithColumn(dal.NewFieldRef("", `id"; DROP TABLE a; --`)), "field name"},
		{"field with a space", exWithColumn(dal.NewFieldRef("", "first name")), "field name"},
		{"field with a comment marker", exWithColumn(dal.NewFieldRef("", "a--b")), "field name"},
		{"field that is too long", exWithColumn(dal.NewFieldRef("", strings.Repeat("x", maxNameLen+1))), "field name"},
		{"field in a condition", exWithWhere(dal.NewComparison(dal.NewFieldRef("", "x;y"), dal.Equal, dal.Constant{Value: 1})), "field name"},
		{"qualifier that is not an identifier", exWithColumn(dal.NewFieldRef("a b", "id")), "identifier"},
		{"qualifier that names nothing in scope", exWithColumn(dal.NewFieldRef("zz-top", "id")), "identifier"},
		{"column alias", dal.WithColumns(exBase(), []dal.Column{{Expression: dal.NewFieldRef("", "id"), Alias: "my alias"}}), "identifier"},
		{"source alias", dal.From(exRef("", "a", "bad-alias")).NewQuery().SelectIntoRecord(nil), "identifier"},
		{"derived source alias", dal.From(dal.NewQuerySource(exPlain("", "b"), "bad alias")).NewQuery().SelectIntoRecord(nil), "identifier"},
		{"wildcard source", dal.WithColumns(exBase(), []dal.Column{{Wildcard: &dal.WildcardProjection{Source: "x y"}}}), "identifier"},
		{"wildcard exclusion", dal.WithColumns(exBase(), []dal.Column{{Wildcard: &dal.WildcardProjection{Exclude: []string{"a b"}}}}), "field name"},
		{"parameter name", exWithWhere(dal.NewComparison(dal.NewFieldRef("", "id"), dal.Equal, dal.Param{Name: "bad name"})), "parameter name"},
		{"field in a scan order", dal.From(exRef("", "a", "").WithScan(1, dal.AscendingField("bad field"))).NewQuery().SelectIntoRecord(nil), "field name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inspect(tc.query)
			if !errors.Is(err, ErrInvalidDocument) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalidDocument containing %q", err, tc.want)
			}
		})
	}
	// The names a document may carry are accepted: nested and key fields, an
	// alias or collection as qualifier (even a collection whose name is not an
	// identifier), a wildcard with exclusions, parameters and constants.
	spaced := exRef("", "Order Details", "")
	ok := dal.From(a).Join(dal.NewJoinedSource(spaced, dal.JoinInner, dal.NewComparison(dal.NewFieldRef("a", "k"), dal.Equal, dal.NewFieldRef("Order Details", "k")))).NewQuery().
		Where(dal.NewGroupCondition(dal.And,
			dal.NewComparison(dal.NewFieldRef("a", "address.city"), dal.Equal, dal.Param{Name: "city"}),
			dal.NewIsNotNullCondition(dal.NewFieldRef("a", "x")),
		)).
		SelectColumns(
			dal.Column{Expression: dal.NewFieldRef("", "$id"), Alias: "key"},
			dal.Column{Wildcard: &dal.WildcardProjection{Source: "a", Exclude: []string{"secret"}}},
			dal.Column{Expression: dal.Binary(dal.NewFieldRef("a", "x"), dal.Add, dal.Constant{Value: 1}), Alias: "x1"},
			dal.Column{Expression: dal.NewAggregate("count", false, dal.Star()), Alias: "n"},
		)
	if _, err := inspect(ok); err != nil {
		t.Fatalf("inspect: %v", err)
	}
}

func TestInspectChecksNamesInsideSubqueries(t *testing.T) {
	bad := dal.From(exRef("", "b", "")).NewQuery().Where(dal.NewComparison(dal.NewFieldRef("", "no good"), dal.Equal, dal.Constant{Value: 1})).SelectIntoRecord(nil)
	for name, q := range map[string]dal.StructuredQuery{
		"derived source":  dal.From(dal.NewQuerySource(bad, "d")).NewQuery().SelectIntoRecord(nil),
		"exists":          exWithWhere(dal.NewExistsCondition(bad)),
		"scalar subquery": exWithColumn(dal.NewQueryExpression(bad, "s")),
	} {
		if _, err := inspect(q); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestCheckProfileComparesTheProfileToTheWalk(t *testing.T) {
	q := exJoin(exRef("one", "A", "a"), exRef("two", "B", "b"), true)
	doc, err := inspect(q)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	good := Profile{Sources: []ProfileSource{{"one", "A"}, {"two", "B"}}}
	if err := checkProfile(doc, good); err != nil {
		t.Fatalf("matching profile: %v", err)
	}
	reordered := Profile{Sources: []ProfileSource{{"two", "B"}, {"one", "A"}}}
	if err := checkProfile(doc, reordered); err != nil {
		t.Fatalf("the order of sources is not compared: %v", err)
	}
	for name, profile := range map[string]Profile{
		"a source missing":      {Sources: []ProfileSource{{"one", "A"}}},
		"a source added":        {Sources: []ProfileSource{{"one", "A"}, {"two", "B"}, {"two", "C"}}},
		"another collection":    {Sources: []ProfileSource{{"one", "A"}, {"two", "Z"}}},
		"another database":      {Sources: []ProfileSource{{"one", "A"}, {"zz", "B"}}},
		"a source listed twice": {Sources: []ProfileSource{{"one", "A"}, {"one", "A"}}},
		"no sources":            {},
		"a subquery claimed":    {Sources: good.Sources, HasSubquery: true},
	} {
		if err := checkProfile(doc, profile); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("%s: err = %v, want ErrInvalidDocument", name, err)
		}
	}
	// A profile that denies a subquery the document has is refused too.
	withSubquery, err := inspect(exWithWhere(dal.NewExistsCondition(exPlain("", "b"))))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	profile := Profile{Sources: []ProfileSource{{"", "a"}, {"", "b"}}}
	if err := checkProfile(withSubquery, profile); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("a hidden subquery: err = %v", err)
	}
	profile.HasSubquery = true
	if err := checkProfile(withSubquery, profile); err != nil {
		t.Fatalf("a declared subquery: %v", err)
	}
}
