package joinexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

func newSource(id string, exec dal.QueryExecutor) *fakeSource {
	return &fakeSource{id: id, engine: "sqlite", exec: exec}
}

func TestLeafCountsRowsIntoStats(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"Customer": makeRows("Customer", 7, "x")}}
	src := newSource("chinook", exec)
	guard := NewGuard(allowAll, Limits{})
	ticks := []time.Time{time.Unix(100, 0), time.Unix(100, int64(250*time.Millisecond))}
	guard.now = func() time.Time { t0 := ticks[0]; ticks = append(ticks[1:], t0.Add(time.Hour)); return t0 }

	reader, err := guard.Leaf(src).ExecuteQueryToRecordsReader(context.Background(), plainQuery("chinook", "Customer"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := drain(t, reader)
	if err != nil || n != 7 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	stats := guard.Stats()
	want := SourceStats{Database: "chinook", Collection: "Customer", Rows: 7, Elapsed: 250 * time.Millisecond}
	if len(stats) != 1 || stats[0] != want {
		t.Fatalf("stats = %+v, want %+v", stats, want)
	}
	if guard.Err() != nil {
		t.Fatalf("guard err: %v", guard.Err())
	}
	if exec.closed != 1 {
		t.Fatalf("inner reader closed %d times", exec.closed)
	}
}

func TestLeafStatsMergePerCollectionAndKeepOrder(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{
		"Customer": makeRows("Customer", 3, "x"),
		"Invoice":  makeRows("Invoice", 5, "x"),
	}}
	src := newSource("chinook", exec)
	src.policies = true
	guard := NewGuard(allowAll, Limits{})
	leaf := guard.Leaf(src)
	read := func(collection string) {
		reader, err := leaf.ExecuteQueryToRecordsReader(context.Background(), plainQuery("", collection))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := drain(t, reader); err != nil {
			t.Fatal(err)
		}
		_ = reader.Close()
	}
	read("Invoice")
	read("Customer")
	read("Invoice")
	stats := guard.Stats()
	if len(stats) != 2 || stats[0].Collection != "Invoice" || stats[0].Rows != 10 || stats[1].Collection != "Customer" || stats[1].Rows != 3 {
		t.Fatalf("stats = %+v", stats)
	}
	if !stats[0].Protected || !stats[1].Protected {
		t.Fatalf("a source with access policies must be marked protected: %+v", stats)
	}
	// Stats returns a copy.
	stats[0].Rows = -1
	if guard.Stats()[0].Rows != 10 {
		t.Fatal("Stats must return a copy")
	}
}

