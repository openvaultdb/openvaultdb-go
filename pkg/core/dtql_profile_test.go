package core

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dtql"
	"github.com/dal-go/record"
)

func mustDeserialize(t *testing.T, doc string) dal.StructuredQuery {
	t.Helper()
	query, err := dtql.Deserialize([]byte(doc))
	if err != nil {
		t.Fatalf("deserialize: %v\n%s", err, doc)
	}
	return query
}

const joinGroupHavingDoc = `
from:
  database: chinook
  name: Invoice
  alias: i
  joins:
    - type: left
      from: {database: chinook, name: Customer, alias: c}
      on:
        - left: {field: CustomerId, source: i}
          op: '=='
          right: {field: CustomerId, source: c}
groupBy: [{field: Country, source: c}]
having:
  op: '>'
  left: {aggregate: {function: sum, args: [{field: Total, source: i}]}}
  right: {value: 10}
orderBy: [{field: Country, source: c}]
limit: 20
columns:
  - {field: Country, source: c}
  - aggregate: {function: sum, args: [{field: Total, source: i}]}
    as: revenue
`

func TestClassifyDTQLRelationalJoinGroupHaving(t *testing.T) {
	profile, err := ClassifyDTQL(mustDeserialize(t, joinGroupHavingDoc))
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	want := Profile{
		Kind: ProfileRelational,
		Sources: []ProfileSource{
			{Database: "chinook", Collection: "Invoice", Alias: "i"},
			{Database: "chinook", Collection: "Customer", Alias: "c"},
		},
		HasAggregation: true,
	}
	if !reflect.DeepEqual(profile, want) {
		t.Fatalf("profile = %+v, want %+v", profile, want)
	}
}

func TestClassifyDTQLAcceptedShapes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		doc         string
		sources     []ProfileSource
		subquery    bool
		aggregation bool
	}{
		{
			name: "nested join tree across two databases with hints",
			doc: `
from:
  database: a
  name: Invoice
  alias: i
  joins:
    - type: inner
      from:
        database: b
        name: Customer
        alias: c
        joins:
          - type: left
            from: {database: b, name: Employee, alias: e}
            on:
              - left: {field: SupportRepId, source: c}
                op: '=='
                right: {field: EmployeeId, source: e}
            hints: {algorithms: [nestedLoop, hash]}
      on:
        - left: {field: CustomerId, source: i}
          op: '=='
          right: {field: CustomerId, source: c}
columns:
  - {field: InvoiceId, source: i, as: invoice_id}
  - wildcard: {source: c, exclude: [CustomerId]}
`,
			sources: []ProfileSource{
				{Database: "a", Collection: "Invoice", Alias: "i"},
				{Database: "b", Collection: "Customer", Alias: "c"},
				{Database: "b", Collection: "Employee", Alias: "e"},
			},
		},
		{
			name: "derived source",
			doc: `
from:
  query:
    as: customers
    from: {name: Customer, alias: c}
    columns: [{field: CustomerId, source: c}]
  joins:
    - from:
        query:
          as: stats
          from: {name: Invoice, alias: i}
          groupBy: [{field: CustomerId, source: i}]
          columns:
            - {field: CustomerId, source: i}
            - aggregate: {function: sum, args: [{field: Total, source: i}]}
              as: spent
      on:
        - left: {field: CustomerId, source: customers}
          op: '=='
          right: {field: CustomerId, source: stats}
columns: [{field: CustomerId, source: customers}, {field: spent, source: stats}]
`,
			sources: []ProfileSource{
				{Collection: "Customer", Alias: "c"},
				{Collection: "Invoice", Alias: "i"},
			},
			subquery: true,
		},
		{
			name: "exists subquery in where",
			doc: `
from: {name: Customer, alias: c}
where:
  and:
    - op: '>'
      left: {field: CustomerId, source: c}
      right: {value: 1}
    - exists:
        query:
          from: {database: other, name: Invoice, alias: i}
          where:
            op: '=='
            left: {field: CustomerId, source: i}
            right: {field: CustomerId, source: c}
columns: [{field: CustomerId, source: c}]
`,
			sources: []ProfileSource{
				{Collection: "Customer", Alias: "c"},
				{Database: "other", Collection: "Invoice", Alias: "i"},
			},
			subquery: true,
		},
		{
			name: "not exists, in-subquery, scalar subquery, null tests, arithmetic and values",
			doc: `
from: {name: Truth, alias: t}
where:
  or:
    - notExists:
        query:
          from: {name: Membership, alias: m}
    - op: In
      left: {field: Value, source: t}
      right:
        query:
          from: {name: Membership, alias: m2}
          columns: [{field: Value, source: m2}]
    - op: In
      left: {field: Value, source: t}
      right: {values: [1, 2]}
    - isNull: {field: Other, source: t}
    - isNotNull: {field: Value, source: t}
orderBy: [{field: Name, source: t}]
columns:
  - binary:
      op: '+'
      left: {field: Value, source: t}
      right: {value: 1}
    as: next
  - query:
      as: one
      from: {name: Invoice, alias: o}
      columns: [{field: Total, source: o}]
`,
			sources: []ProfileSource{
				{Collection: "Truth", Alias: "t"},
				{Collection: "Membership", Alias: "m"},
				{Collection: "Membership", Alias: "m2"},
				{Collection: "Invoice", Alias: "o"},
			},
			subquery: true,
		},
		{
			name: "single source with an alias is relational",
			doc:  "from: {name: customers, alias: c}\n",
			sources: []ProfileSource{
				{Collection: "customers", Alias: "c"},
			},
		},
		{
			name: "aggregate over one collection is relational",
			doc: `
from: {name: orders}
groupBy: [{field: country}]
columns:
  - {field: country}
  - aggregate: {function: count, args: [{star: true}]}
    as: n
`,
			sources:     []ProfileSource{{Collection: "orders"}},
			aggregation: true,
		},
		{
			name:    "root database makes a single collection relational",
			doc:     "from: {database: chinook, name: Customer}\n",
			sources: []ProfileSource{{Database: "chinook", Collection: "Customer"}},
		},
		{
			name:    "limit and offset at their maxima",
			doc:     "from: {name: Customer, alias: c}\nlimit: 1000\noffset: 10000\n",
			sources: []ProfileSource{{Collection: "Customer", Alias: "c"}},
		},
		{
			// The caps bound the answer, so they apply to the outermost query
			// only. A nested read is bounded by the request's read budget, and a
			// top-N derived source is a valid shape.
			name: "limit and offset above the caps in a derived source and a subquery",
			doc: `
from:
  query:
    as: d
    from: {name: a, alias: x}
    limit: 5000
    offset: 20000
where:
  exists:
    query:
      from: {name: b, alias: y}
      limit: 100000
limit: 10
`,
			sources:  []ProfileSource{{Collection: "a", Alias: "x"}, {Collection: "b", Alias: "y"}},
			subquery: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ClassifyDTQL(mustDeserialize(t, tc.doc))
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			if profile.Kind != ProfileRelational {
				t.Fatalf("kind = %q, want relational", profile.Kind)
			}
			if !reflect.DeepEqual(profile.Sources, tc.sources) {
				t.Fatalf("sources = %+v, want %+v", profile.Sources, tc.sources)
			}
			if profile.HasSubquery != tc.subquery || profile.HasAggregation != tc.aggregation {
				t.Fatalf("subquery=%v aggregation=%v, want %v %v", profile.HasSubquery, profile.HasAggregation, tc.subquery, tc.aggregation)
			}
		})
	}
}

