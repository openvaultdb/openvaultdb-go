package joinexec

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

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

// exGroupChain wraps leaf in n group conditions, each inside the next.
func exGroupChain(n int, leaf dal.Condition) dal.Condition {
	for i := 0; i < n; i++ {
		leaf = dal.NewGroupCondition(dal.And, leaf)
	}
	return leaf
}

// exBinaryChain wraps leaf in n binary expressions, each inside the next.
func exBinaryChain(n int, leaf dal.Expression) dal.Expression {
	for i := 0; i < n; i++ {
		leaf = dal.Binary(leaf, dal.Add, dal.NewConstant(1))
	}
	return leaf
}

func exIDEqualsOne() dal.Condition {
	return dal.NewComparison(dal.NewFieldRef("", "id"), dal.Equal, dal.NewConstant(1))
}

// Conditions and expressions nest at most maxWalkNesting levels, counted the way
// the profile walk of pkg/core counts them (the drift test in pkg/core compares
// the two). A comparison is one level and each operand is one more, so a group
// chain of n levels over a comparison reaches n+2, and a binary chain of n levels
// over a field reaches n+1.
func TestInspectRefusesConditionsAndExpressionsNestedTooDeep(t *testing.T) {
	deepest := map[string]dal.StructuredQuery{
		"group conditions at the bound":      exWithWhere(exGroupChain(maxWalkNesting-2, exIDEqualsOne())),
		"binary expressions at the bound":    exWithColumn(exBinaryChain(maxWalkNesting-1, dal.NewFieldRef("", "id"))),
		"a join condition at the bound":      dal.From(exRef("", "a", "a")).Join(dal.NewJoinedSource(exRef("", "b", "b"), dal.JoinInner, exGroupChain(maxWalkNesting-2, exIDEqualsOne()))).NewQuery().SelectIntoRecord(nil),
		"a having condition at the bound":    exShapeQuery{StructuredQuery: exBase(), having: exGroupChain(maxWalkNesting-2, exIDEqualsOne())},
		"an order expression at the bound":   exShapeQuery{StructuredQuery: exBase(), orderBy: []dal.OrderExpression{dal.Ascending(exBinaryChain(maxWalkNesting-1, dal.NewFieldRef("", "id")))}},
		"a group by expression at the bound": exShapeQuery{StructuredQuery: exBase(), groupBy: []dal.Expression{exBinaryChain(maxWalkNesting-1, dal.NewFieldRef("", "id"))}},
		"a scan order at the bound":          dal.From(exRef("", "a", "").WithScan(1, dal.Ascending(exBinaryChain(maxWalkNesting-1, dal.NewFieldRef("", "id"))))).NewQuery().SelectIntoRecord(nil),
		"an aggregate argument at the bound": exWithColumn(dal.NewAggregate("sum", false, exBinaryChain(maxWalkNesting-2, dal.NewFieldRef("", "id")))),
	}
	for name, q := range deepest {
		t.Run("accepts "+name, func(t *testing.T) {
			if _, err := inspect(q); err != nil {
				t.Fatalf("inspect: %v", err)
			}
		})
	}
	tooDeep := map[string]dal.StructuredQuery{
		"group conditions":      exWithWhere(exGroupChain(maxWalkNesting-1, exIDEqualsOne())),
		"binary expressions":    exWithColumn(exBinaryChain(maxWalkNesting, dal.NewFieldRef("", "id"))),
		"a join condition":      dal.From(exRef("", "a", "a")).Join(dal.NewJoinedSource(exRef("", "b", "b"), dal.JoinInner, exGroupChain(maxWalkNesting-1, exIDEqualsOne()))).NewQuery().SelectIntoRecord(nil),
		"a having condition":    exShapeQuery{StructuredQuery: exBase(), having: exGroupChain(maxWalkNesting-1, exIDEqualsOne())},
		"an order expression":   exShapeQuery{StructuredQuery: exBase(), orderBy: []dal.OrderExpression{dal.Ascending(exBinaryChain(maxWalkNesting, dal.NewFieldRef("", "id")))}},
		"a group by expression": exShapeQuery{StructuredQuery: exBase(), groupBy: []dal.Expression{exBinaryChain(maxWalkNesting, dal.NewFieldRef("", "id"))}},
		"a scan order":          dal.From(exRef("", "a", "").WithScan(1, dal.Ascending(exBinaryChain(maxWalkNesting, dal.NewFieldRef("", "id"))))).NewQuery().SelectIntoRecord(nil),
		"an aggregate argument": exWithColumn(dal.NewAggregate("sum", false, exBinaryChain(maxWalkNesting-1, dal.NewFieldRef("", "id")))),
		// A bound a request body can reach is not a bound the walk may spend memory
		// on: a thousand levels are refused, not walked.
		"a thousand group conditions":   exWithWhere(exGroupChain(1000, exIDEqualsOne())),
		"a thousand binary expressions": exWithColumn(exBinaryChain(1000, dal.NewFieldRef("", "id"))),
		// The count runs across subqueries: the outer levels and the inner levels
		// add up.
		"levels split between a query and its subquery":      exWithWhere(exGroupChain(maxWalkNesting/2, dal.NewExistsCondition(exWithWhere(exGroupChain(maxWalkNesting/2, exIDEqualsOne()))))),
		"levels split between a query and a scalar subquery": exWithColumn(exBinaryChain(maxWalkNesting/2, dal.NewQueryExpression(exWithColumn(exBinaryChain(maxWalkNesting/2, dal.NewFieldRef("", "id"))), "q"))),
	}
	for name, q := range tooDeep {
		t.Run("refuses "+name, func(t *testing.T) {
			doc, err := inspect(q)
			if !errors.Is(err, ErrInvalidDocument) || !strings.Contains(err.Error(), "nest") {
				t.Fatalf("err = %v, want ErrInvalidDocument about nesting", err)
			}
			if len(err.Error()) > 512 {
				t.Fatalf("the error is %d bytes long", len(err.Error()))
			}
			if !reflect.DeepEqual(doc, document{}) {
				t.Fatalf("a refused document returned a walk: %+v", doc)
			}
		})
	}
}

