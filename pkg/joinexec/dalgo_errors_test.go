package joinexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
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
	return joinFixtureWith(t, guard, a, b)
}

// joinFixtureWith is joinFixture over executors the test configured.
func joinFixtureWith(t *testing.T, guard *Guard, a, b *fakeExecutor) dal.DatabaseResolver {
	t.Helper()
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

func TestGuardRecordsALeafErrorWhateverShapeDalgoGivesIt(t *testing.T) {
	// This is the reason Guard exists beyond the leaf: the answer is the typed
	// error the guard remembers, not what DALgo's own error says of it. Before
	// dalgo v0.89.6 DALgo wrapped a leaf's error as text, and the typed error was
	// lost unless the guard remembered it; now the chain reaches it, and a request
	// that recorded a failure still never ends well (a reader that ends with an
	// error wrapping io.EOF is read by DALgo as the end of a stream).
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
	const groups = MaxInMemoryGroups + 1
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
	// The limit is the one DALgo's message states (the engine ran to its own
	// bound), and the figure the server advertises is MaxInMemoryGroups: they are
	// one number, so a change of the constant alone fails here.
	budget := mustBudget(t, guard.Classify(err, RouteInMemory))
	if budget.Name != BudgetAggregationGroups || budget.Limit != MaxInMemoryGroups {
		t.Fatalf("budget = %+v, want the limit MaxInMemoryGroups = %d (dalgo error: %v)", budget, MaxInMemoryGroups, err)
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
		// A bound raised inside a derived source arrives flattened into the
		// message of a join_plan error built from these two formats.
		`fmt.Sprintf("cannot scan %s: %v", alias, err)`,
		`fmt.Sprintf("scan %s: %v", alias, err)`,
	},
	// The flattened-bound patterns read the text of these Error() formats:
	// "category at path: message", and "category: message" when the path is empty.
	"q_join_validate.go": {
		`fmt.Sprintf("%s at %s: %s", e.Category, e.Path, e.Message)`,
	},
	"q_subquery.go": {
		`fmt.Sprintf("%s: %s", e.Category, e.Message)`,
		`fmt.Sprintf("%s at %s: %s", e.Category, e.Path, e.Message)`,
	},
	"aggregation_execute.go": {
		`defaultMaxAggregationGroups = 100_000`,
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
			// A skip would pass silently and leave most of the mapped messages
			// unpinned, so an unreadable source fails the test.
			t.Fatalf("dalgo source not readable at %s: %v (run without -trimpath and with the module cache present)", dir, err)
		}
		for _, literal := range literals {
			if !strings.Contains(string(content), literal) {
				t.Errorf("%s no longer contains %q: update pkg/joinexec/dalgo_errors.go and this pin together", name, literal)
			}
		}
	}
}

// runOrdered runs orderedJoinQuery through DALgo and returns the error DALgo
// reports, whether at execution or while reading.
func runOrdered(t *testing.T, resolve dal.DatabaseResolver) error {
	t.Helper()
	reader, err := dal.ExecuteFederatedQuery(context.Background(), orderedJoinQuery(), resolve)
	if err == nil {
		_, err = dal.ReadAllToRecords(context.Background(), reader)
	}
	if err == nil {
		t.Fatal("expected DALgo to fail")
	}
	return err
}