func TestClassifyDTQLSingleCollectionMatchesValidateDTQL(t *testing.T) {
	for _, doc := range []string{
		"from: {name: customers}\n",
		"from: {name: customers}\nwhere: {op: '==', left: {field: \"zip code\"}, right: {value: 1}}\norderBy: [{field: \"zip code\"}]\n",
		"from: {name: customers}\nlimit: 50\noffset: 5\n",
		"from: {name: customers}\ncolumns: [{field: name}]\n",
		"from: {name: customers}\nwhere: {op: '==', left: {field: name}, right: {param: n}}\norderBy: [{field: name, desc: true}]\n",
	} {
		query := mustDeserialize(t, doc)
		collection, err := validateDTQL(query)
		if err != nil {
			t.Fatalf("validateDTQL rejects %q: %v", doc, err)
		}
		profile, err := ClassifyDTQL(query)
		if err != nil {
			t.Fatalf("classify %q: %v", doc, err)
		}
		want := Profile{Kind: ProfileSingleCollection, Sources: []ProfileSource{{Collection: collection}}}
		if !reflect.DeepEqual(profile, want) {
			t.Fatalf("profile for %q = %+v, want %+v", doc, profile, want)
		}
	}
}

func TestClassifyDTQLRefusals(t *testing.T) {
	deepQuery := func(depth int) string {
		doc := "from: {name: leaf, alias: l}\n"
		for i := 0; i < depth; i++ {
			doc = fmt.Sprintf("from:\n  query:\n    as: q%d\n%s", i, indent(doc, "    "))
		}
		return doc
	}
	manySources := func(n int) string {
		var b strings.Builder
		b.WriteString("from:\n  name: t0\n  alias: a0\n  joins:\n")
		for i := 1; i < n; i++ {
			fmt.Fprintf(&b, "    - from: {name: t%d, alias: a%d}\n      on: [{left: {field: id, source: a0}, op: '==', right: {field: id, source: a%d}}]\n", i, i, i)
		}
		return b.String()
	}
	for _, tc := range []struct {
		name string
		doc  string
		rule string
		path string
	}{
		{"schema on the root", "from: {schema: main, name: Customer, alias: c}\n", "schema", "from"},
		{"schema on a joined source", `
from:
  name: a
  alias: a
  joins:
    - from: {schema: main, name: b, alias: b}
      on: [{left: {field: id, source: a}, op: '==', right: {field: id, source: b}}]
`, "schema", "from.joins[0].from"},
		{"scan on the root", "from: {name: a, scan: {limit: 5, orderBy: [{field: id}]}}\n", "scan", "from"},
		{"money", "from: {name: a, alias: a}\nmoney: {minorUnitScale: 2, divisionScale: 4, rounding: halfEven}\n", "money", "$"},
		{"money in a subquery", `
from: {name: a, alias: a}
where:
  exists:
    query:
      from: {name: b, alias: b}
      money: {minorUnitScale: 2, divisionScale: 4, rounding: halfEven}
`, "money", "where.exists.query"},
		{"limit above 1000", "from: {name: a, alias: a}\nlimit: 1001\n", "limit", "$"},
		{"offset above 10000", "from: {name: a, alias: a}\noffset: 10001\n", "offset", "$"},
		{"collection name with a relative path component", "from: {name: '../b', alias: a}\n", "collection-name", "from"},
		{"collection name that is a relative path", "from: {name: '..', alias: a}\n", "collection-name", "from"},
		{"database id with a slash", "from: {database: 'a/b', name: a}\n", "database-id", "from"},
		{"database id that looks like a URL", "from: {database: 'https://example.com', name: a}\n", "database-id", "from"},
		{"database id starting with a dash", "from: {database: '-a', name: a}\n", "database-id", "from"},
		{"too many sources", manySources(9), "source-count", "from.joins[7].from"},
		{"too many sources across subqueries", `
from: {name: a0, alias: a0}
where:
  and:
    - exists: {query: {from: {name: a1, alias: a1}}}
    - exists: {query: {from: {name: a2, alias: a2}}}
    - exists: {query: {from: {name: a3, alias: a3}}}
    - exists: {query: {from: {name: a4, alias: a4}}}
    - exists: {query: {from: {name: a5, alias: a5}}}
    - exists: {query: {from: {name: a6, alias: a6}}}
    - exists: {query: {from: {name: a7, alias: a7}}}
    - exists: {query: {from: {name: a8, alias: a8}}}
`, "source-count", "where.and[7].exists.query.from"},
		{"subquery nested too deep", deepQuery(5), "subquery-depth", "from.query.from.query.from.query.from.query.from.query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ClassifyDTQL(mustDeserialize(t, tc.doc))
			assertRefusal(t, profile, err, tc.rule, tc.path)
		})
	}
}