func TestLeafRecordsStatsWhenReaderClosedEarly(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"Customer": makeRows("Customer", 5, "x")}}
	guard := NewGuard(allowAll, Limits{})
	reader, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "Customer"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := reader.Next(); err != nil {
			t.Fatal(err)
		}
	}
	_ = reader.Close()
	_ = reader.Close() // a second close must not double count
	if stats := guard.Stats(); len(stats) != 1 || stats[0].Rows != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestLeafRowBudgetReturnsBudgetError(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"Customer": makeRows("Customer", 10, "x")}}
	guard := NewGuard(allowAll, Limits{MaxSourceRows: 4})
	leaf := guard.Leaf(newSource("db", exec))
	reader, err := leaf.ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "Customer"))
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := drain(t, reader)
	budget := mustBudget(t, err)
	if budget.Name != BudgetSourceRows || budget.Limit != 4 || budget.Route != RouteInMemory || budget.Path != "db.Customer" {
		t.Fatalf("budget = %+v", budget)
	}
	if delivered != 4 {
		t.Fatalf("delivered %d rows, want exactly the 4 the budget admits", delivered)
	}
	// The row that crosses the budget is withheld, not returned with the error.
	if rec, err := reader.Next(); rec != nil || mustBudget(t, err) != budget {
		t.Fatalf("Next after overrun = %v, %v", rec, err)
	}
	if guard.Err() != error(budget) {
		t.Fatalf("guard must remember the first failure: %v", guard.Err())
	}
	// After the overrun the request is dead: no further read reaches a source.
	before := exec.queryCalls
	if _, err := leaf.ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "Customer")); mustBudget(t, err) != budget {
		t.Fatal("later reads must fail with the same budget error")
	}
	if exec.queryCalls != before {
		t.Fatal("a read after a failed budget reached the source")
	}
	// Only the rows delivered before the overrun are counted.
	if stats := guard.Stats(); len(stats) != 1 || stats[0].Rows != 4 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestLeafRowBudgetIsSharedAcrossLeaves(t *testing.T) {
	a := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 3, "x")}}
	b := &fakeExecutor{rows: map[string][]record.Record{"B": makeRows("B", 3, "x")}}
	guard := NewGuard(allowAll, Limits{MaxSourceRows: 5})
	ra, err := guard.Leaf(newSource("one", a)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("one", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := drain(t, ra); err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	rb, err := guard.Leaf(newSource("two", b)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("two", "B"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := drain(t, rb)
	if n != 2 {
		t.Fatalf("second source delivered %d rows before the shared budget ran out, want 2", n)
	}
	if mustBudget(t, err).Name != BudgetSourceRows {
		t.Fatalf("err = %v", err)
	}
}

func TestLeafByteBudgetReturnsBudgetError(t *testing.T) {
	payload := strings.Repeat("y", 100)
	exec := &fakeExecutor{rows: map[string][]record.Record{"Customer": makeRows("Customer", 10, payload)}}
	// One encoded row is about 125 bytes; 300 bytes admits two rows.
	guard := NewGuard(allowAll, Limits{MaxSourceBytes: 300})
	leaf := guard.Leaf(newSource("db", exec))
	reader, err := leaf.ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "Customer"))
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := drain(t, reader)
	budget := mustBudget(t, err)
	if budget.Name != BudgetSourceBytes || budget.Limit != 300 || budget.Route != RouteInMemory || budget.Path != "db.Customer" {
		t.Fatalf("budget = %+v", budget)
	}
	if delivered != 2 {
		t.Fatalf("delivered %d rows, want the 2 the byte budget admits", delivered)
	}
	if stats := guard.Stats(); len(stats) != 1 || stats[0].Rows != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestBudgetErrorDoesNotReportObservedFigures(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"Customer": makeRows("Customer", 9, "x")}}
	guard := NewGuard(allowAll, Limits{MaxSourceRows: 4})
	reader, _ := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "Customer"))
	_, err := drain(t, reader)
	text := err.Error()
	if !strings.Contains(text, "source_rows") || !strings.Contains(text, "4") || strings.Contains(text, "9") || strings.Contains(text, "5") {
		t.Fatalf("message must name the limit and nothing observed: %q", text)
	}
	noPath := (&BudgetError{Name: BudgetJoinScan, Route: RouteDatabase}).Error()
	if strings.Contains(noPath, " at ") {
		t.Fatalf("no path, no location: %q", noPath)
	}
}

func TestLeafDeniedCollectionIsNeverRead(t *testing.T) {
	exec := &fieldsExecutor{&fakeExecutor{rows: map[string][]record.Record{"Secret": makeRows("Secret", 3, "x")}, fields: []string{"id"}}}
	var asked [][2]string
	guard := NewGuard(func(database, collection string) bool {
		asked = append(asked, [2]string{database, collection})
		return false
	}, Limits{})
	leaf := guard.Leaf(newSource("vault", exec))

	_, err := leaf.ExecuteQueryToRecordsReader(context.Background(), plainQuery("vault", "Secret"))
	denied := mustDenied(t, err)
	if denied.Database != "vault" || denied.Collection != "Secret" {
		t.Fatalf("denied = %+v", denied)
	}
	if exec.queryCalls != 0 {
		t.Fatalf("a denied collection was read %d times", exec.queryCalls)
	}
	if len(asked) != 1 || asked[0] != [2]string{"vault", "Secret"} {
		t.Fatalf("authorize called with %v", asked)
	}
	if mustDenied(t, guard.Err()) != denied {
		t.Fatal("guard must keep the denial as the request failure")
	}
	if !strings.Contains(denied.Error(), "Secret") || !strings.Contains(denied.Error(), "vault") {
		t.Fatalf("message = %q", denied.Error())
	}
	if len(guard.Stats()) != 0 {
		t.Fatalf("a denied read must leave no stats: %+v", guard.Stats())
	}
}

func TestLeafJoinFieldsOfDeniedCollectionIsNeverServed(t *testing.T) {
	exec := &fieldsExecutor{&fakeExecutor{fields: []string{"secret_column"}}}
	guard := NewGuard(func(_, collection string) bool { return collection != "Secret" }, Limits{})
	provider := guard.Leaf(newSource("vault", exec)).(dal.JoinFieldsProvider)
	fields, err := provider.JoinFields(context.Background(), dal.NewDatabaseCollectionRef("vault", "", "Secret", "s"))
	mustDenied(t, err)
	if fields != nil || exec.fieldCalls != 0 || exec.queryCalls != 0 {
		t.Fatalf("fields=%v calls=%d/%d", fields, exec.fieldCalls, exec.queryCalls)
	}
}

