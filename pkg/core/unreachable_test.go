package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/record"
)

// The connection failures of the adapter (a *dalgo2postgres.ConnectionError) are one
// error of the core, whatever the call that met them: *UnreachableError, which holds
// the ID of the mount and the adapter's fixed sentence and nothing of the connection.

const (
	unreachableHost     = "db.internal.example"
	unreachablePort     = "6432"
	unreachableDatabase = "ledger_prod"
)

// connectionFailure is an error of the adapter that names the host, the port and the
// database, as the adapter does once it has checked them.
func connectionFailure(kind dalgo2postgres.FailureKind, sqlState string) error {
	return fmt.Errorf("driver call: %w", &dalgo2postgres.ConnectionError{
		Kind: kind, SQLState: sqlState, Host: unreachableHost, Port: unreachablePort, Database: unreachableDatabase,
	})
}

// requireUnreachable fails unless err is the built error of a mount that cannot be
// reached, for the mount "scripted", holding the sentence and none of the connection.
func requireUnreachable(t *testing.T, err error, sentence string) {
	t.Helper()
	if !errors.Is(err, ErrDatabaseUnreachable) {
		t.Fatalf("error = %v, want ErrDatabaseUnreachable", err)
	}
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) || unreachable.Database != "scripted" {
		t.Fatalf("error = %#v, want an *UnreachableError of the mount scripted", err)
	}
	if !strings.Contains(unreachable.Reason, sentence) {
		t.Errorf("reason = %q, want it to hold %q", unreachable.Reason, sentence)
	}
	for _, leaked := range []string{unreachableHost, unreachablePort, unreachableDatabase, "driver call"} {
		if strings.Contains(err.Error(), leaked) || strings.Contains(unreachable.Reason, leaked) {
			t.Errorf("%q of the connection is in the error: %v", leaked, err)
		}
	}
	if errors.Unwrap(unreachable) != nil {
		t.Errorf("the error wraps a cause: %v", errors.Unwrap(unreachable))
	}
}

func TestUnreachableErrorNamesTheMountAndTheSentence(t *testing.T) {
	err := &UnreachableError{Database: "pg", Reason: "dalgo2postgres: the server could not be reached"}
	if got, want := err.Error(), `database "pg" cannot be reached: dalgo2postgres: the server could not be reached`; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrDatabaseUnreachable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("Is: ErrDatabaseUnreachable %v, ErrNotFound %v", errors.Is(err, ErrDatabaseUnreachable), errors.Is(err, ErrNotFound))
	}
}

// Every kind of failure of the adapter is the same error of the core, with the
// adapter's sentence of that kind and its SQLSTATE code when the server answered.
func TestUnreachableOfKeepsTheAdapterSentenceAndDropsTheConnection(t *testing.T) {
	db := openScripted(t, "postgres", &scriptedQueryDB{})
	for _, c := range []struct {
		name     string
		kind     dalgo2postgres.FailureKind
		sqlState string
		want     string
	}{
		{"network", dalgo2postgres.FailureNetwork, "", "the server could not be reached"},
		{"tls", dalgo2postgres.FailureTLS, "", "the TLS handshake with the server failed"},
		{"timeout", dalgo2postgres.FailureTimeout, "", "the connection timed out or was canceled"},
		{"server", dalgo2postgres.FailureServer, "28P01", "password authentication failed (SQLSTATE 28P01)"},
		{"invalid", dalgo2postgres.FailureInvalidDSN, "", "the connection string cannot be parsed"},
		{"misread", dalgo2postgres.FailureMisread, "", "the connection string is not read as intended"},
		{"other", dalgo2postgres.FailureOther, "", "the connection failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			unreachable := db.unreachableOf(connectionFailure(c.kind, c.sqlState))
			if unreachable == nil {
				t.Fatal("a connection failure is not unreachable")
			}
			requireUnreachable(t, unreachable, c.want)
		})
	}
	for name, err := range map[string]error{
		"nil":       nil,
		"plain":     errors.New("boom"),
		"not found": ErrNotFound,
	} {
		t.Run("not a connection failure/"+name, func(t *testing.T) {
			if got := db.unreachableOf(err); got != nil {
				t.Fatalf("unreachableOf = %v, want nil", got)
			}
		})
	}
}