func TestDalgoSourceErrorsSurviveClassify(t *testing.T) {
	// DALgo reports a source's failed scan as a join validation error whose
	// message carries the source's text and, since v0.89.6, whose chain reaches
	// the source's error. The answer does not rest on that chain alone: the guard
	// remembers the source's own error and Classify returns it, so a policy denial
	// stays a denial, a deadline stays a deadline and a backend failure is not a
	// bad query, and the answer is not DALgo's wrapper.
	denied := fmt.Errorf("%w: policy p, rule r", access.ErrAccessDenied)
	for name, cause := range map[string]error{
		"access denied":     denied,
		"deadline exceeded": context.DeadlineExceeded,
		"backend failure":   errBoom,
	} {
		for mode, configure := range map[string]func(a *fakeExecutor){
			"on open": func(a *fakeExecutor) { a.readErr = cause },
			"on read": func(a *fakeExecutor) { a.readerError = cause },
		} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				guard := NewGuard(allowAll, Limits{})
				a := &fakeExecutor{rows: map[string][]record.Record{"A": keyedRows("A", 3, func(i int) any { return i })}}
				b := &fakeExecutor{rows: map[string][]record.Record{"B": keyedRows("B", 3, func(i int) any { return i })}}
				configure(a)
				err := runOrdered(t, joinFixtureWith(t, guard, a, b))
				var flattened *dal.JoinValidationError
				if !errors.As(err, &flattened) || !errors.Is(err, cause) {
					t.Fatalf("DALgo is expected to report the failed scan as a join validation error whose chain reaches the cause, got %T: %v", err, err)
				}
				got := guard.Classify(err, RouteInMemory)
				if !errors.Is(got, cause) {
					t.Fatalf("Classify = %T %v, want the source's own error", got, got)
				}
				if se := mustSourceError(t, got); se.Database != "one" || se.Collection != "A" {
					t.Fatalf("source error = %+v", se)
				}
				if errors.As(got, &flattened) {
					t.Fatalf("Classify returned DALgo's rewrapped error: %v", got)
				}
			})
		}
	}
}

func TestSourceBudgetOnDalgosStreamingPathArrivesFromALaterNext(t *testing.T) {
	// A flat equality join without ORDER BY takes DALgo's streaming path: it
	// returns a reader and a nil error, rows flow, and the budget error arrives
	// from a later Next. Only a caller that drains before answering returns no
	// rows, which is what Guard.Collect does.
	newRun := func(t *testing.T) (*Guard, dal.RecordsReader) {
		t.Helper()
		guard := NewGuard(allowAll, Limits{MaxSourceRows: 5})
		resolve := joinFixture(t, guard, keyedRows("A", 20, func(i int) any { return i }), keyedRows("B", 3, func(i int) any { return i }))
		reader, err := dal.ExecuteFederatedQuery(context.Background(), joinQuery(), resolve)
		if err != nil {
			t.Fatalf("the streaming path returns its reader before the budget is hit: %v", err)
		}
		if reader == nil {
			t.Fatal("expected a reader")
		}
		return guard, reader
	}

	t.Run("draining ends in the budget error", func(t *testing.T) {
		guard, reader := newRun(t)
		n, err := drain(t, reader)
		if err == nil {
			t.Fatalf("drained %d rows without an error", n)
		}
		// Rows flow before the error: with 3 dimension rows and a budget of 5,
		// two fact rows are read and joined first. That is the behaviour Collect
		// exists for, so it is asserted rather than assumed.
		if n != 2 {
			t.Fatalf("rows delivered before the budget error = %d, want 2", n)
		}
		_ = reader.Close()
		if budget := mustBudget(t, guard.Classify(err, RouteInMemory)); budget.Name != BudgetSourceRows || budget.Limit != 5 {
			t.Fatalf("budget = %+v", budget)
		}
	})

	t.Run("Collect returns no rows", func(t *testing.T) {
		guard, reader := newRun(t)
		records, err := guard.Collect(context.Background(), reader, RouteInMemory)
		if records != nil {
			t.Fatalf("Collect returned %d rows for a request that overran its budget", len(records))
		}
		if budget := mustBudget(t, err); budget.Name != BudgetSourceRows || budget.Limit != 5 {
			t.Fatalf("budget = %+v", budget)
		}
	})
}

func TestDalgoBoundInsideADerivedSourceIsMapped(t *testing.T) {
	// DALgo flattens a bound raised inside a derived source into the message of
	// a join_plan error ("cannot scan d: query_limit at from: fetched_rows").
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": keyedRows("A", 10001, func(i int) any { return i })}}
	q := dal.From(dal.NewQuerySource(plainQuery("", "A"), "d")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "id")})
	reader, err := dal.ExecuteRecursiveQuery(context.Background(), exec, q)
	if err == nil {
		_, err = dal.ReadAllToRecords(context.Background(), reader)
	}
	if err == nil {
		t.Fatal("expected DALgo to refuse 10,001 rows behind a derived source")
	}
	budget := mustBudget(t, MapDalgoError(err, RouteInMemory))
	if budget.Name != BudgetJoinFetchedRows || budget.Limit != 10000 || budget.Path != "from" {
		t.Fatalf("budget = %+v (dalgo error: %v)", budget, err)
	}
}