func TestLeafDeniesWhenAuthorizeIsNil(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}}
	guard := NewGuard(nil, Limits{})
	_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	mustDenied(t, err)
	if exec.queryCalls != 0 {
		t.Fatal("nil Authorize must fail closed")
	}
}

func TestLeafAuthorizesTheCollectionOfEachRead(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"Open": makeRows("Open", 1, "x"), "Closed": makeRows("Closed", 1, "x")}}
	guard := NewGuard(func(database, collection string) bool { return database == "db" && collection == "Open" }, Limits{})
	// Failures are sticky, so use one guard per read to check each decision.
	reader, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "Open"))
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	_, err = guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "Closed"))
	mustDenied(t, err)
	if exec.queryCalls != 1 {
		t.Fatalf("source reads = %d, want 1", exec.queryCalls)
	}
}

func TestLeafRefusesAnotherDatabase(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}}
	guard := NewGuard(allowAll, Limits{})
	_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("other", "A"))
	denied := mustDenied(t, err)
	if denied.Database != "other" || denied.Collection != "A" {
		t.Fatalf("denied = %+v", denied)
	}
	if exec.queryCalls != 0 {
		t.Fatal("a leaf must never read a database other than its own")
	}
}

func TestLeafRefusesQueriesThatAreNotSingleSource(t *testing.T) {
	joined := dal.From(dal.NewDatabaseCollectionRef("db", "", "A", "a")).
		Join(dal.NewJoinedSource(dal.NewDatabaseCollectionRef("db", "", "B", "b"), dal.JoinInner)).
		NewQuery().SelectIntoRecord(nil)
	derived := dal.From(dal.NewQuerySource(plainQuery("db", "B"), "d")).NewQuery().SelectIntoRecord(nil)
	withSubquery := dal.From(dal.NewDatabaseCollectionRef("db", "", "A", "")).NewQuery().
		Where(dal.NewExistsCondition(plainQuery("db", "Secret"))).SelectIntoRecord(nil)
	for name, query := range map[string]dal.Query{
		"join":     joined,
		"derived":  derived,
		"subquery": withSubquery,
		"text":     dal.NewTextQuery("SELECT 1", nil),
	} {
		t.Run(name, func(t *testing.T) {
			exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}}
			guard := NewGuard(allowAll, Limits{})
			_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), query)
			if !errors.Is(err, ErrNotSingleSource) {
				t.Fatalf("err = %v, want ErrNotSingleSource", err)
			}
			if exec.queryCalls != 0 {
				t.Fatal("a refused query reached the source")
			}
			if !errors.Is(guard.Err(), ErrNotSingleSource) {
				t.Fatalf("guard err = %v", guard.Err())
			}
		})
	}
}

func TestLeafRefusesQueryWithoutFrom(t *testing.T) {
	exec := &fakeExecutor{}
	guard := NewGuard(allowAll, Limits{})
	_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), noFromQuery{})
	if !errors.Is(err, ErrNotSingleSource) {
		t.Fatalf("err = %v", err)
	}
}

func TestLeafRefusesSchemaQualifiedAndNestedCollections(t *testing.T) {
	parent := record.NewKeyWithID("Parent", 1)
	for name, ref := range map[string]dal.CollectionRef{
		"schema": dal.NewQualifiedRootCollectionRef("main", "A", ""),
		"nested": dal.NewCollectionRef("A", "", parent),
	} {
		t.Run(name, func(t *testing.T) {
			exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}}
			guard := NewGuard(allowAll, Limits{})
			q := dal.From(ref).NewQuery().SelectIntoRecord(nil)
			_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), q)
			if !errors.Is(err, ErrUnsupportedSource) {
				t.Fatalf("err = %v, want ErrUnsupportedSource", err)
			}
			if exec.queryCalls != 0 {
				t.Fatal("a refused source reached the executor")
			}
		})
	}
}

func TestLeafAcceptsPointerCollectionRef(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 2, "x")}}
	guard := NewGuard(allowAll, Limits{})
	ref := dal.NewDatabaseCollectionRef("db", "", "A", "")
	q := &pointerRefQuery{StructuredQuery: plainQuery("db", "A"), from: dal.From(&ref)}
	reader, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := drain(t, reader); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var nilRef *dal.CollectionRef
	bad := &pointerRefQuery{StructuredQuery: plainQuery("db", "A"), from: dal.From(nilRef)}
	if _, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), bad); !errors.Is(err, ErrNotSingleSource) {
		t.Fatalf("nil pointer ref: %v", err)
	}
}

