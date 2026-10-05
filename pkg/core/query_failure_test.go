package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

const failureMarker = "MARKER-text-of-the-adapter-4f1e"

// scriptedReader is a records reader that fails the way a test says: it holds a
// record, then answers err from Next (when set), and answers closeErr from Close.
type scriptedReader struct {
	rows     int
	nextErr  error
	closeErr error
}

func (r *scriptedReader) Next() (record.Record, error) {
	if r.rows > 0 {
		r.rows--
		return record.NewRecordWithData(record.NewKeyWithID("customers", "c1"), map[string]any{"id": "c1", "name": "Ada"}), nil
	}
	if r.nextErr != nil {
		return nil, r.nextErr
	}
	return nil, io.EOF
}
func (r *scriptedReader) Cursor() (string, error) { return "", nil }
func (r *scriptedReader) Close() error            { return r.closeErr }

// scriptedQueryDB is a dal.DB whose only behaviour is to answer a structured query
// with an error or a reader the test scripted. Every other method panics.
type scriptedQueryDB struct {
	dal.DB
	openErr error
	reader  dal.RecordsReader
	// beginErr fails a read transaction before its function runs, and commitErr after
	// it ran and returned no error: the two ways a driver fails a transaction that its
	// function did not.
	beginErr  error
	commitErr error
}

func (f *scriptedQueryDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f.reader, nil
}

func (f *scriptedQueryDB) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	return nil, f.openErr
}

func (f *scriptedQueryDB) RunReadonlyTransaction(ctx context.Context, worker dal.ROTxWorker, _ ...dal.TransactionOption) error {
	if f.beginErr != nil {
		return f.beginErr
	}
	if err := worker(ctx, &scriptedTx{db: f}); err != nil {
		return err
	}
	return f.commitErr
}

// scriptedFieldsDB is scriptedQueryDB with a catalog: it supplies the field list of a
// collection, or fails to.
type scriptedFieldsDB struct {
	scriptedQueryDB
	fields    []string
	fieldsErr error
}

func (f *scriptedFieldsDB) JoinFields(context.Context, dal.RecordsetSource) ([]string, error) {
	return f.fields, f.fieldsErr
}

// scriptedTx is the read transaction of scriptedQueryDB: it answers the structured
// queries as the database does.
type scriptedTx struct {
	dal.ReadTransaction
	db *scriptedQueryDB
}

func (t *scriptedTx) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	return t.db.ExecuteQueryToRecordsReader(ctx, query)
}

func (t *scriptedTx) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, options ...recordset.Option) (dal.RecordsetReader, error) {
	return t.db.ExecuteQueryToRecordsetReader(ctx, query, options...)
}

