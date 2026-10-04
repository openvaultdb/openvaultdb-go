package joinexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
)

// The helpers of the planner and executor tests all start with "ex" so that
// they cannot clash with the helpers of the leaf and guard tests, which this
// file deliberately does not use: those are another change's to rename.

// exExecutor answers reads from memory and remembers every query it was given,
// so a test can prove which queries a mount received.
type exExecutor struct {
	mu sync.Mutex
	// rows answers a single-source read, by collection name.
	rows map[string][]record.Record
	// whole, when non-nil, answers every query: it stands in for a database
	// that runs a whole document itself.
	whole []record.Record
	// openErr fails the read before it returns a reader.
	openErr error
	// readerErr is returned by the reader after its rows.
	readerErr error
	// nilReader returns no reader and no error.
	nilReader bool
	queries   []dal.Query
	// deadlineSeen holds the context deadline of every read.
	deadlineSeen []time.Time
}

func (e *exExecutor) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queries = append(e.queries, query)
	deadline, _ := ctx.Deadline()
	e.deadlineSeen = append(e.deadlineSeen, deadline)
	if e.openErr != nil {
		return nil, e.openErr
	}
	if e.nilReader {
		return nil, nil
	}
	rows := e.whole
	if rows == nil {
		rows = e.rows[query.(dal.StructuredQuery).From().Base().Name()]
	}
	var reader dal.RecordsReader = dal.EmptyReader{}
	if len(rows) > 0 {
		reader = dal.NewRecordsReader(rows)
	}
	return &exReader{RecordsReader: reader, err: e.readerErr}, nil
}

func (e *exExecutor) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	return nil, errors.New("exExecutor: recordset reads are not offered")
}

func (e *exExecutor) seen() []dal.Query {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]dal.Query(nil), e.queries...)
}

func (e *exExecutor) deadlines() []time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]time.Time(nil), e.deadlineSeen...)
}

// exReader ends with err, when set, instead of the end of the stream.
type exReader struct {
	dal.RecordsReader
	err error
}

func (r *exReader) Next() (record.Record, error) {
	rec, err := r.RecordsReader.Next()
	if errors.Is(err, dal.ErrNoMoreRecords) && r.err != nil {
		return nil, r.err
	}
	return rec, err
}

// exSource is a Source over one exExecutor that counts how it is used.
type exSource struct {
	id       string
	engine   string
	policies bool
	exec     *exExecutor

	mu            sync.Mutex
	executorCalls int
	txCalls       int
	txErr         error // ReadTx fails with it before running its function
	skipTx        bool  // ReadTx returns nil without running its function
	swallowTx     bool  // ReadTx runs its function and returns nil whatever it returned
	afterTxErr    error // ReadTx returns it after its function succeeded, as a failed commit would
}

func (s *exSource) ID() string              { return s.id }
func (s *exSource) Engine() string          { return s.engine }
func (s *exSource) HasAccessPolicies() bool { return s.policies }

func (s *exSource) Executor() dal.QueryExecutor {
	s.mu.Lock()
	s.executorCalls++
	s.mu.Unlock()
	return s.exec
}

func (s *exSource) ReadTx(_ context.Context, fn func(dal.QueryExecutor) error) error {
	s.mu.Lock()
	s.txCalls++
	s.mu.Unlock()
	if s.txErr != nil {
		return s.txErr
	}
	if s.skipTx {
		return nil
	}
	err := fn(s.exec)
	switch {
	case s.swallowTx:
		return nil
	case err == nil:
		return s.afterTxErr
	}
	return err
}

func (s *exSource) counts() (executorCalls, txCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executorCalls, s.txCalls
}

// exGated is an exSource that also answers CanQuery, as core.Database does.
type exGated struct {
	*exSource
	can bool
}

func (g exGated) CanQuery() bool { return g.can }

// exRegistry resolves sources by id and remembers every lookup.
type exRegistry struct {
	sources map[string]Source
	lookups []string
}

func newExRegistry(sources ...Source) *exRegistry {
	r := &exRegistry{sources: map[string]Source{}}
	for _, s := range sources {
		r.sources[s.ID()] = s
	}
	return r
}

func (r *exRegistry) Lookup(database string) (Source, bool) {
	r.lookups = append(r.lookups, database)
	s, ok := r.sources[database]
	return s, ok
}

func exAllow(string, string) bool { return true }

