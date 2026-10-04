package joinexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
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
	// A limit DALgo does not name is left out rather than printed as zero.
	if strings.Contains(noPath, "limit") || !strings.Contains(noPath, "join_scan") || !strings.Contains(noPath, RouteDatabase) {
		t.Fatalf("an unknown limit must not read as a zero limit: %q", noPath)
	}
	withPath := (&BudgetError{Name: BudgetSourceRows, Limit: 4, Route: RouteInMemory, Path: `db."odd" name`}).Error()
	if !strings.Contains(withPath, "limit 4") || !strings.Contains(withPath, `at "db.\"odd\" name"`) {
		t.Fatalf("request-supplied names are quoted: %q", withPath)
	}
}

func TestLeafRefusalsQuoteRequestSuppliedNames(t *testing.T) {
	parent := record.NewKeyWithID("Parent", 1)
	nested := dal.NewCollectionRef("we ird: \"A\"", "", parent)
	schema := dal.NewQualifiedRootCollectionRef("ma in", "A", "")
	for name, ref := range map[string]dal.CollectionRef{"nested": nested, "schema": schema} {
		t.Run(name, func(t *testing.T) {
			guard := NewGuard(allowAll, Limits{})
			q := dal.From(ref).NewQuery().SelectIntoRecord(nil)
			_, err := guard.Leaf(newSource("db", &fakeExecutor{})).ExecuteQueryToRecordsReader(context.Background(), q)
			if !strings.Contains(err.Error(), strconv.Quote(ref.Path())) {
				t.Fatalf("path not quoted: %q", err.Error())
			}
		})
	}
	// The same for a subquery refusal, which names the collection.
	odd := dal.NewDatabaseCollectionRef("db", "", "sp ace", "")
	withSubquery := dal.From(odd).NewQuery().Where(dal.NewExistsCondition(plainQuery("db", "Other"))).SelectIntoRecord(nil)
	guard := NewGuard(allowAll, Limits{})
	_, err := guard.Leaf(newSource("db", &fakeExecutor{})).ExecuteQueryToRecordsReader(context.Background(), withSubquery)
	if !strings.Contains(err.Error(), `"sp ace"`) {
		t.Fatalf("collection not quoted: %q", err.Error())
	}
	// And for a row that cannot be encoded.
	bad := record.NewRecordWithData(record.NewKeyWithID("a b", 1), map[string]any{"f": func() {}})
	exec := &fakeExecutor{rows: map[string][]record.Record{"a b": {bad}}}
	reader, _ := NewGuard(allowAll, Limits{}).Leaf(newSource("d b", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("d b", "a b"))
	if _, err := reader.Next(); !strings.Contains(err.Error(), `"d b"."a b"`) {
		t.Fatalf("source not quoted: %q", err.Error())
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
	_ = mustDenied(t, err)
	if fields != nil || exec.fieldCalls != 0 || exec.queryCalls != 0 {
		t.Fatalf("fields=%v calls=%d/%d", fields, exec.fieldCalls, exec.queryCalls)
	}
}

func TestLeafDeniesWhenAuthorizeIsNil(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}}
	guard := NewGuard(nil, Limits{})
	_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	_ = mustDenied(t, err)
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
	_ = mustDenied(t, err)
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

	// The cancellation above is a recorded failure, so read with a fresh guard.
	guard = NewGuard(allowAll, Limits{})
	leaf = guard.Leaf(newSource("db", exec))
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
	// The cancellation is recorded, so Classify can answer with it even when
	// DALgo flattens the error it saw.
	se := mustSourceError(t, guard.Err())
	if !errors.Is(se, context.Canceled) || se.Database != "db" || se.Collection != "A" {
		t.Fatalf("guard err = %+v", se)
	}
	if stats := guard.Stats(); len(stats) != 1 || stats[0].Rows != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestLeafRecordsDeadlineBeforeTheSourceIsReached(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}}
	guard := NewGuard(allowAll, Limits{})
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(ctx, plainQuery("db", "A"))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(guard.Classify(nil, RouteInMemory), context.DeadlineExceeded) {
		t.Fatalf("err = %v, classify = %v", err, guard.Classify(nil, RouteInMemory))
	}
	if exec.queryCalls != 0 {
		t.Fatal("an expired request reached the source")
	}
}