func TestMapDalgoErrorFlattenedDerivedSourceBounds(t *testing.T) {
	cases := []struct {
		message string
		name    string
		limit   int64
		path    string
	}{
		{"cannot scan d: query_limit at from: fetched_rows", BudgetJoinFetchedRows, 10000, "from"},
		{"cannot scan d: query_limit at from.joins[0]: result_rows", BudgetJoinResultRows, 10000, "from.joins[0]"},
		{"cannot scan d: query_limit: retained_bytes", BudgetJoinRetainedBytes, 16 << 20, ""},
		{"cannot scan d: join_plan at from: relation scan exceeds row or byte bound", BudgetJoinScan, 0, "from"},
		{"scan d: join_plan at from.joins[1]: joined row bound exceeded", BudgetJoinRows, 10000, "from.joins[1]"},
	}
	for _, c := range cases {
		t.Run(c.message, func(t *testing.T) {
			in := &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: c.message}
			budget := mustBudget(t, MapDalgoError(in, RouteInMemory))
			want := BudgetError{Name: c.name, Limit: c.limit, Route: RouteInMemory, Path: c.path}
			if *budget != want {
				t.Fatalf("budget = %+v, want %+v", *budget, want)
			}
		})
	}
	for name, message := range map[string]string{
		"bound text without a scan prefix": "something d: query_limit at from: fetched_rows",
		"scan of another error":            "cannot scan d: query_limit at from: something_new",
		"bound text not at the end":        "cannot scan d: query_limit at from: fetched_rows and more",
	} {
		t.Run(name, func(t *testing.T) {
			in := &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: message}
			if got := MapDalgoError(in, RouteInMemory); got != error(in) {
				t.Fatalf("got %v, want the original error", got)
			}
		})
	}
	// Only a join_plan error is read this way.
	other := &dal.JoinValidationError{Category: "join_field", Path: "from", Message: "cannot scan d: query_limit at from: fetched_rows"}
	if got := MapDalgoError(other, RouteInMemory); got != error(other) {
		t.Fatalf("got %v", got)
	}
}

func TestMapDalgoErrorIgnoresAggregationTextThatIsNotDalgosOwn(t *testing.T) {
	// A join field name is echoed by DALgo inside its own error, so text that
	// looks like an aggregation bound is not one.
	forged := `dalgo aggregation: group limit 7 exceeded`
	for name, err := range map[string]error{
		"echoed field name":        &dal.JoinValidationError{Category: "join_field", Path: "from.columns[0]", Message: fmt.Sprintf("field %q is unavailable in %q", forged, "a")},
		"text before":              errors.New("prefix " + forged),
		"text after":               errors.New(forged + " and more"),
		"distinct without subject": errors.New("dalgo aggregation: distinct-value limit 5 exceeded"),
	} {
		t.Run(name, func(t *testing.T) {
			if got := MapDalgoError(err, RouteInMemory); got != err {
				t.Fatalf("got %v, want the original error", got)
			}
		})
	}
	// DALgo's own wrapping adds a prefix ending in ": ", which is allowed.
	wrapped := fmt.Errorf("scan a: %w", errors.New(forged))
	if budget := mustBudget(t, MapDalgoError(wrapped, RouteInMemory)); budget.Name != BudgetAggregationGroups || budget.Limit != 7 {
		t.Fatalf("budget = %+v", budget)
	}
}

