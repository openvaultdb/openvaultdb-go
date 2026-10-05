package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// This file makes *Database usable as a source of joined reads. It satisfies,
// structurally, the Source interface of pkg/joinexec (ID, Engine,
// HasAccessPolicies, Executor, ReadTx); pkg/core does not import pkg/joinexec,
// so the dependency points one way and the planner may import core. The
// assertion lives in the external test package (join_source_iface_test.go).
//
// Nothing here is a way around the structured-query guard. Executor and ReadTx
// hand out a query-only executor that runs guardStructured before every call, so
// an engine outside the allow-list is refused with *QueryUnsupportedError even
// if an operator lists it as a join engine, and the raw driver (writes, schema,
// transactions) is never reachable from what they return. The executor accepts
// only dal.StructuredQuery values, so a text query (which a SQL driver would
// run verbatim) is refused too.
//
// Before the engine guard, the executor checks the query as Execute and
// ExecuteDTQLQuery do. Its collection, field and alias names are checked with
// the field-name rule of the mount's engine (checkRelationalNames), so an
// engine outside quotedNameEngines is held to the strict rule here, in code. On
// an engine that builds SQL, every collection the query reads must be declared
// (guardSources), and JoinFields reads the schema of declared collections only.
//
// A policy-protected database is a single-source read through Executor. ReadTx
// refuses it (ErrProtectedReadTx): DALgo's access layer authorises only the
// base and first-level join sources of a query, so a joined query inside a
// secured transaction would read deeper sources unsecured. The same limit
// applies to a query given to Executor on a protected database, which refuses
// one that has a join, a derived source, a subquery or a scan bound
// (ErrProtectedSingleSource).

// ErrProtectedReadTx is returned by ReadTx for a database that has access
// policies: such a database is read through Executor, one single-source query
// at a time, never inside a joined read transaction.
var ErrProtectedReadTx = errors.New("a policy-protected database is read through Executor, not a read transaction")

// ErrProtectedSingleSource is returned, on a database that has access policies,
// for a structured query that is not a plain read of one collection (see
// singleSourceRead): one with a join, a derived source, a subquery anywhere, a
// scan bound, or a source that is not a plain collection. DALgo's access layer
// authorises the base and first-level join sources of a query and not what is
// nested deeper, so such a database is read one collection at a time instead of
// partly authorising a query. The decision depends on the shape of the query
// alone, before any collection name is looked at, so the error is the same
// whichever collections the query names and says nothing of which exist. The
// executor of Executor and the entry points that take one query (ExecuteDTQLQuery,
// StreamDTQLSnapshot, SelectAccessSample) return it; ReadTx refuses such a
// database whole (ErrProtectedReadTx).
var ErrProtectedSingleSource = errors.New("a policy-protected database is read one source at a time: no join, derived source or subquery")

// errJoinSourceStructuredOnly refuses a query that is not a dal.StructuredQuery.
var errJoinSourceStructuredOnly = fmt.Errorf("%w: only structured queries are accepted", ErrInvalidDTQL)

// EngineInGitDBGitHub is the Engine of an inGitDB mount whose backend is a
// GitHub repository. Its reads go over the network, so a caller routing by
// engine can tell it from the local "ingitdb" engine, which shares the manifest
// engine name.
const EngineInGitDBGitHub = "ingitdb-github"

// Engine returns the storage engine for routing decisions: the manifest's
// engine name, except that an inGitDB mount backed by GitHub is
// EngineInGitDBGitHub.
func (d *Database) Engine() string {
	engine := d.queryEngine()
	if engine == "ingitdb" && d.Manifest.Storage.InGitDB != nil && d.Manifest.Storage.InGitDB.GitHub != nil {
		return EngineInGitDBGitHub
	}
	return engine
}

// Executor returns the database as a query executor with the request's access
// policies applied (it wraps the secured driver). The executor refuses with
// *QueryUnsupportedError whenever the database cannot be queried (CanQuery).
func (d *Database) Executor() dal.QueryExecutor {
	return guardedQueryExecutor{db: d, executor: d.db}
}

