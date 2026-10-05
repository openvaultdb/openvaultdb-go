package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/record"
)

// What a client and an operator see when the database server of a PostgreSQL mount
// cannot be reached: one status (503) and one code (database_unavailable) with one
// fixed message on every route that reads or writes the mount, whatever the kind of
// failure; and one log line that names the mount by its ID and holds the adapter's
// fixed sentence. Nothing of the connection (the host, the port, the database name,
// the user, the password, the driver's text) is in an answer or in the log. The
// driver here is a stand-in that fails every call with the error the adapter gives,
// so no server is reached.

const (
	unreachableHost     = "db.internal.example"
	unreachablePort     = "6543"
	unreachableDatabase = "ledger_prod"
	unreachableUser     = "ovdb_reader_7c2a"
	unreachablePassword = "s3cret-pass-4f1e"
)

// unreachableDriver is a PostgreSQL driver whose every call fails with failure.
type unreachableDriver struct {
	*previewPGDriver
	failure error
}

func (d *unreachableDriver) Get(context.Context, record.Record) error { return d.failure }
func (d *unreachableDriver) Exists(context.Context, *record.Key) (bool, error) {
	return false, d.failure
}
func (d *unreachableDriver) RunReadwriteTransaction(context.Context, dal.RWTxWorker, ...dal.TransactionOption) error {
	return d.failure
}
func (d *unreachableDriver) ListCollections(context.Context, *record.Key) ([]dal.CollectionRef, error) {
	return nil, d.failure
}
func (d *unreachableDriver) DescribeCollection(context.Context, *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	return nil, d.failure
}
func (d *unreachableDriver) ListIndexes(context.Context, *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	return nil, d.failure
}
func (d *unreachableDriver) ListConstraints(context.Context, *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	return nil, d.failure
}
func (d *unreachableDriver) ListReferrers(context.Context, *dal.CollectionRef) ([]dbschema.Referrer, error) {
	return nil, d.failure
}

// unreachableFailure is what the adapter reports for a connection that fails, at its
// most revealing: it names the host, the port and the database, and the driver's own
// text (which holds the user and the password) is wrapped around it.
func unreachableFailure(kind dalgo2postgres.FailureKind, sqlState string) error {
	return fmt.Errorf("failed to connect to `user=%s password=%s host=%s port=%s database=%s`: %w",
		unreachableUser, unreachablePassword, unreachableHost, unreachablePort, unreachableDatabase,
		&dalgo2postgres.ConnectionError{Kind: kind, SQLState: sqlState, Host: unreachableHost, Port: unreachablePort, Database: unreachableDatabase})
}

// unreachableKinds are the kinds of failure the adapter tells apart, with the
// sentence it gives each.
var unreachableKinds = []struct {
	name     string
	kind     dalgo2postgres.FailureKind
	sqlState string
	sentence string
}{
	{"network", dalgo2postgres.FailureNetwork, "", "the server could not be reached"},
	{"tls", dalgo2postgres.FailureTLS, "", "the TLS handshake with the server failed"},
	{"timeout", dalgo2postgres.FailureTimeout, "", "the connection timed out or was canceled"},
	{"rejected", dalgo2postgres.FailureServer, "28P01", "password authentication failed (SQLSTATE 28P01)"},
	{"too many connections", dalgo2postgres.FailureServer, "53300", "too many connections (SQLSTATE 53300)"},
	{"invalid string", dalgo2postgres.FailureInvalidDSN, "", "the connection string cannot be parsed"},
	{"misread string", dalgo2postgres.FailureMisread, "", "the connection string is not read as intended"},
	{"other", dalgo2postgres.FailureOther, "", "the connection failed"},
}