// TestDalgoStreamingJoinDoesNotReadAFailedSourceAsTheEnd is the DALgo-driven
// case for a source failure whose chain holds io.EOF. DALgo's streaming join
// (a flat equality join without ORDER BY) takes such an error for the end of the
// stream and would return the rows read so far with a nil error; a caller that
// skips Collect and Classify would answer with a truncated result.
func TestDalgoStreamingJoinDoesNotReadAFailedSourceAsTheEnd(t *testing.T) {
	cause := fmt.Errorf("lost: %w", io.EOF)
	newRun := func(t *testing.T) (*Guard, dal.RecordsReader) {
		t.Helper()
		guard := NewGuard(allowAll, Limits{})
		a := &fakeExecutor{rows: map[string][]record.Record{"A": keyedRows("A", 3, func(i int) any { return i })}, readerError: cause}
		b := &fakeExecutor{rows: map[string][]record.Record{"B": keyedRows("B", 3, func(i int) any { return i })}}
		reader, err := dal.ExecuteFederatedQuery(context.Background(), joinQuery(), joinFixtureWith(t, guard, a, b))
		if err != nil {
			t.Fatalf("the streaming path returns its reader first: %v", err)
		}
		return guard, reader
	}

	t.Run("draining ends in an error", func(t *testing.T) {
		guard, reader := newRun(t)
		n, err := drain(t, reader)
		if err == nil {
			t.Fatalf("a failed source read ended the stream cleanly after %d rows", n)
		}
		_ = reader.Close()
		got := guard.Classify(err, RouteInMemory)
		if se := mustSourceError(t, got); !errors.Is(got, cause) || se.Database != "one" || se.Collection != "A" {
			t.Fatalf("Classify = %v", got)
		}
	})

	t.Run("Collect returns no rows and the source error", func(t *testing.T) {
		guard, reader := newRun(t)
		records, err := guard.Collect(context.Background(), reader, RouteInMemory)
		if records != nil {
			t.Fatalf("Collect returned %d rows for a failed read", len(records))
		}
		if se := mustSourceError(t, err); !errors.Is(err, cause) || se.Database != "one" || se.Collection != "A" {
			t.Fatalf("Collect = %v", err)
		}
	})
}

// TestDalgoOrderedJoinReadsABareEOFAsTheEnd: only some of DALgo's read paths
// take a bare io.EOF for the end of a stream; an ordered join tests for
// dal.ErrNoMoreRecords and would fail with "scan a: EOF". The leaf hands it the
// sentinel, so a source that ends with io.EOF ends cleanly on every path.
func TestDalgoOrderedJoinReadsABareEOFAsTheEnd(t *testing.T) {
	guard := NewGuard(allowAll, Limits{})
	a := &fakeExecutor{rows: map[string][]record.Record{"A": keyedRows("A", 3, func(i int) any { return i })}, readerError: io.EOF}
	b := &fakeExecutor{rows: map[string][]record.Record{"B": keyedRows("B", 3, func(i int) any { return i })}, readerError: io.EOF}
	reader, err := dal.ExecuteFederatedQuery(context.Background(), orderedJoinQuery(), joinFixtureWith(t, guard, a, b))
	if err != nil {
		t.Fatalf("execute: %v (recorded failure %v)", err, guard.Err())
	}
	rows, err := guard.Collect(context.Background(), reader, RouteInMemory)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if guard.Err() != nil {
		t.Fatalf("an io.EOF end of stream is not a failure: %v", guard.Err())
	}
}

// TestDalgoSourceCloseErrorSurvivesClassify: DALgo reports the error of a scan
// reader's Close as a join_plan diagnostic ("close scan a: ...") whose chain
// reaches the close error. The leaf records it, so Classify returns the source's
// own error, not DALgo's wrapper.
func TestDalgoSourceCloseErrorSurvivesClassify(t *testing.T) {
	guard := NewGuard(allowAll, Limits{})
	inner := &fakeExecutor{rows: map[string][]record.Record{"A": keyedRows("A", 3, func(i int) any { return i })}}
	b := &fakeExecutor{rows: map[string][]record.Record{"B": keyedRows("B", 3, func(i int) any { return i })}}
	sources := map[string]Source{
		"one": newSource("one", closeFailingExecutor{QueryExecutor: inner, err: errCloseFailed}),
		"two": newSource("two", b),
	}
	resolve := func(_ context.Context, database string) (dal.QueryExecutor, error) {
		return guard.Leaf(sources[database]), nil
	}
	err := runOrdered(t, resolve)
	var flattened *dal.JoinValidationError
	if !errors.As(err, &flattened) || !errors.Is(err, errCloseFailed) {
		t.Fatalf("DALgo is expected to report the close error as a join validation error whose chain reaches it, got %T: %v", err, err)
	}
	got := guard.Classify(err, RouteInMemory)
	if se := mustSourceError(t, got); !errors.Is(got, errCloseFailed) || se.Database != "one" || se.Collection != "A" {
		t.Fatalf("Classify = %T %v, want the source's own close error", got, got)
	}
}

