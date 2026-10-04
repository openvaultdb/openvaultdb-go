package joinexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// stubReader is a dal.RecordsReader that ends cleanly after its rows, or
// returns err from Next once they are read.
type stubReader struct {
	rows     []record.Record
	err      error
	closeErr error
	closed   int
}

func (r *stubReader) Next() (record.Record, error) {
	if len(r.rows) == 0 {
		if r.err != nil {
			return nil, r.err
		}
		return nil, dal.ErrNoMoreRecords
	}
	rec := r.rows[0]
	r.rows = r.rows[1:]
	return rec, nil
}

func (r *stubReader) Cursor() (string, error) { return "", nil }

func (r *stubReader) Close() error {
	r.closed++
	return r.closeErr
}

func TestGuardCollectReturnsEveryRowAndClosesTheReader(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 4, "x")}}
	guard := NewGuard(allowAll, Limits{})
	reader, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := guard.Collect(context.Background(), reader, RouteInMemory)
	if err != nil || len(records) != 4 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
	if exec.closed != 1 {
		t.Fatalf("reader closed %d times", exec.closed)
	}
	// Collect closes the reader, so the statistics are complete when it returns.
	if stats := guard.Stats(); len(stats) != 1 || stats[0].Rows != 4 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestGuardCollectReturnsNoRowsWhenTheBudgetRunsOut(t *testing.T) {
	exec := &fakeExecutor{rows: map[string][]record.Record{"A": makeRows("A", 9, "x")}}
	guard := NewGuard(allowAll, Limits{MaxSourceRows: 3})
	reader, err := guard.Leaf(newSource("db", exec)).ExecuteQueryToRecordsReader(context.Background(), plainQuery("db", "A"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := guard.Collect(context.Background(), reader, RouteInMemory)
	if records != nil {
		t.Fatalf("a request that overran its budget returned %d rows", len(records))
	}
	if budget := mustBudget(t, err); budget.Name != BudgetSourceRows || budget.Limit != 3 {
		t.Fatalf("budget = %+v", budget)
	}
	if exec.closed != 1 {
		t.Fatalf("reader closed %d times", exec.closed)
	}
}

func TestGuardCollectReturnsTheRecordedFailureAfterACleanEnd(t *testing.T) {
	guard := NewGuard(allowAll, Limits{})
	denied := &SourceDeniedError{Database: "d", Collection: "c"}
	_ = guard.fail(denied)
	reader := &stubReader{rows: makeRows("A", 2, "x")}
	records, err := guard.Collect(context.Background(), reader, RouteInMemory)
	if records != nil || err != error(denied) {
		t.Fatalf("records=%v err=%v", records, err)
	}
}

func TestGuardCollectClassifiesReaderErrors(t *testing.T) {
	guard := NewGuard(allowAll, Limits{})
	bound := &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "joined row bound exceeded"}
	reader := &stubReader{rows: makeRows("A", 2, "x"), err: bound}
	records, err := guard.Collect(context.Background(), reader, RouteDatabase)
	if records != nil {
		t.Fatalf("records = %d", len(records))
	}
	if budget := mustBudget(t, err); budget.Name != BudgetJoinRows || budget.Route != RouteDatabase {
		t.Fatalf("budget = %+v", budget)
	}
}

// TestGuardCollectEndsOnlyOnTheEndOfTheStream: Collect reads dal.ErrNoMoreRecords
// (and a bare io.EOF, which a reader no leaf guards, such as the database route's,
// returns) as the end. An error that merely wraps io.EOF is a failed read: no rows.
func TestGuardCollectEndsOnlyOnTheEndOfTheStream(t *testing.T) {
	for name, end := range map[string]error{
		"bare io.EOF":   io.EOF,
		"limit reached": dal.ErrLimitReached,
	} {
		t.Run(name, func(t *testing.T) {
			guard := NewGuard(allowAll, Limits{})
			records, err := guard.Collect(context.Background(), &stubReader{rows: makeRows("A", 2, "x"), err: end}, RouteDatabase)
			if err != nil || len(records) != 2 {
				t.Fatalf("records=%d err=%v", len(records), err)
			}
		})
	}
	wrapped := fmt.Errorf("x: %w", io.EOF)
	guard := NewGuard(allowAll, Limits{})
	reader := &stubReader{rows: makeRows("A", 2, "x"), err: wrapped}
	records, err := guard.Collect(context.Background(), reader, RouteDatabase)
	if records != nil || !errors.Is(err, wrapped) {
		t.Fatalf("records=%v err=%v: a failure that wraps io.EOF is not an end of stream", records, err)
	}
	if reader.closed != 1 {
		t.Fatalf("reader closed %d times", reader.closed)
	}
}

func TestGuardCollectReportsCloseErrorsAndStopsOnContext(t *testing.T) {
	guard := NewGuard(allowAll, Limits{})
	reader := &stubReader{rows: makeRows("A", 1, "x"), closeErr: errBoom}
	if records, err := guard.Collect(context.Background(), reader, RouteInMemory); records != nil || !errors.Is(err, errBoom) {
		t.Fatalf("records=%v err=%v", records, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader = &stubReader{rows: makeRows("A", 1, "x")}
	records, err := guard.Collect(ctx, reader, RouteInMemory)
	if records != nil || !errors.Is(err, context.Canceled) || reader.closed != 1 {
		t.Fatalf("records=%v err=%v closed=%d", records, err, reader.closed)
	}
}

func TestSourceErrorNamesTheSourceAndKeepsTheChain(t *testing.T) {
	err := &SourceError{Database: "vault", Collection: "Weird \"name\"", Err: errBoom}
	text := err.Error()
	if !strings.Contains(text, `"vault"`) || !strings.Contains(text, `"Weird \"name\""`) || !strings.Contains(text, "boom") {
		t.Fatalf("message = %q", text)
	}
	if !errors.Is(err, errBoom) || err.Unwrap() != errBoom {
		t.Fatal("the source's own error must stay reachable")
	}
}
