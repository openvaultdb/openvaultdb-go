package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// The walk of pkg/joinexec repeats the name rules of this package (the strict
// field-name rule ValidateFieldName, ValidateCollectionName and the identifier
// rule) and the nesting bound of the profile walk, because it does not import
// this package. These tests run documents through both and require the answer
// each table declares, so a change to one copy of a rule without the other fails
// here.
//
// The name check compared with the walk is the one the classifier runs on a
// relational document, validateRelationalNames (ClassifyDTQL calls it). A
// document the two answer differently is listed with the named difference that
// says why; there are two, and every other document gets the same answer from
// both.

// The differences between the walk and the classifier's name check. A row of the
// table that carries one is a document the two answer differently on purpose.
const (
	// differenceFieldNames: the classifier applies the wider quoted-name rule
	// (validateQuotedFieldName) to the field names of a relational document. The
	// walk applies the strict rule (ValidateFieldName) on every route, so Execute
	// refuses a field name that only the wider rule accepts. The document
	// classifies and does not run.
	differenceFieldNames = "field names: the classifier applies the wider quoted-name rule, the walk keeps the strict rule and refuses what only the wider rule accepts"
	// differenceQualifierScope: the classifier scopes a field qualifier to its own
	// query and the queries around it; the walk scopes it to the whole document.
	// The name is a validated collection name or alias either way.
	differenceQualifierScope = "qualifier scope: the classifier scopes a qualifier to its own query and the queries around it, the walk to the whole document"
)

// driftRegistry is never asked: the authorise function below denies everything.
type driftRegistry struct{}

func (driftRegistry) Lookup(string) (joinexec.Source, bool) { return nil, false }

// driftProfile is the profile of query, built from the document itself: every
// collection under a FROM tree, a derived source, an EXISTS or a scalar subquery,
// and whether any subquery is met. It does not come from ClassifyDTQL, which
// refuses some of the documents under test and would leave the walk with a
// profile that does not describe them.
func driftProfile(query dal.StructuredQuery) joinexec.Profile {
	collector := &driftCollector{}
	collector.query(query)
	return collector.profile
}

type driftCollector struct{ profile joinexec.Profile }

func (c *driftCollector) query(query dal.StructuredQuery) {
	c.from(query.From())
	c.condition(query.Where())
	for _, group := range query.GroupBy() {
		c.expression(group)
	}
	c.condition(query.Having())
	for _, order := range query.OrderBy() {
		c.expression(order.Expression())
	}
	for _, column := range query.Columns() {
		c.expression(column.Expression)
	}
}

func (c *driftCollector) from(from dal.FromSource) {
	c.source(from.Base())
	for _, join := range from.Joins() {
		child := join.From()
		if child == nil {
			child = dal.From(join.RecordsetSource)
		}
		c.from(child)
		for _, on := range join.On() {
			c.condition(on)
		}
	}
}

func (c *driftCollector) source(source dal.RecordsetSource) {
	switch value := source.(type) {
	case dal.CollectionRef:
		c.profile.Sources = append(c.profile.Sources, joinexec.ProfileSource{Database: value.Database(), Collection: value.Name()})
		for _, order := range value.ScanOrders() {
			c.expression(order.Expression())
		}
	case dal.QuerySource:
		c.profile.HasSubquery = true
		c.query(value.Query())
	}
}

func (c *driftCollector) condition(condition dal.Condition) {
	switch value := condition.(type) {
	case dal.Comparison:
		c.expression(value.Left)
		c.expression(value.Right)
	case dal.GroupCondition:
		for _, child := range value.Conditions() {
			c.condition(child)
		}
	case dal.IsNullCondition:
		c.expression(value.Operand())
	case dal.ExistsCondition:
		c.profile.HasSubquery = true
		c.query(value.Query())
	}
}

