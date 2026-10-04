package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// The name rules of this package (ValidateFieldName, ValidateCollectionName and
// the identifier rule of validateDTQLFields) are repeated by the walk of
// pkg/joinexec, which cannot import this package. This test runs one table of
// documents through both and requires the answer the table declares from each,
// so a change to one copy of a rule without the other fails here.
//
// Every document is unqualified: validateDTQLFields belongs to the
// single-collection profile and refuses a source that names a database, which
// the relational profile accepts. A document where the two answers differ on
// purpose says why.

// driftRegistry is never asked: the authorise function below denies everything.
type driftRegistry struct{}

func (driftRegistry) Lookup(string) (joinexec.Source, bool) { return nil, false }

// driftWalkAccepts reports whether the walk of pkg/joinexec accepts query. It
// runs joinexec.Execute with an authorise function that denies every source: a
// document the walk accepts reaches authorisation and is denied there, and one it
// refuses never does.
func driftWalkAccepts(t *testing.T, query dal.StructuredQuery) bool {
	t.Helper()
	profile := joinexec.Profile{Sources: []joinexec.ProfileSource{{Collection: query.From().Base().Name()}}}
	if classified, err := ClassifyDTQL(query); err == nil {
		profile = joinexec.Profile{HasSubquery: classified.HasSubquery}
		for _, source := range classified.Sources {
			profile.Sources = append(profile.Sources, joinexec.ProfileSource{Database: source.Database, Collection: source.Collection})
		}
	}
	deny := func(string, string) bool { return false }
	_, err := joinexec.Execute(context.Background(), query, profile, "db", driftRegistry{}, deny, joinexec.Limits{})
	var denied *joinexec.SourceDeniedError
	switch {
	case errors.As(err, &denied):
		return true
	case errors.Is(err, joinexec.ErrInvalidDocument):
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

	for _, tc := range []struct {
		name  string
		query dal.StructuredQuery
		// core and walk are the answers the table declares: true is accepted.
		core, walk bool
		// why explains a document the two answer differently.
		why string
	}{
		// Field names.
		{"plain field", driftWith(root("a", ""), nil, driftColumn("", "id")), true, true, ""},
		{"nested field", driftWith(root("a", ""), nil, driftColumn("", "address.city")), true, true, ""},
		{"key field", driftWith(root("a", ""), nil, driftColumn("", "$id")), true, true, ""},
		{"hyphenated field", driftWith(root("a", ""), nil, driftColumn("", "first-name")), true, true, ""},
		{"digit-leading segment", driftWith(root("a", ""), nil, driftColumn("", "byYear.2024")), true, true, ""},
		{"non-ASCII field", driftWith(root("a", ""), nil, driftColumn("", "Données")), true, true, ""},
		{"positional parameter as a field", driftWith(root("a", ""), nil, driftColumn("", "$1")), false, false, ""},
		{"field with a quote", driftWith(root("a", ""), nil, driftColumn("", `id"; DROP TABLE a; --`)), false, false, ""},
		{"field with a space", driftWith(root("a", ""), nil, driftColumn("", "first name")), false, false, ""},
		{"field with a comment marker", driftWith(root("a", ""), nil, driftColumn("", "a--b")), false, false, ""},
		{"field with a semicolon", driftWith(root("a", ""), nil, driftColumn("", "a;b")), false, false, ""},
		{"empty field", driftWith(root("a", ""), nil, driftColumn("", "")), false, false, ""},
		{"empty nested segment", driftWith(root("a", ""), nil, driftColumn("", "a..b")), false, false, ""},
		{"field of the longest length", driftWith(root("a", ""), nil, driftColumn("", long(256))), true, true, ""},
		{"field one byte too long", driftWith(root("a", ""), nil, driftColumn("", long(257))), false, false, ""},
		{"field in WHERE", driftWith(root("a", ""), where(eq(dal.NewFieldRef("", "x y")))), false, false, ""},
		{"field in ORDER BY", driftWith(root("a", ""), func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.OrderBy(dal.AscendingField("x y")) }), false, false, ""},
		{"field in GROUP BY", driftWith(root("a", ""), func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.GroupBy(dal.NewFieldRef("", "x y")) }, driftColumn("", "id")), false, false, ""},
		{"field in HAVING", driftWith(root("a", ""), func(b dal.IQueryBuilder) dal.IQueryBuilder { return b.Having(eq(dal.NewFieldRef("", "x y"))) }, driftColumn("", "id")), false, false, ""},
		{"field in an aggregate", driftWith(root("a", ""), nil, dal.Column{Expression: dal.NewAggregate("sum", false, dal.NewFieldRef("", "x y")), Alias: "s"}), false, false, ""},
		{"field in a binary expression", driftWith(root("a", ""), nil, dal.Column{Expression: dal.Binary(dal.NewFieldRef("", "x y"), dal.Add, dal.NewConstant(1)), Alias: "s"}), false, false, ""},
		{"field in a scan order", driftWith(root("a", "").WithScan(1, dal.AscendingField("x y")), nil), false, false, ""},
		{"field in a join condition", dal.From(root("a", "a")).Join(dal.NewJoinedSource(root("b", "b"), dal.JoinInner, eq(dal.NewFieldRef("a", "x y")))).NewQuery().SelectIntoRecord(nil), false, false, ""},
		{"field in a subquery", driftWith(root("a", ""), where(dal.NewExistsCondition(driftWith(root("b", ""), where(eq(dal.NewFieldRef("", "x y")))))), driftColumn("", "id")), false, false, ""},

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
		{"wildcard exclusion that is not a field name", driftWith(root("a", ""), nil, dal.Column{Wildcard: &dal.WildcardProjection{Exclude: []string{"a b"}}}), false, false, ""},
		{
			"qualifier that names a source of a subquery only", driftWith(root("a", ""), where(dal.NewExistsCondition(innerWithSpaced)), driftColumn("Order Details", "id")), false, true,
			"core scopes a qualifier to its own query and the queries around it; the walk scopes it to the whole document. The name is a validated collection name either way",
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

		// Shapes the two do not share.
		{
			"null test", driftWith(root("a", ""), where(dal.NewIsNullCondition(dal.NewFieldRef("", "id")))), false, true,
			"core's name check predates null tests and refuses the condition; the relational profile accepts it and the walk checks its operand",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (tc.core != tc.walk) == (tc.why == "") {
				t.Fatalf("a document the two answer differently says why, and only that one: core=%v walk=%v why=%q", tc.core, tc.walk, tc.why)
			}
			if got := validateDTQLFields(tc.query, 0) == nil; got != tc.core {
				t.Errorf("validateDTQLFields accepts = %v, the table says %v", got, tc.core)
			}
			if got := driftWalkAccepts(t, tc.query); got != tc.walk {
				t.Errorf("the walk of pkg/joinexec accepts = %v, the table says %v", got, tc.walk)
			}
		})
	}
}
