package joinexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// The mapping is pinned to DALgo's own error text. These tests fail when a
// dalgo bump rewords a bound, which is the signal to update dalgo_errors.go.
// The two layers: behavioural tests drive the real DALgo engine to each bound
// that can be reached cheaply, and a source pin checks every message literal
// exists in the dalgo version the module builds against.

func TestMapDalgoErrorTypedJoinBounds(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		category, message string
		query             bool
		name              string
		limit             int64
	}{
		{"join_plan", "joined row bound exceeded", false, BudgetJoinRows, 10000},
		{"join_plan", "joined byte bound exceeded", false, BudgetJoinRetainedBytes, 16 * mib},
		{"join_plan", "relation scan exceeds row or byte bound", false, BudgetJoinScan, 0},
		{"join_plan", "candidate evaluation bound exceeded", false, BudgetJoinCandidateEvaluations, 100000},
		{"query_limit", "result_rows", true, BudgetJoinResultRows, 10000},
		{"query_limit", "retained_bytes", true, BudgetJoinRetainedBytes, 16 * mib},
		{"query_limit", "fetched_rows", true, BudgetJoinFetchedRows, 10000},
		{"query_limit", "candidate_evaluations", true, BudgetJoinCandidateEvaluations, 100000},
	}
	for _, c := range cases {
		t.Run(c.category+"/"+c.message, func(t *testing.T) {
			var in error
			if c.query {
				in = &dal.QueryValidationError{Category: c.category, Path: "from.joins[1]", Message: c.message}
			} else {
				in = &dal.JoinValidationError{Category: c.category, Path: "from.joins[1]", Message: c.message}
			}
			// Wrapping must not hide the bound.
			out := MapDalgoError(fmt.Errorf("outer: %w", in), RouteInMemory)
			budget := mustBudget(t, out)
			want := BudgetError{Name: c.name, Limit: c.limit, Route: RouteInMemory, Path: "from.joins[1]"}
			if *budget != want {
				t.Fatalf("budget = %+v, want %+v", *budget, want)
			}
		})
	}
}

func TestMapDalgoErrorLeavesOtherJoinErrorsAlone(t *testing.T) {
	for name, err := range map[string]error{
		"join_plan other message": &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "generic JOIN does not support provider cursors"},
		"other category":          &dal.JoinValidationError{Category: "join_shape", Path: "from", Message: "joined row bound exceeded"},
		"query other message":     &dal.QueryValidationError{Category: "query_limit", Path: "query", Message: "something_new"},
		"query other category":    &dal.QueryValidationError{Category: "query_shape", Path: "query", Message: "result_rows"},
		"plain":                   errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			if got := MapDalgoError(err, RouteInMemory); got != err {
				t.Fatalf("got %v, want the original error", got)
			}
		})
	}
	if MapDalgoError(nil, RouteInMemory) != nil {
		t.Fatal("nil in, nil out")
	}
}

func TestMapDalgoErrorKeepsTypedSecurityAndBudgetErrors(t *testing.T) {
	denied := &SourceDeniedError{Database: "d", Collection: "c"}
	budget := &BudgetError{Name: BudgetSourceRows, Limit: 5, Route: RouteInMemory, Path: "d.c"}
	if MapDalgoError(denied, RouteDatabase) != error(denied) || MapDalgoError(budget, RouteDatabase) != error(budget) {
		t.Fatal("already typed errors must not be re-mapped")
	}
	wrapped := fmt.Errorf("x: %w", budget)
	if MapDalgoError(wrapped, RouteDatabase) != wrapped {
		t.Fatal("a wrapped typed error must pass through")
	}
}

func TestMapDalgoErrorAggregationLimits(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		message string
		name    string
		limit   int64
	}{
		{"dalgo aggregation: group limit 100000 exceeded", BudgetAggregationGroups, 100000},
		{"dalgo aggregation: aggregate-state limit 1000000 exceeded", BudgetAggregationStates, 1000000},
		{"dalgo aggregation: retained aggregation byte limit 67108864 exceeded", BudgetAggregationBytes, 64 * mib},
		{"dalgo aggregation: distinct-value limit 100000 exceeded for COUNT(DISTINCT x)", BudgetAggregationDistinctValues, 100000},
		{"dalgo aggregation: total distinct-value limit 1000000 exceeded", BudgetAggregationTotalDistinct, 1000000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			budget := mustBudget(t, MapDalgoError(fmt.Errorf("read: %w", errors.New(c.message)), RouteDatabase))
			want := BudgetError{Name: c.name, Limit: c.limit, Route: RouteDatabase}
			if *budget != want {
				t.Fatalf("budget = %+v, want %+v", *budget, want)
			}
		})
	}
	// The limit is read from the message, so a changed constant stays honest.
	budget := mustBudget(t, MapDalgoError(errors.New("dalgo aggregation: group limit 7 exceeded"), RouteInMemory))
	if budget.Limit != 7 {
		t.Fatalf("limit = %d, want 7", budget.Limit)
	}
}