// exRows builds n rows of one collection: id, k (equal to id) and a name.
func exRows(collection, prefix string, n int) []record.Record {
	rows := make([]record.Record, n)
	for i := range rows {
		id := i + 1
		rows[i] = record.NewRecordWithData(record.NewKeyWithID(collection, id), map[string]any{
			"id": id, "k": id, "name": fmt.Sprintf("%s%d", prefix, id),
		})
	}
	return rows
}

// exAnswer builds result rows as a database that ran a whole document returns
// them.
func exAnswer(rows ...map[string]any) []record.Record {
	out := make([]record.Record, len(rows))
	for i, data := range rows {
		out[i] = record.NewRecordWithData(record.NewKeyWithID("answer", i+1), data)
	}
	return out
}

func exMount(id, engine string, policies bool, rows map[string][]record.Record) *exSource {
	return &exSource{id: id, engine: engine, policies: policies, exec: &exExecutor{rows: rows}}
}

func exRef(database, collection, alias string) dal.CollectionRef {
	if database == "" {
		return dal.NewRootCollectionRef(collection, alias)
	}
	return dal.NewDatabaseCollectionRef(database, "", collection, alias)
}

func exKeyEquals(a, b dal.CollectionRef) dal.Condition {
	return dal.NewComparison(dal.NewFieldRef(a.Alias(), "k"), dal.Equal, dal.NewFieldRef(b.Alias(), "k"))
}

// exJoin joins a to b on k and selects a.id as aid and b.name as bname. An
// ordered join takes DALgo's generic engine; an unordered flat equality join
// takes its streaming one.
func exJoin(a, b dal.CollectionRef, ordered bool) dal.StructuredQuery {
	var builder dal.IQueryBuilder = dal.From(a).Join(dal.NewJoinedSource(b, dal.JoinInner, exKeyEquals(a, b))).NewQuery()
	if ordered {
		builder = builder.OrderBy(dal.Ascending(dal.NewFieldRef(a.Alias(), "id")))
	}
	return builder.SelectColumns(
		dal.Column{Expression: dal.NewFieldRef(a.Alias(), "id"), Alias: "aid"},
		dal.Column{Expression: dal.NewFieldRef(b.Alias(), "name"), Alias: "bname"},
	)
}

// exPlain selects every row of one collection.
func exPlain(database, collection string) dal.StructuredQuery {
	return dal.From(exRef(database, collection, "")).NewQuery().SelectIntoRecord(nil)
}

// exCount counts the rows of one collection.
func exCount(database, collection string) dal.StructuredQuery {
	return dal.From(exRef(database, collection, "")).NewQuery().SelectColumns(dal.CountAs(dal.Star(), "n"))
}

// exProfile classifies q the way the caller of Execute does, with the walk
// this package already makes (the test of the walk compares it to a profile
// that is built by hand).
func exProfile(t *testing.T, q dal.StructuredQuery) Profile {
	t.Helper()
	doc, err := inspect(q)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	profile := Profile{HasSubquery: doc.hasSubquery}
	for _, s := range doc.sources {
		profile.Sources = append(profile.Sources, ProfileSource{Database: s.database, Collection: s.collection})
	}
	return profile
}

// exRun executes q with a profile derived from it.
func exRun(t *testing.T, q dal.StructuredQuery, defaultDatabase string, registry Registry, authorize Authorize, limits Limits, opts ...Option) (Result, error) {
	t.Helper()
	return Execute(context.Background(), q, exProfile(t, q), defaultDatabase, registry, authorize, limits, opts...)
}

// exFixedClock returns a clock that moves forward step at every reading.
func exFixedClock(step time.Duration) func() time.Time {
	now := time.Unix(1_700_000_000, 0)
	return func() time.Time {
		now = now.Add(step)
		return now
	}
}

func exAsBudget(t *testing.T, err error) *BudgetError {
	t.Helper()
	var budget *BudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("want *BudgetError, got %T: %v", err, err)
	}
	return budget
}

func exAsDenied(t *testing.T, err error) *SourceDeniedError {
	t.Helper()
	var denied *SourceDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("want *SourceDeniedError, got %T: %v", err, err)
	}
	return denied
}

func exAsSourceError(t *testing.T, err error) *SourceError {
	t.Helper()
	var source *SourceError
	if !errors.As(err, &source) {
		t.Fatalf("want *SourceError, got %T: %v", err, err)
	}
	return source
}

func exJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

var errExBoom = errors.New("ex boom")