// DALgo evaluates a derived source in the base position of a query by running its inner
// query, and flattens what that raises with %v into the message of a join_plan error. A
// refusal of the document inside it (a field that is not there, an ambiguous field)
// arrives as that text, not as the typed error it was, and without this a caller is
// answered a server fault for a document it can change. The category, the path (under
// the derived source: the path of the join_plan error, ".query", and the inner path) and
// the message are read back from the text and the typed error is returned.
func TestMapDalgoErrorFlattenedRefusalsInsideADerivedSourceAreRecovered(t *testing.T) {
	type refusal struct {
		join     bool
		category string
		path     string
		message  string
	}
	for name, tc := range map[string]struct {
		message string
		want    refusal
	}{
		"a field no source has": {`cannot scan recent: shape at columns[0]: field "Nope" is unavailable`,
			refusal{false, "shape", "from.query.columns[0]", `field "Nope" is unavailable`}},
		"a field of a source that is not in its list": {`cannot scan recent: join_field at columns[0].field: field "Nope" is unavailable in "i"`,
			refusal{true, "join_field", "from.query.columns[0].field", `field "Nope" is unavailable in "i"`}},
		"an ambiguous field": {"cannot scan joined: scope at columns[0]: ambiguous unqualified field CustomerId",
			refusal{false, "scope", "from.query.columns[0]", "ambiguous unqualified field CustomerId"}},
		"a scalar subquery of many rows": {"cannot scan d: cardinality at columns[1]: scalar subquery returned more than one row",
			refusal{false, "cardinality", "from.query.columns[1]", "scalar subquery returned more than one row"}},
		"an unknown alias": {`cannot scan d: join_scope at where.left.source: unknown alias "z"`,
			refusal{true, "join_scope", "from.query.where.left.source", `unknown alias "z"`}},
		"a shape of the query": {"cannot scan d: query_shape at where: a condition is required",
			refusal{false, "query_shape", "from.query.where", "a condition is required"}},
		"a shape of the join": {"cannot scan d: join_shape at from.joins[0].on: on must contain at least one predicate",
			refusal{true, "join_shape", "from.query.from.joins[0].on", "on must contain at least one predicate"}},
		"a key type": {"cannot scan d: join_key_type at from: keys differ",
			refusal{true, "join_key_type", "from.query.from", "keys differ"}},
		"an error without a path": {"cannot scan d: scope: no source",
			refusal{false, "scope", "from.query", "no source"}},
		"a message of several lines": {"cannot scan d: shape at columns[0]: first\nsecond",
			refusal{false, "shape", "from.query.columns[0]", "first\nsecond"}},
		"a read of the reader": {"scan d: shape at orderBy[0]: field \"x\" is unavailable",
			refusal{false, "shape", "from.query.orderBy[0]", `field "x" is unavailable`}},
		"a derived source inside a derived source": {`cannot scan e: join_plan at from: cannot scan d: shape at orderBy[0]: field "x" is unavailable`,
			refusal{false, "shape", "from.query.from.query.orderBy[0]", `field "x" is unavailable`}},
		"three levels, the middle without a path": {"cannot scan e: join_plan: cannot scan d: join_plan at from.joins[0]: cannot scan c: scope at columns[0]: ambiguous unqualified field k",
			refusal{false, "scope", "from.query.query.from.joins[0].query.columns[0]", "ambiguous unqualified field k"}},
		// The join_plan messages that are refusals of the document (IsJoinPlanRefusal), read the
		// same way, as the join_plan error DALgo raised.
		"a wildcard over a source with no field list": {"cannot scan d: join_plan at columns[0]: wildcard expansion requires ordered schema metadata",
			refusal{true, "join_plan", "from.query.columns[0]", "wildcard expansion requires ordered schema metadata"}},
		"an array that is not one": {"cannot scan d: join_plan at where: IN or NOT IN requires an array",
			refusal{true, "join_plan", "from.query.where", "IN or NOT IN requires an array"}},
		"an operator the join does not evaluate": {"cannot scan d: join_plan at where: unsupported operator <>",
			refusal{true, "join_plan", "from.query.where", "unsupported operator <>"}},
		"a null test of nothing, read by the reader": {"scan d: join_plan at where: IS NULL requires an operand",
			refusal{true, "join_plan", "from.query.where", "IS NULL requires an operand"}},
		"a type of condition the join does not evaluate": {"cannot scan d: join_plan at where: unsupported condition dal.weird",
			refusal{true, "join_plan", "from.query.where", "unsupported condition dal.weird"}},
		"a cursor": {"cannot scan d: join_plan at from: generic JOIN does not support provider cursors",
			refusal{true, "join_plan", "from.query.from", "generic JOIN does not support provider cursors"}},
		"a refusal of the join, without a path": {"cannot scan d: join_plan: IS NULL requires an operand",
			refusal{true, "join_plan", "from.query", "IS NULL requires an operand"}},
		"a derived source inside a derived source, the inner one a join_plan refusal": {"cannot scan e: join_plan at from: cannot scan d: join_plan at columns[0]: wildcard expansion requires ordered schema metadata",
			refusal{true, "join_plan", "from.query.from.query.columns[0]", "wildcard expansion requires ordered schema metadata"}},
	} {
		t.Run(name, func(t *testing.T) {
			in := &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: tc.message}
			for _, wrap := range []bool{false, true} {
				var err error = in
				if wrap {
					err = fmt.Errorf("while reading: %w", in)
				}
				got := MapDalgoError(err, RouteInMemory)
				var category, path, message string
				var join *dal.JoinValidationError
				var query *dal.QueryValidationError
				switch {
				case tc.want.join && errors.As(got, &join):
					category, path, message = join.Category, join.Path, join.Message
				case !tc.want.join && errors.As(got, &query):
					category, path, message = query.Category, query.Path, query.Message
				default:
					t.Fatalf("wrapped %v: got %T %v, want a refusal of join kind %v", wrap, got, got, tc.want.join)
				}
				if category != tc.want.category || path != tc.want.path || message != tc.want.message {
					t.Fatalf("wrapped %v: got %s at %s: %s", wrap, category, path, message)
				}
			}
		})
	}
}

