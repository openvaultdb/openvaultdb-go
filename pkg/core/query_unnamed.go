package core

import (
	"context"

	"github.com/dal-go/dalgo/dal"
)

// A document of /v1/dtql names the database of every source, and the endpoint of a
// database hands the mount the document as it was written, so a join of the mount
// arrives with the database named on each of its sources. The compiler of the adapter
// of a PostgreSQL mount declines such a join (a source in a named database needs the
// generic engine), because the generic join reads that database from the one it is
// asked about. DALgo, which stands between the guarded executor and the adapter, then
// reads each table whole, with no WHERE, under a fixed bound of its own, and joins the
// rows in the transaction: not the join the operator's server runs, and one that
// refuses a name or a size in ways of its own.
//
// Every source of a document that reached a mount has already been checked (guardSources):
// a source that names a database names this one, so the name says nothing the mount does
// not know, and the document is handed to the adapter of a server engine without it.
// Nothing else of the document changes: the sources keep their names and aliases and
// their scan bounds, the joins keep their kind, conditions, hints and the form they were
// built in (one source, or a relation tree), and so do the clauses of the query.

// withoutDatabaseNames returns query with the database named by each collection source of
// its relation tree removed, or query itself when it has no join or names none. A derived
// source is left as it is: it is not a table the adapter reads in a statement, and DALgo
// evaluates the query inside it above the adapter.
func withoutDatabaseNames(query dal.StructuredQuery) dal.StructuredQuery {
	from := query.From()
	if from == nil || len(from.Joins()) == 0 {
		return query
	}
	unnamed, changed := unnamedFrom(from)
	if !changed {
		return query
	}
	return unnamedQuery{StructuredQuery: query, from: unnamed}
}

// unnamedFrom rebuilds a relation tree without the database names of its sources, and
// reports whether it removed any.
func unnamedFrom(from dal.FromSource) (dal.FromSource, bool) {
	base, baseChanged := unnamedSource(from.Base())
	joins := from.Joins()
	rebuiltJoins := make([]dal.JoinedSource, len(joins))
	changed := baseChanged
	for i, join := range joins {
		var joinChanged bool
		rebuiltJoins[i], joinChanged = unnamedJoin(join)
		changed = changed || joinChanged
	}
	if !changed {
		return from, false
	}
	rebuilt := dal.From(base)
	for _, join := range rebuiltJoins {
		rebuilt = rebuilt.Join(join)
	}
	return rebuilt, true
}

// unnamedJoin rebuilds a join without the database names of its source or tree. It keeps
// the kind of the join, its conditions and hints, and the form it was built in.
func unnamedJoin(join dal.JoinedSource) (dal.JoinedSource, bool) {
	var (
		rebuilt dal.JoinedSource
		changed bool
	)
	if tree := join.From(); tree != nil {
		var child dal.FromSource
		child, changed = unnamedFrom(tree)
		rebuilt = dal.NewNestedJoinedSource(child, join.JoinType(), join.On()...)
	} else {
		var source dal.RecordsetSource
		source, changed = unnamedSource(join.RecordsetSource)
		rebuilt = dal.NewJoinedSource(source, join.JoinType(), join.On()...)
	}
	if !changed {
		return join, false
	}
	if algorithms := join.Algorithms(); algorithms != nil {
		rebuilt = rebuilt.WithAlgorithms(algorithms...)
	}
	return rebuilt, true
}

// unnamedSource is a collection source without its database name. A source that names none,
// one that a parent record or a schema qualifies (the source guard refuses both before the
// adapter is reached) and a derived source are returned as they are.
func unnamedSource(source dal.RecordsetSource) (dal.RecordsetSource, bool) {
	ref, ok := source.(dal.CollectionRef)
	if !ok || ref.Database() == "" || ref.Parent() != nil || ref.Schema() != "" {
		return source, false
	}
	unnamed := dal.NewRootCollectionRef(ref.Name(), ref.Alias())
	if ref.ScanLimit() > 0 || len(ref.ScanOrders()) > 0 {
		unnamed = unnamed.WithScan(ref.ScanLimit(), ref.ScanOrders()...)
	}
	return unnamed, true
}

// unnamedQuery is a query with the relation tree withoutDatabaseNames rebuilt. The rest of it
// (the clauses, the paging, the cursors, the record it reads into) is the query it wraps.
// Like the wrapper DALgo gives a foreign query, it hands itself, not the query it wraps, to
// an executor, and prints the clauses it holds.
type unnamedQuery struct {
	dal.StructuredQuery
	from dal.FromSource
}

func (q unnamedQuery) From() dal.FromSource { return q.from }
func (q unnamedQuery) String() string       { return dal.QueryString(q) }

// Money hands on the money configuration the wrapped query declares, which DALgo reads from
// a query by type assertion: a wrapper that did not have the method would hide it.
func (q unnamedQuery) Money() *dal.MoneyConfig {
	if declarative, ok := q.StructuredQuery.(interface{ Money() *dal.MoneyConfig }); ok {
		return declarative.Money()
	}
	return nil
}

func (q unnamedQuery) GetRecordsReader(ctx context.Context, executor dal.QueryExecutor) (dal.RecordsReader, error) {
	return executor.ExecuteQueryToRecordsReader(ctx, q)
}

func (q unnamedQuery) GetRecordsetReader(ctx context.Context, executor dal.QueryExecutor) (dal.RecordsetReader, error) {
	return executor.ExecuteQueryToRecordsetReader(ctx, q)
}