// unreachableRoutes are the routes that read or write the PostgreSQL mount "pg".
var unreachableRoutes = []struct{ name, method, path, body string }{
	{"key read", http.MethodGet, "/v1/databases/pg/records/customers/c1", ""},
	{"key read, URL query form", http.MethodGet, "/v1/databases/pg/read?key=customers/c1", ""},
	{"key write", http.MethodPut, "/v1/databases/pg/records/customers/c1", `{"data":{"name":"Ada"}}`},
	{"key insert", http.MethodPost, "/v1/databases/pg/records/customers/c1", `{"data":{"name":"Ada"}}`},
	{"key update", http.MethodPatch, "/v1/databases/pg/records/customers/c1", `{"updates":[{"fieldName":"name","value":"Bob"}]}`},
	{"key delete", http.MethodDelete, "/v1/databases/pg/records/customers/c1", ""},
	{"batch", http.MethodPost, "/v1/databases/pg/batch", `{"ops":[{"op":"delete","key":"customers/c1"}]}`},
	{"database metadata", http.MethodGet, "/v1/databases/pg", ""},
	{"wire query", http.MethodPost, "/v1/databases/pg/query", `{"collection":"customers"}`},
	{"wire query, URL form", http.MethodGet, `/v1/databases/pg/query?q=%7B%22collection%22%3A%22customers%22%7D`, ""},
	{"DTQL of the database", http.MethodPost, "/v1/databases/pg/dtql", "from: {name: customers}\n"},
	{"relational document, alone", http.MethodPost, "/v1/dtql", "from: {database: pg, name: customers}\n"},
	{"relational document, in the database", http.MethodPost, "/v1/dtql", previewPGSameDatabaseJoin},
	{"relational document, joined to another database", http.MethodPost, "/v1/dtql", previewPGRoutes[3].body},
}

// unreachableServer serves the PostgreSQL mount "pg" over a driver that fails every
// call with failure, with the preview of queries on, and returns what the server logs.
func unreachableServer(t *testing.T, failure error) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	return previewPGServerWith(t, &unreachableDriver{previewPGDriver: &previewPGDriver{openErr: failure, fieldsErr: failure}, failure: failure})
}

// requireNoConnection fails when text holds anything of the connection.
func requireNoConnection(t *testing.T, what, text string) {
	t.Helper()
	for _, leaked := range []string{unreachableHost, unreachablePort, unreachableDatabase, unreachableUser, unreachablePassword, "failed to connect", "driver"} {
		if strings.Contains(text, leaked) {
			t.Errorf("%q of the connection is in %s: %s", leaked, what, text)
		}
	}
}

// logLines decodes the lines the server logged.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if raw == "" {
			continue
		}
		line := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("a line of the log is not JSON: %s", raw)
		}
		lines = append(lines, line)
	}
	return lines
}

// Every route, every kind of failure: 503 database_unavailable with the one message.
func TestAMountThatCannotBeReachedIsAFixed503OnEveryRouteWhateverTheFailure(t *testing.T) {
	for _, kind := range unreachableKinds {
		for _, route := range unreachableRoutes {
			t.Run(kind.name+"/"+route.name, func(t *testing.T) {
				host, logs := unreachableServer(t, unreachableFailure(kind.kind, kind.sqlState))
				resp := relFakeDo(t, host, route.method, route.path, "", route.body, nil)
				if resp.status != http.StatusServiceUnavailable || resp.code() != "database_unavailable" {
					t.Fatalf("status %d: %s", resp.status, resp.raw)
				}
				if message, _ := resp.errorDetail()["message"].(string); message != "the database of this mount cannot be reached" {
					t.Errorf("message = %q, want the fixed message", message)
				}
				requireNoConnection(t, "the answer", resp.raw)
				requireNoConnection(t, "the log", logs.String())
				if strings.Contains(resp.raw, kind.sentence) {
					t.Errorf("the adapter's sentence is in the answer: %s", resp.raw)
				}
				lines := logLines(t, logs)
				if len(lines) != 1 {
					t.Fatalf("the server logged %d lines, want one: %s", len(lines), logs)
				}
				line := lines[0]
				reason, _ := line["reason"].(string)
				if line["level"] != "ERROR" || line["msg"] != "database unreachable" || line["database"] != "pg" ||
					line["method"] != route.method || !strings.Contains(reason, kind.sentence) {
					t.Errorf("log line = %v, want an error naming the mount pg, the route and the sentence %q", line, kind.sentence)
				}
			})
		}
	}
}

// A HEAD of a record is a 503 with no body, and logs like the others.
func TestAMountThatCannotBeReachedIsA503ForAHeadToo(t *testing.T) {
	host, logs := unreachableServer(t, unreachableFailure(dalgo2postgres.FailureNetwork, ""))
	resp := relFakeDo(t, host, http.MethodHead, "/v1/databases/pg/records/customers/c1", "", "", nil)
	if resp.status != http.StatusServiceUnavailable || resp.raw != "" {
		t.Fatalf("status %d: %q", resp.status, resp.raw)
	}
	requireNoConnection(t, "the log", logs.String())
	if lines := logLines(t, logs); len(lines) != 1 || lines[0]["database"] != "pg" {
		t.Fatalf("log = %s", logs)
	}
}

