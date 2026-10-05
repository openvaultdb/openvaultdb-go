package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

var (
	errJoinSrcReader  = errors.New("join source test: reader reached")
	errJoinSrcFields  = errors.New("join source test: fields reached")
	errJoinSrcTxStart = errors.New("join source test: transaction did not start")
	errJoinSrcWorker  = errors.New("join source test: worker failed")
	errJoinSrcRollbk  = errors.New("join source test: rollback failed")
	errJoinSrcPanic   = errors.New("join source test: callback panicked")
)

// joinSrcRecorder counts the calls that reach the fake driver.
type joinSrcRecorder struct {
	readers    int
	recordsets int
	fields     int
	txStarts   int
	txCtx      context.Context // the context the driver was given for the transaction
}

func joinSrcReaderErr(calls *joinSrcRecorder) (dal.RecordsReader, error) {
	calls.readers++
	return nil, errJoinSrcReader
}

func joinSrcRecordsetErr(calls *joinSrcRecorder) (dal.RecordsetReader, error) {
	calls.recordsets++
	return nil, errJoinSrcReader
}

// joinSrcTx is a read transaction with only the query surface; every other
// method panics (nil embedded interface).
type joinSrcTx struct {
	dal.ReadTransaction
	calls *joinSrcRecorder
}

func (t joinSrcTx) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return joinSrcReaderErr(t.calls)
}

func (t joinSrcTx) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	return joinSrcRecordsetErr(t.calls)
}

// joinSrcTxFields is a transaction that also supplies join fields.
type joinSrcTxFields struct{ joinSrcTx }

func (t joinSrcTxFields) JoinFields(context.Context, dal.RecordsetSource) ([]string, error) {
	t.calls.fields++
	return []string{"id", "name"}, errJoinSrcFields
}

// joinSrcDB is a driver with the query surface and a read-only transaction.
// When tx is nil the transaction fails to start.
type joinSrcDB struct {
	dal.DB
	calls *joinSrcRecorder
	tx    dal.ReadTransaction
	// wrapRollback makes the driver answer a failed worker the way dalgo2sql
	// does once the context has expired: dal.NewRollbackError, which hides the
	// worker's error from errors.Is.
	wrapRollback bool
}

func (d joinSrcDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return joinSrcReaderErr(d.calls)
}

func (d joinSrcDB) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	return joinSrcRecordsetErr(d.calls)
}

func (d joinSrcDB) RunReadonlyTransaction(ctx context.Context, f dal.ROTxWorker, _ ...dal.TransactionOption) error {
	d.calls.txStarts++
	d.calls.txCtx = ctx
	if d.tx == nil {
		return errJoinSrcTxStart
	}
	err := f(ctx, d.tx)
	if err != nil && d.wrapRollback {
		return dal.NewRollbackError(errJoinSrcRollbk, err)
	}
	return err
}

// joinSrcFieldsDB is a driver that also supplies join fields.
type joinSrcFieldsDB struct{ joinSrcDB }

func (d joinSrcFieldsDB) JoinFields(context.Context, dal.RecordsetSource) ([]string, error) {
	d.calls.fields++
	return []string{"id"}, errJoinSrcFields
}

func joinSrcOpen(t *testing.T, engine string, mutate func(*manifest.Storage), build func(*joinSrcRecorder) dal.DB) (*Database, *joinSrcRecorder) {
	t.Helper()
	calls := &joinSrcRecorder{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "joinsrc", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas:  joinSrcSchemas(),
	}
	if mutate != nil {
		mutate(&m.Storage)
	}
	db, err := Open(m, build(calls), []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db, calls
}

// joinSrcSchemas declares the one collection the tests of this file read.
func joinSrcSchemas() *schema.Schemas {
	return &schema.Schemas{Collections: map[string]schema.Collection{
		"items": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
	}}
}

func joinSrcPlainDB(calls *joinSrcRecorder) dal.DB {
	return joinSrcDB{calls: calls}
}

func joinSrcQuery(t *testing.T) dal.Query {
	t.Helper()
	q, _, err := ParseDTQL([]byte("from: {name: items}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestJoinSourceEngineDistinguishesGitHubBackend(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		mutate func(*manifest.Storage)
		want   string
	}{
		{"sqlite", "sqlite", nil, "sqlite"},
		{"postgres", "postgres", nil, "postgres"},
		{"local ingitdb without options", "ingitdb", nil, "ingitdb"},
		{"local ingitdb with options", "ingitdb", func(s *manifest.Storage) { s.InGitDB = &manifest.InGitDBOptions{} }, "ingitdb"},
		{"ingitdb on github", "ingitdb", func(s *manifest.Storage) {
			s.InGitDB = &manifest.InGitDBOptions{GitHub: &manifest.InGitDBGitHubOptions{}}
		}, EngineInGitDBGitHub},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := joinSrcOpen(t, tc.engine, tc.mutate, joinSrcPlainDB)
			if got := db.Engine(); got != tc.want {
				t.Fatalf("Engine() = %q, want %q", got, tc.want)
			}
		})
	}
	if got := (&Database{}).Engine(); got != "" {
		t.Fatalf("Engine() without a manifest = %q, want empty", got)
	}
}

