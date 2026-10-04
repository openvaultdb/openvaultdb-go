package joinexec

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
)

var (
	_ dal.QueryExecutor      = (*leaf)(nil)
	_ dal.JoinFieldsProvider = (*leaf)(nil)
)

// leaf is the guarded dal.QueryExecutor of one Source.
type leaf struct {
	guard *Guard
	src   Source
}

// resolve checks that source is a collection this leaf may read and returns
// its name. Every failure is recorded on the guard. It never touches the
// source's data.
func (l *leaf) resolve(source dal.RecordsetSource) (string, error) {
	ref, ok := source.(dal.CollectionRef)
	if !ok {
		if pointer, isPointer := source.(*dal.CollectionRef); isPointer && pointer != nil {
			ref, ok = *pointer, true
		}
	}
	if !ok {
		return "", l.guard.fail(fmt.Errorf("%w: %T is not a collection", ErrNotSingleSource, source))
	}
	if ref.Schema() != "" || ref.Parent() != nil {
		return "", l.guard.fail(fmt.Errorf("%w: %s", ErrUnsupportedSource, ref.Path()))
	}
	database := l.src.ID()
	if named := ref.Database(); named != "" && named != database {
		return "", l.guard.fail(&SourceDeniedError{Database: named, Collection: ref.Name()})
	}
	if l.guard.authorize == nil || !l.guard.authorize(database, ref.Name()) {
		return "", l.guard.fail(&SourceDeniedError{Database: database, Collection: ref.Name()})
	}
	return ref.Name(), nil
}

// ExecuteQueryToRecordsReader reads one collection of the source. The
// collection is authorised first; a denied or refused read never reaches the
// source.
func (l *leaf) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	if err := l.guard.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q, ok := query.(dal.StructuredQuery)
	if !ok || q.From() == nil || len(q.From().Joins()) != 0 {
		return nil, l.guard.fail(fmt.Errorf("%w: %T", ErrNotSingleSource, query))
	}
	collection, err := l.resolve(q.From().Base())
	if err != nil {
		return nil, err
	}
	// After authorisation: a subquery would read a collection this check has
	// not seen, so it is refused rather than inspected.
	if dal.HasSubquery(q) {
		return nil, l.guard.fail(fmt.Errorf("%w: query on %s contains a subquery", ErrNotSingleSource, collection))
	}
	executor := l.src.Executor()
	if executor == nil {
		return nil, l.guard.fail(fmt.Errorf("%w: %s", ErrNoExecutor, l.src.ID()))
	}
	started := l.guard.now()
	reader, err := executor.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		return nil, err
	}
	return &countingReader{RecordsReader: reader, ctx: ctx, leaf: l, collection: collection, started: started}, nil
}

// ExecuteQueryToRecordsetReader is not offered: only the records path is
// guarded.
func (l *leaf) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	return nil, ErrRecordsetUnsupported
}

// JoinFields passes schema-ordered fields through from the source's executor,
// after the same authorisation as a read: field names are schema, and a denied
// collection's schema is not served either.
func (l *leaf) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	if err := l.guard.Err(); err != nil {
		return nil, err
	}
	if _, err := l.resolve(source); err != nil {
		return nil, err
	}
	if provider, ok := l.src.Executor().(dal.JoinFieldsProvider); ok {
		return provider.JoinFields(ctx, source)
	}
	return nil, nil
}

// countingReader counts what a source delivers, enforces the request budget
// and records statistics when the read ends.
type countingReader struct {
	dal.RecordsReader
	ctx        context.Context
	leaf       *leaf
	collection string
	started    time.Time
	rows       int
	finished   bool
}

func (r *countingReader) Next() (record.Record, error) {
	g := r.leaf.guard
	if err := g.Err(); err != nil {
		r.finish()
		return nil, err
	}
	if err := r.ctx.Err(); err != nil {
		r.finish()
		return nil, err
	}
	rec, err := r.RecordsReader.Next()
	if err != nil {
		r.finish()
		return nil, err
	}
	encoded, err := json.Marshal(rec.Data())
	if err != nil {
		r.finish()
		return nil, g.fail(fmt.Errorf("%w: %s.%s: %v", ErrRowNotEncodable, r.leaf.src.ID(), r.collection, err))
	}
	if err := g.charge(len(encoded), r.leaf.src.ID()+"."+r.collection); err != nil {
		r.finish()
		return nil, err
	}
	r.rows++
	return rec, nil
}

func (r *countingReader) Close() error {
	r.finish()
	return r.RecordsReader.Close()
}

// finish records the statistics once, however the read ends.
func (r *countingReader) finish() {
	if r.finished {
		return
	}
	r.finished = true
	g := r.leaf.guard
	g.record(r.leaf.src.ID(), r.collection, r.leaf.src.HasAccessPolicies(), r.rows, g.now().Sub(r.started))
}
