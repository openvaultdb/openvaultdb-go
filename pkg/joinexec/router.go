package joinexec

import (
	"context"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
)

// guardedLeaf is what Guard.Leaf returns: the guarded executor of one source,
// which also serves the fields of its collections.
type guardedLeaf interface {
	dal.QueryExecutor
	dal.JoinFieldsProvider
}

var _ guardedLeaf = (*leaf)(nil)

// router is the executor DALgo's recursive evaluation reads through when one
// leaf is not enough: it sends each single-collection read to the guarded leaf
// of the database the collection names, and a collection that names none to
// the document's only database. A database outside the preflight set has no
// leaf, so a read of it fails closed with an UnknownDatabaseError.
//
// The router decides nothing about access. Every read still passes through a
// leaf, which authorises the collection again, refuses a read that is not a
// plain query over one collection and counts the rows.
type router struct {
	run *run
	// fallback is the database of a collection that names none. It is empty for
	// a document that reads several databases, where every collection names one.
	fallback string
}

var (
	_ dal.QueryExecutor      = (*router)(nil)
	_ dal.JoinFieldsProvider = (*router)(nil)
)

func newRouter(r *run, databases []string) *router {
	rt := &router{run: r}
	if len(databases) == 1 {
		rt.fallback = databases[0]
	}
	return rt
}

// leafOf returns the leaf that reads source. A source that is not a collection
// has no database to read from and is refused: DALgo never asks a leaf to read
// one.
func (rt *router) leafOf(source dal.RecordsetSource) (guardedLeaf, error) {
	ref, ok := source.(dal.CollectionRef)
	if !ok {
		return nil, rt.run.guard.fail(fmt.Errorf("%w: %T is not a collection", ErrNotSingleSource, source))
	}
	database := ref.Database()
	if database == "" {
		database = rt.fallback
	}
	return rt.run.leaf(database)
}

// ExecuteQueryToRecordsReader reads one collection through the leaf of its
// database.
func (rt *router) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	q, ok := query.(dal.StructuredQuery)
	if !ok || q.From() == nil {
		return nil, rt.run.guard.fail(fmt.Errorf("%w: %T", ErrNotSingleSource, query))
	}
	leaf, err := rt.leafOf(q.From().Base())
	if err != nil {
		return nil, err
	}
	return leaf.ExecuteQueryToRecordsReader(ctx, query)
}

// ExecuteQueryToRecordsetReader is not offered: only the records path is
// guarded.
func (rt *router) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	return nil, ErrRecordsetUnsupported
}

// JoinFields serves the fields of a collection through the leaf of its
// database. DALgo asks for the fields of every FROM node, derived or not,
// before it decides how to run it; a derived source has no schema of its own to
// serve and reads nothing here, so it answers no fields, and DALgo then runs its
// inner query through ExecuteQueryToRecordsReader, where each read is routed
// and authorised.
func (rt *router) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	if _, derived := source.(dal.QuerySource); derived {
		return nil, nil
	}
	leaf, err := rt.leafOf(source)
	if err != nil {
		return nil, err
	}
	return leaf.JoinFields(ctx, source)
}