func TestLeafRecordsExecutorErrors(t *testing.T) {
	exec := &fakeExecutor{readErr: errBoom}
	guard := NewGuard(allowAll, Limits{})
	_, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	se := mustSourceError(t, err)
	if !errors.Is(err, errBoom) || se.Database != "db" || se.Collection != "A" {
		t.Fatalf("err = %v", err)
	}
	if guard.Err() != err {
		t.Fatalf("the source's own error must be the recorded failure: %v", guard.Err())
	}
	if len(guard.Stats()) != 0 {
		t.Fatal("a read that never opened leaves no stats")
	}
	if !strings.Contains(err.Error(), `"A"`) || !strings.Contains(err.Error(), `"db"`) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestLeafRecordsReaderErrorsAndKeepsStats(t *testing.T) {
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
	if se := mustSourceError(t, guard.Err()); se.Collection != "A" || !errors.Is(se, errBoom) {
		t.Fatalf("guard err = %+v", se)
	}
	// The request is dead: a later Next repeats the failure.
	if _, err := reader.Next(); !errors.Is(err, errBoom) {
		t.Fatalf("Next after failure: %v", err)
	}
}

func TestLeafTreatsBareEOFAsEndOfStream(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}, readerError: io.EOF}
	guard := NewGuard(allowAll, Limits{})
	reader, _ := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	// DALgo reads a bare io.EOF as the end of the stream on two of its read
	// paths only (a streaming join and ReadAllToRecords); an ordered join and an
	// aggregate scan test for dal.ErrNoMoreRecords. The leaf hands over the
	// sentinel they all recognise.
	if _, err := reader.Next(); err != dal.ErrNoMoreRecords {
		t.Fatalf("a bare io.EOF must reach DALgo as dal.ErrNoMoreRecords, got %v", err)
	}
	if guard.Err() != nil {
		t.Fatalf("a bare io.EOF is the end of the stream, not a failure: %v", guard.Err())
	}
	if n, err := drain(t, reader); n != 0 || err != nil {
		t.Fatalf("drain after the end: n=%d err=%v", n, err)
	}
}

func TestLeafPassesTheEndOfStreamSentinelsThrough(t *testing.T) {
	for name, end := range map[string]error{
		"no more records": dal.ErrNoMoreRecords,
		"limit reached":   dal.ErrLimitReached,
	} {
		t.Run(name, func(t *testing.T) {
			exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}, readerError: end}
			guard := NewGuard(allowAll, Limits{})
			reader, _ := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
			_, _ = reader.Next()
			if _, err := reader.Next(); err != end {
				t.Fatalf("Next = %v, want %v unchanged", err, end)
			}
			if guard.Err() != nil {
				t.Fatalf("an end of stream is not a failure: %v", guard.Err())
			}
		})
	}
}

func TestLeafRecordsAWrappedEOFAsAFailure(t *testing.T) {
	// DALgo reads any error wrapping io.EOF as the end of a stream, so a source
	// error that wraps it would pass for a clean end unless the guard remembers it.
	wrapped := fmt.Errorf("connection lost: %w", io.EOF)
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}, readerError: wrapped}
	guard := NewGuard(allowAll, Limits{})
	reader, _ := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	_, _ = drain(t, reader)
	if !errors.Is(guard.Err(), wrapped) {
		t.Fatalf("guard err = %v", guard.Err())
	}
	if got := guard.Classify(nil, RouteInMemory); !errors.Is(got, wrapped) {
		t.Fatalf("Classify(nil) = %v, want the recorded failure", got)
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

func TestLeafJoinFieldsRecordsProviderErrors(t *testing.T) {
	exec := fieldsExecutor{&fakeExecutor{fieldsErr: errBoom}}
	guard := NewGuard(allowAll, Limits{})
	provider := guard.Leaf(newSource("db", exec)).(dal.JoinFieldsProvider)
	_, err := provider.JoinFields(context.Background(), dal.NewDatabaseCollectionRef("db", "", "A", "a"))
	se := mustSourceError(t, err)
	if !errors.Is(err, errBoom) || se.Database != "db" || se.Collection != "A" || guard.Err() != err {
		t.Fatalf("err = %v, guard = %v", err, guard.Err())
	}
}

func TestLeafJoinFieldsServesNothingForADerivedSource(t *testing.T) {
	// DALgo asks for the fields of every FROM node, derived or not, before it
	// runs the inner query through the executor. A derived source has no schema
	// to serve and reads nothing, so it must not fail the request: the inner
	// query is authorised when it reads.
	exec := fieldsExecutor{&fakeExecutor{fields: []string{"secret"}}}
	guard := NewGuard(allowAll, Limits{})
	provider := guard.Leaf(newSource("db", exec)).(dal.JoinFieldsProvider)
	derived := dal.NewQuerySource(plainQuery("db", "B"), "d")
	for name, source := range map[string]dal.RecordsetSource{"value": derived, "pointer": &derived} {
		fields, err := provider.JoinFields(context.Background(), source)
		if err != nil || fields != nil {
			t.Fatalf("%s: fields=%v err=%v", name, fields, err)
		}
	}
	if exec.fieldCalls != 0 || exec.queryCalls != 0 || guard.Err() != nil {
		t.Fatalf("calls=%d/%d guard=%v", exec.fieldCalls, exec.queryCalls, guard.Err())
	}
}

func TestLeafJoinFieldsRefusesOtherSourcesAndFailedRequests(t *testing.T) {
	guard := NewGuard(allowAll, Limits{})
	provider := guard.Leaf(newSource("db", fieldsExecutor{&fakeExecutor{}})).(dal.JoinFieldsProvider)
	var nilDerived *dal.QuerySource
	if _, err := provider.JoinFields(context.Background(), nilDerived); !errors.Is(err, ErrNotSingleSource) {
		t.Fatalf("err = %v", err)
	}
	// The guard now holds a failure; every later call fails closed.
	if _, err := provider.JoinFields(context.Background(), dal.NewDatabaseCollectionRef("db", "", "A", "a")); !errors.Is(err, ErrNotSingleSource) {
		t.Fatalf("err = %v", err)
	}
	derived := dal.NewQuerySource(plainQuery("db", "B"), "d")
	if _, err := provider.JoinFields(context.Background(), derived); !errors.Is(err, ErrNotSingleSource) {
		t.Fatalf("a derived source after a failure must fail too: %v", err)
	}
}

func TestLeafServesDerivedSourcesThroughDalgo(t *testing.T) {
	exec := fieldsExecutor{&fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 3, "x")}, fields: []string{"id", "payload"}}}
	guard := NewGuard(allowAll, Limits{})
	leaf := guard.Leaf(newSource("db", exec))
	q := dal.From(dal.NewQuerySource(plainQuery("", "A"), "d")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "id")})
	reader, err := dal.ExecuteRecursiveQuery(context.Background(), leaf, q)
	if err != nil {
		t.Fatalf("a derived source over an allowed collection must run: %v (guard: %v)", err, guard.Err())
	}
	records, err := guard.Collect(context.Background(), reader, RouteInMemory)
	if err != nil || len(records) != 3 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
}