func indent(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n") + "\n"
}

func assertRefusal(t *testing.T, profile Profile, err error, rule, path string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidDTQL) {
		t.Fatalf("err = %v, want ErrInvalidDTQL (profile %+v)", err, profile)
	}
	if !reflect.DeepEqual(profile, Profile{}) {
		t.Fatalf("a refused query returned a profile: %+v", profile)
	}
	wantText := fmt.Sprintf("relational profile: %s at %s:", rule, path)
	if !strings.Contains(err.Error(), wantText) {
		t.Fatalf("err = %q, want it to contain %q", err.Error(), wantText)
	}
}

func buildQuery(from dal.FromSource) *dal.QueryBuilder { return from.NewQuery() }

// unknownExpression and unknownCondition are shapes no DTQL document can
// produce; the validator must refuse them rather than pass them through.
type unknownExpression struct{}

func (unknownExpression) String() string { return "unknown" }

type unknownCondition struct{}

func (unknownCondition) String() string { return "unknown" }

// unknownSource is a source type the validator has never heard of.
type unknownSource struct{ dal.RecordsetSource }

type moneyQuery struct{ dal.StructuredQuery }

func (moneyQuery) Money() *dal.MoneyConfig { return &dal.MoneyConfig{} }

type nilMoneyQuery struct{ dal.StructuredQuery }

func (nilMoneyQuery) Money() *dal.MoneyConfig { return nil }

// cursorQuery sets the cursors that the DTQL decoder never produces.
type cursorQuery struct {
	dal.StructuredQuery
	from, after dal.Cursor
}

func (q cursorQuery) StartFrom() dal.Cursor  { return q.from }
func (q cursorQuery) StartAfter() dal.Cursor { return q.after }

// shapeQuery overrides the members a hand-built query cannot otherwise set.
type shapeQuery struct {
	dal.StructuredQuery
	from    dal.FromSource
	hasFrom bool
	orderBy []dal.OrderExpression
	groupBy []dal.Expression
	having  dal.Condition
}

func (q shapeQuery) From() dal.FromSource {
	if q.hasFrom {
		return q.from
	}
	return q.StructuredQuery.From()
}
func (q shapeQuery) OrderBy() []dal.OrderExpression { return q.orderBy }
func (q shapeQuery) GroupBy() []dal.Expression      { return q.groupBy }
func (q shapeQuery) Having() dal.Condition          { return q.having }