func (c *driftCollector) expression(expression dal.Expression) {
	switch value := expression.(type) {
	case dal.BinaryExpression:
		c.expression(value.Left)
		c.expression(value.Right)
	case dal.AggregateFunc:
		for _, arg := range value.FuncArgs() {
			c.expression(arg)
		}
	case dal.QueryExpression:
		c.profile.HasSubquery = true
		c.query(value.Query())
	}
}

// driftWalkAccepts reports whether the walk of pkg/joinexec accepts query. It
// runs joinexec.Execute with the profile of the document and an authorise
// function that denies every source: a document the walk accepts reaches
// authorisation and is denied there, and one it refuses never does. An
// ErrInvalidDocument that says the profile does not match the query is not a
// refusal by the walk, it is a defect of this helper, and fails the test.
func driftWalkAccepts(t *testing.T, query dal.StructuredQuery) bool {
	t.Helper()
	deny := func(string, string) bool { return false }
	_, err := joinexec.Execute(context.Background(), query, driftProfile(query), "db", driftRegistry{}, deny, joinexec.Limits{})
	var denied *joinexec.SourceDeniedError
	switch {
	case errors.As(err, &denied):
		return true
	case errors.Is(err, joinexec.ErrInvalidDocument):
		if strings.Contains(err.Error(), "the profile does not match the query") {
			t.Fatalf("the profile the test built does not describe the document: %v", err)
		}
		return false
	}
	t.Fatalf("Execute = %v, want a denial (the walk accepted the document) or ErrInvalidDocument", err)
	return false
}

func driftWith(source dal.RecordsetSource, mutate func(dal.IQueryBuilder) dal.IQueryBuilder, columns ...dal.Column) dal.StructuredQuery {
	var builder dal.IQueryBuilder = dal.From(source).NewQuery()
	if mutate != nil {
		builder = mutate(builder)
	}
	if len(columns) == 0 {
		return builder.SelectIntoRecord(nil)
	}
	return builder.SelectColumns(columns...)
}

func driftColumn(source, name string) dal.Column {
	return dal.Column{Expression: dal.NewFieldRef(source, name)}
}