// ReadTx runs fn in one read transaction of the secured driver. It returns
// *QueryUnsupportedError, without starting a transaction, when the database
// cannot be queried, and ErrProtectedReadTx when it has access policies. fn
// receives a guarded executor over the transaction, never the transaction
// itself. The error fn returns is returned as fn gave it: a driver that
// replaces it (SQL drivers wrap it with a rollback error that hides it from
// errors.Is once the context has expired) does not change what the caller sees.
// The transaction runs under a context that is cancelled when ReadTx returns,
// so the driver releases it on every exit, a panic in fn included.
func (d *Database) ReadTx(ctx context.Context, fn func(dal.QueryExecutor) error) error {
	if err := d.guardQuery(); err != nil {
		return err
	}
	if d.HasAccessPolicies() {
		return ErrProtectedReadTx
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var fnErr error
	err := d.db.RunReadonlyTransaction(ctx, func(_ context.Context, tx dal.ReadTransaction) error {
		fnErr = fn(guardedQueryExecutor{db: d, executor: tx})
		return fnErr
	})
	if fnErr != nil {
		return fnErr
	}
	return err
}

// guardedQueryExecutor offers only the query surface of an executor (and its
// optional join fields) and checks every call against the database it reads.
type guardedQueryExecutor struct {
	db       *Database
	executor dal.QueryExecutor
}

var (
	_ dal.QueryExecutor      = guardedQueryExecutor{}
	_ dal.JoinFieldsProvider = guardedQueryExecutor{}
)

// guardStructured refuses, before the driver is reached, any query that is not
// a dal.StructuredQuery (a text query runs verbatim on a SQL driver); a query
// whose names are not plain for the engine; on a database with access policies, a
// query that is not a single-source read (guardProtectedSources, which looks at
// the shape of the query and at no name, so a declared and an undeclared source
// in the same place get the same error); on a SQL engine a query that reads a
// collection the database does not declare; and an engine that is not cleared
// for queries.
func (g guardedQueryExecutor) guardStructured(query dal.Query) error {
	structured, ok := query.(dal.StructuredQuery)
	if !ok {
		return errJoinSourceStructuredOnly
	}
	if err := g.db.checkRelationalNames(structured); err != nil {
		return err
	}
	if err := g.db.guardProtectedSources(structured); err != nil {
		return err
	}
	if err := g.db.guardSources(structured); err != nil {
		return err
	}
	return g.db.guardQuery()
}

// singleSourceRead reports whether query reads one plain collection: no join,
// no derived source, no subquery anywhere and no scan bound on the source.
func singleSourceRead(query dal.StructuredQuery) bool {
	from := query.From()
	if from == nil || len(from.Joins()) != 0 || dal.HasSubquery(query) {
		return false
	}
	ref, ok := from.Base().(dal.CollectionRef)
	return ok && ref.ScanLimit() == 0 && len(ref.ScanOrders()) == 0
}

func (g guardedQueryExecutor) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	if err := g.guardStructured(query); err != nil {
		return nil, err
	}
	return g.executor.ExecuteQueryToRecordsReader(ctx, query)
}

func (g guardedQueryExecutor) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, options ...recordset.Option) (dal.RecordsetReader, error) {
	if err := g.guardStructured(query); err != nil {
		return nil, err
	}
	return g.executor.ExecuteQueryToRecordsetReader(ctx, query, options...)
}

// JoinFields supplies the fields of a declared collection to the join engine, in
// the order the engine reads them in, or none. The declared-collection and name
// checks come first, as for a read, and so does the engine guard.
//
// A database with access policies supplies no schema at all: whether it declares a
// collection is decided where its policy decides a read, at the read, and not by a
// question that comes before it (the leaf of pkg/joinexec holds the same rule).
//
// Otherwise the fields are the driver's own when it supplies them (the SQLite mount
// reads the columns of the table), and else those the manifest declares
// (declaredJoinFields). A driver that supplies none and a manifest that does not
// declare them all supplies nothing, which DALgo reads as "no schema supplied",
// exactly as for an executor that is not a JoinFieldsProvider: the engine then
// refuses a wildcard of that source and cannot tell which source carries an
// unqualified field.
func (g guardedQueryExecutor) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	if err := g.db.guardSource(source); err != nil {
		return nil, err
	}
	if err := g.db.guardQuery(); err != nil {
		return nil, err
	}
	if g.db.HasAccessPolicies() {
		return nil, nil
	}
	if provider, ok := g.executor.(dal.JoinFieldsProvider); ok {
		return provider.JoinFields(ctx, source)
	}
	return g.db.declaredJoinFields(source), nil
}

// declaredJoinFields returns the fields the manifest declares for the collection
// source reads, or nil when it does not declare them all.
//
// Only a strict database declares all the fields of a collection: a partial one
// allows fields the manifest does not list and a schemaless one declares none, and
// a list of the declared fields alone would make the engine refuse a field a record
// really holds. The source must be a plain root collection of this database (one
// that no schema, parent record or other database qualifies) that the manifest
// declares, under whichever spelling it declares it.
//
// The manifest keeps the fields of a collection in a map, which has no order, so
// the order is the one the mount provisions the collection's columns in
// (ensureCollection): the key column first on a SQL engine, whose table holds it
// whether or not the manifest declares it, and then the declared fields by name. A
// document engine holds only the declared fields in a record, by name.
func (d *Database) declaredJoinFields(source dal.RecordsetSource) []string {
	ref, ok := source.(dal.CollectionRef)
	if !ok || ref.Parent() != nil || ref.Schema() != "" {
		return nil
	}
	if named := ref.Database(); named != "" && named != d.Manifest.Database.ID {
		return nil
	}
	if d.Manifest.Database.SchemaMode != schema.ModeStrict {
		return nil
	}
	collection := d.schemaCollection(ref.Name())
	if collection == nil || len(collection.Fields) == 0 {
		return nil
	}
	fields := slices.Sorted(maps.Keys(collection.Fields))
	if !d.isDocumentEngine() {
		fields = slices.DeleteFunc(fields, func(name string) bool { return name == "id" })
		fields = slices.Insert(fields, 0, "id")
	}
	return fields
}