type nilFrom struct{ dal.FromSource }

func (nilFrom) Base() dal.RecordsetSource { return nil }

func TestClassifyDTQLRefusesShapesDTQLCannotProduce(t *testing.T) {
	base := func() dal.StructuredQuery { return buildQuery(dal.From(rootRef("a"))).SelectIntoRecordset() }
	withColumn := func(expr dal.Expression) dal.StructuredQuery {
		return dal.WithColumns(base(), []dal.Column{{Expression: expr}})
	}
	withWhere := func(cond dal.Condition) dal.StructuredQuery { return dal.WithWhere(base(), cond) }
	field := dal.NewFieldRef("", "id")
	parent := record.NewKeyWithID("p", "1")
	joined := func(src dal.RecordsetSource, jt dal.JoinType) dal.StructuredQuery {
		from := dal.From(rootRef("a"))
		from.Join(dal.NewJoinedSource(src, jt, dal.NewComparison(field, dal.Equal, field)))
		return buildQuery(from).SelectIntoRecordset()
	}
	nestedQuery := func() dal.StructuredQuery { return buildQuery(dal.From(rootRef("b"))).SelectIntoRecordset() }

	for _, tc := range []struct {
		name  string
		query dal.StructuredQuery
		rule  string
		path  string
	}{
		{"nil query", nil, "query-shape", "$"},
		{"nil from", shapeQuery{StructuredQuery: base(), hasFrom: true}, "query-shape", "from"},
		{"nil base", shapeQuery{StructuredQuery: base(), hasFrom: true, from: nilFrom{dal.From(rootRef("a"))}}, "query-shape", "from"},
		{"parent collection", buildQuery(dal.From(dal.NewCollectionRef("kids", "", parent))).SelectIntoRecordset(), "parent-source", "from"},
		{"collection group", buildQuery(dal.From(dal.NewCollectionGroupRef("g", ""))).SelectIntoRecordset(), "collection-group", "from"},
		{"unknown source type", buildQuery(dal.From(unknownSource{rootRef("x")})).SelectIntoRecordset(), "source-shape", "from"},
		{"pointer to a derived source", buildQuery(dal.From(ptrQuerySource(nestedQuery()))).SelectIntoRecordset(), "source-shape", "from"},
		{"qualified root", buildQuery(dal.From(dal.NewQualifiedRootCollectionRef("main", "a", ""))).SelectIntoRecordset(), "schema", "from"},
		{"scan source", buildQuery(dal.From(dal.NewRootCollectionRef("a", "").WithScan(5, dal.AscendingField("id")))).SelectIntoRecordset(), "scan", "from"},
		{"right join", joined(rootRef("b"), dal.JoinRight), "join-type", "from.joins[0]"},
		{"full join", joined(rootRef("b"), dal.JoinFull), "join-type", "from.joins[0]"},
		{"cross join", joined(rootRef("b"), dal.JoinCross), "join-type", "from.joins[0]"},
		{"joined collection group", joined(dal.NewCollectionGroupRef("g", ""), dal.JoinInner), "collection-group", "from.joins[0].from"},
		{"cursor start from", cursorQuery{StructuredQuery: base(), from: "x"}, "cursor", "$"},
		{"cursor start after", cursorQuery{StructuredQuery: base(), after: "x"}, "cursor", "$"},
		{"money config", moneyQuery{base()}, "money", "$"},
		{"unknown column expression", withColumn(unknownExpression{}), "expression-shape", "columns[0]"},
		{"nil column expression", withColumn(nil), "expression-shape", "columns[0]"},
		{"pointer constant", withColumn(&dal.Constant{Value: 1}), "expression-shape", "columns[0]"},
		{"unknown where condition", withWhere(unknownCondition{}), "condition-shape", "where"},
		{"pointer comparison", withWhere(&dal.Comparison{Operator: dal.Equal, Left: field, Right: field}), "condition-shape", "where"},
		{"nil operand in a comparison", withWhere(dal.NewComparison(nil, dal.Equal, field)), "expression-shape", "where.left"},
		{"nil right operand in a comparison", withWhere(dal.NewComparison(field, dal.Equal, nil)), "expression-shape", "where.right"},
		{"nil condition in a group", withWhere(dal.NewGroupCondition(dal.And, nil)), "condition-shape", "where.and[0]"},
		{"unknown condition in an or group", withWhere(dal.NewGroupCondition(dal.Or, dal.NewComparison(field, dal.Equal, field), unknownCondition{})), "condition-shape", "where.or[1]"},
		{"nil is-null operand", withWhere(dal.NewIsNullCondition(nil)), "expression-shape", "where.isNull"},
		{"nil is-not-null operand", withWhere(dal.NewIsNotNullCondition(nil)), "expression-shape", "where.isNotNull"},
		{"nil exists query", withWhere(dal.NewExistsCondition(nil)), "query-shape", "where.exists.query"},
		{"nil not-exists query", withWhere(dal.NewNotExistsCondition(nil)), "query-shape", "where.notExists.query"},
		{"unknown binary operand", withColumn(dal.Binary(field, dal.Add, unknownExpression{})), "expression-shape", "columns[0].binary.right"},
		{"unknown binary left operand", withColumn(dal.Binary(unknownExpression{}, dal.Add, field)), "expression-shape", "columns[0].binary.left"},
		{"unknown aggregate argument", withColumn(dal.NewAggregate("sum", false, field, unknownExpression{})), "expression-shape", "columns[0].aggregate.args[1]"},
		{"nil scalar subquery", withColumn(dal.NewQueryExpression(nil, "q")), "query-shape", "columns[0].query"},
		{"unknown group by", shapeQuery{StructuredQuery: base(), groupBy: []dal.Expression{unknownExpression{}}}, "expression-shape", "groupBy[0]"},
		{"unknown order by", shapeQuery{StructuredQuery: base(), orderBy: []dal.OrderExpression{dal.Ascending(unknownExpression{})}}, "expression-shape", "orderBy[0]"},
		{"nil order by entry", shapeQuery{StructuredQuery: base(), orderBy: []dal.OrderExpression{nil}}, "expression-shape", "orderBy[0]"},
		{"unknown having", shapeQuery{StructuredQuery: base(), having: unknownCondition{}}, "condition-shape", "having"},
		{"unknown join condition", func() dal.StructuredQuery {
			from := dal.From(rootRef("a"))
			from.Join(dal.NewJoinedSource(rootRef("b"), dal.JoinInner, unknownCondition{}))
			return buildQuery(from).SelectIntoRecordset()
		}(), "condition-shape", "from.joins[0].on[0]"},
		{"unknown join condition in a nested tree", func() dal.StructuredQuery {
			child := dal.From(rootRef("b"))
			from := dal.From(rootRef("a"))
			from.Join(dal.NewJoinedFrom(child, dal.JoinLeft, unknownCondition{}))
			return buildQuery(from).SelectIntoRecordset()
		}(), "condition-shape", "from.joins[0].on[0]"},
		{"nil derived query", buildQuery(dal.From(dal.NewQuerySource(nil, "d"))).SelectIntoRecordset(), "query-shape", "from.query"},
		{"cycle through scalar subqueries", cyclicQuery(), "subquery-depth", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ClassifyDTQL(tc.query)
			if tc.path == "" {
				if !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), tc.rule) {
					t.Fatalf("err = %v, want rule %s", err, tc.rule)
				}
				return
			}
			assertRefusal(t, profile, err, tc.rule, tc.path)
		})
	}
}

