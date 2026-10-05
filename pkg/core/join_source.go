package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
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
// one that has a join, a derived source or a subquery (ErrProtectedSingleSource).

// ErrProtectedReadTx is returned by ReadTx for a database that has access
// policies: such a database is read through Executor, one single-source query
// at a time, never inside a joined read transaction.
var ErrProtectedReadTx = errors.New("a policy-protected database is read through Executor, not a read transaction")

// ErrProtectedSingleSource is returned by the executors of a database that has
// access policies for a query that is not a plain single-source read: one with a
// join, a derived source, a subquery or a scan bound. Such a database is read
// one collection at a time.
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
// whose names are not plain for the engine; on a SQL engine a query that reads a
// collection the database does not declare; an engine that is not cleared for
// queries; and, on a database with access policies, a query that is not a
// single-source read.
func (g guardedQueryExecutor) guardStructured(query dal.Query) error {
	structured, ok := query.(dal.StructuredQuery)
	if !ok {
		return errJoinSourceStructuredOnly
	}
	if err := g.db.checkRelationalNames(structured); err != nil {
		return err
	}
	if err := g.db.guardSources(structured); err != nil {
		return err
	}
	if err := g.db.guardQuery(); err != nil {
		return err
	}
	if g.db.HasAccessPolicies() && !singleSourceRead(structured) {
		return ErrProtectedSingleSource
	}
	return nil
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

// JoinFields passes the driver's schema-ordered fields through when it has
// them. A driver without them serves none, which DALgo reads as "no schema
// supplied", exactly as for an executor that is not a JoinFieldsProvider.
// No current mount supplies fields through this path: the SQLite mount hides
// the driver's JoinFields, and inGitDB, Firestore and the secured wrapper have
// none, so a qualified wildcard column fails in DALgo on the in-memory join
// route ("wildcard expansion requires ordered schema metadata"). Callers
// (OJ-03, OJ-05) must use explicit columns until a mount forwards JoinFields.
func (g guardedQueryExecutor) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	if err := g.db.guardSource(source); err != nil {
		return nil, err
	}
	if err := g.db.guardQuery(); err != nil {
		return nil, err
	}
	if provider, ok := g.executor.(dal.JoinFieldsProvider); ok {
		return provider.JoinFields(ctx, source)
	}
	return nil, nil
}