func TestLeafRecordsetReaderIsNotSupported(t *testing.T) {
	exec := &fakeExecutor{}
	guard := NewGuard(allowAll, Limits{})
	_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsetReader(context.Background(), plainQuery("db", "A"))
	if !errors.Is(err, ErrRecordsetUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if exec.queryCalls != 0 {
		t.Fatal("the recordset path must not reach the source")
	}
}

func TestLeafSourceWithoutExecutorFails(t *testing.T) {
	src := newSource("db", nil)
	src.noExecutors = true
	guard := NewGuard(allowAll, Limits{})
	_, err := guard.Leaf(src).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	if !errors.Is(err, ErrNoExecutor) {
		t.Fatalf("err = %v", err)
	}
}

func TestLeafChecksContext(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 3, "x")}}
	guard := NewGuard(allowAll, Limits{})
	leaf := guard.Leaf(newSource("db", exec))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := leaf.ExecuteQueryToRecordsReader(cancelled, plainQuery("db", "A")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if exec.queryCalls != 0 {
		t.Fatal("a cancelled request reached the source")
	}

	ctx, cancel := context.WithCancel(context.Background())
	reader, err := leaf.ExecuteQueryToRecordsReader(ctx, plainQuery("db", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := reader.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next after cancel: %v", err)
	}
	if guard.Err() != nil {
		t.Fatalf("a cancelled context is not a guard failure: %v", guard.Err())
	}
	if stats := guard.Stats(); len(stats) != 1 || stats[0].Rows != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestLeafPassesExecutorErrorsThrough(t *testing.T) {
	exec := &fakeExecutor{readErr: errBoom}
	guard := NewGuard(allowAll, Limits{})
	_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if guard.Err() != nil {
		t.Fatalf("a backend error is not a guard failure: %v", guard.Err())
	}
	if len(guard.Stats()) != 0 {
		t.Fatal("a read that never opened leaves no stats")
	}
}

func TestLeafPassesReaderErrorsThroughAndKeepsStats(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 2, "x")}, readerError: errBoom}
	guard := NewGuard(allowAll, Limits{})
	reader, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := drain(t, reader)
	if n != 2 || !errors.Is(err, errBoom) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if stats := guard.Stats(); len(stats) != 1 || stats[0].Rows != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if guard.Err() != nil {
		t.Fatalf("guard err: %v", guard.Err())
	}
}

func TestLeafRowThatCannotBeEncodedFailsClosed(t *testing.T) {
	bad := record.NewRecordWithData(record.NewKeyWithID("A", 1), map[string]any{"f": func() {}})
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": {bad}}}
	guard := NewGuard(allowAll, Limits{})
	reader, _ := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	_, err := reader.Next()
	if !errors.Is(err, ErrRowNotEncodable) {
		t.Fatalf("err = %v", err)
	}
	if !errors.Is(guard.Err(), ErrRowNotEncodable) {
		t.Fatalf("guard err = %v", guard.Err())
	}
	// A reader whose request already failed refuses to deliver more rows.
	if _, err := reader.Next(); !errors.Is(err, ErrRowNotEncodable) {
		t.Fatalf("Next after failure: %v", err)
	}
}

func TestLeafPassesJoinFieldsThrough(t *testing.T) {
	exec := fieldsExecutor{&fakeExecutor{fields: []string{"id", "name"}}}
	guard := NewGuard(allowAll, Limits{})
	provider, ok := guard.Leaf(newSource("db", exec)).(dal.JoinFieldsProvider)
	if !ok {
		t.Fatal("leaf must implement dal.JoinFieldsProvider")
	}
	fields, err := provider.JoinFields(context.Background(), dal.NewDatabaseCollectionRef("db", "", "A", "a"))
	if err != nil || len(fields) != 2 || fields[0] != "id" || fields[1] != "name" {
		t.Fatalf("fields=%v err=%v", fields, err)
	}
	if exec.fieldCalls != 1 {
		t.Fatalf("fieldCalls = %d", exec.fieldCalls)
	}
}

func TestLeafJoinFieldsWithoutProviderReturnsNil(t *testing.T) {
	guard := NewGuard(allowAll, Limits{})
	provider := guard.Leaf(newSource("db", &fakeExecutor{})).(dal.JoinFieldsProvider)
	fields, err := provider.JoinFields(context.Background(), dal.NewDatabaseCollectionRef("db", "", "A", "a"))
	if err != nil || fields != nil {
		t.Fatalf("fields=%v err=%v", fields, err)
	}
}

