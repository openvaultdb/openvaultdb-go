package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// previewPGDriver is the driver of a mount that fails every structured read with
// openErr, or, when it is nil, answers an empty result. It counts the reads that
// reach it, serves them in a read transaction too, and supplies no field list.
type previewPGDriver struct {
	dal.DB
	openErr error
	reads   atomic.Int32
	// beginErr fails a read transaction before it runs, commitErr fails it after its
	// function returned without an error, and fieldsErr fails the field list of a
	// collection (the catalog lookup of the adapter).
	beginErr, commitErr, fieldsErr error
}

// previewPGEmpty is a result with no row.
type previewPGEmpty struct{}

func (previewPGEmpty) Next() (record.Record, error) { return nil, io.EOF }
func (previewPGEmpty) Cursor() (string, error)      { return "", nil }
func (previewPGEmpty) Close() error                 { return nil }

func (f *previewPGDriver) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	f.reads.Add(1)
	if f.openErr != nil {
		return nil, f.openErr
	}
	return previewPGEmpty{}, nil
}

func (f *previewPGDriver) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	f.reads.Add(1)
	return nil, f.openErr
}

func (f *previewPGDriver) RunReadonlyTransaction(ctx context.Context, worker dal.ROTxWorker, _ ...dal.TransactionOption) error {
	if f.beginErr != nil {
		return f.beginErr
	}
	if err := worker(ctx, &previewPGTx{driver: f}); err != nil {
		return err
	}
	return f.commitErr
}

// previewPGTx is the read transaction of previewPGDriver.
type previewPGTx struct {
	dal.ReadTransaction
	driver *previewPGDriver
}

func (t *previewPGTx) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	return t.driver.ExecuteQueryToRecordsReader(ctx, query)
}

func (t *previewPGTx) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, options ...recordset.Option) (dal.RecordsetReader, error) {
	return t.driver.ExecuteQueryToRecordsetReader(ctx, query, options...)
}

func (f *previewPGDriver) JoinFields(context.Context, dal.RecordsetSource) ([]string, error) {
	return nil, f.fieldsErr
}

const previewPGMarker = "MARKER-text-of-the-database-server-7c2a"

// previewPGCollections are the collections both mounts declare. A strict mount holds
// the fields it declares and its key, and a query that names another is refused before
// the driver is reached.
func previewPGCollections() map[string]schema.Collection {
	return map[string]schema.Collection{
		"orders":    {Fields: map[string]schema.Field{"customer_id": {Type: schema.TypeString}}},
		"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
	}
}

// previewPGServer serves a PostgreSQL mount (the preview switch on, so it is
// queried) whose driver fails every read with openErr, beside a SQLite mount, and
// returns the host and what the server logs.
func previewPGServer(t *testing.T, openErr error) (*httptest.Server, *bytes.Buffer, *previewPGDriver) {
	t.Helper()
	return previewPGServerOf(t, &previewPGDriver{openErr: openErr})
}

// previewPGServerOf is previewPGServer for a driver the test scripted.
func previewPGServerOf(t *testing.T, driver *previewPGDriver) (*httptest.Server, *bytes.Buffer, *previewPGDriver) {
	t.Helper()
	host, logs := previewPGServerWith(t, driver)
	return host, logs, driver
}