func TestLeafDerivedSourceOverADeniedCollectionIsNeverRead(t *testing.T) {
	exec := fieldsExecutor{&fakeExecutor{rows: map[string][]record.Record{"Secret": makeRows("Secret", 3, "x")}, fields: []string{"id"}}}
	guard := NewGuard(func(_, collection string) bool { return collection != "Secret" }, Limits{})
	leaf := guard.Leaf(newSource("db", exec))
	q := dal.From(dal.NewQuerySource(plainQuery("", "Secret"), "d")).NewQuery().SelectColumns(dal.Column{Expression: dal.NewFieldRef("d", "id")})
	_, err := dal.ExecuteRecursiveQuery(context.Background(), leaf, q)
	if err == nil {
		t.Fatal("expected the denied inner collection to fail the request")
	}
	denied := mustDenied(t, guard.Classify(err, RouteInMemory))
	if denied.Collection != "Secret" {
		t.Fatalf("denied = %+v", denied)
	}
	if exec.queryCalls != 0 || exec.fieldCalls != 0 {
		t.Fatalf("a denied collection reached the source: %d reads, %d field lookups", exec.queryCalls, exec.fieldCalls)
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
		t.Fatal("nil in, nil out while nothing is recorded")
	}
	plain := errors.New("something else")
	if got := g.Classify(plain, RouteInMemory); got != plain {
		t.Fatalf("an unrelated error must pass through unchanged, got %v", got)
	}
	// DALgo flattens a leaf error to text inside its own error; the recorded
	// failure is what the caller must see.
	denied := &SourceDeniedError{Database: "d", Collection: "c"}
	_ = g.fail(denied)
	flattened := &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "scan c: " + denied.Error()}
	if got := g.Classify(flattened, RouteInMemory); got != error(denied) {
		t.Fatalf("Classify = %v, want the recorded denial", got)
	}
	// A recorded failure also wins over a clean end: DALgo reads an error that
	// wraps io.EOF, and a materialising reader's call after an error, as the end
	// of the stream.
	if got := g.Classify(nil, RouteInMemory); got != error(denied) {
		t.Fatalf("Classify(nil) = %v, want the recorded denial", got)
	}
	// Only the first failure is kept.
	_ = g.fail(errBoom)
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

// errCloseFailed is the error of a source reader whose Close fails.
var errCloseFailed = errors.New("close failed")

// closeFailingExecutor wraps an executor so that the readers it opens fail to
// close with err.
type closeFailingExecutor struct {
	dal.QueryExecutor
	err error
}