func TestJoinSourceExecutorRefusesEnginesOutsideTheQueryAllowList(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []string{"postgres", "mysql", "unknown", ""} {
		t.Run("engine="+engine, func(t *testing.T) {
			db, calls := joinSrcOpen(t, engine, nil, func(c *joinSrcRecorder) dal.DB {
				return joinSrcFieldsDB{joinSrcDB{calls: c, tx: joinSrcTx{calls: c}}}
			})
			executor := db.Executor()
			if executor == nil {
				t.Fatal("Executor() must return a refusing executor, not nil")
			}
			check := func(what string, err error) {
				t.Helper()
				var unsupported *QueryUnsupportedError
				if !errors.As(err, &unsupported) || unsupported.Engine != engine || !errors.Is(err, ErrQueryUnsupported) {
					t.Errorf("%s: want *QueryUnsupportedError for %q, got %v", what, engine, err)
				}
			}
			_, err := executor.ExecuteQueryToRecordsReader(ctx, joinSrcQuery(t))
			check("ExecuteQueryToRecordsReader", err)
			_, err = executor.ExecuteQueryToRecordsetReader(ctx, joinSrcQuery(t))
			check("ExecuteQueryToRecordsetReader", err)
			provider, ok := executor.(dal.JoinFieldsProvider)
			if !ok {
				t.Fatal("executor must offer JoinFields so a refusal is not read as an absent schema")
			}
			fields, err := provider.JoinFields(ctx, dal.NewRootCollectionRef("items", ""))
			check("JoinFields", err)
			if fields != nil {
				t.Errorf("JoinFields served fields %v on a refused engine", fields)
			}
			readTxRan := false
			err = db.ReadTx(ctx, func(dal.QueryExecutor) error { readTxRan = true; return nil })
			check("ReadTx", err)
			if readTxRan {
				t.Error("ReadTx called its worker on a refused engine")
			}
			if *calls != (joinSrcRecorder{}) {
				t.Errorf("the driver was reached on a refused engine: %+v", *calls)
			}
		})
	}
}

func TestJoinSourceExecutorForwardsOnAllowedEngines(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []string{"sqlite", "ingitdb", "firestore"} {
		t.Run(engine, func(t *testing.T) {
			db, calls := joinSrcOpen(t, engine, nil, joinSrcPlainDB)
			executor := db.Executor()
			if _, err := executor.ExecuteQueryToRecordsReader(ctx, joinSrcQuery(t)); !errors.Is(err, errJoinSrcReader) {
				t.Fatalf("records reader: got %v, want the driver's error", err)
			}
			if _, err := executor.ExecuteQueryToRecordsetReader(ctx, joinSrcQuery(t)); !errors.Is(err, errJoinSrcReader) {
				t.Fatalf("recordset reader: got %v, want the driver's error", err)
			}
			if calls.readers != 1 || calls.recordsets != 1 {
				t.Fatalf("driver calls = %+v, want one of each", *calls)
			}
		})
	}
}