// The count is of levels above a node, not of nodes walked: siblings do not add
// up, so a wide document is not a deep one.
func TestInspectDoesNotCountSiblingsAsNesting(t *testing.T) {
	children := make([]dal.Condition, 500)
	for i := range children {
		children[i] = exGroupChain(maxWalkNesting-4, exIDEqualsOne())
	}
	if _, err := inspect(exWithWhere(dal.NewGroupCondition(dal.And, children...))); err != nil {
		t.Fatalf("a wide group of deep conditions: %v", err)
	}
	columns := make([]dal.Column, 500)
	for i := range columns {
		columns[i] = dal.Column{Expression: exBinaryChain(maxWalkNesting-3, dal.NewFieldRef("", "id"))}
	}
	if _, err := inspect(dal.WithColumns(exBase(), columns)); err != nil {
		t.Fatalf("a wide column list of deep expressions: %v", err)
	}
}

// A parameter is bound before a document runs (a JSON body binds its parameters;
// a YAML body binds none), and DALgo's join evaluates no parameter, so one that
// reaches the executor is refused in the walk, wherever it sits, whatever the
// route the document would take.
func TestInspectRefusesAParameterNothingBound(t *testing.T) {
	param := dal.NewComparison(dal.NewFieldRef("", "id"), dal.Equal, dal.Param{Name: "n"})
	a, b := exRef("", "a", "a"), exRef("", "b", "b")
	inner := dal.From(b).NewQuery().Where(param).SelectIntoRecord(nil)
	for _, tc := range []struct {
		name  string
		query dal.StructuredQuery
	}{
		{"in WHERE", exWithWhere(param)},
		{"in an arithmetic expression", exWithColumn(dal.Binary(dal.NewFieldRef("", "x"), dal.Add, dal.Param{Name: "n"}))},
		{"in HAVING", dal.From(a).NewQuery().GroupBy(dal.NewFieldRef("", "x")).Having(param).SelectIntoRecord(nil)},
		{"in a join condition", dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, dal.NewComparison(dal.NewFieldRef("a", "k"), dal.Equal, dal.Param{Name: "n"}))).NewQuery().SelectIntoRecord(nil)},
		{"in a subquery", exWithWhere(dal.NewExistsCondition(inner))},
		{"in a derived source", dal.From(dal.NewQuerySource(inner, "d")).NewQuery().SelectIntoRecord(nil)},
		{"in an aggregate", exWithColumn(dal.NewAggregate("sum", false, dal.Param{Name: "n"}))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inspect(tc.query)
			if !errors.Is(err, ErrInvalidDocument) || !strings.Contains(err.Error(), `parameter "n" is not bound`) {
				t.Fatalf("err = %v, want ErrInvalidDocument naming the parameter", err)
			}
		})
	}
	// A name of any length is clipped.
	_, err := inspect(exWithWhere(dal.NewComparison(dal.NewFieldRef("", "id"), dal.Equal, dal.Param{Name: strings.Repeat("p", 1<<16)})))
	if !errors.Is(err, ErrInvalidDocument) || len(err.Error()) > 512 {
		t.Fatalf("err = %v (%d bytes)", err, len(err.Error()))
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
		{"field that starts with a combining mark", exWithColumn(dal.NewFieldRef("", "́e")), "field name"},
		{"nested segment that starts with a combining mark", exWithColumn(dal.NewFieldRef("", "a.́e")), "field name"},
		{"field that is too long", exWithColumn(dal.NewFieldRef("", strings.Repeat("x", maxNameLen+1))), "field name"},
		{"field in a condition", exWithWhere(dal.NewComparison(dal.NewFieldRef("", "x;y"), dal.Equal, dal.Constant{Value: 1})), "field name"},
		{"qualifier that is not an identifier", exWithColumn(dal.NewFieldRef("a b", "id")), "identifier"},
		{"qualifier that names nothing in scope", exWithColumn(dal.NewFieldRef("zz-top", "id")), "identifier"},
		{"column alias", dal.WithColumns(exBase(), []dal.Column{{Expression: dal.NewFieldRef("", "id"), Alias: "my alias"}}), "identifier"},
		{"source alias", dal.From(exRef("", "a", "bad-alias")).NewQuery().SelectIntoRecord(nil), "identifier"},
		{"derived source alias", dal.From(dal.NewQuerySource(exPlain("", "b"), "bad alias")).NewQuery().SelectIntoRecord(nil), "identifier"},
		{"wildcard source", dal.WithColumns(exBase(), []dal.Column{{Wildcard: &dal.WildcardProjection{Source: "x y"}}}), "identifier"},
		{"wildcard exclusion", dal.WithColumns(exBase(), []dal.Column{{Wildcard: &dal.WildcardProjection{Exclude: []string{"a b"}}}}), "field name"},
		{"scalar subquery result name", exWithColumn(dal.NewQueryExpression(exPlain("", "b"), "bad alias")), "identifier"},
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
	// identifier), a wildcard with exclusions and constants.
	spaced := exRef("", "Order Details", "")
	ok := dal.From(a).Join(dal.NewJoinedSource(spaced, dal.JoinInner, dal.NewComparison(dal.NewFieldRef("a", "k"), dal.Equal, dal.NewFieldRef("Order Details", "k")))).NewQuery().
		Where(dal.NewGroupCondition(dal.And,
			dal.NewComparison(dal.NewFieldRef("a", "address.city"), dal.Equal, dal.Constant{Value: "Dublin"}),
			dal.NewIsNotNullCondition(dal.NewFieldRef("a", "x")),
		)).
		SelectColumns(
			dal.Column{Expression: dal.NewFieldRef("", "$id"), Alias: "key"},
			dal.Column{Wildcard: &dal.WildcardProjection{Source: "a", Exclude: []string{"secret"}}},
			dal.Column{Expression: dal.Binary(dal.NewFieldRef("a", "x"), dal.Add, dal.Constant{Value: 1}), Alias: "x1"},
			dal.Column{Expression: dal.NewAggregate("count", false, dal.Star()), Alias: "n"},
			// A combining mark continues a segment, in decomposed Latin text and in
			// scripts such as Devanagari, as core's strict rule has it.
			dal.Column{Expression: dal.NewFieldRef("", "Café"), Alias: "cafe"},
			dal.Column{Expression: dal.NewFieldRef("a", "नमस्ते.नमस्ते")},
			dal.Column{Expression: dal.NewQueryExpression(exPlain("", "c"), "total")},
			dal.Column{Expression: dal.NewQueryExpression(exPlain("", "c"), "")},
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

// The classifier of pkg/core refuses an arithmetic operator other than + - * /
// and an aggregate function outside its seven names, in any position, because
// DALgo reads any text as an operator or a name and checks it only where it runs
// an aggregation. The walk refuses both itself, without repeating the text.
func TestInspectRefusesAnArithmeticOperatorOutsideTheFourAndAnUnknownAggregate(t *testing.T) {
	field := dal.NewFieldRef("", "id")
	const hostile = "evil'op; DROP TABLE a"
	for _, tc := range []struct {
		name  string
		query dal.StructuredQuery
		want  string
	}{
		{"operator in a column", exWithColumn(dal.Binary(field, dal.ArithmeticOperator(hostile), dal.Constant{Value: 1})), "arithmetic operator"},
		{"operator in a condition", exWithWhere(dal.NewComparison(dal.Binary(field, dal.ArithmeticOperator("%"), field), dal.Equal, dal.Constant{Value: 1})), "arithmetic operator"},
		{"empty operator", exWithColumn(dal.Binary(field, dal.ArithmeticOperator(""), field)), "arithmetic operator"},
		{"operator nested in an operand", exWithColumn(dal.Binary(dal.Binary(field, dal.ArithmeticOperator("||"), field), dal.Add, field)), "arithmetic operator"},
		{"operator in an aggregate argument", exWithColumn(dal.NewAggregate("sum", false, dal.Binary(field, dal.ArithmeticOperator("^"), field))), "arithmetic operator"},
		{"operator in a subquery", exWithWhere(dal.NewExistsCondition(exWithColumn(dal.Binary(field, dal.ArithmeticOperator("%"), field)))), "arithmetic operator"},
		{"aggregate in a column", exWithColumn(dal.NewAggregate(hostile, false, field)), "aggregate function"},
		{"aggregate in a condition", exWithWhere(dal.NewComparison(dal.NewAggregate("median", false, field), dal.Equal, dal.Constant{Value: 1})), "aggregate function"},
		{"empty aggregate name", exWithColumn(dal.NewAggregate("", false, field)), "aggregate function"},
		{"aggregate in HAVING", exShapeQuery{StructuredQuery: exBase(), having: dal.NewComparison(dal.NewAggregate("stddev", false, field), dal.Equal, dal.Constant{Value: 1})}, "aggregate function"},
		{"aggregate in a subquery", exWithWhere(dal.NewExistsCondition(exWithColumn(dal.NewAggregate("median", false, field)))), "aggregate function"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := inspect(tc.query)
			if !errors.Is(err, ErrInvalidDocument) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalidDocument containing %q", err, tc.want)
			}
			for _, text := range []string{"evil", "DROP", "median", "stddev", "%", "||", "^"} {
				if strings.Contains(err.Error(), text) {
					t.Fatalf("the refusal echoes %q: %v", text, err)
				}
			}
			if !reflect.DeepEqual(doc, document{}) {
				t.Fatalf("a refused document returned a walk: %+v", doc)
			}
		})
	}
	// The four operators and the seven names are accepted, whatever the case of a name.
	for _, op := range []dal.ArithmeticOperator{dal.Add, dal.Subtract, dal.Multiply, dal.Divide} {
		if _, err := inspect(exWithColumn(dal.Binary(field, op, field))); err != nil {
			t.Fatalf("operator %q: %v", op, err)
		}
	}
	for _, name := range []string{"COUNT", "SUM", "AVG", "MIN", "MAX", "FIRST", "LAST", "count", "Sum", "aVg", "min", "max", "first", "last"} {
		if _, err := inspect(exWithColumn(dal.NewAggregate(name, false, field))); err != nil {
			t.Fatalf("aggregate %q: %v", name, err)
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
		if err := checkProfile(doc, profile); !errors.Is(err, ErrInvalidDocument) || !errors.Is(err, ErrProfileMismatch) {
			t.Fatalf("%s: err = %v, want ErrInvalidDocument and ErrProfileMismatch", name, err)
		}
	}
	// A profile that denies a subquery the document has is refused too.
	withSubquery, err := inspect(exWithWhere(dal.NewExistsCondition(exPlain("", "b"))))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	profile := Profile{Sources: []ProfileSource{{"", "a"}, {"", "b"}}}
	if err := checkProfile(withSubquery, profile); !errors.Is(err, ErrInvalidDocument) || !errors.Is(err, ErrProfileMismatch) {
		t.Fatalf("a hidden subquery: err = %v, want ErrInvalidDocument and ErrProfileMismatch", err)
	}
	profile.HasSubquery = true
	if err := checkProfile(withSubquery, profile); err != nil {
		t.Fatalf("a declared subquery: %v", err)
	}
}

// The collection name is the one name that becomes a path on a file-backed
// engine. The walk applies the rule of pkg/core's ValidateCollectionName to it,
// so a collection that is not a plain name is not authorised, resolved or read,
// and does not become a qualifier either.
func TestInspectChecksCollectionNames(t *testing.T) {
	for name, collection := range map[string]string{
		"dot":                      ".",
		"dot dot":                  "..",
		"parent component":         "a/../b",
		"backslash parent":         `a\..\b`,
		"leading parent":           "../secrets",
		"only separators":          "//",
		"control character":        "a\x00b",
		"newline":                  "a\nb",
		"delete character":         "a\x7fb",
		"relative component alone": "x/./y",
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := inspect(dal.From(exRef("one", collection, "")).NewQuery().SelectIntoRecord(nil))
			if !errors.Is(err, ErrInvalidDocument) || !strings.Contains(err.Error(), "collection name") {
				t.Fatalf("err = %v, want ErrInvalidDocument naming the collection", err)
			}
			if !reflect.DeepEqual(doc, document{}) {
				t.Fatalf("a refused document returned a walk: %+v", doc)
			}
		})
	}
	// DALgo will not build a collection without a name, but the zero value is
	// one, and the walk refuses it.
	if _, err := inspect(dal.From(dal.CollectionRef{}).NewQuery().SelectIntoRecord(nil)); !errors.Is(err, ErrInvalidDocument) || !strings.Contains(err.Error(), "collection name") {
		t.Fatalf("an empty collection name: %v", err)
	}
	// The same rule accepts the names real schemas use: spaces, hyphens, dots
	// inside a component, non-ASCII letters and an escaped separator.
	for _, collection := range []string{"Order Details", "order-items", "a.b", "a..b", "Données", "x/y", "$special"} {
		if _, err := inspect(dal.From(exRef("one", collection, "")).NewQuery().SelectIntoRecord(nil)); err != nil {
			t.Fatalf("collection %q: %v", collection, err)
		}
	}
	// A collection that is refused is not in scope, so no qualifier can borrow
	// its name.
	q := dal.From(exRef("", "..", "")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("..", "id")})
	if _, err := inspect(q); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("err = %v", err)
	}
}