// previewPGServerWith is previewPGServerOf for any handle of the PostgreSQL mount: one that
// went through dal.NewDB, as an adapter's does, plans a join before its adapter sees it.
func previewPGServerWith(t *testing.T, driver dal.DB) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	previewPGSwitch(t, "1", true)
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "pg", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres"},
		Schemas:  &schema.Schemas{Collections: previewPGCollections()},
	}
	pg, err := core.Open(m, driver, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	am := &manifest.Manifest{
		Database: manifest.Database{ID: "alpha", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: previewPGCollections()},
	}
	alpha, err := core.Open(am, &previewPGDriver{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	service := New("test", map[string]*core.Database{"pg": pg, "alpha": alpha},
		WithLogger(slog.New(slog.NewJSONHandler(logs, nil))),
		WithQueryLimits(QueryLimits{JoinEngines: []string{"sqlite", "postgres"}}))
	t.Cleanup(service.CloseSnapshots)
	host := httptest.NewServer(service.Handler())
	t.Cleanup(host.Close)
	return host, logs
}

// previewPGRoutes are the routes a structured query takes to the PostgreSQL mount
// of previewPGServer: the wire query, the DTQL of the database, and a relational
// document of the cross-database endpoint, alone and joined to another mount.
var previewPGRoutes = []struct{ name, path, body string }{
	{"wire query", "/v1/databases/pg/query", `{"collection":"customers"}`},
	{"DTQL of the database", "/v1/databases/pg/dtql", "from: {name: customers}\n"},
	{"relational document, alone", "/v1/dtql", "from: {database: pg, name: customers}\n"},
	{"relational document, joined", "/v1/dtql", `from:
  database: pg
  name: orders
  alias: o
  joins:
    - type: inner
      from: {database: alpha, name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
`},
}

// TestAQueryThePostgresAdapterCannotRunIsA422WithAFixedMessage: dal.ErrNotSupported
// and the adapter's refusal of a dialect it does not know are answered 422
// query_unsupported with one fixed message, on every route, and nothing of the
// adapter's text is in the answer or in the log.
func TestAQueryThePostgresAdapterCannotRunIsA422WithAFixedMessage(t *testing.T) {
	for name, cause := range map[string]error{
		"not supported":            fmt.Errorf("%w: "+previewPGMarker, dal.ErrNotSupported),
		"not supported, wrapped":   fmt.Errorf("failed to get SQL reader: %w", fmt.Errorf("%w: "+previewPGMarker, dal.ErrNotSupported)),
		"unknown dialect":          errors.New(`unsupported structured query dialect "` + previewPGMarker + `"`),
		"unknown dialect, wrapped": fmt.Errorf("failed to get SQL reader: %w", errors.New(`unsupported structured query dialect "`+previewPGMarker+`"`)),
	} {
		for _, route := range previewPGRoutes {
			t.Run(name+"/"+route.name, func(t *testing.T) {
				host, logs, driver := previewPGServer(t, cause)
				resp := relFakeDo(t, host, http.MethodPost, route.path, "", route.body, nil)
				if resp.status != http.StatusUnprocessableEntity || resp.code() != "query_unsupported" {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
				if message, _ := resp.errorDetail()["message"].(string); message != "the storage engine cannot run this query" {
					t.Errorf("message = %q, want the fixed message", message)
				}
				if strings.Contains(resp.raw, previewPGMarker) || strings.Contains(logs.String(), previewPGMarker) {
					t.Errorf("text of the adapter is in the answer %s or in the log %s", resp.raw, logs)
				}
				if logs.Len() != 0 {
					t.Errorf("a refusal the caller can act on was logged: %s", logs)
				}
				if driver.reads.Load() == 0 {
					t.Errorf("the driver was not reached: the route refused before it ran")
				}
			})
		}
	}
}

// TestAFailureOfAPostgresServerRepeatsNoDriverTextInTheAnswerOrTheLog: any other
// failure of the mount's driver is a 500 internal that says nothing of it, and the
// line the server logs is built: it names the step and the collection and holds no
// text of the driver's error, which can carry a value of the request.
func TestAFailureOfAPostgresServerRepeatsNoDriverTextInTheAnswerOrTheLog(t *testing.T) {
	cause := errors.New(`ERROR: invalid input syntax for type bigint: "` + previewPGMarker + `" (SQLSTATE 22P02)`)
	for _, route := range previewPGRoutes {
		t.Run(route.name, func(t *testing.T) {
			host, logs, _ := previewPGServer(t, cause)
			resp := relFakeDo(t, host, http.MethodPost, route.path, "", route.body, nil)
			if resp.status != http.StatusInternalServerError || resp.code() != "internal" {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
			if strings.Contains(resp.raw, previewPGMarker) || strings.Contains(logs.String(), previewPGMarker) {
				t.Errorf("text of the driver is in the answer %s or in the log %s", resp.raw, logs)
			}
			if !strings.Contains(logs.String(), "the database server could not run the query") {
				t.Errorf("the log does not say what failed: %s", logs)
			}
		})
	}
}

// TestADeadlineOfAPostgresQueryIsStillATimeout: the built error of a server engine
// keeps the identity of a deadline, so the relational route answers 504 as it does
// for every engine, and the driver's text is not in the answer or in the log.
func TestADeadlineOfAPostgresQueryIsStillATimeout(t *testing.T) {
	cause := fmt.Errorf("driver: %w: "+previewPGMarker, context.DeadlineExceeded)
	for _, route := range previewPGRoutes[2:] {
		t.Run(route.name, func(t *testing.T) {
			host, logs, _ := previewPGServer(t, cause)
			resp := relFakeDo(t, host, http.MethodPost, route.path, "", route.body, nil)
			if resp.status != http.StatusGatewayTimeout || resp.code() != "query_timeout" {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
			if strings.Contains(resp.raw, previewPGMarker) || strings.Contains(logs.String(), previewPGMarker) {
				t.Errorf("text of the driver is in the answer %s or in the log %s", resp.raw, logs)
			}
		})
	}
}

// previewPGSameDatabaseJoin is a join of two collections of the PostgreSQL mount: the
// document runs in one read transaction of the mount (the route "database").
const previewPGSameDatabaseJoin = `from:
  database: pg
  name: orders
  alias: o
  joins:
    - type: inner
      from: {database: pg, name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
`

// TestAFailedTransactionOrFieldListOfAPostgresMountRepeatsNoDriverTextInTheAnswerOrTheLog:
// a read transaction that cannot begin or cannot commit (the text of the driver names
// the user, the database and the host of the connection) and a field list that cannot
// be loaded (it names the nearest table the database has) are answered 500 internal
// with a line that is built, as a failed query is, and a deadline in any of them is
// still 504. None of the driver's text is in the answer or in the log.
func TestAFailedTransactionOrFieldListOfAPostgresMountRepeatsNoDriverTextInTheAnswerOrTheLog(t *testing.T) {
	driverFailure := errors.New(`failed to begin transaction: failed to connect to user=ovdb database=` + previewPGMarker + ` host=db.internal`)
	database := []struct{ name, body string }{
		{"relational document, alone", "from: {database: pg, name: customers}\n"},
		{"relational document, joined in the database", previewPGSameDatabaseJoin},
	}
	for _, c := range []struct {
		name   string
		fail   func(*previewPGDriver, error)
		routes []struct{ name, body string }
	}{
		{"begin", func(d *previewPGDriver, err error) { d.beginErr = err }, database},
		{"commit", func(d *previewPGDriver, err error) { d.commitErr = err }, database},
		{"field list", func(d *previewPGDriver, err error) { d.fieldsErr = err }, []struct{ name, body string }{
			{"relational document, joined to another database", previewPGRoutes[3].body},
		}},
	} {
		for _, route := range c.routes {
			t.Run(c.name+"/the driver fails/"+route.name, func(t *testing.T) {
				driver := &previewPGDriver{}
				c.fail(driver, driverFailure)
				host, logs, _ := previewPGServerOf(t, driver)
				resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", route.body, nil)
				if resp.status != http.StatusInternalServerError || resp.code() != "internal" {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
				if strings.Contains(resp.raw, previewPGMarker) || strings.Contains(logs.String(), previewPGMarker) {
					t.Errorf("text of the driver is in the answer %s or in the log %s", resp.raw, logs)
				}
				if !strings.Contains(logs.String(), "the database server could not run the query") {
					t.Errorf("the log does not say what failed: %s", logs)
				}
			})
			t.Run(c.name+"/a deadline/"+route.name, func(t *testing.T) {
				driver := &previewPGDriver{}
				c.fail(driver, fmt.Errorf("driver: %w: "+previewPGMarker, context.DeadlineExceeded))
				host, logs, _ := previewPGServerOf(t, driver)
				resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", route.body, nil)
				if resp.status != http.StatusGatewayTimeout || resp.code() != "query_timeout" {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
				if strings.Contains(resp.raw, previewPGMarker) || strings.Contains(logs.String(), previewPGMarker) {
					t.Errorf("text of the driver is in the answer %s or in the log %s", resp.raw, logs)
				}
			})
		}
	}
}

// previewPGStateError is a refusal of the database server with its SQLSTATE, as the
// driver of PostgreSQL gives it.
type previewPGStateError struct{ code string }

func (e *previewPGStateError) Error() string {
	return `ERROR: invalid input syntax for type bigint: "` + previewPGMarker + `" (SQLSTATE ` + e.code + `)`
}
func (e *previewPGStateError) SQLState() string { return e.code }

// TestAValueOrANameThePostgresServerRefusesIsA400WithAFixedMessageAndNoLog: a word
// compared with an integer field (22P02), a number compared with a text field (42883)
// and a name the table does not have (42703) are mistakes of the caller. On every route
// they are answered 400 invalid_dtql with one fixed message that repeats nothing of the
// request or of the server, and they are not logged: a caller who may read cannot
// fill the log of the server with them.
func TestAValueOrANameThePostgresServerRefusesIsA400WithAFixedMessageAndNoLog(t *testing.T) {
	for _, code := range []string{"22P02", "42883", "42703"} {
		for _, route := range previewPGRoutes {
			t.Run(code+"/"+route.name, func(t *testing.T) {
				host, logs, driver := previewPGServer(t, fmt.Errorf("failed to get SQL reader: %w", &previewPGStateError{code: code}))
				resp := relFakeDo(t, host, http.MethodPost, route.path, "", route.body, nil)
				if resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
				if message, _ := resp.errorDetail()["message"].(string); message != core.ErrQueryDoesNotFit.Error() {
					t.Errorf("message = %q, want the fixed message", message)
				}
				if strings.Contains(resp.raw, previewPGMarker) || strings.Contains(logs.String(), previewPGMarker) {
					t.Errorf("text of the server is in the answer %s or in the log %s", resp.raw, logs)
				}
				if logs.Len() != 0 {
					t.Errorf("a mistake of the caller was logged: %s", logs)
				}
				if driver.reads.Load() == 0 {
					t.Errorf("the driver was not reached: the route refused before it ran")
				}
			})
		}
	}
}

// TestAFieldOrQualifierThePostgresTablesDoNotHaveIsARefusalBeforeTheDriver: a field no
// column of a declared collection has and a qualifier no source has are the caller's
// mistakes, and on a strict PostgreSQL mount every route refuses them before the driver
// is reached: 400 invalid_dtql with a sentence that repeats a bounded name, and no log
// record. The adapter would only fail them as a fault of the server.
func TestAFieldOrQualifierThePostgresTablesDoNotHaveIsARefusalBeforeTheDriver(t *testing.T) {
	for _, c := range []struct{ name, path, body, want string }{
		{"wire query, where", "/v1/databases/pg/query", `{"collection":"customers","where":[{"field":"nosuch","op":"==","value":"x"}]}`, `has no field "nosuch"`},
		{"wire query, order by", "/v1/databases/pg/query", `{"collection":"customers","orderBy":[{"field":"nosuch"}]}`, `has no field "nosuch"`},
		{"wire query, a dotted name", "/v1/databases/pg/query", `{"collection":"customers","where":[{"field":"name.first","op":"==","value":"x"}]}`, `has no field "name.first"`},
		{"DTQL of the database, where", "/v1/databases/pg/dtql", "from: {name: customers}\nwhere: {op: '==', left: {field: nosuch}, right: {value: x}}\n", `has no field "nosuch"`},
		{"DTQL of the database, a column", "/v1/databases/pg/dtql", "from: {name: customers}\ncolumns: [{field: nosuch}]\n", `has no field "nosuch"`},
		{"relational document, a field", "/v1/dtql", "from: {database: pg, name: customers, alias: c}\nwhere: {op: '==', left: {field: nosuch, source: c}, right: {value: x}}\n", `has no field "nosuch"`},
		{"relational document, a qualifier", "/v1/dtql", "from: {database: pg, name: customers, alias: c}\nwhere: {op: '==', left: {field: name, source: x}, right: {value: x}}\n", `no source of the query is named "x"`},
		{"relational document, the collection of an aliased source", "/v1/dtql", "from: {database: pg, name: customers, alias: c}\nwhere: {op: '==', left: {field: name, source: customers}, right: {value: x}}\n", `no source of the query is named "customers"`},
		{"relational document, a join on", "/v1/dtql", strings.Replace(previewPGSameDatabaseJoin, "{field: customer_id, source: o}", "{field: nosuch, source: o}", 1), `collection "orders" has no field "nosuch"`},
		{"relational document, a join order", "/v1/dtql", previewPGSameDatabaseJoin + "orderBy: [{field: nosuch, source: c}]\n", `collection "customers" has no field "nosuch"`},
		// A name that a column carries as its alias is read as the alias only in HAVING and ORDER BY.
		{"relational document, a name that is only an alias, in where", "/v1/dtql", "from: {database: pg, name: customers}\ncolumns: [{field: name, as: x}]\nwhere: {op: '==', left: {field: x}, right: {value: x}}\n", `has no field "x"`},
		{"relational document, a name that is only an alias, in group by", "/v1/dtql", "from: {database: pg, name: customers}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\ngroupBy: [{field: n}]\n", `has no field "n"`},
		{"relational document, a name that is only an alias, in an aggregate", "/v1/dtql", "from: {database: pg, name: customers}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\nhaving: {op: '>', left: {aggregate: {function: sum, args: [{field: n}]}}, right: {value: 1}}\n", `has no field "n"`},
		{"DTQL of the database, a name that is only an alias, in where", "/v1/databases/pg/dtql", "from: {name: customers}\ncolumns: [{field: name, as: x}]\nwhere: {op: '==', left: {field: x}, right: {value: x}}\n", `has no field "x"`},
		// An alias over the 63 bytes of a name in PostgreSQL: the sentence gives the limit and not the alias.
		{"relational document, a source alias over the length of a name", "/v1/dtql", "from: {database: pg, name: customers, alias: " + strings.Repeat("a", 64) + "}\ncolumns: [{field: id, source: " + strings.Repeat("a", 64) + "}]\n", `a source alias is longer than 63 bytes`},
	} {
		t.Run(c.name, func(t *testing.T) {
			host, logs, driver := previewPGServer(t, nil)
			resp := relFakeDo(t, host, http.MethodPost, c.path, "", c.body, nil)
			if resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
			if message, _ := resp.errorDetail()["message"].(string); !strings.Contains(message, c.want) {
				t.Errorf("message = %q, want it to say %q", message, c.want)
			}
			if driver.reads.Load() != 0 {
				t.Errorf("the driver was reached %d times by a refused query", driver.reads.Load())
			}
			if logs.Len() != 0 {
				t.Errorf("a mistake of the caller was logged: %s", logs)
			}
		})
	}
	t.Run("a field the table has reaches the driver", func(t *testing.T) {
		host, _, driver := previewPGServer(t, nil)
		resp := relFakeDo(t, host, http.MethodPost, "/v1/databases/pg/query", "", `{"collection":"customers","where":[{"field":"Name","op":"==","value":"x"}],"orderBy":[{"field":"id"}]}`, nil)
		if resp.status != http.StatusOK || driver.reads.Load() != 1 {
			t.Fatalf("status %d with %d driver reads: %s", resp.status, driver.reads.Load(), resp.raw)
		}
	})
}