func ptrQuerySource(q dal.StructuredQuery) *dal.QuerySource {
	source := dal.NewQuerySource(q, "d")
	return &source
}

// cyclicQuery returns a query whose scalar subquery is the query itself.
func cyclicQuery() dal.StructuredQuery {
	inner := &selfReferentialPtr{}
	inner.StructuredQuery = buildQuery(dal.From(rootRef("a"))).SelectIntoRecordset()
	inner.columns = []dal.Column{{Expression: dal.NewQueryExpression(inner, "again")}}
	return inner
}

// selfReferentialPtr is the pointer-backed query that cyclicQuery builds.
type selfReferentialPtr struct {
	dal.StructuredQuery
	columns []dal.Column
}

func (q *selfReferentialPtr) Columns() []dal.Column { return q.columns }

func TestClassifyDTQLAcceptsSubqueriesFourLevelsDeep(t *testing.T) {
	// Four levels are accepted, five are refused (the refusal is asserted above).
	doc := "from: {name: leaf, alias: l}\n"
	for i := 0; i < 4; i++ {
		doc = fmt.Sprintf("from:\n  query:\n    as: q%d\n%s", i, indent(doc, "    "))
	}
	profile, err := ClassifyDTQL(mustDeserialize(t, doc))
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if !profile.HasSubquery || len(profile.Sources) != 1 {
		t.Fatalf("profile = %+v", profile)
	}
}

func TestClassifyDTQLNilMoneyIsAccepted(t *testing.T) {
	query := nilMoneyQuery{buildQuery(dal.From(rootRef("a"))).SelectIntoRecordset()}
	profile, err := ClassifyDTQL(query)
	if err != nil || profile.Kind != ProfileSingleCollection {
		t.Fatalf("profile = %+v err = %v", profile, err)
	}
}

