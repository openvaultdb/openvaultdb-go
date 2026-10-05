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

// TestDeserializeDTQLRefusesAnUnknownAggregateWithoutRepeatingTheName: DALgo's
// deserializer quotes the caller's text when it refuses an aggregate function it does
// not know. The error DeserializeDTQL (and ParseDTQL, which uses it) returns wraps
// ErrInvalidDTQL, lists the functions of the profile and holds no text of the
// caller's, wherever the aggregate stands and whatever the quoting of the name; any
// other refusal keeps DALgo's message.
func TestDeserializeDTQLRefusesAnUnknownAggregateWithoutRepeatingTheName(t *testing.T) {
	const marker = "zzmarkerfn"
	documents := map[string]string{
		"a column": "from: {name: orders, alias: o}\ncolumns: [{aggregate: {function: NAME, args: [{field: total, source: o}]}, as: n}]\n",
		"HAVING":   "from: {name: orders, alias: o}\ngroupBy: [{field: c, source: o}]\nhaving: {op: '>', left: {aggregate: {function: NAME, args: [{field: total, source: o}]}}, right: {value: 1}}\ncolumns: [{field: c, source: o}]\n",
		"ORDER BY": "from: {name: orders, alias: o}\ngroupBy: [{field: c, source: o}]\norderBy: [{aggregate: {function: NAME, args: [{field: total, source: o}]}}]\ncolumns: [{field: c, source: o}]\n",
	}
	for position, document := range documents {
		for _, name := range []string{marker, `"quoted \" name; --"`, "'" + marker + "'"} {
			doc := strings.ReplaceAll(document, "NAME", name)
			for label, parse := range map[string]func() error{
				"DeserializeDTQL": func() error { _, err := DeserializeDTQL([]byte(doc)); return err },
				"ParseDTQL":       func() error { _, _, err := ParseDTQL([]byte(doc)); return err },
			} {
				err := parse()
				if !errors.Is(err, ErrInvalidDTQL) {
					t.Errorf("%s %s %s: %v, want an error that wraps ErrInvalidDTQL", position, name, label, err)
					continue
				}
				text := err.Error()
				if !strings.Contains(text, "an aggregate function must be one of count, sum, avg, min, max") {
					t.Errorf("%s %s %s: %q does not list the functions of the profile", position, name, label, text)
				}
				for _, echoed := range []string{"marker", "MARKER", "quoted", "QUOTED", "DROP"} {
					if strings.Contains(text, echoed) {
						t.Errorf("%s %s %s: %q repeats %q", position, name, label, text, echoed)
					}
				}
			}
		}
	}
	// Another refusal is DALgo's own, and a document that parses has no error.
	if _, err := DeserializeDTQL([]byte("from: {name: orders}\ncolumns: [{aggregate: {function: sum, args: [{field: a}, {field: b}]}}]\n")); !errors.Is(err, ErrInvalidDTQL) || strings.Contains(err.Error(), "must be one of") {
		t.Errorf("another refusal: %v", err)
	}
	if query, err := DeserializeDTQL([]byte("from: {name: orders}\n")); err != nil || query == nil {
		t.Errorf("a plain document: %v", err)
	}
}
