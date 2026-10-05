package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// A read of one collection whose result is larger than the buffer of the server is a
// refusal the client can act on: 422 query_budget_exceeded, the answer a relational
// result over its bound gets, with a hint that says to narrow or to page the read, and
// no ERROR line in the log. It was a 500 internal before. A result of exactly the
// buffer is answered.

// bufferDriver is a PostgreSQL driver whose structured read answers one row whose name
// is pad bytes long.
type bufferDriver struct {
	*previewPGDriver
	pad int
}

type bufferReader struct {
	pad  int
	done bool
}

func (r *bufferReader) Next() (record.Record, error) {
	if r.done {
		return nil, dal.ErrNoMoreRecords
	}
	r.done = true
	return record.NewRecordWithData(record.NewKeyWithID("customers", "c1"), map[string]any{"name": strings.Repeat("x", r.pad)}), nil
}
func (r *bufferReader) Cursor() (string, error) { return "", nil }
func (r *bufferReader) Close() error            { return nil }

func (d *bufferDriver) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	d.reads.Add(1)
	return &bufferReader{pad: d.pad}, nil
}

// bufferRowOverhead is what a row counts in the buffer beside the pad of its name: the
// JSON of its data and its key.
var bufferRowOverhead = len(`{"name":""}`) + len("customers/c1")

func TestTheBufferOfTheServerHoldsTheBoundOfARelationalAnswer(t *testing.T) {
	if core.ResultBufferBytes != joinexec.MaxResultBytes {
		t.Fatalf("the buffer of a read of one collection is %d bytes and the bound of a relational answer %d", core.ResultBufferBytes, joinexec.MaxResultBytes)
	}
}

func TestAReadWhoseResultIsLargerThanTheBufferIsARefusalAndNotAnError(t *testing.T) {
	for _, route := range []struct{ name, method, path, body string }{
		{"wire query", http.MethodPost, "/v1/databases/pg/query", `{"collection":"customers"}`},
		{"wire query, URL form", http.MethodGet, `/v1/databases/pg/query?q=%7B%22collection%22%3A%22customers%22%7D`, ""},
		{"DTQL of the database", http.MethodPost, "/v1/databases/pg/dtql", "from: {name: customers}\n"},
	} {
		t.Run(route.name+"/one byte over", func(t *testing.T) {
			driver := &bufferDriver{previewPGDriver: &previewPGDriver{}, pad: core.ResultBufferBytes - bufferRowOverhead + 1}
			host, logs := previewPGServerWith(t, driver)
			resp := relFakeDo(t, host, route.method, route.path, "", route.body, nil)
			if resp.status != http.StatusUnprocessableEntity || resp.code() != "query_budget_exceeded" {
				t.Fatalf("status %d: %.300s", resp.status, resp.raw)
			}
			budget, _ := resp.errorDetail()["budget"].(map[string]any)
			if budget["name"] != joinexec.BudgetResponseBytes || budget["limit"] != float64(core.ResultBufferBytes) || budget["route"] != joinexec.RouteDatabase {
				t.Errorf("budget = %v, want the response bytes bound of the database route", budget)
			}
			hint, _ := resp.errorDetail()["hint"].(string)
			if !strings.Contains(hint, "Narrow the read") || !strings.Contains(hint, "in pages") {
				t.Errorf("hint = %q, want it to say to narrow or to page the read", hint)
			}
			if logs.Len() != 0 {
				t.Errorf("a request of the client was logged: %s", logs)
			}
		})
		t.Run(route.name+"/at the bound", func(t *testing.T) {
			driver := &bufferDriver{previewPGDriver: &previewPGDriver{}, pad: core.ResultBufferBytes - bufferRowOverhead}
			host, logs := previewPGServerWith(t, driver)
			resp := relFakeDo(t, host, route.method, route.path, "", route.body, nil)
			if resp.status != http.StatusOK {
				t.Fatalf("a result of exactly the buffer: status %d: %.300s", resp.status, resp.raw)
			}
			if logs.Len() != 0 {
				t.Errorf("an answered request was logged: %s", logs)
			}
		})
	}
}