func openScripted(t *testing.T, engine string, fake dal.DB) *Database {
	t.Helper()
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "scripted", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
		}},
	}
	db, err := Open(m, fake, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// queryRoutes runs the same query through every route that hands a structured query
// to the driver and reads what comes back, and returns what each ended with.
func queryRoutes(t *testing.T, db *Database) map[string]error {
	t.Helper()
	ctx := context.Background()
	parsed, _, err := ParseDTQL([]byte(guardDTQL))
	if err != nil {
		t.Fatal(err)
	}
	drain := func(reader dal.RecordsReader, err error) error {
		if err != nil {
			return err
		}
		for {
			if _, err := reader.Next(); err != nil {
				closeErr := reader.Close()
				if err != io.EOF {
					return err
				}
				return closeErr
			}
		}
	}
	out := map[string]error{}
	_, out["Execute"] = db.Execute(ctx, Query{Collection: "customers"})
	_, out["ExecuteDTQLQuery"] = db.ExecuteDTQLQuery(ctx, parsed)
	out["StreamDTQLSnapshot"] = db.StreamDTQLSnapshot(ctx, parsed, func(Record) error { return nil })
	out["Executor, records"] = drain(db.Executor().ExecuteQueryToRecordsReader(ctx, parsed))
	_, recordsetErr := db.Executor().ExecuteQueryToRecordsetReader(ctx, parsed)
	out["Executor, recordset"] = recordsetErr
	out["ReadTx"] = db.ReadTx(ctx, func(tx dal.QueryExecutor) error { return drain(tx.ExecuteQueryToRecordsReader(ctx, parsed)) })
	return out
}

// TestAQueryTheAdapterCannotRunIsNotRunnableAndRepeatsNoAdapterText: an adapter that
// reports dal.ErrNotSupported, or that does not know the structured-query dialect
// its mount asked for, makes the query an error that matches ErrQueryNotRunnable
// (HTTP 422 query_unsupported), on every route that reads, at the open of the read
// and at its end. The error is built here: no text of the adapter's error is in it,
// on any engine.
func TestAQueryTheAdapterCannotRunIsNotRunnableAndRepeatsNoAdapterText(t *testing.T) {
	setPreview(t, true, "1")
	causes := map[string]error{
		"not supported":                  fmt.Errorf("%w: "+failureMarker, dal.ErrNotSupported),
		"not supported, wrapped":         fmt.Errorf("failed to get SQL reader: %w", fmt.Errorf("%w: "+failureMarker, dal.ErrNotSupported)),
		"not supported by a driver type": &dalNotSupported{marker: failureMarker},
		"unknown dialect":                errors.New(`unsupported structured query dialect "` + failureMarker + `"`),
		"unknown dialect, wrapped":       fmt.Errorf("failed to read: %w", errors.New(`unsupported structured query dialect "`+failureMarker+`"`)),
	}
	for _, engine := range []string{"sqlite", "postgres"} {
		for name, cause := range causes {
			for _, phase := range []string{"open", "next", "close"} {
				t.Run(engine+"/"+name+"/"+phase, func(t *testing.T) {
					fake := &scriptedQueryDB{}
					switch phase {
					case "open":
						fake.openErr = cause
					case "next":
						fake.reader = &scriptedReader{nextErr: cause}
					case "close":
						fake.reader = &scriptedReader{closeErr: cause}
					}
					for route, err := range queryRoutes(t, openScripted(t, engine, fake)) {
						executor := strings.HasPrefix(route, "Executor") || route == "ReadTx"
						if route == "Executor, recordset" && phase != "open" {
							continue // there is no reader to read
						}
						if phase == "close" && !executor {
							continue // the core routes ignore the error of a Close that follows the last row
						}
						if phase != "open" && executor && engine != "postgres" {
							continue // the reader of an engine that is not a server is not wrapped
						}
						if !errors.Is(err, ErrQueryNotRunnable) {
							t.Errorf("%s: %v, want an error that matches ErrQueryNotRunnable", route, err)
							continue
						}
						if strings.Contains(err.Error(), failureMarker) {
							t.Errorf("%s: the error repeats the text of the adapter's: %v", route, err)
						}
						if errors.Is(err, dal.ErrNotSupported) {
							t.Errorf("%s: the error wraps the adapter's own", route)
						}
					}
				})
			}
		}
	}
}

// dalNotSupported is an error of a driver that matches dal.ErrNotSupported by its
// own Is method and holds text of its own.
type dalNotSupported struct{ marker string }

func (e *dalNotSupported) Error() string        { return "driver refuses: " + e.marker }
func (e *dalNotSupported) Is(target error) bool { return target == dal.ErrNotSupported }

// TestAFailureOfAServerEngineIsBuiltAndRepeatsNoDriverText: what a database server's
// adapter fails a query with is never wrapped. The error is a fixed sentence and the
// collection name, and no text of the driver's error is in it, so no text of the
// server (a value, a name, a hint) reaches a response or a log. A cancellation and a
// deadline keep their identity, so the timeout answer still holds.
func TestAFailureOfAServerEngineIsBuiltAndRepeatsNoDriverText(t *testing.T) {
	setPreview(t, true, "1")
	driverFailure := errors.New(`ERROR: invalid input syntax for type bigint: "` + failureMarker + `" (SQLSTATE 22P02)`)
	for _, phase := range []string{"open", "next"} {
		t.Run(phase, func(t *testing.T) {
			fake := &scriptedQueryDB{}
			if phase == "open" {
				fake.openErr = driverFailure
			} else {
				fake.reader = &scriptedReader{nextErr: driverFailure}
			}
			for route, err := range queryRoutes(t, openScripted(t, "postgres", fake)) {
				if route == "Executor, recordset" && phase != "open" {
					continue
				}
				if err == nil {
					t.Errorf("%s: no error", route)
					continue
				}
				if strings.Contains(err.Error(), failureMarker) || errors.Is(err, driverFailure) {
					t.Errorf("%s: the error repeats or wraps the driver's: %v", route, err)
				}
				if errors.Is(err, ErrQueryNotRunnable) {
					t.Errorf("%s: a failure of the server is not a query the adapter cannot run: %v", route, err)
				}
			}
		})
	}
	for name, cause := range map[string]error{"canceled": context.Canceled, "deadline": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			wrapped := fmt.Errorf("driver: %w: "+failureMarker, cause)
			for route, err := range queryRoutes(t, openScripted(t, "postgres", &scriptedQueryDB{openErr: wrapped})) {
				if !errors.Is(err, cause) || strings.Contains(err.Error(), failureMarker) {
					t.Errorf("%s: %v, want the identity of %v and none of the driver's text", route, err, cause)
				}
			}
		})
	}
}