func TestInspectRecordsNullTestsWhereverTheyAre(t *testing.T) {
	field := dal.NewFieldRef("", "id")
	a, b := exRef("", "a", "a"), exRef("", "b", "b")
	onEdge := dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinLeft, dal.NewIsNullCondition(dal.NewFieldRef("b", "id")))).NewQuery().SelectIntoRecord(nil)
	inGroup := exWithWhere(dal.NewGroupCondition(dal.And, dal.NewComparison(field, dal.Equal, dal.Constant{Value: 1}), dal.NewIsNotNullCondition(field)))
	inSubquery := exWithWhere(dal.NewExistsCondition(exWithWhere(dal.NewIsNullCondition(field))))
	inHaving := dal.From(a).NewQuery().GroupBy(field).Having(dal.NewIsNullCondition(field)).SelectColumns(dal.Column{Expression: field})
	for name, q := range map[string]dal.StructuredQuery{
		"in WHERE":       exWithWhere(dal.NewIsNullCondition(field)),
		"negated":        exWithWhere(dal.NewIsNotNullCondition(field)),
		"in a group":     inGroup,
		"on a join edge": onEdge,
		"in a subquery":  inSubquery,
		"in HAVING":      inHaving,
	} {
		doc, err := inspect(q)
		if err != nil || !doc.hasNull {
			t.Fatalf("%s: hasNull = %v, err = %v", name, doc.hasNull, err)
		}
	}
	plain, err := inspect(exWithWhere(dal.NewComparison(field, dal.Equal, dal.Constant{Value: 1})))
	if err != nil || plain.hasNull {
		t.Fatalf("a document without a null test: hasNull = %v, err = %v", plain.hasNull, err)
	}
}

