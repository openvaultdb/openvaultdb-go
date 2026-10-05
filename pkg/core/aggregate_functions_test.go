package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// The relational profile has five aggregate functions. first and last are not
// among them: a document that uses either, in any position and any letter case,
// is refused by the classifier with one fixed message that names the function.

// aggregateFunctionPositions are the places a document can put an aggregate call;
// the placeholder FN is the function name.
var aggregateFunctionPositions = map[string]string{
	"a column":           "from: {name: a, alias: a}\ncolumns: [{aggregate: {function: FN, args: [{field: x, source: a}]}, as: r}]\n",
	"where":              "from: {name: a, alias: a}\nwhere: {op: '>', left: {aggregate: {function: FN, args: [{field: x, source: a}]}}, right: {value: 1}}\n",
	"having":             "from: {name: a, alias: a}\ngroupBy: [{field: k, source: a}]\nhaving: {op: '>', left: {aggregate: {function: FN, args: [{field: x, source: a}]}}, right: {value: 1}}\n",
	"a derived source":   "from:\n  query:\n    as: d\n    from: {name: a, alias: a}\n    columns: [{aggregate: {function: FN, args: [{field: x, source: a}]}, as: r}]\n",
	"an EXISTS subquery": "from: {name: a, alias: a}\nwhere:\n  exists:\n    query:\n      from: {name: b, alias: b}\n      having: {op: '>', left: {aggregate: {function: FN, args: [{field: x, source: b}]}}, right: {value: 1}}\n",
}

// aggregateFunctionBuilt are the positions DALgo's deserializer does not let a
// document reach with an aggregate call (it checks the name only where it
// aggregates, and takes no aggregate in a grouping, an ordering or a join condition), built with the
// query builder, which is how any other caller of ClassifyDTQL reaches them.
func aggregateFunctionBuilt(function string) map[string]dal.StructuredQuery {
	call := func() dal.Expression { return dal.NewAggregate(function, false, dal.NewFieldRef("a", "x")) }
	base := func() dal.IQueryBuilder { return dal.From(dal.NewRootCollectionRef("a", "a")).NewQuery() }
	return map[string]dal.StructuredQuery{
		"order by": base().OrderBy(dal.Ascending(call())).SelectIntoRecord(nil),
		"group by": base().GroupBy(call()).SelectIntoRecord(nil),
		"a join condition": dal.From(dal.NewRootCollectionRef("a", "a")).Join(dal.NewJoinedSource(dal.NewRootCollectionRef("b", "b"), dal.JoinInner,
			dal.NewComparison(call(), dal.Equal, dal.NewFieldRef("b", "id")))).NewQuery().SelectIntoRecord(nil),
		"an argument": base().SelectColumns(dal.Column{Expression: dal.NewAggregate("sum", false, call()), Alias: "r"}),
	}
}

func TestClassifyDTQLRefusesFirstAndLastInEveryPosition(t *testing.T) {
	check := func(t *testing.T, function string, query dal.StructuredQuery) {
		t.Helper()
		profile, err := ClassifyDTQL(query)
		if !errors.Is(err, ErrInvalidDTQL) {
			t.Fatalf("err = %v, want ErrInvalidDTQL (profile %+v)", err, profile)
		}
		if profile.Kind != "" || len(profile.Sources) != 0 {
			t.Fatalf("a refused query returned a profile: %+v", profile)
		}
		want := strings.ToLower(function) + " is not in the relational profile: the aggregate functions are count, sum, avg, min, max"
		if !strings.Contains(err.Error(), "relational profile: aggregate-function at ") || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("err = %q, want the fixed refusal ending %q", err.Error(), want)
		}
	}
	for _, function := range []string{"first", "last", "FIRST", "LAST", "First", "lAsT"} {
		for position, query := range aggregateFunctionBuilt(function) {
			t.Run(position+" "+function, func(t *testing.T) { check(t, function, query) })
		}
	}
	for position, template := range aggregateFunctionPositions {
		for _, function := range []string{"first", "last", "FIRST", "LAST", "First", "lAsT"} {
			t.Run(position+" "+function, func(t *testing.T) {
				check(t, function, mustDeserialize(t, strings.ReplaceAll(template, "FN", function)))
			})
		}
	}
}

// A name that is not an aggregate function at all is refused with the same rule
// and a message that does not repeat it: the text is the caller's.
func TestClassifyDTQLRefusesAnUnknownAggregateWithoutEchoingIt(t *testing.T) {
	for _, function := range []string{"stddev", "median", "x; DROP TABLE a --"} {
		doc := strings.ReplaceAll(aggregateFunctionPositions["where"], "FN", "'"+function+"'")
		_, err := ClassifyDTQL(mustDeserialize(t, doc))
		if !errors.Is(err, ErrInvalidDTQL) || !strings.Contains(err.Error(), "relational profile: aggregate-function at ") {
			t.Fatalf("%q: err = %v", function, err)
		}
		if strings.Contains(err.Error(), function) || !strings.HasSuffix(err.Error(), "an aggregate function must be one of count, sum, avg, min, max") {
			t.Fatalf("%q: err = %q", function, err)
		}
	}
}

// The five names are classified in any letter case.
func TestClassifyDTQLAcceptsTheFiveAggregateFunctionsInAnyCase(t *testing.T) {
	for _, function := range []string{"count", "sum", "avg", "min", "max", "COUNT", "Sum", "aVg", "MIN", "Max"} {
		doc := strings.ReplaceAll(aggregateFunctionPositions["a column"], "FN", function)
		if profile, err := ClassifyDTQL(mustDeserialize(t, doc)); err != nil || profile.Kind != ProfileRelational {
			t.Fatalf("%s: profile %+v, err %v", function, profile, err)
		}
	}
}

// AggregateFunctions is the list discovery advertises: the five of the profile, in
// lower case, in this order. The caller may change the slice it gets without
// changing the list.
func TestAggregateFunctionsIsTheFiveOfTheProfile(t *testing.T) {
	got := AggregateFunctions()
	if strings.Join(got, ",") != "count,sum,avg,min,max" {
		t.Fatalf("AggregateFunctions = %v", got)
	}
	got[0] = "changed"
	if again := AggregateFunctions(); again[0] != "count" {
		t.Fatalf("the list changed through a copy: %v", again)
	}
	for _, name := range AggregateFunctions() {
		if !IsAggregateFunction(name) || !IsAggregateFunction(strings.ToUpper(name)) {
			t.Errorf("%q is listed and is not an aggregate function", name)
		}
	}
	for _, name := range []string{"first", "last", "FIRST", "Last", "stddev", ""} {
		if IsAggregateFunction(name) {
			t.Errorf("%q is an aggregate function of the profile", name)
		}
	}
}