func TestJoinSourceExecutorDoesNotExposeTheDriver(t *testing.T) {
	db, _ := joinSrcOpen(t, "sqlite", nil, func(c *joinSrcRecorder) dal.DB {
		return joinSrcDB{calls: c, tx: joinSrcTx{calls: c}}
	})
	executor := db.Executor()
	if _, ok := executor.(dal.DB); ok {
		t.Error("executor exposes the dal.DB")
	}
	if _, ok := executor.(dal.WriteSession); ok {
		t.Error("executor exposes a write session")
	}
	if _, ok := executor.(interface {
		RunReadonlyTransaction(context.Context, dal.ROTxWorker, ...dal.TransactionOption) error
	}); ok {
		t.Error("executor exposes the transaction coordinator")
	}
	ran := false
	err := db.ReadTx(context.Background(), func(tx dal.QueryExecutor) error {
		ran = true
		if _, ok := tx.(dal.ReadTransaction); ok {
			t.Error("transaction executor exposes the raw read transaction")
		}
		if _, ok := tx.(dal.DB); ok {
			t.Error("transaction executor exposes the dal.DB")
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("err = %v, callback ran = %v", err, ran)
	}
}

func TestJoinSourceJoinFields(t *testing.T) {
	ctx := context.Background()
	source := dal.NewRootCollectionRef("items", "")

	t.Run("driver without join fields serves the fields the manifest declares", func(t *testing.T) {
		db, calls := joinSrcOpen(t, "sqlite", nil, joinSrcPlainDB)
		fields, err := db.Executor().(dal.JoinFieldsProvider).JoinFields(ctx, source)
		if want := []string{"id", "name"}; err != nil || !reflect.DeepEqual(fields, want) {
			t.Fatalf("got %v, %v; want %v", fields, err, want)
		}
		if calls.fields != 0 {
			t.Fatalf("fields calls = %d", calls.fields)
		}
	})

	t.Run("driver with join fields is forwarded", func(t *testing.T) {
		db, calls := joinSrcOpen(t, "sqlite", nil, func(c *joinSrcRecorder) dal.DB {
			return joinSrcFieldsDB{joinSrcDB{calls: c}}
		})
		fields, err := db.Executor().(dal.JoinFieldsProvider).JoinFields(ctx, source)
		if !errors.Is(err, errJoinSrcFields) || len(fields) != 1 || fields[0] != "id" {
			t.Fatalf("got %v, %v", fields, err)
		}
		if calls.fields != 1 {
			t.Fatalf("fields calls = %d", calls.fields)
		}
	})
}

func TestJoinSourceReadTx(t *testing.T) {
	ctx := context.Background()

	t.Run("a transaction that cannot start is reported", func(t *testing.T) {
		db, calls := joinSrcOpen(t, "sqlite", nil, joinSrcPlainDB)
		ran := false
		err := db.ReadTx(ctx, func(dal.QueryExecutor) error { ran = true; return nil })
		if !errors.Is(err, errJoinSrcTxStart) || ran {
			t.Fatalf("err = %v, worker ran = %v", err, ran)
		}
		if calls.txStarts != 1 {
			t.Fatalf("tx starts = %d", calls.txStarts)
		}
	})

	t.Run("the worker's error is returned unchanged", func(t *testing.T) {
		db, _ := joinSrcOpen(t, "sqlite", nil, func(c *joinSrcRecorder) dal.DB {
			return joinSrcDB{calls: c, tx: joinSrcTx{calls: c}}
		})
		err := db.ReadTx(ctx, func(dal.QueryExecutor) error { return errJoinSrcWorker })
		if err != errJoinSrcWorker {
			t.Fatalf("got %v, want the worker's error itself", err)
		}
	})

	t.Run("the transaction executor forwards queries and fields", func(t *testing.T) {
		db, calls := joinSrcOpen(t, "sqlite", nil, func(c *joinSrcRecorder) dal.DB {
			tx := joinSrcTxFields{joinSrcTx{calls: c}}
			return joinSrcDB{calls: c, tx: tx}
		})
		err := db.ReadTx(ctx, func(tx dal.QueryExecutor) error {
			if _, err := tx.ExecuteQueryToRecordsReader(ctx, joinSrcQuery(t)); !errors.Is(err, errJoinSrcReader) {
				t.Errorf("records reader: %v", err)
			}
			if _, err := tx.ExecuteQueryToRecordsetReader(ctx, joinSrcQuery(t)); !errors.Is(err, errJoinSrcReader) {
				t.Errorf("recordset reader: %v", err)
			}
			fields, err := tx.(dal.JoinFieldsProvider).JoinFields(ctx, dal.NewRootCollectionRef("items", ""))
			if !errors.Is(err, errJoinSrcFields) || len(fields) != 2 {
				t.Errorf("fields: %v, %v", fields, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls.readers != 1 || calls.recordsets != 1 || calls.fields != 1 {
			t.Fatalf("driver calls = %+v", *calls)
		}
	})

	t.Run("a rollback error that hides the worker's error is not returned", func(t *testing.T) {
		db, _ := joinSrcOpen(t, "sqlite", nil, func(c *joinSrcRecorder) dal.DB {
			return joinSrcDB{calls: c, tx: joinSrcTx{calls: c}, wrapRollback: true}
		})
		err := db.ReadTx(ctx, func(dal.QueryExecutor) error { return errJoinSrcWorker })
		if err != errJoinSrcWorker {
			t.Fatalf("got %v, want the worker's error itself", err)
		}
	})

	t.Run("the transaction is released when the callback panics", func(t *testing.T) {
		db, calls := joinSrcOpen(t, "sqlite", nil, func(c *joinSrcRecorder) dal.DB {
			return joinSrcDB{calls: c, tx: joinSrcTx{calls: c}}
		})
		func() {
			defer func() {
				if recovered := recover(); recovered != errJoinSrcPanic {
					t.Errorf("recovered %v, want the callback's panic", recovered)
				}
			}()
			_ = db.ReadTx(ctx, func(dal.QueryExecutor) error { panic(errJoinSrcPanic) })
		}()
		if calls.txCtx == nil || calls.txCtx.Err() == nil {
			t.Fatal("the context given to the driver was not cancelled after the panic")
		}
	})

	t.Run("a cancelled request context is passed on", func(t *testing.T) {
		db, calls := joinSrcOpen(t, "sqlite", nil, func(c *joinSrcRecorder) dal.DB {
			return joinSrcDB{calls: c, tx: joinSrcTx{calls: c}}
		})
		parent, cancel := context.WithCancel(ctx)
		err := db.ReadTx(parent, func(dal.QueryExecutor) error {
			cancel()
			if calls.txCtx.Err() == nil {
				t.Error("cancelling the request context did not reach the driver's context")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestJoinSourceReadTxRefusesAPolicyProtectedDatabase(t *testing.T) {
	calls := &joinSrcRecorder{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "joinsrc", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  joinSrcSchemas(),
	}
	db, err := Open(m, joinSrcDB{calls: calls, tx: joinSrcTx{calls: calls}}, []schema.Mode{schema.ModeStrict}, "", joinSrcPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !db.HasAccessPolicies() {
		t.Fatal("the database must have access policies")
	}
	ran := false
	err = db.ReadTx(context.Background(), func(dal.QueryExecutor) error { ran = true; return nil })
	if !errors.Is(err, ErrProtectedReadTx) || ran {
		t.Fatalf("err = %v, callback ran = %v; want ErrProtectedReadTx", err, ran)
	}
	if *calls != (joinSrcRecorder{}) {
		t.Fatalf("the driver was reached for a protected database: %+v", *calls)
	}
}

// joinSrcPolicy is an access policy that is never consulted by these tests.
type joinSrcPolicy struct{}

func (joinSrcPolicy) Name() string { return "joinsrc-test" }
func (joinSrcPolicy) Decide(context.Context, access.Request) access.Decision {
	return access.Decision{}
}
func (joinSrcPolicy) Authorize(context.Context, access.Request) error { return nil }

func TestJoinSourceExecutorRefusesNonStructuredQueries(t *testing.T) {
	ctx := context.Background()
	text := dal.NewTextQuery("DELETE FROM items", nil)
	db, calls := joinSrcOpen(t, "sqlite", nil, func(c *joinSrcRecorder) dal.DB {
		return joinSrcDB{calls: c, tx: joinSrcTx{calls: c}}
	})
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrInvalidDTQL) {
			t.Errorf("%s: want ErrInvalidDTQL, got %v", what, err)
		}
	}
	_, err := db.Executor().ExecuteQueryToRecordsReader(ctx, text)
	check("Executor records reader", err)
	_, err = db.Executor().ExecuteQueryToRecordsetReader(ctx, text)
	check("Executor recordset reader", err)
	err = db.ReadTx(ctx, func(tx dal.QueryExecutor) error {
		_, err := tx.ExecuteQueryToRecordsReader(ctx, text)
		check("ReadTx records reader", err)
		_, err = tx.ExecuteQueryToRecordsetReader(ctx, text)
		check("ReadTx recordset reader", err)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.readers != 0 || calls.recordsets != 0 {
		t.Fatalf("a text query reached the driver: %+v", *calls)
	}
}

// joinSrcAllowPolicy allows every request; joinSrcDenyPolicy denies every one.
// The decision is the policy's own, so a read that is denied never reaches the
// driver, and one that is allowed reaches it through the same wrapper.
type joinSrcAllowPolicy struct{}

func (joinSrcAllowPolicy) Name() string { return "joinsrc-allow" }
func (joinSrcAllowPolicy) Decide(_ context.Context, r access.Request) access.Decision {
	return access.Decision{Allowed: true, Operation: r.Operation, Policy: "joinsrc-allow"}
}
func (joinSrcAllowPolicy) Authorize(context.Context, access.Request) error { return nil }

type joinSrcDenyPolicy struct{}

func (joinSrcDenyPolicy) Name() string { return "joinsrc-deny" }
func (joinSrcDenyPolicy) Decide(_ context.Context, r access.Request) access.Decision {
	return access.Decision{Allowed: false, Operation: r.Operation, Policy: "joinsrc-deny"}
}
func (joinSrcDenyPolicy) Authorize(context.Context, access.Request) error {
	return access.ErrAccessDenied
}

// joinSrcProtected opens a fake-backed database with one access policy. It
// declares the collections of srcGuardCases and items.
func joinSrcProtected(t *testing.T, engine string, policy access.Policy) (*Database, *joinSrcRecorder) {
	t.Helper()
	calls := &joinSrcRecorder{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "joinsrc", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas:  joinSrcSchemas(),
	}
	for _, name := range []string{"customers", "orders", "ghost"} {
		m.Schemas.Collections[name] = schema.Collection{Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}
	}
	db, err := Open(m, joinSrcDB{calls: calls, tx: joinSrcTx{calls: calls}}, []schema.Mode{schema.ModeStrict}, "", policy)
	if err != nil {
		t.Fatal(err)
	}
	if !db.HasAccessPolicies() {
		t.Fatal("the database must have access policies")
	}
	return db, calls
}

// TestJoinSourceExecutorReadsThroughThePolicyWrapper: Executor wraps the secured
// driver, so the access policies are applied to a read it serves.
func TestJoinSourceExecutorReadsThroughThePolicyWrapper(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []string{"sqlite", "ingitdb"} {
		t.Run(engine, func(t *testing.T) {
			denied, deniedCalls := joinSrcProtected(t, engine, joinSrcDenyPolicy{})
			_, err := denied.Executor().ExecuteQueryToRecordsReader(ctx, joinSrcQuery(t))
			if !errors.Is(err, access.ErrAccessDenied) {
				t.Fatalf("a denying policy: got %v, want access.ErrAccessDenied", err)
			}
			if _, err = denied.Executor().ExecuteQueryToRecordsetReader(ctx, joinSrcQuery(t)); !errors.Is(err, access.ErrAccessDenied) {
				t.Fatalf("a denying policy, recordset reader: got %v, want access.ErrAccessDenied", err)
			}
			if deniedCalls.readers != 0 || deniedCalls.recordsets != 0 {
				t.Fatalf("a denied read reached the driver: %+v", *deniedCalls)
			}

			allowed, allowedCalls := joinSrcProtected(t, engine, joinSrcAllowPolicy{})
			if _, err = allowed.Executor().ExecuteQueryToRecordsReader(ctx, joinSrcQuery(t)); !errors.Is(err, errJoinSrcReader) {
				t.Fatalf("an allowing policy: got %v, want the driver's error", err)
			}
			if allowedCalls.readers != 1 {
				t.Fatalf("an allowed read must reach the driver once: %+v", *allowedCalls)
			}
		})
	}
}

// TestJoinSourceExecutorRefusesNonSingleSourceReadsOnAProtectedDatabase: DALgo's
// access layer authorises only the base and first-level join sources, so a
// protected database serves one plain collection per query, and no query with a
// join, a derived source or a subquery reaches the driver. The refusal depends on
// the shape of the query alone: a source the database declares and a source it
// does not declare, in the same place, get the same error, so the error does not
// say which collections exist. A read of one plain collection is a different
// case: an undeclared one is ErrNotFound, and a declared one reaches the driver.
func TestJoinSourceExecutorRefusesNonSingleSourceReadsOnAProtectedDatabase(t *testing.T) {
	ctx := context.Background()
	for _, engine := range []string{"sqlite", "ingitdb"} {
		for _, source := range []struct {
			name, collection string
			declared         bool
		}{{"declared", "ghost", true}, {"undeclared", "nowhere", false}} {
			for _, tc := range srcGuardCases(source.collection) {
				t.Run(engine+"/"+source.name+"/"+tc.name, func(t *testing.T) {
					db, calls := joinSrcProtected(t, engine, joinSrcAllowPolicy{})
					plain := tc.name == "root"
					_, err := db.Executor().ExecuteQueryToRecordsReader(ctx, tc.query)
					_, recordsetErr := db.Executor().ExecuteQueryToRecordsetReader(ctx, tc.query)
					if plain {
						if source.declared {
							if !errors.Is(err, errJoinSrcReader) || !errors.Is(recordsetErr, errJoinSrcReader) || calls.readers != 1 || calls.recordsets != 1 {
								t.Fatalf("a single-source read: %v, %v, calls %+v", err, recordsetErr, *calls)
							}
							return
						}
						// On sqlite the source guard answers the undeclared collection; ingitdb
						// keeps the collection rule of the document engines and reaches the
						// driver.
						if engine == "sqlite" && (!errors.Is(err, ErrNotFound) || !errors.Is(recordsetErr, ErrNotFound) || calls.readers != 0 || calls.recordsets != 0) {
							t.Fatalf("a single-source read of an undeclared collection: %v, %v, calls %+v", err, recordsetErr, *calls)
						}
						if engine == "ingitdb" && (!errors.Is(err, errJoinSrcReader) || !errors.Is(recordsetErr, errJoinSrcReader) || calls.readers != 1 || calls.recordsets != 1) {
							t.Fatalf("a single-source read on a document engine: %v, %v, calls %+v", err, recordsetErr, *calls)
						}
						return
					}
					if !errors.Is(err, ErrProtectedSingleSource) || !errors.Is(recordsetErr, ErrProtectedSingleSource) {
						t.Fatalf("got %v, %v; want ErrProtectedSingleSource", err, recordsetErr)
					}
					if calls.readers != 0 || calls.recordsets != 0 || calls.txStarts != 0 {
						t.Fatalf("a refused read reached the driver: %+v", *calls)
					}
				})
			}
		}
	}
}

// TestJoinSourceExecutorServesJoinsOnAnUnprotectedDatabase is the control of
// the test above: without access policies the same joined query is passed on.
func TestJoinSourceExecutorServesJoinsOnAnUnprotectedDatabase(t *testing.T) {
	db, calls := srcGuardJoinOpen(t, "sqlite", "customers", "orders", "ghost")
	for _, tc := range srcGuardCases("ghost") {
		if _, err := db.Executor().ExecuteQueryToRecordsReader(context.Background(), tc.query); !errors.Is(err, errJoinSrcReader) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if calls.readers != len(srcGuardCases("ghost")) {
		t.Fatalf("driver reads = %d", calls.readers)
	}
}

func TestSingleSourceRead(t *testing.T) {
	plain := dal.NewRootCollectionRef("items", "")
	for label, tc := range map[string]struct {
		query dal.StructuredQuery
		want  bool
	}{
		"plain":            {selectQuery(fromTree(plain).NewQuery()), true},
		"aliased":          {selectQuery(fromTree(dal.NewRootCollectionRef("items", "i")).NewQuery()), true},
		"filtered":         {selectQuery(fromTree(plain).NewQuery().WhereField("name", dal.Equal, "x").OrderBy(dal.AscendingField("name"))), true},
		"scan limit":       {selectQuery(fromTree(plain.WithScan(5)).NewQuery()), false},
		"scan order":       {selectQuery(fromTree(plain.WithScan(0, dal.AscendingField("name"))).NewQuery()), false},
		"join":             {selectQuery(fromTree(plain, dal.NewJoinedSource(rootRef("b"), dal.JoinInner, srcGuardJoinOn("items", "b"))).NewQuery()), false},
		"derived":          {selectQuery(fromTree(dal.NewQuerySource(srcGuardInner("items"), "d")).NewQuery()), false},
		"subquery":         {withExists("items", srcGuardInner("b")), false},
		"collection group": {selectQuery(fromTree(dal.NewCollectionGroupRef("items", "")).NewQuery()), false},
		"unknown source":   {selectQuery(fromTree(unknownSource{plain}).NewQuery()), false},
		"pointer":          {selectQuery(fromTree(&plain).NewQuery()), false},
		"no base":          {shapeQuery{StructuredQuery: srcGuardInner("items"), from: nilFrom{dal.From(plain)}, hasFrom: true}, false},
		"no from":          {noFromQuery{srcGuardInner("items")}, false},
	} {
		if got := singleSourceRead(tc.query); got != tc.want {
			t.Errorf("%s: singleSourceRead = %v, want %v", label, got, tc.want)
		}
	}
}
