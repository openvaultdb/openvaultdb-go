package joinexec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// The helpers of this file start with "sd" so that they cannot clash with the helpers of
// the tests around them.
//
// DALgo checks every field of a query against the lists its sources supply, in every
// clause, except for the query of an EXISTS that reads one source: that one is evaluated
// row by row, and a field its row does not have is read as a null, so a name the source
// does not carry (the declared spelling of a field a PostgreSQL mount holds in lower case,
// for one) matched nothing and the document was answered 200 with no rows. Such a field is
// refused, in every clause of that query, before anything is read.

// sdUnknown is a field of source a, the one source of the query of the EXISTS, that no list
// below carries.
var (
	sdUnknown   = dal.NewFieldRef("a", "nope")
	sdUnqualify = dal.NewFieldRef("", "nope")
	sdColumn    = dal.Column{Expression: dal.NewFieldRef("a", "x")}
)

func sdEquals(field dal.Expression) dal.Condition {
	return dal.NewComparison(field, dal.Equal, dal.NewConstant("v"))
}

// sdExists wraps the query build makes over source A (alias a) in the EXISTS of a query of
// source C.
func sdExists(build func(b dal.IQueryBuilder) dal.StructuredQuery) dal.StructuredQuery {
	inner := build(dal.From(exRef("", "A", "a")).NewQuery())
	return dal.From(exRef("", "C", "c")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
}

func TestAFieldTheListOfTheSourceOfAnExistsQueryDoesNotCarryIsRefusedInEveryClause(t *testing.T) {
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		path  string
	}{
		"WHERE, qualified": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(sdEquals(sdUnknown)).SelectColumns(sdColumn)
		}), "where.query.where.left"},
		"WHERE, the right operand": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(dal.NewConstant("v"), dal.Equal, sdUnknown)).SelectColumns(sdColumn)
		}), "where.query.where.right"},
		"WHERE, in a group": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewGroupCondition(dal.And, sdEquals(dal.NewFieldRef("a", "x")), sdEquals(sdUnknown))).SelectColumns(sdColumn)
		}), "where.query.where.conditions[1].left"},
		"WHERE, a null test": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewIsNullCondition(sdUnknown)).SelectColumns(sdColumn)
		}), "where.query.where.operand"},
		"WHERE, unqualified": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(sdEquals(sdUnqualify)).SelectColumns(sdColumn)
		}), "where.query.where.left"},
		"GROUP BY": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(sdUnknown).SelectColumns(srCount)
		}), "where.query.groupBy[0]"},
		"HAVING": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(dal.NewFieldRef("a", "x")).Having(dal.NewComparison(sdUnknown, dal.GreaterThen, srOne)).SelectColumns(srCount)
		}), "where.query.having.left"},
		"ORDER BY of a query that aggregates": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.GroupBy(dal.NewFieldRef("a", "x")).OrderBy(dal.Ascending(sdUnknown)).SelectColumns(srCount)
		}), "where.query.orderBy[0]"},
		"the EXISTS of an EXISTS": {func() dal.StructuredQuery {
			innermost := dal.From(exRef("", "A", "a")).NewQuery().Where(sdEquals(sdUnknown)).SelectColumns(sdColumn)
			return sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
				return b.Where(dal.NewExistsCondition(innermost)).SelectColumns(sdColumn)
			})
		}(), "where.query.where.query.where.left"},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := srSupplied()
			scope := scAsScope2(t, checkScopes(context.Background(), tc.query, supplier.fields), "shape")
			carrier := "the source a does not carry it"
			if name == "WHERE, unqualified" {
				carrier = "no source of the query carries it"
			}
			if scope.Path != tc.path || !strings.Contains(scope.Message, `unknown field "nope"`) || !strings.Contains(scope.Message, carrier) {
				t.Fatalf("got %+v, want an unknown field at %s that says %q", scope, tc.path, carrier)
			}
			if strings.Contains(scope.Message, "ORDER BY") {
				t.Errorf("the message names a clause the field is not in: %s", scope.Message)
			}
		})
	}
}

