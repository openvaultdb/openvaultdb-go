package joinexec

import (
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

func exDataRows(rows ...any) []record.Record {
	out := make([]record.Record, len(rows))
	for i, data := range rows {
		out[i] = record.NewRecordWithData(record.NewKeyWithID("c", i+1), data)
	}
	return out
}

func TestOrderedColumnsFollowTheSelectList(t *testing.T) {
	q := dal.From(exRef("", "A", "a")).NewQuery().SelectColumns(
		dal.Column{Expression: dal.NewFieldRef("a", "name"), Alias: "who"},
		dal.Column{Expression: dal.NewFieldRef("a", "city")},
		dal.CountAs(dal.Star(), "n"),
	)
	rows := exDataRows(map[string]any{"n": 1, "city": "Rio", "who": "x"})
	want := []string{"who", "city", "n"}
	if got := orderedColumns(q, rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
}

func TestOrderedColumnsOfAZeroRowResultComeFromTheQuery(t *testing.T) {
	scalar := dal.NewQueryExpression(exPlain("", "B"), "latest")
	projected := dal.From(exRef("", "A", "a")).NewQuery().SelectColumns(
		dal.Column{Expression: dal.NewFieldRef("a", "id")},
		dal.Column{Expression: scalar},
		dal.Column{Expression: dal.NewQueryExpression(exPlain("", "B"), "")},
		dal.Column{Expression: dal.Binary(dal.NewFieldRef("a", "x"), dal.Add, dal.Constant{Value: 1})},
	)
	if got, want := orderedColumns(projected, nil), []string{"id", "latest", "column_2", "column_3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("projected columns = %v, want %v", got, want)
	}
	aggregated := dal.From(exRef("", "A", "a")).NewQuery().GroupBy(dal.NewFieldRef("a", "k")).SelectColumns(
		dal.Column{Expression: dal.NewFieldRef("a", "k")},
		dal.Count(),
		dal.CountAs(dal.Star(), "n"),
	)
	if got, want := orderedColumns(aggregated, nil), []string{"k", "COUNT(*)", "n"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregated columns = %v, want %v", got, want)
	}
	if got := orderedColumns(exPlain("", "A"), nil); got == nil || len(got) != 0 {
		t.Fatalf("no columns and no rows = %#v, want an empty list", got)
	}
}

func TestOrderedColumnsPreferTheNameTheRowsCarry(t *testing.T) {
	// DALgo names an unaliased expression one way when it projects a join row
	// and another when it aggregates; the rows say which happened.
	q := dal.From(exRef("", "A", "a")).NewQuery().SelectColumns(
		dal.Column{Expression: dal.NewQueryExpression(exPlain("", "B"), "latest")},
		dal.Column{Expression: dal.Binary(dal.NewFieldRef("a", "x"), dal.Add, dal.Constant{Value: 1})},
	)
	for name, tc := range map[string]struct {
		keys map[string]any
		want []string
	}{
		"projection":  {map[string]any{"latest": 1, "column_1": 2}, []string{"latest", "column_1"}},
		"aggregation": {map[string]any{"(SUBQUERY AS latest)": 1, "(a.x + 1)": 2}, []string{"(SUBQUERY AS latest)", "(a.x + 1)"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := orderedColumns(q, exDataRows(tc.keys)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("columns = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOrderedColumnsExpandWildcardsInPlace(t *testing.T) {
	q := dal.From(exRef("", "A", "a")).NewQuery().SelectColumns(
		dal.Column{Expression: dal.NewFieldRef("a", "id"), Alias: "key"},
		dal.Column{Wildcard: &dal.WildcardProjection{Source: "a", Exclude: []string{"secret"}}},
		dal.Column{Expression: dal.NewFieldRef("a", "name"), Alias: "who"},
	)
	rows := exDataRows(
		map[string]any{"key": 1, "zeta": 1, "alpha": 2, "who": "x"},
		map[string]any{"key": 2, "alpha": 3, "beta": 4, "who": "y", "secret": 5},
	)
	// The wildcard takes the columns nothing else names, sorted, where it
	// stands, and leaves out the names it excludes. A row that carries an
	// excluded name anyway (DALgo's rows do not) still has it listed, last: no
	// field a row carries is left out of the columns.
	want := []string{"key", "alpha", "beta", "zeta", "who", "secret"}
	if got := orderedColumns(q, rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
}

func TestOrderedColumnsOfASelectStarAreTheSortedRowKeys(t *testing.T) {
	rows := exDataRows(map[string]any{"b": 1, "a": 2}, map[string]any{"c": 3})
	if got, want := orderedColumns(exPlain("", "A"), rows), []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
}

func TestOrderedColumnsKeepEveryFieldTheRowsCarry(t *testing.T) {
	// A key the select list does not explain is not lost.
	q := dal.From(exRef("", "A", "a")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "id")})
	rows := exDataRows(map[string]any{"id": 1, "extra": 2})
	if got, want := orderedColumns(q, rows), []string{"id", "extra"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
}

func TestOrderedColumnsIgnoreRowsThatAreNotObjects(t *testing.T) {
	type payload struct{ Name string }
	rows := exDataRows(payload{"x"}, map[string]any{"id": 1})
	if got, want := orderedColumns(exPlain("", "A"), rows), []string{"id"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
}

func TestOrderedColumnsNeverRepeatAName(t *testing.T) {
	q := dal.From(exRef("", "A", "a")).NewQuery().SelectColumns(
		dal.Column{Expression: dal.NewFieldRef("a", "id")},
		dal.Column{Expression: dal.NewFieldRef("b", "id")},
	)
	if got, want := orderedColumns(q, exDataRows(map[string]any{"id": 1})), []string{"id"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
}