func (e closeFailingExecutor) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	reader, err := e.QueryExecutor.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		return nil, err
	}
	return &closeFailingReader{RecordsReader: reader, err: e.err}, nil
}

type closeFailingReader struct {
	dal.RecordsReader
	err error
}

func (r *closeFailingReader) Close() error {
	_ = r.RecordsReader.Close()
	return r.err
}

func TestLeafRecordsACloseErrorAsTheFailure(t *testing.T) {
	inner := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 2, "x")}}
	guard := NewGuard(allowAll, Limits{})
	reader, err := guard.Leaf(newSource("db", closeFailingExecutor{QueryExecutor: inner, err: errCloseFailed})).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := drain(t, reader); n != 2 || err != nil {
		t.Fatalf("drain: n=%d err=%v", n, err)
	}
	closeErr := reader.Close()
	if se := mustSourceError(t, closeErr); !errors.Is(closeErr, errCloseFailed) || se.Database != "db" || se.Collection != "A" {
		t.Fatalf("Close = %v", closeErr)
	}
	if guard.Err() != closeErr {
		t.Fatalf("the close error must be the recorded failure: %v", guard.Err())
	}
	if stats := guard.Stats(); len(stats) != 1 || stats[0].Rows != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestLeafCloseErrorDoesNotShadowAnEarlierFailure(t *testing.T) {
	inner := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}, readerError: errBoom}
	guard := NewGuard(allowAll, Limits{})
	reader, _ := guard.Leaf(newSource("db", closeFailingExecutor{QueryExecutor: inner, err: errCloseFailed})).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	if _, err := drain(t, reader); !errors.Is(err, errBoom) {
		t.Fatalf("drain: %v", err)
	}
	closeErr := reader.Close()
	if !errors.Is(closeErr, errBoom) || errors.Is(closeErr, errCloseFailed) {
		t.Fatalf("Close = %v, want the first failure, not the close error", closeErr)
	}
}

func TestLeafCloseWithoutAnErrorReturnsNil(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}}
	guard := NewGuard(allowAll, Limits{})
	reader, _ := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	if err := reader.Close(); err != nil || guard.Err() != nil {
		t.Fatalf("Close = %v, guard = %v", err, guard.Err())
	}
}

// TestLeafNeverHandsDalgoAnErrorThatReadsAsTheEndOfTheStream: DALgo's
// streaming join and ReadAllToRecords take any error satisfying
// errors.Is(err, io.EOF) for the end of the stream and return the rows read so
// far with no error. A source failure whose chain holds io.EOF (net/http's
// `Get "...": EOF` is one) must not reach them as such. The guard still keeps
// the chain, so Classify is unchanged.
func TestLeafNeverHandsDalgoAnErrorThatReadsAsTheEndOfTheStream(t *testing.T) {
	wrapped := fmt.Errorf("lost: %w", io.EOF)

	t.Run("on read", func(t *testing.T) {
		exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}, readerError: wrapped}
		guard := NewGuard(allowAll, Limits{})
		reader, _ := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
		n, err := drain(t, reader)
		if n != 1 || err == nil || errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "lost: EOF") {
			t.Fatalf("n=%d err=%v (chain reads as the end: %v)", n, err, errors.Is(err, io.EOF))
		}
		if !errors.Is(guard.Err(), wrapped) || !errors.Is(guard.Classify(nil, RouteInMemory), wrapped) {
			t.Fatalf("the guard must keep the chain: %v", guard.Err())
		}
		// A later Next repeats the failure, still not as an end of stream.
		if _, err := reader.Next(); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("Next after failure: %v", err)
		}
	})

	t.Run("on close", func(t *testing.T) {
		inner := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}}
		guard := NewGuard(allowAll, Limits{})
		reader, _ := guard.Leaf(newSource("db", closeFailingExecutor{QueryExecutor: inner, err: wrapped})).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
		_, _ = drain(t, reader)
		if err := reader.Close(); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("Close = %v", err)
		}
		if !errors.Is(guard.Err(), wrapped) {
			t.Fatalf("the guard must keep the chain: %v", guard.Err())
		}
	})

	t.Run("a failure recorded earlier by another leaf", func(t *testing.T) {
		guard := NewGuard(allowAll, Limits{})
		_ = guard.fail(&SourceError{Database: "other", Collection: "B", Err: wrapped})
		exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 1, "x")}}
		reader := &countingReader{RecordsReader: dal.NewRecordsReader(exec.rows["A"]), ctx: context.Background(), leaf: &leaf{guard: guard, src: newSource("db", exec)}, collection: "A", started: time.Now()}
		if _, err := reader.Next(); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("Next = %v", err)
		}
	})
}
