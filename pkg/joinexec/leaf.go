package joinexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		return "", l.guard.fail(fmt.Errorf("%w: %q", ErrUnsupportedSource, ref.Path()))
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

// failSource records err, the source's own error, as the request failure. The
// failure keeps the source's error chain, so a policy denial, a deadline or a
// backend sentinel stays visible to Guard.Classify after DALgo has rewrapped
// the error it saw.
func (l *leaf) failSource(collection string, err error) error {
	return l.guard.fail(&SourceError{Database: l.src.ID(), Collection: collection, Err: err})
}

// ExecuteQueryToRecordsReader reads one collection of the source. The
// collection is authorised first; a denied or refused read never reaches the
// source.
func (l *leaf) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	if err := l.guard.Err(); err != nil {
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
		return nil, l.guard.fail(fmt.Errorf("%w: query on %q contains a subquery", ErrNotSingleSource, collection))
	}
	executor := l.src.Executor()
	if executor == nil {
		return nil, l.guard.fail(fmt.Errorf("%w: %q", ErrNoExecutor, l.src.ID()))
	}
	if err := ctx.Err(); err != nil {
		return nil, l.failSource(collection, err)
	}
	started := l.guard.now()
	reader, err := executor.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		return nil, l.failSource(collection, err)
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
//
// DALgo asks for the fields of every FROM node, derived or not, before it
// decides how to run it. A derived source has no schema of its own to serve and
// reads nothing here, so it answers no fields without failing the request: DALgo
// then runs the inner query through the leaf, where each read is authorised.
func (l *leaf) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	if err := l.guard.Err(); err != nil {
		return nil, err
	}
	switch derived := source.(type) {
	case dal.QuerySource:
		return nil, nil
	case *dal.QuerySource:
		if derived != nil {
			return nil, nil
		}
	}
	collection, err := l.resolve(source)
	if err != nil {
		return nil, err
	}
	if provider, ok := l.src.Executor().(dal.JoinFieldsProvider); ok {
		fields, err := provider.JoinFields(ctx, source)
		if err != nil {
			return nil, l.failSource(collection, err)
		}
		return fields, nil
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

// Next returns the next row. Anything but the end of the stream ends the
// request: a source error or the end of the request context is recorded with
// its chain intact (see SourceError), and the row that would cross the budget
// is withheld. A bare io.EOF is read as the end of the stream, as DALgo reads
// it; an error that merely wraps io.EOF is a failure, recorded so that
// Guard.Classify reports it even though DALgo would take it for the end.
func (r *countingReader) Next() (record.Record, error) {
	g := r.leaf.guard
	if err := g.Err(); err != nil {
		r.finish()
		return nil, err
	}
	if err := r.ctx.Err(); err != nil {
		r.finish()
		return nil, r.leaf.failSource(r.collection, err)
	}
	rec, err := r.RecordsReader.Next()
	if err != nil {
		r.finish()
		if err == io.EOF || errors.Is(err, dal.ErrNoMoreRecords) {
			return nil, err
		}
		return nil, r.leaf.failSource(r.collection, err)
	}
	encoded, err := json.Marshal(rec.Data())
	if err != nil {
		r.finish()
		return nil, g.fail(fmt.Errorf("%w: %q.%q: %v", ErrRowNotEncodable, r.leaf.src.ID(), r.collection, err))
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
