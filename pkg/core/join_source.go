package core

import (
	"context"

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
// transactions) is never reachable from what they return.

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
// cannot be queried. fn receives a guarded executor over the transaction, never
// the transaction itself, and its error is returned as the driver gave it.
func (d *Database) ReadTx(ctx context.Context, fn func(dal.QueryExecutor) error) error {
	if err := d.guardQuery(); err != nil {
		return err
	}
	return d.db.RunReadonlyTransaction(ctx, func(_ context.Context, tx dal.ReadTransaction) error {
		return fn(guardedQueryExecutor{guard: d.guardQuery, executor: tx})
	})
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

func (g guardedQueryExecutor) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	if err := g.guard(); err != nil {
		return nil, err
	}
	return g.executor.ExecuteQueryToRecordsReader(ctx, query)
}

func (g guardedQueryExecutor) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, options ...recordset.Option) (dal.RecordsetReader, error) {
	if err := g.guard(); err != nil {
		return nil, err
	}
	return g.executor.ExecuteQueryToRecordsetReader(ctx, query, options...)
}

// JoinFields passes the driver's schema-ordered fields through when it has
// them. A driver without them serves none, which DALgo reads as "no schema
// supplied", exactly as for an executor that is not a JoinFieldsProvider.
func (g guardedQueryExecutor) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	if err := g.guard(); err != nil {
		return nil, err
	}
	if provider, ok := g.executor.(dal.JoinFieldsProvider); ok {
		return provider.JoinFields(ctx, source)
	}
	return nil, nil
}