func TestLeafJoinFieldsRefusesDerivedSourceAndFailedRequest(t *testing.T) {
	guard := NewGuard(allowAll, Limits{})
	provider := guard.Leaf(newSource("db", fieldsExecutor{&fakeExecutor{}})).(dal.JoinFieldsProvider)
	if _, err := provider.JoinFields(context.Background(), dal.NewQuerySource(plainQuery("db", "B"), "d")); !errors.Is(err, ErrNotSingleSource) {
		t.Fatalf("err = %v", err)
	}
	// The guard now holds a failure; every later call fails closed.
	if _, err := provider.JoinFields(context.Background(), dal.NewDatabaseCollectionRef("db", "", "A", "a")); !errors.Is(err, ErrNotSingleSource) {
		t.Fatalf("err = %v", err)
	}
}

func TestLimitsDefaultsFillZeroValues(t *testing.T) {
	g := NewGuard(allowAll, Limits{MaxSourceRows: -1, MaxSourceBytes: 0, Timeout: 0})
	d := DefaultLimits()
	if g.limits != d {
		t.Fatalf("limits = %+v, want defaults %+v", g.limits, d)
	}
	if d.MaxSourceRows != 100_000 || d.MaxSourceBytes != 64<<20 || d.Timeout != 10*time.Second {
		t.Fatalf("defaults = %+v", d)
	}
	custom := Limits{MaxSourceRows: 3, MaxSourceBytes: 4, Timeout: time.Second}
	if got := NewGuard(allowAll, custom).limits; got != custom {
		t.Fatalf("limits = %+v", got)
	}
}

func TestGuardContextAppliesTimeout(t *testing.T) {
	g := NewGuard(allowAll, Limits{Timeout: 3 * time.Second})
	before := time.Now()
	ctx, cancel := g.Context(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(2*time.Second)) || deadline.After(before.Add(5*time.Second)) {
		t.Fatalf("deadline = %v ok=%v", deadline, ok)
	}
}

func TestGuardClassifyPrefersTheRecordedFailure(t *testing.T) {
	g := NewGuard(allowAll, Limits{})
	if g.Classify(nil, RouteInMemory) != nil {
		t.Fatal("nil in, nil out")
	}
	plain := errors.New("something else")
	if got := g.Classify(plain, RouteInMemory); got != plain {
		t.Fatalf("an unrelated error must pass through unchanged, got %v", got)
	}
	// DALgo flattens a leaf error to text inside its own error; the recorded
	// failure is what the caller must see.
	denied := &SourceDeniedError{Database: "d", Collection: "c"}
	g.fail(denied)
	flattened := &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "scan c: " + denied.Error()}
	if got := g.Classify(flattened, RouteInMemory); got != error(denied) {
		t.Fatalf("Classify = %v, want the recorded denial", got)
	}
	// Only the first failure is kept.
	g.fail(errBoom)
	if g.Err() != error(denied) {
		t.Fatalf("Err = %v", g.Err())
	}
}

func TestGuardClassifyMapsDalgoBoundsWhenNothingRecorded(t *testing.T) {
	g := NewGuard(allowAll, Limits{})
	err := &dal.JoinValidationError{Category: "join_plan", Path: "from.joins[0]", Message: "joined row bound exceeded"}
	budget := mustBudget(t, g.Classify(err, RouteDatabase))
	if budget.Name != BudgetJoinRows || budget.Route != RouteDatabase || budget.Path != "from.joins[0]" {
		t.Fatalf("budget = %+v", budget)
	}
}

func TestChargeAfterFailureReturnsTheSameFailure(t *testing.T) {
	g := NewGuard(allowAll, Limits{MaxSourceRows: 1})
	if err := g.charge(1, "d.c"); err != nil {
		t.Fatal(err)
	}
	first := g.charge(1, "d.c")
	if mustBudget(t, first).Name != BudgetSourceRows {
		t.Fatalf("first = %v", first)
	}
	// A second reader charging after the failure sees the same failure.
	if second := g.charge(1, "d.other"); second != first {
		t.Fatalf("second = %v", second)
	}
}

// noFromQuery is a StructuredQuery with no FROM.
type noFromQuery struct{ dal.StructuredQuery }

func (noFromQuery) From() dal.FromSource { return nil }

// pointerRefQuery serves a chosen FromSource.
type pointerRefQuery struct {
	dal.StructuredQuery
	from dal.FromSource
}

func (q *pointerRefQuery) From() dal.FromSource { return q.from }
