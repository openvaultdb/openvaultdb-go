package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
)

// PostgreSQL keeps 63 bytes of a name and silently cuts the rest, so a name of 64 bytes
// or more would address the table or the column named by its first 63. A PostgreSQL
// mount refuses such a collection, field or key path on every route with the 400 of its
// kind (invalid_key for a collection, bad_request for a field of a write, invalid_dtql
// for a field of a query) before any statement is sent. The driver here counts the calls
// that reach it, so no server is involved.

// namesDriver is a PostgreSQL driver that counts the key reads and writes that reach it
// (the structured reads and the read transactions are counted by previewPGDriver) and fails
// each with errNamesReached.
type namesDriver struct {
	*previewPGDriver
	keyCalls atomic.Int32
}

var errNamesReached = errors.New("the driver was reached")

func (d *namesDriver) Get(context.Context, record.Record) error {
	d.keyCalls.Add(1)
	return errNamesReached
}

func (d *namesDriver) Exists(context.Context, *record.Key) (bool, error) {
	d.keyCalls.Add(1)
	return false, errNamesReached
}

func (d *namesDriver) RunReadwriteTransaction(context.Context, dal.RWTxWorker, ...dal.TransactionOption) error {
	d.keyCalls.Add(1)
	return errNamesReached
}

func (d *namesDriver) statements() int32 { return d.keyCalls.Load() + d.reads.Load() + d.begins.Load() }

func TestAPostgresNameOver63BytesIsA400BeforeAnyStatementOnEveryRoute(t *testing.T) {
	long := strings.Repeat("n", 64)
	exact := strings.Repeat("n", 63)
	for _, c := range []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"key read, a collection", http.MethodGet, "/v1/databases/pg/records/" + long + "/c1", "", http.StatusBadRequest, "invalid_key"},
		{"key read, URL query form", http.MethodGet, "/v1/databases/pg/read?key=" + long + "/c1", "", http.StatusBadRequest, "invalid_key"},
		{"key head, a collection", http.MethodHead, "/v1/databases/pg/records/" + long + "/c1", "", http.StatusBadRequest, ""},
		{"key write, a collection", http.MethodPut, "/v1/databases/pg/records/" + long + "/c1", `{"data":{"name":"Ada"}}`, http.StatusBadRequest, "invalid_key"},
		{"key write, a field", http.MethodPut, "/v1/databases/pg/records/customers/c1", `{"data":{"` + long + `":"Ada"}}`, http.StatusBadRequest, "bad_request"},
		{"key insert, a field", http.MethodPost, "/v1/databases/pg/records/customers/c1", `{"data":{"` + long + `":"Ada"}}`, http.StatusBadRequest, "bad_request"},
		{"key update, a field", http.MethodPatch, "/v1/databases/pg/records/customers/c1", `{"updates":[{"fieldName":"` + long + `","value":"x"}]}`, http.StatusBadRequest, "bad_request"},
		{"key update, a nested path", http.MethodPatch, "/v1/databases/pg/records/customers/c1", `{"updates":[{"fieldPath":["name","` + long + `"],"value":"x"}]}`, http.StatusBadRequest, "bad_request"},
		{"key delete, a collection", http.MethodDelete, "/v1/databases/pg/records/" + long + "/c1", "", http.StatusBadRequest, "invalid_key"},
		{"batch, a collection", http.MethodPost, "/v1/databases/pg/batch", `{"ops":[{"op":"delete","key":"` + long + `/c1"}]}`, http.StatusBadRequest, "invalid_key"},
		{"batch, a field", http.MethodPost, "/v1/databases/pg/batch", `{"ops":[{"op":"set","key":"customers/c1","data":{"` + long + `":"x"}}]}`, http.StatusBadRequest, "bad_request"},
		{"wire query, a collection", http.MethodPost, "/v1/databases/pg/query", `{"collection":"` + long + `"}`, http.StatusBadRequest, "invalid_key"},
		{"wire query, a field", http.MethodPost, "/v1/databases/pg/query", `{"collection":"customers","where":[{"field":"` + long + `","op":"==","value":"x"}]}`, http.StatusBadRequest, "invalid_dtql"},
		{"wire query, an order", http.MethodPost, "/v1/databases/pg/query", `{"collection":"customers","orderBy":[{"field":"` + long + `"}]}`, http.StatusBadRequest, "invalid_dtql"},
		{"DTQL of the database, a collection", http.MethodPost, "/v1/databases/pg/dtql", "from: {name: " + long + "}\n", http.StatusBadRequest, "invalid_key"},
		{"DTQL of the database, a field", http.MethodPost, "/v1/databases/pg/dtql", "from: {name: customers}\nwhere: {op: '==', left: {field: " + long + "}, right: {value: x}}\n", http.StatusBadRequest, "invalid_dtql"},
		{"relational document, a collection", http.MethodPost, "/v1/dtql", "from: {database: pg, name: " + long + "}\n", http.StatusBadRequest, "invalid_key"},
		{"relational document, a field", http.MethodPost, "/v1/dtql", "from: {database: pg, name: customers, alias: c}\nwhere: {op: '==', left: {field: " + long + ", source: c}, right: {value: x}}\n", http.StatusBadRequest, "invalid_dtql"},
		{"relational document, joined", http.MethodPost, "/v1/dtql", previewPGSameDatabaseJoin + "orderBy: [{field: " + long + ", source: c}]\n", http.StatusBadRequest, "invalid_dtql"},
	} {
		t.Run(c.name, func(t *testing.T) {
			driver := &namesDriver{previewPGDriver: &previewPGDriver{}}
			host, logs := previewPGServerWith(t, driver)
			resp := relFakeDo(t, host, c.method, c.path, "", c.body, nil)
			if resp.status != c.status || (c.code != "" && resp.code() != c.code) {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
			if driver.statements() != 0 {
				t.Errorf("%d statements reached the driver", driver.statements())
			}
			if logs.Len() != 0 {
				t.Errorf("a mistake of the caller was logged: %s", logs)
			}
			if message, _ := resp.errorDetail()["message"].(string); c.code != "" && !strings.Contains(message, "63 bytes") {
				t.Errorf("message = %q, want it to give the limit", message)
			}
			if strings.Contains(resp.raw, long) {
				t.Errorf("the answer repeats the name: %s", resp.raw)
			}
		})
	}
	t.Run("a name of 63 bytes reaches the driver", func(t *testing.T) {
		driver := &namesDriver{previewPGDriver: &previewPGDriver{}}
		host, _ := previewPGServerWith(t, driver)
		resp := relFakeDo(t, host, http.MethodGet, "/v1/databases/pg/records/customers/"+exact, "", "", nil)
		if driver.statements() == 0 {
			t.Fatalf("status %d: the driver was not reached by a key under a collection that is declared", resp.status)
		}
	})
}