func TestMapDalgoErrorLeavesTextThatIsNotAFlattenedRefusalAlone(t *testing.T) {
	for name, message := range map[string]string{
		"no scan prefix":                                           "something d: shape at columns[0]: field is unavailable",
		"a category that is not a refusal":                         "cannot scan d: weird at columns[0]: field is unavailable",
		"a bound that is not one DALgo reports":                    "cannot scan d: query_limit at from: something_new",
		"the failure of a read":                                    "cannot scan d: disk I/O error",
		"a failed read inside a derived source":                    "cannot scan e: join_plan at from: close scan d: boom",
		"text that is not a category at all":                       "cannot scan d: Shape at columns[0]: field is unavailable",
		"a category with nothing after it":                         "cannot scan d: shape at columns[0]",
		"a category glued to the word before it":                   "cannot scan d: reshape at columns[0]: field is unavailable",
		"a unknown refusal behind a derived one":                   "cannot scan e: join_plan at from: cannot scan d: weird at columns[0]: boom",
		"a join_plan message that is not a refusal":                "cannot scan d: join_plan at columns[0]: output is not JSON serializable: boom",
		"a join_plan message that is a refusal, not behind a scan": "join_plan at columns[0]: wildcard expansion requires ordered schema metadata",
		"a refusal that goes on after the words":                   "cannot scan d: join_plan at where: IS NULL requires an operand, or something else",
		"a bound that is a join_plan message":                      "cannot scan d: join_plan at from: relation scan exceeds row or byte bound and more",
	} {
		t.Run(name, func(t *testing.T) {
			in := &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: message}
			if got := MapDalgoError(in, RouteInMemory); got != error(in) {
				t.Fatalf("got %v, want the original error", got)
			}
		})
	}
	// Only a join_plan error is read this way.
	for _, in := range []error{
		&dal.JoinValidationError{Category: "join_field", Path: "from", Message: "cannot scan d: shape at columns[0]: field is unavailable"},
		&dal.QueryValidationError{Category: "shape", Path: "from", Message: "cannot scan d: shape at columns[0]: field is unavailable"},
		errors.New("join_plan at from: cannot scan d: shape at columns[0]: field is unavailable"),
	} {
		if got := MapDalgoError(in, RouteInMemory); got != in {
			t.Fatalf("got %v, want the original error %v", got, in)
		}
	}
}