func TestJoinexecWalkAndCoreAgreeOnNames(t *testing.T) {
	root := func(name, alias string) dal.CollectionRef { return dal.NewRootCollectionRef(name, alias) }
	where := func(c dal.Condition) func(dal.IQueryBuilder) dal.IQueryBuilder {
		return func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.Where(c) }
	}
	eq := func(left dal.Expression) dal.Condition {
		return dal.NewComparison(left, dal.Equal, dal.NewConstant(1))
	}
	long := func(n int) string { return strings.Repeat("x", n) }
	// "Order Details" is a source of the document, so it may qualify a field
	// although it is not an identifier.
	spaced := root("Order Details", "")
	derived := func(inner dal.StructuredQuery, alias string) dal.QuerySource { return dal.NewQuerySource(inner, alias) }
	innerWithSpaced := driftWith(spaced, nil, driftColumn("", "id"))
	// "x;y" is a name neither field-name rule accepts, so a field in any position
	// of a document is refused by both wherever the walk looks for it.
	const bad = "x;y"

	seen := map[string]bool{}
	for _, tc := range []struct {
		name  string
		query dal.StructuredQuery
		// classifier and walk are the answers the table declares: true is accepted.
		classifier, walk bool
		// difference names the difference of a document the two answer differently.
		difference string
	}{
		// Field names both rules accept or both refuse.
		{"plain field", driftWith(root("a", ""), nil, driftColumn("", "id")), true, true, ""},
		{"nested field", driftWith(root("a", ""), nil, driftColumn("", "address.city")), true, true, ""},
		{"key field", driftWith(root("a", ""), nil, driftColumn("", "$id")), true, true, ""},
		{"hyphenated field", driftWith(root("a", ""), nil, driftColumn("", "first-name")), true, true, ""},
		{"digit-leading segment", driftWith(root("a", ""), nil, driftColumn("", "byYear.2024")), true, true, ""},
		{"non-ASCII field", driftWith(root("a", ""), nil, driftColumn("", "Données")), true, true, ""},
		{"field with a combining mark in a segment", driftWith(root("a", ""), nil, driftColumn("", "Café")), true, true, ""},
		{"field with a combining mark in a nested segment", driftWith(root("a", ""), nil, driftColumn("", "a.Café")), true, true, ""},
		{"field in a script that joins letters with marks", driftWith(root("a", ""), nil, driftColumn("", "नमस्ते")), true, true, ""},
		{"field with a quote", driftWith(root("a", ""), nil, driftColumn("", `id"; DROP TABLE a; --`)), false, false, ""},
		{"field with a semicolon", driftWith(root("a", ""), nil, driftColumn("", "a;b")), false, false, ""},
		{"field with a backslash", driftWith(root("a", ""), nil, driftColumn("", `a\b`)), false, false, ""},
		{"field with a control character", driftWith(root("a", ""), nil, driftColumn("", "a\x00b")), false, false, ""},
		{"empty field", driftWith(root("a", ""), nil, driftColumn("", "")), false, false, ""},
		{"empty nested segment", driftWith(root("a", ""), nil, driftColumn("", "a..b")), false, false, ""},
		{"field of the longest length", driftWith(root("a", ""), nil, driftColumn("", long(256))), true, true, ""},
		{"field one byte too long", driftWith(root("a", ""), nil, driftColumn("", long(257))), false, false, ""},

		// Field names only the wider rule accepts: the classifier takes them, the
		// walk refuses them, and Execute fails closed on a document that classified.
		{"field with a space", driftWith(root("a", ""), nil, driftColumn("", "first name")), true, false, differenceFieldNames},
		{"field with a space in a nested segment", driftWith(root("a", ""), nil, driftColumn("", "address.zip code")), true, false, differenceFieldNames},
		{"field with a comment marker", driftWith(root("a", ""), nil, driftColumn("", "a--b")), true, false, differenceFieldNames},
		{"positional parameter as a field", driftWith(root("a", ""), nil, driftColumn("", "$1")), true, false, differenceFieldNames},
		{"field that starts with a combining mark", driftWith(root("a", ""), nil, driftColumn("", "́e")), true, false, differenceFieldNames},
		{"field with a slash", driftWith(root("a", ""), nil, driftColumn("", "a/b")), true, false, differenceFieldNames},

		// A field name in every position the walk reads.
		{"field in WHERE", driftWith(root("a", ""), where(eq(dal.NewFieldRef("", bad)))), false, false, ""},
		{"field in ORDER BY", driftWith(root("a", ""), func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.OrderBy(dal.AscendingField(bad)) }), false, false, ""},
		{"field in GROUP BY", driftWith(root("a", ""), func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.GroupBy(dal.NewFieldRef("", bad)) }, driftColumn("", "id")), false, false, ""},
		{"field in HAVING", driftWith(root("a", ""), func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.Having(eq(dal.NewFieldRef("", bad))) }, driftColumn("", "id")), false, false, ""},
		{"field in an aggregate", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewAggregate("sum", false, dal.NewFieldRef("", bad)), Alias: "s"}), false, false, ""},
		{"field in a binary expression", driftWith(root("a", ""), nil, dal.Column{Expression: dal.Binary(dal.NewFieldRef("", bad), dal.Add, dal.NewConstant(1)), Alias: "s"}), false, false, ""},
		{"field in a scan order", driftWith(root("a", "").WithScan(1, dal.AscendingField(bad)), nil), false, false, ""},
		{"field in a join condition", dal.From(root("a", "a")).Join(dal.NewJoinedSource(root("b", "b"), dal.JoinInner, eq(dal.NewFieldRef("a", bad)))).NewQuery().SelectIntoRecord(nil), false, false, ""},
		{"field in a subquery", driftWith(root("a", ""), where(dal.NewExistsCondition(driftWith(root("b", ""), where(eq(dal.NewFieldRef("", bad)))))), driftColumn("", "id")), false, false, ""},
		{"field in a null test", driftWith(root("a", ""), where(dal.NewIsNullCondition(dal.NewFieldRef("", bad)))), false, false, ""},

		// Aliases.
		{"plain alias", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewFieldRef("", "id"), Alias: "my_alias1"}), true, true, ""},
		{"alias with a space", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewFieldRef("", "id"), Alias: "my alias"}), false, false, ""},
		{"alias with a hyphen", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewFieldRef("", "id"), Alias: "my-alias"}), false, false, ""},
		{"alias that starts with a digit", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewFieldRef("", "id"), Alias: "1a"}), false, false, ""},
		{"non-ASCII alias", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewFieldRef("", "id"), Alias: "Données"}), false, false, ""},
		{"alias of the longest length", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewFieldRef("", "id"), Alias: long(256)}), true, true, ""},
		{"alias one byte too long", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewFieldRef("", "id"), Alias: long(257)}), false, false, ""},
		{"source alias", driftWith(root("a", "bad-alias"), nil), false, false, ""},
		{"derived source alias", driftWith(derived(driftWith(root("b", ""), nil), "bad alias"), nil), false, false, ""},
		{"result name of a scalar subquery", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewQueryExpression(driftWith(root("b", ""), nil), "total")}), true, true, ""},
		{"result name of a scalar subquery with a space", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewQueryExpression(driftWith(root("b", ""), nil), "bad name")}), false, false, ""},
		{"result name of a scalar subquery with a hyphen", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewQueryExpression(driftWith(root("b", ""), nil), "bad-name")}), false, false, ""},
		{"result name of a scalar subquery one byte too long", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewQueryExpression(driftWith(root("b", ""), nil), long(257))}), false, false, ""},
		{"scalar subquery without a result name", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewQueryExpression(driftWith(root("b", ""), nil), "")}), true, true, ""},

		// Qualifiers.
		{"qualifier that is an alias", driftWith(root("a", "x"), nil, driftColumn("x", "id")), true, true, ""},
		{"qualifier that is the collection", driftWith(root("a", ""), nil, driftColumn("a", "id")), true, true, ""},
		{"qualifier that names a spaced collection of the document", driftWith(spaced, nil, driftColumn("Order Details", "id")), true, true, ""},
		{"qualifier that names nothing and is an identifier", driftWith(root("a", ""), nil, driftColumn("zz", "id")), true, true, ""},
		{"qualifier that names nothing and is not an identifier", driftWith(root("a", ""), nil, driftColumn("zz-top", "id")), false, false, ""},
		{"qualifier with a space that names nothing", driftWith(root("a", ""), nil, driftColumn("a b", "id")), false, false, ""},
		{"qualifier of the outer query inside a subquery", driftWith(root("a", "x"), where(dal.NewExistsCondition(driftWith(root("b", ""), where(eq(dal.NewFieldRef("x", "id")))))), driftColumn("", "id")), true, true, ""},
		{"wildcard source that is an identifier", driftWith(root("a", ""), nil, dal.Column{Wildcard: &dal.WildcardProjection{Source: "a"}}), true, true, ""},
		{"wildcard source that is not an identifier", driftWith(root("a", ""), nil, dal.Column{Wildcard: &dal.WildcardProjection{Source: "x y"}}), false, false, ""},
		{"wildcard exclusion", driftWith(root("a", ""), nil, dal.Column{Wildcard: &dal.WildcardProjection{Exclude: []string{"secret"}}}), true, true, ""},
		{"wildcard exclusion that no rule accepts", driftWith(root("a", ""), nil, dal.Column{Wildcard: &dal.WildcardProjection{Exclude: []string{"a;b"}}}), false, false, ""},
		{"wildcard exclusion with a space", driftWith(root("a", ""), nil, dal.Column{Wildcard: &dal.WildcardProjection{Exclude: []string{"a b"}}}), true, false, differenceFieldNames},
		{
			"qualifier that names a source of a subquery only", driftWith(root("a", ""), where(dal.NewExistsCondition(innerWithSpaced)), driftColumn("Order Details", "id")), false, true,
			differenceQualifierScope,
		},

		// Parameters.
		{"plain parameter", driftWith(root("a", ""), where(dal.NewComparison(dal.NewFieldRef("", "id"), dal.Equal, dal.Param{Name: "city"}))), true, true, ""},
		{"parameter with a space", driftWith(root("a", ""), where(dal.NewComparison(dal.NewFieldRef("", "id"), dal.Equal, dal.Param{Name: "bad name"}))), false, false, ""},

		// Collection names.
		{"collection with a space", driftWith(root("Order Details", ""), nil), true, true, ""},
		{"collection with a hyphen", driftWith(root("order-items", ""), nil), true, true, ""},
		{"collection with dots inside a component", driftWith(root("a..b", ""), nil), true, true, ""},
		{"collection that is a dot", driftWith(root(".", ""), nil), false, false, ""},
		{"collection that is two dots", driftWith(root("..", ""), nil), false, false, ""},
		{"collection with a parent component", driftWith(root("a/../b", ""), nil), false, false, ""},
		{"collection with a backslash parent component", driftWith(root(`a\..\b`, ""), nil), false, false, ""},
		{"collection with a control character", driftWith(root("a\x00b", ""), nil), false, false, ""},
		{"collection with a newline", driftWith(root("a\nb", ""), nil), false, false, ""},
		{"collection of only separators", driftWith(root("//", ""), nil), false, false, ""},
		{"joined collection with a parent component", dal.From(root("a", "a")).Join(dal.NewJoinedSource(root("../b", "b"), dal.JoinInner, eq(dal.NewFieldRef("a", "id")))).NewQuery().SelectIntoRecord(nil), false, false, ""},
		{"collection of a subquery with a control character", driftWith(root("a", ""), where(dal.NewExistsCondition(driftWith(root("b\x01", ""), nil))), driftColumn("", "id")), false, false, ""},

		// Shapes the relational variant of the name check accepts and the
		// single-collection variant does not.
		{"null test", driftWith(root("a", ""), where(dal.NewIsNullCondition(dal.NewFieldRef("", "id")))), true, true, ""},
		{"source that names a database", driftWith(dal.NewDatabaseCollectionRef("hr", "", "a", ""), nil, driftColumn("", "id")), true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (tc.classifier != tc.walk) == (tc.difference == "") {
				t.Fatalf("a document the two answer differently names the difference, and only that one: classifier=%v walk=%v difference=%q", tc.classifier, tc.walk, tc.difference)
			}
			if tc.difference != "" {
				if tc.difference != differenceFieldNames && tc.difference != differenceQualifierScope {
					t.Fatalf("%q is not a named difference", tc.difference)
				}
				seen[tc.difference] = true
			}
			if got := validateRelationalNames(tc.query) == nil; got != tc.classifier {
				t.Errorf("the name check of the classifier accepts = %v, the table says %v", got, tc.classifier)
			}
			if got := driftWalkAccepts(t, tc.query); got != tc.walk {
				t.Errorf("the walk of pkg/joinexec accepts = %v, the table says %v", got, tc.walk)
			}
		})
	}
	// A difference no row exercises would be listed and untested.
	for _, difference := range []string{differenceFieldNames, differenceQualifierScope} {
		if !seen[difference] {
			t.Errorf("no row of the table exercises the difference %q", difference)
		}
	}
}