// listedDriver is a PostgreSQL driver that supplies the field list of each collection the
// way the adapter reads it from the catalog (one statement each time, which fieldLists
// counts), and records the text of every query handed to it.
type listedDriver struct {
	*previewPGDriver
	fieldLists atomic.Int32
	mu         sync.Mutex
	queries    []string
}

func (d *listedDriver) JoinFields(_ context.Context, source dal.RecordsetSource) ([]string, error) {
	d.fieldLists.Add(1)
	if ref, ok := source.(dal.CollectionRef); ok {
		switch ref.Name() {
		case "orders":
			return []string{"id", "customer_id"}, nil
		case "customers":
			return []string{"id", "name"}, nil
		}
	}
	return nil, nil
}

func (d *listedDriver) handed(query dal.Query) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, fmt.Sprint(query))
}

func (d *listedDriver) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	d.handed(query)
	return d.previewPGDriver.ExecuteQueryToRecordsReader(ctx, query)
}

func (d *listedDriver) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, options ...recordset.Option) (dal.RecordsetReader, error) {
	d.handed(query)
	return d.previewPGDriver.ExecuteQueryToRecordsetReader(ctx, query, options...)
}

// The length of the name of a field of a query is checked on the query a mount is handed, so
// that no statement is sent for it. A document the server evaluates itself (a join across
// databases, a document with a subquery) hands each mount a plain scan of its collection, so
// the length is not looked at there: the document reads the field list of each source from
// the catalog, and the name, which no list holds, is an unknown field (400 invalid_dtql) or,
// inside a scalar subquery of one source, which DALgo checks no field of, a null. In no case
// is the name written into a query handed to the driver. The documentation says this and
// no more.
func TestAPostgresFieldNameOver63BytesInADocumentTheServerEvaluatesItselfIsNeverInAStatement(t *testing.T) {
	long := strings.Repeat("n", 64)
	for _, c := range []struct {
		name, body string
		status     int
	}{
		{"a join across databases, in WHERE", previewPGRoutes[3].body + "where: {op: '==', left: {field: " + long + ", source: o}, right: {value: x}}\n", http.StatusBadRequest},
		{"a join across databases, in ORDER BY", previewPGRoutes[3].body + "orderBy: [{field: " + long + ", source: o}]\n", http.StatusBadRequest},
		{"a document with a subquery, an EXISTS test", "from: {database: pg, name: customers, alias: c}\n" +
			"where: {exists: {query: {from: {database: pg, name: orders, alias: o}, where: {op: '==', left: {field: " + long + ", source: o}, right: {value: x}}}}}\n", http.StatusBadRequest},
		{"a document with a subquery, a scalar subquery of one source", "from: {database: pg, name: customers, alias: c}\n" +
			"columns:\n  - {field: id, source: c}\n  - query:\n      as: s\n      from: {database: pg, name: orders, alias: o}\n" +
			"      where: {op: '==', left: {field: " + long + ", source: o}, right: {value: x}}\n      columns: [{field: id, source: o}]\n", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			driver := &listedDriver{previewPGDriver: &previewPGDriver{}}
			host, logs := previewPGServerWith(t, driver)
			resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", c.body, nil)
			if resp.status != c.status || (c.status == http.StatusBadRequest && resp.code() != "invalid_dtql") {
				t.Fatalf("status %d: %s", resp.status, resp.raw)
			}
			if driver.fieldLists.Load() == 0 {
				t.Error("the field list was not read from the catalog before the answer")
			}
			driver.mu.Lock()
			defer driver.mu.Unlock()
			for _, query := range driver.queries {
				if strings.Contains(query, long) {
					t.Errorf("the name is in a query handed to the driver: %s", query)
				}
			}
			if logs.Len() != 0 {
				t.Errorf("a mistake of the caller was logged: %s", logs)
			}
		})
	}
	// The control: a name of a field the list holds is answered, and the observation sees the
	// queries handed to the driver, so that "never in a statement" says something.
	t.Run("the control, a field the list holds", func(t *testing.T) {
		driver := &listedDriver{previewPGDriver: &previewPGDriver{}}
		host, _ := previewPGServerWith(t, driver)
		resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", previewPGRoutes[3].body+"where: {op: '==', left: {field: customer_id, source: o}, right: {value: x}}\n", nil)
		driver.mu.Lock()
		defer driver.mu.Unlock()
		if resp.status != http.StatusOK || len(driver.queries) == 0 || !strings.Contains(driver.queries[0], "orders") {
			t.Fatalf("status %d: %s, queries %v", resp.status, resp.raw, driver.queries)
		}
	})
}
