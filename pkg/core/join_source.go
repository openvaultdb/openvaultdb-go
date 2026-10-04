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
// hand out a query-only executor that runs guardQuery before every call, so an
// engine outside the allow-list is refused with *QueryUnsupportedError even if
// an operator lists it as a join engine, and the raw driver (writes, schema,
// transactions) is never reachable from what they return. The executor accepts
// only dal.StructuredQuery values, so a text query (which a SQL driver would
// run verbatim) is refused too.
//
// Not checked here: Executor and ReadTx do not validate collection, field or
// alias names, unlike Execute, ExecuteDTQLQuery and StreamDTQLSnapshot. The
// caller must validate the names of every relational document before it
// reaches an executor; a caller (the planner, the database route) must not be
// wired to Executor or ReadTx before the name-check follow-up is on main.
//
// A policy-protected database is a single-source read through Executor. ReadTx
// refuses it (ErrProtectedReadTx): DALgo's access layer authorises only the
// base and first-level join sources of a query, so a joined query inside a
// secured transaction would read deeper sources unsecured. The same limit
// applies to a joined query given to Executor on a protected database, so a
// caller must send protected databases single-source queries only.

// ErrProtectedReadTx is returned by ReadTx for a database that has access
// policies: such a database is read through Executor, one single-source query
// at a time, never inside a joined read transaction.
var ErrProtectedReadTx = errors.New("a policy-protected database is read through Executor, not a read transaction")

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
	return guardedQueryExecutor{guard: d.guardQuery, executor: d.db}
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
		fnErr = fn(guardedQueryExecutor{guard: d.guardQuery, executor: tx})
		return fnErr
	})
	if fnErr != nil {
		return fnErr
	}
	return err
}

// guardedQueryExecutor offers only the query surface of an executor (and its
// optional join fields) and runs guard before each call.
type guardedQueryExecutor struct {
	guard    func() error
	executor dal.QueryExecutor
}

var (
	_ dal.QueryExecutor      = guardedQueryExecutor{}
	_ dal.JoinFieldsProvider = guardedQueryExecutor{}
)

// guardStructured runs the engine guard, then refuses any query that is not a
// dal.StructuredQuery (a text query runs verbatim on a SQL driver).
func (g guardedQueryExecutor) guardStructured(query dal.Query) error {
	if err := g.guard(); err != nil {
		return err
	}
	if _, ok := query.(dal.StructuredQuery); !ok {
		return errJoinSourceStructuredOnly
	}
	return nil
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
	if err := g.guard(); err != nil {
		return nil, err
	}
	if provider, ok := g.executor.(dal.JoinFieldsProvider); ok {
		return provider.JoinFields(ctx, source)
	}
	return nil, nil
}
