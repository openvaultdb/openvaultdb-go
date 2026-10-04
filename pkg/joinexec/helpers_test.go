package joinexec

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
)

// fakeExecutor is a dal.QueryExecutor over in-memory rows that records every
// call, so tests can prove a denied collection was never read.
type fakeExecutor struct {
	rows        map[string][]record.Record
	readErr     error
	queryCalls  int
	fieldCalls  int
	fields      []string
	fieldsErr   error // returned by JoinFields of fieldsExecutor
	readerError error // returned by the reader's Next after the rows
	closed      int
}

func (f *fakeExecutor) ExecuteQueryToRecordsReader(_ context.Context, query dal.Query) (dal.RecordsReader, error) {
	f.queryCalls++
	if f.readErr != nil {
		return nil, f.readErr
	}
	name := query.(dal.StructuredQuery).From().Base().Name()
	return &fakeReader{RecordsReader: dal.NewRecordsReader(f.rows[name]), owner: f}, nil
}

func (f *fakeExecutor) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	f.queryCalls++
	return nil, errors.New("fake: recordset reader")
}

type fakeReader struct {
	dal.RecordsReader
	owner *fakeExecutor
	done  bool
}

func (r *fakeReader) Next() (record.Record, error) {
	rec, err := r.RecordsReader.Next()
	if errors.Is(err, dal.ErrNoMoreRecords) && r.owner.readerError != nil && !r.done {
		r.done = true
		return nil, r.owner.readerError
	}
	return rec, err
}

func (r *fakeReader) Close() error {
	r.owner.closed++
	return r.RecordsReader.Close()
}

// fieldsExecutor adds dal.JoinFieldsProvider to fakeExecutor.
type fieldsExecutor struct {
	*fakeExecutor
}

func (f fieldsExecutor) JoinFields(context.Context, dal.RecordsetSource) ([]string, error) {
	f.fieldCalls++
	if f.fieldsErr != nil {
		return nil, f.fieldsErr
	}
	return f.fields, nil
}

// fakeSource is a Source over one executor.
type fakeSource struct {
	id          string
	engine      string
	policies    bool
	exec        dal.QueryExecutor
	txCalls     int
	txErr       error
	noExecutors bool
}

func (s *fakeSource) ID() string              { return s.id }
func (s *fakeSource) Engine() string          { return s.engine }
func (s *fakeSource) HasAccessPolicies() bool { return s.policies }
func (s *fakeSource) Executor() dal.QueryExecutor {
	if s.noExecutors {
		return nil
	}
	return s.exec
}
func (s *fakeSource) ReadTx(_ context.Context, fn func(dal.QueryExecutor) error) error {
	s.txCalls++
	if s.txErr != nil {
		return s.txErr
	}
	return fn(s.exec)
}

// makeRows builds n rows of one collection; each row has an id and a payload.
func makeRows(collection string, n int, payload string) []record.Record {
	rows := make([]record.Record, n)
	for i := range rows {
		rows[i] = record.NewRecordWithData(record.NewKeyWithID(collection, i+1), map[string]any{"id": i + 1, "payload": payload})
	}
	return rows
}

func allowAll(string, string) bool { return true }

// plainQuery is the single-source read DALgo hands a leaf.
func plainQuery(database, collection string) dal.StructuredQuery {
	ref := dal.NewRootCollectionRef(collection, "")
	if database != "" {
		ref = dal.NewDatabaseCollectionRef(database, "", collection, "")
	}
	return dal.From(ref).NewQuery().SelectIntoRecord(nil)
}

// drain reads the reader to the end and returns the number of rows and the
// terminating error (nil on a clean end).
func drain(t *testing.T, reader dal.RecordsReader) (int, error) {
	t.Helper()
	n := 0
	for {
		_, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n++
	}
}

func mustBudget(t *testing.T, err error) *BudgetError {
	t.Helper()
	var budget *BudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("want *BudgetError, got %T: %v", err, err)
	}
	return budget
}

func mustDenied(t *testing.T, err error) *SourceDeniedError {
	t.Helper()
	var denied *SourceDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want *SourceDeniedError, got %T: %v", err, err)
	}
	return denied
}

func mustSourceError(t *testing.T, err error) *SourceError {
	t.Helper()
	var source *SourceError
	if !errors.As(err, &source) {
		t.Fatalf("want *SourceError, got %T: %v", err, err)
	}
	return source
}

var errBoom = fmt.Errorf("boom")