func TestDocumentAnyScanLooksAtEverySource(t *testing.T) {
	none := document{sources: []walkedSource{{collection: "A"}, {collection: "B"}}}
	if none.anyScan() || (document{}).anyScan() {
		t.Fatal("a document without a scan clause has none")
	}
	last := document{sources: []walkedSource{{collection: "A"}, {collection: "B", scan: true}}}
	if !last.anyScan() {
		t.Fatal("a scan clause on the last source is missed")
	}
}

// A name of the size of the request body must not come back in an error.
func TestRefusalsDoNotEchoALongNameWhole(t *testing.T) {
	long := strings.Repeat("x y ", 1<<18) // 1 MiB, with spaces so that it is refused
	for name, q := range map[string]dal.StructuredQuery{
		"field":                       exWithColumn(dal.NewFieldRef("", long)),
		"alias":                       dal.WithColumns(exBase(), []dal.Column{{Expression: dal.NewFieldRef("", "id"), Alias: long}}),
		"qualifier":                   exWithColumn(dal.NewFieldRef(long, "id")),
		"parameter":                   exWithWhere(dal.NewComparison(dal.NewFieldRef("", "id"), dal.Equal, dal.Param{Name: long})),
		"collection":                  dal.From(exRef("", "a/../"+long, "")).NewQuery().SelectIntoRecord(nil),
		"scalar subquery result name": exWithColumn(dal.NewQueryExpression(exPlain("", "b"), long)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := inspect(q)
			if !errors.Is(err, ErrInvalidDocument) {
				t.Fatalf("err = %v, want ErrInvalidDocument", err)
			}
			if len(err.Error()) > 512 {
				t.Fatalf("the error is %d bytes long: the name came back", len(err.Error()))
			}
		})
	}
	// Execute does not echo a long collection name either: not where a source has
	// no database, and not where a scan clause is refused on a protected source.
	_, err := exRun(t, exPlain("", strings.Repeat("c", 1<<20)), "", newExRegistry(), exAllow, Limits{})
	if !errors.Is(err, ErrSourceWithoutDatabase) || len(err.Error()) > 512 {
		t.Fatalf("err = %v (%d bytes)", err, len(err.Error()))
	}
	protected := exMount("hr", "sqlite", true, nil)
	scanned := dal.From(exRef("hr", strings.Repeat("c", 1<<20), "").WithScan(2, dal.AscendingField("id"))).NewQuery().SelectIntoRecord(nil)
	_, err = exRun(t, scanned, "", newExRegistry(protected), exAllow, Limits{})
	if !errors.Is(err, ErrScanOnProtectedSource) || len(err.Error()) > 512 {
		t.Fatalf("err = %v (%d bytes)", err, len(err.Error()))
	}
}

func TestClipKeepsShortNamesAndCutsLongOnesOnACharacterBoundary(t *testing.T) {
	if got := clip("short"); got != "short" {
		t.Fatalf("clip = %q", got)
	}
	exact := strings.Repeat("a", maxEchoLen)
	if got := clip(exact); got != exact {
		t.Fatalf("a name of exactly %d bytes is kept whole, got %q", maxEchoLen, got)
	}
	if got := clip(exact + "b"); got != exact+"..." {
		t.Fatalf("clip = %q", got)
	}
	// A three-byte character straddles the cut: it is dropped, not split.
	straddling := strings.Repeat("a", maxEchoLen-1) + "€€"
	got := clip(straddling)
	if got != strings.Repeat("a", maxEchoLen-1)+"..." || !utf8.ValidString(got) {
		t.Fatalf("clip = %q", got)
	}
}