func TestClassifyDTQLSubqueryOrDatabaseKeepsASingleSourceRelational(t *testing.T) {
	// These name one root collection, but must not be classified
	// single-collection: a subquery or a database changes what the server has
	// to authorise. (An alias and an aggregate are covered by the accepted
	// shapes above.)
	for _, doc := range []string{
		"from: {name: a}\nwhere:\n  exists:\n    query:\n      from: {name: b, alias: b}\n",
		"from: {name: a}\nwhere:\n  op: In\n  left: {field: x}\n  right:\n    query:\n      from: {name: b, alias: b}\n      columns: [{field: x, source: b}]\n",
		"from: {database: d, name: a}\n",
	} {
		profile, err := ClassifyDTQL(mustDeserialize(t, doc))
		if err != nil {
			t.Fatalf("classify %q: %v", doc, err)
		}
		if profile.Kind != ProfileRelational {
			t.Fatalf("%q classified as %q, want relational", doc, profile.Kind)
		}
	}
}

// TestClassifyDTQLRefusesTheNamesTheQueryGuardRefuses is the regression test
// for the relational walk that checked no name: a name /dtql refuses through
// ParseDTQL must not become an accepted relational document through
// ClassifyDTQL, and a refusal returns a zero Profile. (Field names use a
// semicolon as the unsafe character: a name with a space is a valid name since
// the quoted-name rule. Aliases and qualifiers keep the identifier rule.)
func TestClassifyDTQLRefusesTheNamesTheQueryGuardRefuses(t *testing.T) {
	const joined = `
from:
  name: a
  alias: a
  joins:
    - from: {name: b, alias: b}
      on: [{left: {field: id, source: a}, op: '==', right: {field: id, source: b}}]
`
	for _, tc := range []struct{ name, doc string }{
		{"unsafe field in where of a join document", joined + "where: {op: '==', left: {field: \"first;name\", source: a}, right: {value: 1}}\n"},
		{"unsafe field in a join condition", strings.Replace(joined, "{field: id, source: b}", "{field: \"i;d\", source: b}", 1)},
		{"unsafe field in an ordering", joined + "orderBy: [{field: \"a;b\", source: a}]\n"},
		{"unsafe field in group by", joined + "groupBy: [{field: \"a'b\", source: a}]\n"},
		{"unsafe field in having", joined + "having: {op: '>', left: {aggregate: {function: sum, args: [{field: \"a;b\", source: a}]}}, right: {value: 1}}\n"},
		{"unsafe qualifier", "from: {name: a}\ncolumns: [{field: id, source: \"x y\"}]\n"},
		{"unsafe source alias", "from:\n  name: a\n  alias: a\n  joins:\n    - from: {name: b, alias: \"b b\"}\n      on: [{left: {field: id, source: a}, op: '==', right: {field: id, source: \"b b\"}}]\n"},
		{"unsafe root alias", "from:\n  name: a\n  alias: \"a;a\"\n  joins:\n    - from: {name: b, alias: b}\n      on: [{left: {field: id, source: \"a;a\"}, op: '==', right: {field: id, source: b}}]\n"},
		{"unsafe column alias", joined + "columns: [{field: id, source: a, as: \"a b\"}]\n"},
		{"unsafe wildcard exclude", joined + "columns: [{wildcard: {source: a, exclude: [\"a;b\"]}}]\n"},
		{"unsafe derived alias", "from:\n  query:\n    as: \"d e\"\n    from: {name: a, alias: a}\n"},
		{"unsafe field inside a derived source", "from:\n  query:\n    as: d\n    from: {name: a, alias: a}\n    where: {op: '==', left: {field: \"a;b\", source: a}, right: {value: 1}}\n"},
		{"unsafe field inside an exists subquery", joined + "where:\n  exists:\n    query:\n      from: {name: c, alias: c}\n      where: {op: '==', left: {field: \"a;b\", source: c}, right: {value: 1}}\n"},
		{"unsafe field in a null test", joined + "where: {isNull: {field: \"a;b\", source: a}}\n"},
		{"unsafe field in a not-null test", joined + "where: {isNotNull: {field: \"a;b\", source: a}}\n"},
		{"single source with an unsafe field", "from: {name: customers}\nwhere: {op: '==', left: {field: \"first;name\"}, right: {value: 1}}\n"},
		{"single source with an unsafe ordering", "from: {name: customers}\norderBy: [{field: \"a;b\"}]\n"},
		{"single source with an unsafe column", "from: {name: customers}\ncolumns: [{field: \"a'b\"}]\n"},
		{"database root with an unsafe field", "from: {database: chinook, name: Customer}\nwhere: {op: '==', left: {field: \"a;b\"}, right: {value: 1}}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ClassifyDTQL(mustDeserialize(t, tc.doc))
			if !errors.Is(err, ErrInvalidDTQL) {
				t.Fatalf("err = %v, want ErrInvalidDTQL (profile %+v)", err, profile)
			}
			if !reflect.DeepEqual(profile, Profile{}) {
				t.Fatalf("a refused query returned a profile: %+v", profile)
			}
		})
	}
}

