package joinexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
)

type streamTestReader struct {
	row      record.Record
	waiting  chan struct{}
	release  chan struct{}
	closed   int
	nextErr  error
	closeErr error
}

func (r *streamTestReader) Next() (record.Record, error) {
	if r.row != nil {
		row := r.row
		r.row = nil
		return row, nil
	}
	if r.waiting != nil {
		close(r.waiting)
		<-r.release
		r.waiting = nil
	}
	if r.nextErr != nil {
		err := r.nextErr
		r.nextErr = nil
		return nil, err
	}
	return nil, io.EOF
}
func (*streamTestReader) Cursor() (string, error) { return "", nil }
func (r *streamTestReader) Close() error          { r.closed++; return r.closeErr }

type streamTestExecutor struct{ reader dal.RecordsReader }

func (e *streamTestExecutor) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return e.reader, nil
}
func (e *streamTestExecutor) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	return nil, errors.New("unused")
}

type streamTestSource struct {
	id       string
	executor dal.QueryExecutor
	afterTx  error
}

func (s *streamTestSource) ID() string                  { return s.id }
func (*streamTestSource) Engine() string                { return "sqlite" }
func (*streamTestSource) HasAccessPolicies() bool       { return false }
func (s *streamTestSource) Executor() dal.QueryExecutor { return s.executor }
func (s *streamTestSource) CanQuery() bool              { return true }
func (s *streamTestSource) ReadTx(_ context.Context, fn func(dal.QueryExecutor) error) error {
	if err := fn(s.executor); err != nil {
		return err
	}
	return s.afterTx
}

type streamTestRegistry map[string]Source

func (r streamTestRegistry) Lookup(id string) (Source, bool) { source, ok := r[id]; return source, ok }

func TestExecuteStreamEmitsBeforeReaderReachesEOFAndClosesOnce(t *testing.T) {
	reader := &streamTestReader{
		row:     record.NewRecordWithData(record.NewKeyWithID("A", "one"), map[string]any{"id": "one"}),
		waiting: make(chan struct{}), release: make(chan struct{}),
	}
	source := &streamTestSource{id: "db", executor: &streamTestExecutor{reader: reader}}
	query := exPlain("", "A")
	profile := exProfile(t, query)
	gotRow := make(chan record.Record, 1)
	gotResult := make(chan error, 1)
	go func() {
		_, err := ExecuteStream(context.Background(), query, profile, "db", streamTestRegistry{"db": source}, allowAll, Limits{}, func(row record.Record) error {
			gotRow <- row
			return nil
		})
		gotResult <- err
	}()
	select {
	case row := <-gotRow:
		if row.Key().ID != "one" {
			t.Fatalf("row key = %v", row.Key())
		}
	case <-time.After(time.Second):
		t.Fatal("first row did not reach the callback")
	}
	select {
	case <-reader.waiting:
	case <-time.After(time.Second):
		t.Fatal("reader did not pause before EOF")
	}
	close(reader.release)
	if err := <-gotResult; err != nil {
		t.Fatal(err)
	}
	if reader.closed != 1 {
		t.Fatalf("reader closed %d times", reader.closed)
	}
}

func TestExecuteStreamReportsTransactionFailureAfterRows(t *testing.T) {
	txErr := errors.New("commit failed")
	reader := &streamTestReader{row: record.NewRecordWithData(record.NewKeyWithID("A", "one"), map[string]any{"id": "one"})}
	source := &streamTestSource{id: "db", executor: &streamTestExecutor{reader: reader}, afterTx: txErr}
	query := exPlain("", "A")
	rows := 0
	_, err := ExecuteStream(context.Background(), query, exProfile(t, query), "db", streamTestRegistry{"db": source}, allowAll, Limits{}, func(record.Record) error { rows++; return nil })
	if err != txErr {
		t.Fatalf("error = %v, want transaction error", err)
	}
	if rows != 1 {
		t.Fatalf("emitted rows = %d, want one", rows)
	}
	if reader.closed != 1 {
		t.Fatalf("reader closed %d times", reader.closed)
	}
}