// The join_plan messages of DALgo that are refusals of the document, a mistake of the
// caller who can change it, and not the text of a failed read, an encoding fault or a bound.
func TestIsJoinPlanRefusal(t *testing.T) {
	for message, want := range map[string]bool{
		"wildcard expansion requires ordered schema metadata": true,
		"generic JOIN does not support provider cursors":      true,
		"IN or NOT IN requires an array":                      true,
		"IS NULL requires an operand":                         true,
		"unsupported operator <>":                             true,
		"unsupported operator ":                               true,
		"unsupported expression dal.weird":                    true,
		"unsupported condition dal.weird":                     true,

		"cannot scan d: boom":                             false,
		"close scan d: boom":                              false,
		"cannot load fields for d: boom":                  false,
		"output is not JSON serializable: boom":           false,
		"joined row bound exceeded":                       false,
		"unsupported expression dal.weird and more":       false,
		"cannot scan c: unsupported expression dal.weird": false,
		"unsupported operatorx":                           false,
		"IS NULL requires an operand, or something else":  false,
		"": false,
	} {
		if got := IsJoinPlanRefusal(message); got != want {
			t.Errorf("IsJoinPlanRefusal(%q) = %v, want %v", message, got, want)
		}
	}
}

// Through Execute: a document that DALgo refuses inside a derived source in the base
// position, a wildcard over a source that supplies no field list, is the refusal DALgo gave,
// with the path under the derived source, and not the text of a failed scan. (The other
// join_plan refusals are read from the text in the table above: DALgo's recursive plan
// raises them as refusals of another category, which that table reads too.)
func TestAJoinPlanRefusalInsideADerivedSourceInTheBasePositionIsReturnedAsTheRefusal(t *testing.T) {
	derived := func(inner dal.StructuredQuery) dal.StructuredQuery {
		return dal.From(dal.NewQuerySource(inner, "d")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "name")})
	}
	for name, tc := range map[string]struct {
		query   dal.StructuredQuery
		path    string
		message string
	}{
		"a wildcard over a source with no field list": {
			derived(dal.From(exRef("", "A", "x")).NewQuery().SelectColumns(dal.Column{Wildcard: &dal.WildcardProjection{Source: "x", Exclude: []string{"none"}}})),
			"from.query.columns[0]", "wildcard expansion requires ordered schema metadata"},
		"a derived source inside a derived source": {
			derived(dal.From(dal.NewQuerySource(dal.From(exRef("", "A", "x")).NewQuery().SelectColumns(dal.Column{Wildcard: &dal.WildcardProjection{Source: "x", Exclude: []string{"none"}}}), "e")).
				NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("e", "name")})),
			"from.query.from.query.columns[0]", "wildcard expansion requires ordered schema metadata"},
	} {
		t.Run(name, func(t *testing.T) {
			mount := exMount("one", "ingitdb", false, map[string][]record.Record{"A": exRows("A", "a", 3)}) // no field list
			_, err := exRun(t, tc.query, "one", newExRegistry(mount), exAllow, Limits{})
			var refused *dal.JoinValidationError
			if !errors.As(err, &refused) || refused.Category != "join_plan" || refused.Path != tc.path || refused.Message != tc.message {
				t.Fatalf("got %T %v, want the join_plan refusal %q at %s", err, err, tc.message, tc.path)
			}
		})
	}
}