// TestAFailureOfAnEngineThatIsNotAServerKeepsItsError: the engines that are not
// reached through a connection string keep the error of their adapter in the chain,
// as they always did.
func TestAFailureOfAnEngineThatIsNotAServerKeepsItsError(t *testing.T) {
	cause := errors.New("sqlite says " + failureMarker)
	for route, err := range queryRoutes(t, openScripted(t, "sqlite", &scriptedQueryDB{openErr: cause})) {
		if !errors.Is(err, cause) {
			t.Errorf("%s: %v, want the adapter's error in the chain", route, err)
		}
	}
	reader := &scriptedReader{nextErr: cause}
	for route, err := range queryRoutes(t, openScripted(t, "sqlite", &scriptedQueryDB{reader: reader})) {
		if route == "Executor, recordset" {
			continue
		}
		if !errors.Is(err, cause) {
			t.Errorf("%s: %v, want the adapter's error in the chain", route, err)
		}
	}
}

// TestAReadThatEndsOrFailsCleanlyIsNotChangedByTheBuiltErrors: the end of a result, a
// result of rows and a close that succeeds are carried through untouched on a
// server engine.
func TestAReadThatEndsOrFailsCleanlyIsNotChangedByTheBuiltErrors(t *testing.T) {
	setPreview(t, true, "1")
	records, err := openScripted(t, "postgres", &scriptedQueryDB{reader: &scriptedReader{rows: 2}}).Execute(context.Background(), Query{Collection: "customers"})
	if err != nil || len(records) != 2 {
		t.Fatalf("Execute: %d records, %v", len(records), err)
	}
	parsed, _, err := ParseDTQL([]byte(guardDTQL))
	if err != nil {
		t.Fatal(err)
	}
	db := openScripted(t, "postgres", &scriptedQueryDB{reader: &scriptedReader{rows: 2}})
	reader, err := db.Executor().ExecuteQueryToRecordsReader(context.Background(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := reader.Next(); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("the end of the result is %v, want io.EOF", err)
	}
	if cursor, err := reader.Cursor(); cursor != "" || err != nil {
		t.Fatalf("Cursor = %q, %v", cursor, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestAFailedTransactionOrFieldListOfAServerEngineIsBuiltAndRepeatsNoDriverText: the
// two ways a join source reaches the driver besides a query (a read transaction that
// cannot begin or cannot commit, and the field list of a collection) fail with the
// built error of a server engine too. The driver's text there can name the user, the
// database and the host of the connection, or the nearest table the database has, and
// it would reach a log. A cancellation and a deadline keep their identity, and an engine
// that is not a server keeps the adapter's error in the chain.
func TestAFailedTransactionOrFieldListOfAServerEngineIsBuiltAndRepeatsNoDriverText(t *testing.T) {
	setPreview(t, true, "1")
	ctx := context.Background()
	driverFailure := errors.New(`failed to begin transaction: dial tcp: connection to server "` + failureMarker + `" refused`)
	source := dal.NewRootCollectionRef("customers", "")
	routes := map[string]func(*testing.T, string, error) (*Database, error){
		"begin": func(t *testing.T, engine string, cause error) (*Database, error) {
			db := openScripted(t, engine, &scriptedQueryDB{beginErr: cause})
			return db, db.ReadTx(ctx, func(dal.QueryExecutor) error { return nil })
		},
		"commit": func(t *testing.T, engine string, cause error) (*Database, error) {
			db := openScripted(t, engine, &scriptedQueryDB{commitErr: cause})
			return db, db.ReadTx(ctx, func(dal.QueryExecutor) error { return nil })
		},
		"field list": func(t *testing.T, engine string, cause error) (*Database, error) {
			db := openScripted(t, engine, &scriptedFieldsDB{fieldsErr: cause})
			fields, err := db.Executor().(dal.JoinFieldsProvider).JoinFields(ctx, source)
			if fields != nil {
				t.Errorf("a failed field list supplied %v", fields)
			}
			return db, err
		},
	}
	for name, route := range routes {
		t.Run(name+"/the driver fails", func(t *testing.T) {
			_, err := route(t, "postgres", driverFailure)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), failureMarker) || errors.Is(err, driverFailure) {
				t.Errorf("the error repeats or wraps the driver's: %v", err)
			}
			if errors.Is(err, ErrQueryNotRunnable) {
				t.Errorf("a failure of the server is not a query the adapter cannot run: %v", err)
			}
		})
		t.Run(name+"/the adapter cannot run it", func(t *testing.T) {
			_, err := route(t, "postgres", fmt.Errorf("%w: "+failureMarker, dal.ErrNotSupported))
			if !errors.Is(err, ErrQueryNotRunnable) || strings.Contains(err.Error(), failureMarker) {
				t.Errorf("%v, want a built ErrQueryNotRunnable with none of the adapter's text", err)
			}
		})
		for cname, cause := range map[string]error{"canceled": context.Canceled, "deadline": context.DeadlineExceeded} {
			t.Run(name+"/"+cname, func(t *testing.T) {
				_, err := route(t, "postgres", fmt.Errorf("driver: %w: "+failureMarker, cause))
				if !errors.Is(err, cause) || strings.Contains(err.Error(), failureMarker) {
					t.Errorf("%v, want the identity of %v and none of the driver's text", err, cause)
				}
			})
		}
		t.Run(name+"/an engine that is not a server", func(t *testing.T) {
			cause := errors.New("sqlite says " + failureMarker)
			if _, err := route(t, "sqlite", cause); !errors.Is(err, cause) {
				t.Errorf("%v, want the adapter's error in the chain", err)
			}
		})
	}
}

// TestAFunctionOfAReadTransactionThatFailsIsReturnedAsItWasGiven: the error the function
// of a read transaction returns reaches the caller as the function gave it, whatever
// the driver reports, and a transaction that fails after a function that did not is
// the failure of the transaction.
func TestAFunctionOfAReadTransactionThatFailsIsReturnedAsItWasGiven(t *testing.T) {
	setPreview(t, true, "1")
	given := errors.New("the function's own error")
	for _, engine := range []string{"sqlite", "postgres"} {
		db := openScripted(t, engine, &scriptedQueryDB{commitErr: errors.New("the commit " + failureMarker)})
		if err := db.ReadTx(context.Background(), func(dal.QueryExecutor) error { return given }); err != given {
			t.Errorf("%s: %v, want the function's error as it was given", engine, err)
		}
	}
}