// A request whose context has ended keeps the error it had: the context is the
// reason, not the database.
func TestReachedLeavesAnEndedRequestAlone(t *testing.T) {
	db := openScripted(t, "postgres", &scriptedQueryDB{})
	failure := connectionFailure(dalgo2postgres.FailureTimeout, "")
	if got := db.reached(context.Background(), nil); got != nil {
		t.Errorf("reached(nil) = %v", got)
	}
	plain := errors.New("plain")
	if got := db.reached(context.Background(), plain); got != plain {
		t.Errorf("reached(plain) = %v, want it unchanged", got)
	}
	requireUnreachable(t, db.reached(context.Background(), failure), "the connection timed out or was canceled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := db.reached(ctx, failure); got != failure {
		t.Errorf("reached(ended context) = %v, want the adapter's error as it was", got)
	}
}

// scriptedReaderDB is scriptedQueryDB with the key reads, the write transaction and the
// schema reader of a driver, each failing with failure.
type scriptedReaderDB struct {
	scriptedQueryDB
	failure error
}

func (f *scriptedReaderDB) Get(context.Context, record.Record) error { return f.failure }
func (f *scriptedReaderDB) Exists(context.Context, *record.Key) (bool, error) {
	return false, f.failure
}
func (f *scriptedReaderDB) RunReadwriteTransaction(context.Context, dal.RWTxWorker, ...dal.TransactionOption) error {
	return f.failure
}
func (f *scriptedReaderDB) ListCollections(context.Context, *record.Key) ([]dal.CollectionRef, error) {
	return nil, f.failure
}
func (f *scriptedReaderDB) DescribeCollection(context.Context, *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	return nil, f.failure
}
func (f *scriptedReaderDB) ListIndexes(context.Context, *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	return nil, f.failure
}
func (f *scriptedReaderDB) ListConstraints(context.Context, *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	return nil, f.failure
}
func (f *scriptedReaderDB) ListReferrers(context.Context, *dal.CollectionRef) ([]dbschema.Referrer, error) {
	return nil, f.failure
}

// A connection that fails in a key read, an existence check, a write, the list of
// collections or the description of one is the built error, on the engine that
// reaches a server; any other error of the driver is returned as it was.
func TestAConnectionFailureOfAKeyReadAWriteOrTheSchemaReaderIsUnreachable(t *testing.T) {
	ctx := context.Background()
	key := record.NewKeyWithID("customers", "c1")
	calls := map[string]func(*Database) error{
		"get":    func(d *Database) error { _, err := d.Get(ctx, key); return err },
		"exists": func(d *Database) error { _, err := d.Exists(ctx, key); return err },
		"apply": func(d *Database) error {
			_, err := d.Apply(ctx, []Op{{Op: "delete", Key: key}}, "")
			return err
		},
		"collections": func(d *Database) error { _, err := d.Collections(ctx); return err },
		"foreign keys": func(d *Database) error {
			_, err := d.CollectionForeignKeys(ctx, "customers")
			return err
		},
	}
	for name, call := range calls {
		t.Run(name+"/a connection failure", func(t *testing.T) {
			db := openScripted(t, "postgres", &scriptedReaderDB{failure: connectionFailure(dalgo2postgres.FailureNetwork, "")})
			requireUnreachable(t, call(db), "the server could not be reached")
		})
		t.Run(name+"/another failure", func(t *testing.T) {
			other := errors.New("some other failure of the driver")
			db := openScripted(t, "postgres", &scriptedReaderDB{failure: other})
			if err := call(db); errors.Is(err, ErrDatabaseUnreachable) || !errors.Is(err, other) {
				t.Fatalf("error = %v, want the driver's own error", err)
			}
		})
	}
}

// A query that fails on a connection is the built error through every route that
// hands a query to the driver, at every step, while the context of the request is
// alive.
func TestAConnectionFailureOfAQueryIsUnreachableOnEveryRoute(t *testing.T) {
	setPreview(t, true, "1")
	ctx := context.Background()
	failure := connectionFailure(dalgo2postgres.FailureServer, "28P01")
	const sentence = "password authentication failed (SQLSTATE 28P01)"
	for _, phase := range []string{"open", "next", "close"} {
		t.Run(phase, func(t *testing.T) {
			fake := &scriptedQueryDB{}
			switch phase {
			case "open":
				fake.openErr = failure
			case "next":
				fake.reader = &scriptedReader{nextErr: failure}
			case "close":
				fake.reader = &scriptedReader{closeErr: failure}
			}
			for route, err := range queryRoutes(t, openScripted(t, "postgres", fake)) {
				executor := strings.HasPrefix(route, "Executor") || route == "ReadTx"
				if route == "Executor, recordset" && phase != "open" {
					continue // there is no reader to read
				}
				if phase == "close" && !executor {
					continue // the core routes ignore the error of a Close that follows the last row
				}
				requireUnreachable(t, err, sentence)
			}
		})
	}
	source := dal.NewRootCollectionRef("customers", "")
	for name, run := range map[string]func() error{
		"begin": func() error {
			return openScripted(t, "postgres", &scriptedQueryDB{beginErr: failure}).ReadTx(ctx, func(dal.QueryExecutor) error { return nil })
		},
		"commit": func() error {
			return openScripted(t, "postgres", &scriptedQueryDB{commitErr: failure}).ReadTx(ctx, func(dal.QueryExecutor) error { return nil })
		},
		"field list": func() error {
			_, err := openScripted(t, "postgres", &scriptedFieldsDB{fieldsErr: failure}).Executor().(dal.JoinFieldsProvider).JoinFields(ctx, source)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { requireUnreachable(t, run(), sentence) })
	}
}