// The walk keeps the strict field-name rule of this package whatever the
// classifier accepts, so the strict rule itself is compared with it directly:
// every name below gets the same answer from ValidateFieldName and from the walk.
func TestJoinexecWalkFieldRuleIsTheStrictRuleOfCore(t *testing.T) {
	for _, name := range []string{
		"id", "$id", "$1", "$", "address.city", "address.$id", "first-name", "-first", "first-", "a--b", "byYear.2024", "2024", "_x",
		"Données", "Café", "a.Café", "́e", "a.́e", "नमस्ते", "$́e",
		"first name", "a b.c", " a", "a ", "a;b", `a"b`, "a'b", "a`b", `a\b`, "a/b", "a#b", "a[0]", "a\x00b", "a\nb", "a\tb",
		"", ".", "a.", ".a", "a..b", "\xff", strings.Repeat("x", 256), strings.Repeat("x", 257),
	} {
		t.Run(name, func(t *testing.T) {
			strict := ValidateFieldName(name) == nil
			query := driftWith(dal.NewRootCollectionRef("a", ""), nil, driftColumn("", name))
			if walk := driftWalkAccepts(t, query); walk != strict {
				t.Errorf("ValidateFieldName accepts %q = %v, the walk of pkg/joinexec accepts it = %v", name, strict, walk)
			}
		})
	}
}