// What the refusal would take away from a document that is read right stays, and what DALgo
// checks itself is left to it.
func TestAFieldOfAnExistsQueryIsLeftAloneWhenItsListCarriesItOrDALgoChecksIt(t *testing.T) {
	for name, tc := range map[string]struct {
		query dal.StructuredQuery
		lists map[string][]string
		asked []string
	}{
		"a field of the list": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(sdEquals(dal.NewFieldRef("a", "x"))).SelectColumns(sdColumn)
		}), map[string][]string{"A": {"id", "x"}}, []string{"A"}},
		"an unqualified field of the list": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(sdEquals(dal.NewFieldRef("", "x"))).SelectColumns(sdColumn)
		}), map[string][]string{"A": {"id", "x"}}, []string{"A"}},
		"the key pseudo-field": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(sdEquals(dal.NewFieldRef("a", keyField))).SelectColumns(sdColumn)
		}), map[string][]string{"A": {"id", "x"}}, []string{"A"}},
		"a column the query selects, which does not change whether it finds a row": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.SelectColumns(dal.Column{Expression: sdUnknown}, dal.Column{Expression: dal.NewAggregate("SUM", false, sdUnknown), Alias: "s"})
		}), map[string][]string{"A": {"id", "x"}}, nil},
		"a source that supplies no list": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(sdEquals(sdUnknown)).SelectColumns(sdColumn)
		}), map[string][]string{}, []string{"A"}},
		"a field of the query around": {sdExists(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(dal.NewComparison(dal.NewFieldRef("a", "x"), dal.Equal, dal.NewFieldRef("c", "k"))).SelectColumns(sdColumn)
		}), map[string][]string{"A": {"x"}, "C": {"k"}}, []string{"A"}},
		"the query of an EXISTS that joins two sources, which DALgo checks": {func() dal.StructuredQuery {
			a, b := exRef("", "A", "a"), exRef("", "B", "b")
			inner := dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, dal.NewComparison(dal.NewFieldRef("a", "id"), dal.Equal, dal.NewFieldRef("b", "id")))).NewQuery().Where(sdEquals(sdUnknown)).SelectColumns(sdColumn)
			return dal.From(exRef("", "C", "c")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("c", "k")})
		}(), map[string][]string{"A": {"id", "x"}, "B": {"id"}}, nil},
		"a query that is not in an EXISTS, which DALgo checks": {srSingle(func(b dal.IQueryBuilder) dal.StructuredQuery {
			return b.Where(sdEquals(sdUnknown)).SelectColumns(sdColumn)
		}), map[string][]string{"A": {"id", "x"}}, nil},
		"a derived source, which DALgo checks": {dal.From(dal.NewQuerySource(
			dal.From(exRef("", "A", "a")).NewQuery().Where(sdEquals(sdUnknown)).SelectColumns(sdColumn), "d")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "x")}),
			map[string][]string{"A": {"id", "x"}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			supplier := &scSupplier{lists: tc.lists}
			if err := checkScopes(context.Background(), tc.query, supplier.fields); err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !reflect.DeepEqual(supplier.asked, tc.asked) {
				t.Fatalf("asked %v, want %v", supplier.asked, tc.asked)
			}
		})
	}
}

// Through Execute, over DALgo: the document of the PostgreSQL job (a field of the declared
// spelling in the query of an EXISTS, where the list holds the lower-case one) is refused,
// where it was answered with no rows, and the spelling the list holds still answers.
func TestADocumentWhoseExistsQueryNamesAFieldTheListLacksIsRefusedNotAnsweredEmpty(t *testing.T) {
	exists := func(field string) dal.StructuredQuery {
		inner := dal.From(exRef("", "A", "x")).NewQuery().
			Where(dal.NewComparison(dal.NewFieldRef("x", field), dal.Equal, dal.NewConstant("a1"))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("x", "id")})
		return dal.From(exRef("", "A", "p")).NewQuery().Where(dal.NewExistsCondition(inner)).SelectColumns(dal.Column{Expression: dal.NewFieldRef("p", "id")})
	}
	t.Run("the spelling the list lacks", func(t *testing.T) {
		mount := alMount()
		_, err := exRun(t, exists("Name"), "one", newExRegistry(mount), exAllow, Limits{})
		var refused *dal.QueryValidationError
		if !errors.As(err, &refused) || refused.Category != "shape" || !strings.Contains(refused.Message, `unknown field "Name"`) {
			t.Fatalf("got %T %v, want a shape refusal of the field", err, err)
		}
		if reads := len(mount.exec.seen()); reads != 0 {
			t.Fatalf("%d reads reached the mount for a refused document", reads)
		}
	})
	t.Run("the spelling the list holds", func(t *testing.T) {
		res, err := exRun(t, exists("name"), "one", newExRegistry(alMount()), exAllow, Limits{})
		if err != nil || len(res.Records) != 3 {
			t.Fatalf("rows = %d, err = %v, want the three rows", len(res.Records), err)
		}
	})
}

// What the refusal above does not reach: the scalar subquery of one source with no
// grouping, ordering, offset or aggregate is evaluated by DALgo on the same row-by-row path
// as the query of an EXISTS, which checks no field, so a field the list of its source lacks
// (the declared spelling, where the list holds the lower-case one) is read as a null there:
// in its WHERE the subquery finds no row, and in its columns the value is a null, and the
// document is answered. This pins the answer, so that the documentation of it stays as true
// as the code; refusing it is a decision of its own.
func TestAFieldTheListLacksInAScalarSubqueryOfOneSourceIsStillReadAsANull(t *testing.T) {
	scalar := func(where, selected string) dal.StructuredQuery {
		inner := dal.From(exRef("", "A", "x")).NewQuery().
			Where(dal.NewComparison(dal.NewFieldRef("x", where), dal.Equal, dal.NewConstant("a1"))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("x", selected)})
		return dal.From(exRef("", "A", "p")).NewQuery().
			OrderBy(dal.Ascending(dal.NewFieldRef("p", "id"))).
			SelectColumns(dal.Column{Expression: dal.NewFieldRef("p", "id")}, dal.Column{Expression: dal.NewQueryExpression(inner, "s")})
	}
	for name, tc := range map[string]struct {
		where, selected string
		want            any
	}{
		"the spellings the list holds":          {"name", "name", "a1"},
		"the spelling the list lacks, in WHERE": {"Name", "name", nil},
		"the spelling the list lacks, selected": {"name", "Name", nil},
	} {
		t.Run(name, func(t *testing.T) {
			mount := alMount()
			res, err := exRun(t, scalar(tc.where, tc.selected), "one", newExRegistry(mount), exAllow, Limits{})
			if err != nil || len(res.Records) != 3 {
				t.Fatalf("rows = %d, err = %v, want the three rows answered", len(res.Records), err)
			}
			// Every row of the outer query carries the one value of the subquery.
			for _, rec := range res.Records {
				if got := rec.Data().(map[string]any)["s"]; got != tc.want {
					t.Fatalf("s = %v, want %v (row %v)", got, tc.want, rec.Data())
				}
			}
		})
	}
}