// joinFixture returns a federated resolver over two in-memory sources and the
// Guard that wraps every read.
func joinFixture(t *testing.T, guard *Guard, aRows, bRows []record.Record) dal.DatabaseResolver {
	t.Helper()
	a := &fakeExecutor{rows: map[string][]record.Record{"A": aRows}}
	b := &fakeExecutor{rows: map[string][]record.Record{"B": bRows}}
	sources := map[string]Source{"one": newSource("one", a), "two": newSource("two", b)}
	return func(_ context.Context, database string) (dal.QueryExecutor, error) {
		return guard.Leaf(sources[database]), nil
	}
}

func joinQuery() dal.StructuredQuery {
	a := dal.NewDatabaseCollectionRef("one", "", "A", "a")
	b := dal.NewDatabaseCollectionRef("two", "", "B", "b")
	return dal.From(a).
		Join(dal.NewJoinedSource(b, dal.JoinInner, dal.NewComparison(dal.NewFieldRef("a", "k"), dal.Equal, dal.NewFieldRef("b", "k")))).
		NewQuery().SelectColumns(
		dal.Column{Expression: dal.NewFieldRef("a", "id"), Alias: "aid"},
		dal.Column{Expression: dal.NewFieldRef("b", "id"), Alias: "bid"},
	)
}

// orderedJoinQuery is joinQuery with ORDER BY, which keeps DALgo off its
// streaming path (the fact side of a flat unordered equality join is not
// bounded) and on the generic engine whose bounds are mapped.
func orderedJoinQuery() dal.StructuredQuery {
	a := dal.NewDatabaseCollectionRef("one", "", "A", "a")
	b := dal.NewDatabaseCollectionRef("two", "", "B", "b")
	return dal.From(a).
		Join(dal.NewJoinedSource(b, dal.JoinInner, dal.NewComparison(dal.NewFieldRef("a", "k"), dal.Equal, dal.NewFieldRef("b", "k")))).
		NewQuery().OrderBy(dal.Ascending(dal.NewFieldRef("a", "id"))).SelectColumns(
		dal.Column{Expression: dal.NewFieldRef("a", "id"), Alias: "aid"},
		dal.Column{Expression: dal.NewFieldRef("b", "id"), Alias: "bid"},
	)
}

func keyedRows(collection string, n int, key func(i int) any) []record.Record {
	rows := make([]record.Record, n)
	for i := range rows {
		rows[i] = record.NewRecordWithData(record.NewKeyWithID(collection, i+1), map[string]any{"id": i + 1, "k": key(i)})
	}
	return rows
}

func TestDalgoJoinBoundIsMappedToBudgetError(t *testing.T) {
	// 10,001 rows on one side exceeds DALgo's own scan bound while staying
	// inside the leaf's request budget, so the error comes from DALgo.
	guard := NewGuard(allowAll, Limits{})
	resolve := joinFixture(t, guard, keyedRows("A", 10001, func(i int) any { return i }), keyedRows("B", 1, func(i int) any { return i }))
	reader, err := dal.ExecuteFederatedQuery(context.Background(), orderedJoinQuery(), resolve)
	if err == nil {
		_, err = dal.ReadAllToRecords(context.Background(), reader)
	}
	if err == nil {
		t.Fatal("expected DALgo to refuse 10,001 rows on one side")
	}
	budget := mustBudget(t, guard.Classify(err, RouteInMemory))
	if budget.Name != BudgetJoinScan && budget.Name != BudgetJoinFetchedRows {
		t.Fatalf("budget = %+v (dalgo error: %v)", budget, err)
	}
}

func TestDalgoFlattensLeafErrorsSoGuardMustRecordThem(t *testing.T) {
	// This is the reason Guard exists beyond the leaf: DALgo wraps a leaf's
	// error as text, so the typed error is lost unless the guard remembers it.
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 3, "x")}}
	guard := NewGuard(func(database, collection string) bool { return database != "one" }, Limits{})
	src := newSource("one", exec)
	resolve := func(context.Context, string) (dal.QueryExecutor, error) { return guard.Leaf(src), nil }
	_, err := dal.ExecuteFederatedQuery(context.Background(), joinQuery(), resolve)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := mustDenied(t, guard.Classify(err, RouteInMemory)); got.Database != "one" || got.Collection != "A" {
		t.Fatalf("denied = %+v", got)
	}
	if exec.queryCalls != 0 {
		t.Fatalf("denied collection read %d times", exec.queryCalls)
	}
}

