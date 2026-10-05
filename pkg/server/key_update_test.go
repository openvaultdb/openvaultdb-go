package server_test

import (
	"encoding/json"
	"net/http"
	"testing"

	az "github.com/dal-go/dalgo/dtql/authorization"

	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
)

// TestUpdateOfTheKeyColumnIsABadRequestOnSQLMounts: an update that names the id
// column of a record (the PATCH of /records and a batch update) is refused with
// 400 bad_request, in any spelling of its case, and the row keeps its key. An
// update of another field of the same record is served.
func TestUpdateOfTheKeyColumnIsABadRequestOnSQLMounts(t *testing.T) {
	f := startSQLNames(t)
	const record = "/v1/databases/dev/records/Orders/1"
	const batch = "/v1/databases/dev/batch"
	for name, update := range map[string]string{
		"field name":           `{"fieldName":"id","value":"2"}`,
		"field name, upper":    `{"fieldName":"ID","value":"2"}`,
		"field path":           `{"fieldPath":["id"],"value":"2"}`,
		"nested field path":    `{"fieldPath":["Id","part"],"value":"2"}`,
		"delete of the column": `{"fieldName":"id","delete":true}`,
	} {
		for route, call := range map[string]struct{ method, path, body string }{
			"PATCH": {"PATCH", record, `{"updates":[` + update + `]}`},
			"batch": {"POST", batch, `{"ops":[{"op":"update","key":"Orders/1","updates":[` + update + `]}]}`},
		} {
			t.Run(name+"/"+route, func(t *testing.T) {
				status, body := f.call(t, call.method, call.path, call.body)
				if status != http.StatusBadRequest || errorCodeOf(body) != "bad_request" {
					t.Errorf("status %d: %v", status, body)
				}
				if got, want := f.rows(t, "Orders"), "1=row of Orders"; got != want {
					t.Errorf("Orders holds %q, want %q", got, want)
				}
			})
		}
	}
	status, body := f.call(t, "PATCH", record, `{"updates":[{"fieldName":"name","value":"changed"}]}`)
	if status != http.StatusNoContent {
		t.Fatalf("an update of another field: status %d: %v", status, body)
	}
	if got, want := f.rows(t, "Orders"), "1=changed"; got != want {
		t.Errorf("Orders holds %q, want %q", got, want)
	}
}

// TestProtectedUpdateOfTheKeyColumnIsABadRequest: the protected PATCH and the
// inspection of an update refuse a change that names the id column, as the plain
// update does, before the coordinator reads or writes the table.
func TestProtectedUpdateOfTheKeyColumnIsABadRequest(t *testing.T) {
	for name, path := range map[string][]string{"id": {"id"}, "ID": {"ID"}, "nested": {"Id", "part"}} {
		t.Run(name, func(t *testing.T) {
			f := startCanonicalNames(t)
			op := writeGuardProtectedOp("update", "/customers/01", writeGuardProtectedSet(path...))
			data, _ := json.Marshal(op)
			status, body := writeGuardRequest(t, f.ts, "PATCH", "/v1/databases/crm/records/customers/01", "application/vnd.dtql.operation+json", string(data))
			if status != http.StatusBadRequest {
				t.Errorf("protected PATCH: status %d: %s", status, body)
			}
			request, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: []api.Operation{op}})
			status, body = writeGuardRequest(t, f.ts, "POST", "/v1/databases/crm/access/evaluate", "application/json", string(request))
			if status != http.StatusBadRequest {
				t.Errorf("inspection: status %d: %s", status, body)
			}
			if got := f.file.rows(t, "customers"); got != "01=Original" {
				t.Errorf("customers holds %q, want 01=Original", got)
			}
		})
	}
}

// TestProtectedInsertAndSetOfTheKeyColumnInAnotherCaseIsABadRequest: the
// inspection of an insert or a set whose data names the key column in a case
// other than "id" is a 400 for a declared table and for one the database does not
// declare, before the coordinator reads the table; "id" itself is left to the
// adapter, which skips it.
func TestProtectedInsertAndSetOfTheKeyColumnInAnotherCaseIsABadRequest(t *testing.T) {
	f := startCanonicalNames(t)
	for _, action := range []string{"insert", "set"} {
		for _, table := range []string{"customers", "ghost"} {
			t.Run(action+"/"+table, func(t *testing.T) {
				for key, want := range map[string]int{"ID": http.StatusBadRequest, "Id": http.StatusBadRequest, "id": http.StatusOK} {
					op := writeGuardProtectedOp(action, "/"+table+"/zz", &api.Mutation{Data: map[string]any{key: "x"}})
					request, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: []api.Operation{op}})
					status, body := writeGuardRequest(t, f.ts, "POST", "/v1/databases/crm/access/evaluate", "application/json", string(request))
					if status != want {
						t.Errorf("data key %s: status %d, want %d: %s", key, status, want, body)
					}
				}
				if got := f.file.rows(t, "customers"); got != "01=Original" {
					t.Errorf("customers holds %q, want 01=Original", got)
				}
			})
		}
	}
}