// A query whose request ended is the context's, not the database's.
func TestAConnectionFailureOfAQueryWhoseRequestEndedKeepsTheContext(t *testing.T) {
	t.Setenv(PreviewPostgresQueriesEnv, "1")
	db := openScripted(t, "postgres", &scriptedQueryDB{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := db.queryError(ctx, "failed to query", connectionFailure(dalgo2postgres.FailureNetwork, ""))
	if errors.Is(err, ErrDatabaseUnreachable) {
		t.Fatalf("error = %v: an ended request is not an unreachable database", err)
	}
	if !strings.Contains(err.Error(), "the database server could not run the query") {
		t.Fatalf("error = %v", err)
	}
}

// budgetDB is a driver whose connection attempt does not answer: a structured read waits
// until its context ends (the budget the server gave it, or the request) and then fails
// with failure, as the adapter fails an attempt that its context cut short.
type budgetDB struct {
	scriptedQueryDB
	failure error
}

func (f *budgetDB) ExecuteQueryToRecordsReader(ctx context.Context, _ dal.Query) (dal.RecordsReader, error) {
	<-ctx.Done()
	return nil, f.failure
}

// makeTheBudgetsEnd gives the time budgets of the single-collection reads no time.
func makeTheBudgetsEnd(t *testing.T) {
	t.Helper()
	dtql, snapshot := dtqlBudget, snapshotBudget
	dtqlBudget, snapshotBudget = time.Nanosecond, time.Nanosecond
	t.Cleanup(func() { dtqlBudget, snapshotBudget = dtql, snapshot })
}

// A connection that fails because the time budget of the read ended, while the request is
// alive, is the unreachable database on the routes of one collection, the same answer as a
// key read gets for the same outage; the budget is the server's, not the request's. When
// the request itself ends, or the failure is no connection failure, the read keeps the
// answer it had.
func TestAConnectionThatFailsUnderTheBudgetOfTheServerIsUnreachable(t *testing.T) {
	setPreview(t, true, "1")
	makeTheBudgetsEnd(t)
	parsed, _, err := ParseDTQL([]byte(guardDTQL))
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]func(ctx context.Context, db *Database) error{
		"ExecuteDTQLQuery": func(ctx context.Context, db *Database) error {
			_, err := db.ExecuteDTQLQuery(ctx, parsed)
			return err
		},
		"StreamDTQLSnapshot": func(ctx context.Context, db *Database) error {
			return db.StreamDTQLSnapshot(ctx, parsed, func(Record) error { return nil })
		},
	}
	for route, run := range routes {
		t.Run(route+"/the connection fails under the budget", func(t *testing.T) {
			db := openScripted(t, "postgres", &budgetDB{failure: connectionFailure(dalgo2postgres.FailureTimeout, "")})
			requireUnreachable(t, run(context.Background(), db), "the connection timed out or was canceled")
		})
		t.Run(route+"/the request ended", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			db := openScripted(t, "postgres", &budgetDB{failure: connectionFailure(dalgo2postgres.FailureTimeout, "")})
			if err := run(ctx, db); errors.Is(err, ErrDatabaseUnreachable) {
				t.Fatalf("error = %v: an ended request is not an unreachable database", err)
			}
		})
		t.Run(route+"/the failure is not a connection", func(t *testing.T) {
			db := openScripted(t, "postgres", &budgetDB{failure: context.DeadlineExceeded})
			if err := run(context.Background(), db); errors.Is(err, ErrDatabaseUnreachable) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want the deadline of the budget as it was", err)
			}
		})
	}
}