// The human page of a collection reads the foreign keys from the database: the answer
// is a 503 whose text is fixed.
func TestTheHumanPageOfAMountThatCannotBeReachedIsA503(t *testing.T) {
	host, logs := unreachableServer(t, unreachableFailure(dalgo2postgres.FailureNetwork, ""))
	resp := relFakeDo(t, host, http.MethodGet, "/ovdb/dbs/pg/collections/customers", "", "", nil)
	if resp.status != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
	requireNoConnection(t, "the page", resp.raw)
	requireNoConnection(t, "the log", logs.String())
	if lines := logLines(t, logs); len(lines) != 1 || lines[0]["database"] != "pg" {
		t.Fatalf("log = %s", logs)
	}
}

// A failure of the driver that is not a connection keeps the answer it always had.
func TestAFailureOfTheDriverThatIsNotAConnectionIsStillA500(t *testing.T) {
	host, logs := unreachableServer(t, fmt.Errorf("some other failure: %s", unreachableHost))
	resp := relFakeDo(t, host, http.MethodGet, "/v1/databases/pg/records/customers/c1", "", "", nil)
	if resp.status != http.StatusInternalServerError || resp.code() != "internal" {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
	if lines := logLines(t, logs); len(lines) != 1 || lines[0]["msg"] != "internal server error" {
		t.Fatalf("log = %s", logs)
	}
}

// The human page of a collection whose foreign keys cannot be read for another reason is
// the 500 it always was, with a text that says nothing of the failure.
func TestTheHumanPageOfAMountWhoseDriverFailsForAnotherReasonIsStillA500(t *testing.T) {
	host, _ := unreachableServer(t, fmt.Errorf("some other failure: %s", unreachableHost))
	resp := relFakeDo(t, host, http.MethodGet, "/ovdb/dbs/pg/collections/customers", "", "", nil)
	if resp.status != http.StatusInternalServerError || strings.Contains(resp.raw, unreachableHost) {
		t.Fatalf("status %d: %s", resp.status, resp.raw)
	}
}

// The paged capture of a DTQL read, which no route above covers (it takes the header of a
// page size), is the same 503 with the same log line. The decision that a connection that
// fails under the server's time budget is this answer too is made in core, where the budget
// is a seam (core.TestAConnectionThatFailsUnderTheBudgetOfTheServerIsUnreachable); this
// holds that the capture maps it.
func TestAPagedCaptureOfAMountThatCannotBeReachedIsAFixed503(t *testing.T) {
	for _, kind := range unreachableKinds {
		t.Run(kind.name, func(t *testing.T) {
			host, logs := unreachableServer(t, unreachableFailure(kind.kind, kind.sqlState))
			resp := relFakeDo(t, host, http.MethodPost, "/v1/databases/pg/dtql", "", "from: {name: customers}\n", map[string]string{"OVDB-Page-Size": "10"})
			if resp.status != http.StatusServiceUnavailable || resp.code() != "database_unavailable" {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
			requireNoConnection(t, "the answer", resp.raw)
			requireNoConnection(t, "the log", logs.String())
			if lines := logLines(t, logs); len(lines) != 1 || lines[0]["database"] != "pg" || lines[0]["msg"] != "database unreachable" {
				t.Fatalf("log = %s", logs)
			}
		})
	}
}

// A relational document that its own time budget ends while a connection is being made is
// the timeout it is documented to be (504 query_timeout), not the 503 of an unreachable
// database: the budget of the document is the server's limit on the whole document, and the
// adapter reports the attempt it cut short as a connection failure that is also the
// deadline. This pins the choice made for the documents of /v1/dtql; the routes of one
// collection decide on the request, and answer 503 (see core).
func TestARelationalDocumentWhoseBudgetEndsOnAConnectionAttemptIsStillA504(t *testing.T) {
	failure := errors.Join(unreachableFailure(dalgo2postgres.FailureTimeout, ""), context.DeadlineExceeded)
	for _, route := range []struct{ name, body string }{
		{"alone", "from: {database: pg, name: customers}\n"},
		{"in the database", previewPGSameDatabaseJoin},
	} {
		t.Run(route.name, func(t *testing.T) {
			host, logs := previewPGServerLimits(t, &unreachableDriver{previewPGDriver: &previewPGDriver{openErr: failure, fieldsErr: failure}, failure: failure}, time.Nanosecond)
			resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", route.body, nil)
			if resp.status != http.StatusGatewayTimeout || resp.code() != "query_timeout" {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
			requireNoConnection(t, "the answer", resp.raw)
			requireNoConnection(t, "the log", logs.String())
		})
	}
}