func TestDalgoAggregationGroupLimitIsMapped(t *testing.T) {
	// A flat unordered join feeding GROUP BY on a fact column streams the fact
	// side, so the group count (one per fact row here) hits DALgo's own bound.
	const groups = 100001
	guard := NewGuard(allowAll, Limits{MaxSourceRows: groups + 10, MaxSourceBytes: 1 << 30})
	aRows := make([]record.Record, groups)
	for i := range aRows {
		aRows[i] = record.NewRecordWithData(record.NewKeyWithID("A", i+1), map[string]any{"id": i + 1, "k": 1, "g": i})
	}
	bRows := []record.Record{record.NewRecordWithData(record.NewKeyWithID("B", 1), map[string]any{"id": 1, "k": 1})}
	resolve := joinFixture(t, guard, aRows, bRows)
	q := dal.From(dal.NewDatabaseCollectionRef("one", "", "A", "a")).
		Join(dal.NewJoinedSource(dal.NewDatabaseCollectionRef("two", "", "B", "b"), dal.JoinInner, dal.NewComparison(dal.NewFieldRef("a", "k"), dal.Equal, dal.NewFieldRef("b", "k")))).
		NewQuery().GroupBy(dal.NewFieldRef("a", "g")).
		SelectColumns(dal.Column{Expression: dal.NewFieldRef("a", "g")}, dal.Count())
	reader, err := dal.ExecuteFederatedQuery(context.Background(), q, resolve)
	if err == nil {
		_, err = dal.ReadAllToRecords(context.Background(), reader)
	}
	if err == nil {
		t.Fatal("expected DALgo to refuse more than 100,000 groups")
	}
	budget := mustBudget(t, guard.Classify(err, RouteInMemory))
	if budget.Name != BudgetAggregationGroups || budget.Limit != 100000 {
		t.Fatalf("budget = %+v (dalgo error: %v)", budget, err)
	}
}

func TestSourceBudgetThroughDalgoReturnsNoRows(t *testing.T) {
	// End to end for the acceptance line: a source budget overrun inside a
	// DALgo join yields an error and no reader, so no rows.
	guard := NewGuard(allowAll, Limits{MaxSourceRows: 5})
	resolve := joinFixture(t, guard, keyedRows("A", 20, func(i int) any { return i }), keyedRows("B", 3, func(i int) any { return i }))
	reader, err := dal.ExecuteFederatedQuery(context.Background(), orderedJoinQuery(), resolve)
	if err == nil {
		t.Fatalf("expected an error, got a reader %v", reader)
	}
	if reader != nil {
		t.Fatal("a budget overrun must not return a reader")
	}
	budget := mustBudget(t, guard.Classify(err, RouteInMemory))
	if budget.Name != BudgetSourceRows || budget.Limit != 5 {
		t.Fatalf("budget = %+v", budget)
	}
}

// pinnedMessages lists every DALgo literal the mapping depends on, with the
// file that holds it. TestDalgoSourceStillContainsPinnedMessages checks them
// against the dalgo version in go.mod.
var pinnedMessages = map[string][]string{
	"join_execute.go": {
		`queryError("query_limit", "columns", "result_rows")`,
		`queryError("query_limit", "columns", "retained_bytes")`,
		`joinError("join_plan", "columns", "joined byte bound exceeded")`,
		`queryError("query_limit", path, "fetched_rows")`,
		`queryError("query_limit", path, "retained_bytes")`,
		`joinError("join_plan", path, "relation scan exceeds row or byte bound")`,
		`joinError("join_plan", path, "joined row bound exceeded")`,
		`queryError("query_limit", path, "candidate_evaluations")`,
		`joinError("join_plan", path, "candidate evaluation bound exceeded")`,
		`maxJoinRows    = 10000`,
		`maxJoinBytes   = 16 << 20`,
		`maxJoinRows*10`,
	},
	"aggregation_execute.go": {
		`"dalgo aggregation: group limit %d exceeded"`,
		`"dalgo aggregation: aggregate-state limit %d exceeded"`,
		`"dalgo aggregation: retained aggregation byte limit %d exceeded"`,
		`"dalgo aggregation: distinct-value limit %d exceeded for %s"`,
		`"dalgo aggregation: total distinct-value limit %d exceeded"`,
	},
}

func TestDalgoSourceStillContainsPinnedMessages(t *testing.T) {
	// dal.NewRecordsReader lives in the dalgo module; its file path locates the
	// module source without running any process.
	pc := reflect.ValueOf(dal.NewRecordsReader).Pointer()
	file, _ := runtime.FuncForPC(pc).FileLine(pc)
	dir := filepath.Dir(file)
	for name, literals := range pinnedMessages {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Skipf("dalgo source not readable at %s (built with -trimpath?): %v", dir, err)
		}
		for _, literal := range literals {
			if !strings.Contains(string(content), literal) {
				t.Errorf("%s no longer contains %q: update pkg/joinexec/dalgo_errors.go and this pin together", name, literal)
			}
		}
	}
}