// boundsQuery overrides the limit and offset of a query, which a DTQL document
// cannot set to a negative number.
type boundsQuery struct {
	dal.StructuredQuery
	limit, offset int
}

func (q boundsQuery) Limit() int  { return q.limit }
func (q boundsQuery) Offset() int { return q.offset }

// TestClassifyDTQLCapsApplyToTheOutermostQueryOnly: a nested query keeps the
// cursor, money and sign checks, and loses only the two caps.
func TestClassifyDTQLCapsApplyToTheOutermostQueryOnly(t *testing.T) {
	base := func() dal.StructuredQuery { return buildQuery(dal.From(rootRef("a"))).SelectIntoRecordset() }
	derived := func(inner dal.StructuredQuery) dal.StructuredQuery {
		return buildQuery(dal.From(dal.NewQuerySource(inner, "d"))).SelectIntoRecordset()
	}
	for _, tc := range []struct {
		name  string
		query dal.StructuredQuery
		rule  string
		path  string
	}{
		{"negative limit in a derived source", derived(boundsQuery{StructuredQuery: base(), limit: -1}), "limit", "from.query"},
		{"negative offset in a derived source", derived(boundsQuery{StructuredQuery: base(), offset: -1}), "offset", "from.query"},
		{"cursor in a derived source", derived(cursorQuery{StructuredQuery: base(), after: "x"}), "cursor", "from.query"},
		{"money in a derived source", derived(moneyQuery{base()}), "money", "from.query"},
		{"limit above the cap on the outermost query", boundsQuery{StructuredQuery: base(), limit: relationalMaxLimit + 1}, "limit", "$"},
		{"offset above the cap on the outermost query", boundsQuery{StructuredQuery: base(), offset: relationalMaxOffset + 1}, "offset", "$"},
		{"negative limit on the outermost query", boundsQuery{StructuredQuery: base(), limit: -1}, "limit", "$"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ClassifyDTQL(tc.query)
			assertRefusal(t, profile, err, tc.rule, tc.path)
		})
	}
	// A nested query over the caps is accepted.
	profile, err := ClassifyDTQL(derived(boundsQuery{StructuredQuery: base(), limit: relationalMaxLimit + 1, offset: relationalMaxOffset + 1}))
	if err != nil || profile.Kind != ProfileRelational || !profile.HasSubquery {
		t.Fatalf("profile = %+v err = %v", profile, err)
	}
}

// TestClassifyDTQLCollectionNameRefusalKeepsBothSentinels: a collection name
// /dtql refuses today is 400 invalid_key (the key rule's sentinel), and it must
// stay so when the document is classified: the refusal carries ErrInvalidKey as
// well as ErrInvalidDTQL.
func TestClassifyDTQLCollectionNameRefusalKeepsBothSentinels(t *testing.T) {
	for _, name := range []string{"../b", "..", "a\x00b"} {
		query := buildQuery(dal.From(rootRef(name))).SelectIntoRecordset()
		profile, err := ClassifyDTQL(query)
		assertRefusal(t, profile, err, "collection-name", "from")
		if !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%q: err = %v, want it to wrap ErrInvalidKey as well", name, err)
		}
	}
	// A refusal that is not about a key stays a DTQL refusal only.
	profile, err := ClassifyDTQL(mustDeserialize(t, "from: {schema: s, name: a, alias: a}\n"))
	assertRefusal(t, profile, err, "schema", "from")
	if errors.Is(err, ErrInvalidKey) {
		t.Errorf("a schema refusal must not be an invalid key: %v", err)
	}
}

// TestClassifyDTQLRefusesWhatParseDTQLAcceptsOnlyForMoney documents the one
// shape the single-collection profile accepts and the classifier refuses: the
// relational rules are checked for every document, single-collection ones
// included. (A schema-qualified root, a scan root and a database on the root
// are refused by ParseDTQL too since the query guard.)
func TestClassifyDTQLRefusesWhatParseDTQLAcceptsOnlyForMoney(t *testing.T) {
	const money = "from: {name: a}\nmoney: {minorUnitScale: 2, divisionScale: 4, rounding: halfEven}\n"
	if _, _, err := ParseDTQL([]byte(money)); err != nil {
		t.Fatalf("ParseDTQL refuses a money document: %v", err)
	}
	profile, err := ClassifyDTQL(mustDeserialize(t, money))
	assertRefusal(t, profile, err, "money", "$")
	for label, doc := range map[string]string{
		"schema-qualified root": "from: {schema: main, name: a}\n",
		"scan root":             "from: {name: a, scan: {limit: 5, orderBy: [{field: id}]}}\n",
	} {
		if _, _, err := ParseDTQL([]byte(doc)); !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("%s: ParseDTQL err = %v", label, err)
		}
	}
}