func TestExecuteStreamClosesReaderWhenConsumerStops(t *testing.T) {
	writeErr := errors.New("client disconnected")
	reader := &streamTestReader{row: record.NewRecordWithData(record.NewKeyWithID("A", "one"), map[string]any{"id": "one"})}
	source := &streamTestSource{id: "db", executor: &streamTestExecutor{reader: reader}}
	query := exPlain("", "A")
	_, err := ExecuteStream(context.Background(), query, exProfile(t, query), "db", streamTestRegistry{"db": source}, allowAll, Limits{}, func(record.Record) error { return writeErr })
	if err != writeErr {
		t.Fatalf("error = %v, want callback error", err)
	}
	if reader.closed != 1 {
		t.Fatalf("reader closed %d times", reader.closed)
	}
}

func TestExecuteStreamClosesReaderWhenRequestIsCanceled(t *testing.T) {
	reader := &streamTestReader{row: record.NewRecordWithData(record.NewKeyWithID("A", "one"), map[string]any{"id": "one"})}
	source := &streamTestSource{id: "db", executor: &streamTestExecutor{reader: reader}}
	query := exPlain("", "A")
	ctx, cancel := context.WithCancel(context.Background())
	rows := 0
	_, err := ExecuteStream(ctx, query, exProfile(t, query), "db", streamTestRegistry{"db": source}, allowAll, Limits{}, func(record.Record) error {
		rows++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || rows != 1 || reader.closed != 1 {
		t.Fatalf("error = %v, rows = %d, closed = %d", err, rows, reader.closed)
	}
}

func TestExecuteStreamPropagatesReadAndCloseFailuresAfterEmittedRows(t *testing.T) {
	for _, tc := range []struct {
		name      string
		nextErr   error
		closeErr  error
		wantError error
	}{
		{name: "read", nextErr: errors.New("late read failed"), wantError: errors.New("late read failed")},
		{name: "close", closeErr: errors.New("close failed"), wantError: errors.New("close failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &streamTestReader{
				row:      record.NewRecordWithData(record.NewKeyWithID("A", "one"), map[string]any{"id": "one"}),
				nextErr:  tc.nextErr,
				closeErr: tc.closeErr,
			}
			source := &streamTestSource{id: "db", executor: &streamTestExecutor{reader: reader}}
			query := exPlain("", "A")
			rows := 0
			_, err := ExecuteStream(context.Background(), query, exProfile(t, query), "db", streamTestRegistry{"db": source}, allowAll, Limits{}, func(record.Record) error { rows++; return nil })
			if err == nil || err.Error() != tc.wantError.Error() {
				t.Fatalf("error = %v, want %v", err, tc.wantError)
			}
			if rows != 1 || reader.closed != 1 {
				t.Fatalf("emitted rows = %d, reader closed = %d; want one each", rows, reader.closed)
			}
		})
	}
}

type generatedStreamReader struct {
	left  int
	index int
}

func (r *generatedStreamReader) Next() (record.Record, error) {
	if r.left == 0 {
		return nil, io.EOF
	}
	r.left--
	r.index++
	return record.NewRecordWithData(record.NewKeyWithID("A", fmt.Sprint(r.index)), map[string]any{"id": fmt.Sprint(r.index), "payload": strings.Repeat("x", 6<<10)}), nil
}
func (*generatedStreamReader) Cursor() (string, error) { return "", nil }
func (*generatedStreamReader) Close() error            { return nil }

func TestExecuteStreamRetainsBoundedStateForLargeSyntheticResult(t *testing.T) {
	reader := &generatedStreamReader{left: 1000}
	source := &streamTestSource{id: "db", executor: &streamTestExecutor{reader: reader}}
	query := exPlain("", "A")
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	rows := 0
	var during uint64
	result, err := ExecuteStream(context.Background(), query, exProfile(t, query), "db", streamTestRegistry{"db": source}, allowAll, Limits{}, func(record.Record) error {
		rows++
		if rows == 900 {
			runtime.GC()
			var sample runtime.MemStats
			runtime.ReadMemStats(&sample)
			during = sample.HeapAlloc
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if rows != 1000 || result.RowsReturned != 1000 {
		t.Fatalf("callback rows = %d, result rows = %d", rows, result.RowsReturned)
	}
	retainedDuring := int64(during) - int64(before.HeapAlloc)
	retainedAfter := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if retainedDuring > 2<<20 || retainedAfter > 2<<20 {
		t.Fatalf("stream retained too much heap during/after a 1000-row result: %d / %d bytes", retainedDuring, retainedAfter)
	}
}
