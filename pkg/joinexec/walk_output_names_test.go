package joinexec

import (
	"errors"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// Two columns of one query that carry the same output name cannot both reach the
// answer: a row is keyed by the name, so the later column would replace the
// earlier one on a database that runs the document and DALgo refuses the document
// in memory. The walk refuses it before anything is read, on both routes. The
// helpers of this file start with exOutput so they cannot clash with the others
// of the package.

func exOutputColumns(columns ...dal.Column) dal.StructuredQuery {
	return dal.WithColumns(exBase(), columns)
}

func exOutputField(source, name, alias string) dal.Column {
	return dal.Column{Expression: dal.NewFieldRef(source, name), Alias: alias}
}

func TestInspectRefusesTwoColumnsWithOneOutputName(t *testing.T) {
	long := strings.Repeat("n", 200)
	derived := func(columns ...dal.Column) dal.StructuredQuery {
		return dal.From(dal.NewQuerySource(exOutputColumns(columns...), "d")).NewQuery().SelectColumns(exOutputField("d", "id", ""))
	}
	for name, query := range map[string]dal.StructuredQuery{
		"the same field of two sources":                             exOutputColumns(exOutputField("i", "id", ""), exOutputField("c", "id", "")),
		"the same field twice":                                      exOutputColumns(exOutputField("", "id", ""), exOutputField("", "id", "")),
		"two aliases of one name":                                   exOutputColumns(exOutputField("", "total", "a"), exOutputField("", "name", "a")),
		"an alias that repeats the field name of another column":    exOutputColumns(exOutputField("i", "id", ""), exOutputField("c", "name", "id")),
		"a field name that repeats the alias of an earlier column":  exOutputColumns(exOutputField("c", "name", "id"), exOutputField("i", "id", "")),
		"an alias that repeats its own field name beside the field": exOutputColumns(exOutputField("", "id", "id"), exOutputField("c", "id", "")),
		"a scalar subquery named like a field": exOutputColumns(
			exOutputField("", "id", ""),
			dal.Column{Expression: dal.NewQueryExpression(exPlain("", "b"), "id")},
		),
		"two aggregates with no alias": exOutputColumns(
			dal.Column{Expression: dal.NewAggregate("count", false, dal.Star())},
			dal.Column{Expression: dal.NewAggregate("count", false, dal.Star())},
		),
		"a derived source": derived(exOutputField("", "id", ""), exOutputField("", "name", "id")),
		"a subquery of an EXISTS": dal.WithWhere(exBase(), dal.NewExistsCondition(
			exOutputColumns(exOutputField("", "id", ""), exOutputField("", "id", "")),
		)),
		"a name long enough to be clipped": exOutputColumns(exOutputField("", long+"1", long), exOutputField("", "id", long)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := inspect(query)
			if !errors.Is(err, ErrInvalidDocument) {
				t.Fatalf("err = %v, want ErrInvalidDocument", err)
			}
			if !strings.Contains(err.Error(), "duplicate output name") || errors.Is(err, ErrProfileMismatch) {
				t.Fatalf("err = %v, want a refusal that names the duplicate output name", err)
			}
			if strings.Contains(err.Error(), long) {
				t.Fatalf("the refusal repeats a long name whole: %v", err)
			}
		})
	}
}

func TestInspectAcceptsColumnsWhoseOutputNamesDiffer(t *testing.T) {
	for name, query := range map[string]dal.StructuredQuery{
		"two fields":                            exOutputColumns(exOutputField("i", "id", ""), exOutputField("c", "name", "")),
		"one field under two aliases":           exOutputColumns(exOutputField("", "id", "a"), exOutputField("", "id", "b")),
		"fields that differ in case":            exOutputColumns(exOutputField("", "id", ""), exOutputField("", "Id", "")),
		"a wildcard beside a field":             exOutputColumns(dal.Column{Wildcard: &dal.WildcardProjection{Source: "c"}}, exOutputField("i", "id", "")),
		"two wildcards":                         exOutputColumns(dal.Column{Wildcard: &dal.WildcardProjection{Source: "c"}}, dal.Column{Wildcard: &dal.WildcardProjection{Source: "i"}}),
		"a scalar subquery with another name":   exOutputColumns(exOutputField("", "id", ""), dal.Column{Expression: dal.NewQueryExpression(exPlain("", "b"), "n")}),
		"scalar subqueries with no name":        exOutputColumns(dal.Column{Expression: dal.NewQueryExpression(exPlain("", "b"), "")}, dal.Column{Expression: dal.NewQueryExpression(exPlain("", "b"), "")}),
		"two aggregates with different aliases": exOutputColumns(dal.Column{Expression: dal.NewAggregate("count", false, dal.Star()), Alias: "n"}, dal.Column{Expression: dal.NewAggregate("count", false, dal.Star()), Alias: "m"}),
		"a constant with no alias beside one": exOutputColumns(
			dal.Column{Expression: dal.NewConstant(1)}, dal.Column{Expression: dal.NewConstant(2)},
		),
		"the same name in an outer query and a derived source": dal.From(dal.NewQuerySource(
			exOutputColumns(exOutputField("", "id", ""), exOutputField("", "name", "")), "d",
		)).NewQuery().SelectColumns(exOutputField("d", "id", ""), exOutputField("d", "name", "")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := inspect(query); err != nil {
				t.Fatalf("inspect: %v", err)
			}
		})
	}
}