// The walk counts conditions and expressions nested one inside the next the way
// the profile walk does and refuses past the same bound, so it neither refuses a
// document the classifier accepted nor lets through one it refused. Each shape is
// built at every size from one to a few levels past the bound; both must answer
// alike at every size, and the sizes must cover both answers.
func TestJoinexecWalkAndCoreAgreeOnTheNestingBound(t *testing.T) {
	field := func() dal.Expression { return dal.NewFieldRef("", "id") }
	comparison := func() dal.Condition { return dal.NewComparison(field(), dal.Equal, dal.NewConstant(1)) }
	groups := func(n int, leaf dal.Condition) dal.Condition {
		for i := 0; i < n; i++ {
			leaf = dal.NewGroupCondition(dal.And, leaf)
		}
		return leaf
	}
	binaries := func(n int, leaf dal.Expression) dal.Expression {
		for i := 0; i < n; i++ {
			leaf = dal.Binary(leaf, dal.Add, dal.NewConstant(1))
		}
		return leaf
	}
	root := func() dal.CollectionRef { return dal.NewRootCollectionRef("a", "") }
	inner := func(n int) dal.StructuredQuery {
		return driftWith(dal.NewRootCollectionRef("b", ""), func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.Where(groups(n, comparison())) })
	}
	for _, shape := range []struct {
		name  string
		build func(n int) dal.StructuredQuery
	}{
		{"group conditions in WHERE", func(n int) dal.StructuredQuery {
			return driftWith(root(), func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.Where(groups(n, comparison())) })
		}},
		{"binary expressions in a column", func(n int) dal.StructuredQuery {
			return driftWith(root(), nil, dal.Column{Expression: binaries(n, field())})
		}},
		{"group conditions in HAVING", func(n int) dal.StructuredQuery {
			return driftWith(root(), func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.Having(groups(n, comparison())) }, driftColumn("", "id"))
		}},
		{"group conditions in a join condition", func(n int) dal.StructuredQuery {
			return dal.From(dal.NewRootCollectionRef("a", "a")).Join(dal.NewJoinedSource(dal.NewRootCollectionRef("b", "b"), dal.JoinInner, groups(n, comparison()))).NewQuery().SelectIntoRecord(nil)
		}},
		{"levels split between a query and an EXISTS subquery", func(n int) dal.StructuredQuery {
			return driftWith(root(), func(b dal.IQueryBuilder) dal.IQueryBuilder {
				return b.Where(groups(n/2, dal.NewExistsCondition(inner(n-n/2))))
			})
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			var accepted, refused bool
			for n := 1; n <= relationalMaxNesting+4; n++ {
				query := shape.build(n)
				core := (&profileWalk{}).query(query, 0) == nil
				if walk := driftWalkAccepts(t, query); walk != core {
					t.Fatalf("at size %d the profile walk of pkg/core accepts = %v, the walk of pkg/joinexec accepts = %v", n, core, walk)
				}
				accepted, refused = accepted || core, refused || !core
			}
			if !accepted || !refused {
				t.Fatalf("the sizes tried must cover both answers: accepted = %v, refused = %v", accepted, refused)
			}
		})
	}
}
