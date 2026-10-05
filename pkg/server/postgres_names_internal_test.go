package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dal-go/dalgo/dal"
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