// nestedWhere builds a condition nested depth levels deep, and nestedArithmetic
// an expression.
func nestedWhere(depth int) dal.Condition {
	condition := dal.Condition(dal.NewComparison(dal.NewFieldRef("", "id"), dal.Equal, dal.NewFieldRef("", "id")))
	for i := 1; i < depth; i++ {
		condition = dal.NewGroupCondition(dal.And, condition)
	}
	return condition
}

func nestedArithmetic(depth int) dal.Expression {
	var expression dal.Expression = dal.NewFieldRef("", "id")
	for i := 1; i < depth; i++ {
		expression = dal.Binary(expression, dal.Add, dal.NewFieldRef("", "id"))
	}
	return expression
}

// TestClassifyDTQLNestingCap: the relational walk bounds how deep conditions
// and expressions may nest, as one more relational rule. (The query guard's own
// tighter limit still applies afterwards; this is the walk's own bound.)
func TestClassifyDTQLNestingCap(t *testing.T) {
	base := func() dal.StructuredQuery { return buildQuery(dal.From(rootRef("a"))).SelectIntoRecordset() }
	cases := map[string]func(depth int) dal.StructuredQuery{
		"condition": func(depth int) dal.StructuredQuery { return dal.WithWhere(base(), nestedWhere(depth)) },
		"expression": func(depth int) dal.StructuredQuery {
			return dal.WithColumns(base(), []dal.Column{{Expression: nestedArithmetic(depth)}})
		},
	}
	for label, build := range cases {
		t.Run(label, func(t *testing.T) {
			// At the cap the walk itself accepts the query.
			if err := (&profileWalk{}).query(build(relationalMaxNesting-2), 0); err != nil {
				t.Fatalf("below the cap: %v", err)
			}
			// Beyond it the walk refuses, naming the rule.
			err := (&profileWalk{}).query(build(relationalMaxNesting+2), 0)
			if !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), "relational profile: nesting at ") {
				t.Fatalf("above the cap: err = %v", err)
			}
			// ClassifyDTQL refuses it with a zero profile.
			profile, err := ClassifyDTQL(build(relationalMaxNesting + 2))
			if !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), "relational profile: nesting at ") || !reflect.DeepEqual(profile, Profile{}) {
				t.Fatalf("ClassifyDTQL: profile %+v err %v", profile, err)
			}
		})
	}
}

// TestClassifyDTQLCollectsSourcesFromEverySubqueryPosition: a subquery in
// GROUP BY, ORDER BY, HAVING, an aggregate argument or a JOIN ON adds its
// collection to Sources, so the caller authorises it. (Only the unknown-type
// refusals proved these positions are walked.) The documents are hand-built
// because a DTQL document cannot place a subquery in some of them.
func TestClassifyDTQLCollectsSourcesFromEverySubqueryPosition(t *testing.T) {
	inner := func(name string) dal.StructuredQuery {
		return buildQuery(dal.From(rootRef(name))).SelectIntoRecordset()
	}
	base := func() dal.StructuredQuery { return buildQuery(dal.From(rootRef("a"))).SelectIntoRecordset() }
	joinedOn := func(on dal.Condition) dal.StructuredQuery {
		from := dal.From(rootRef("a"))
		from.Join(dal.NewJoinedSource(rootRef("b"), dal.JoinInner, on))
		return buildQuery(from).SelectIntoRecordset()
	}
	for _, tc := range []struct {
		name    string
		query   dal.StructuredQuery
		sources []string
	}{
		{"group by", shapeQuery{StructuredQuery: base(), groupBy: []dal.Expression{dal.NewQueryExpression(inner("g"), "s")}}, []string{"a", "g"}},
		{"order by", shapeQuery{StructuredQuery: base(), orderBy: []dal.OrderExpression{dal.Ascending(dal.NewQueryExpression(inner("o"), "s"))}}, []string{"a", "o"}},
		{"having", shapeQuery{StructuredQuery: base(), having: dal.NewExistsCondition(inner("h"))}, []string{"a", "h"}},
		{"aggregate argument", dal.WithColumns(base(), []dal.Column{{Expression: dal.NewAggregate("sum", false, dal.NewQueryExpression(inner("m"), "s"))}}), []string{"a", "m"}},
		{"join on", joinedOn(dal.NewExistsCondition(inner("j"))), []string{"a", "b", "j"}},
		{"join on comparison", joinedOn(dal.NewComparison(dal.NewFieldRef("", "id"), dal.Equal, dal.NewQueryExpression(inner("k"), "s"))), []string{"a", "b", "k"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := ClassifyDTQL(tc.query)
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			var got []string
			for _, source := range profile.Sources {
				got = append(got, source.Collection)
			}
			if profile.Kind != ProfileRelational || !profile.HasSubquery || !reflect.DeepEqual(got, tc.sources) {
				t.Fatalf("profile = %+v, want relational subquery over %v", profile, tc.sources)
			}
		})
	}
}
