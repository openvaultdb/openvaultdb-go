package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	az "github.com/dal-go/dalgo/dtql/authorization"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// TestTableThePolicyCannotDecideGetsNoLayerDetailForAPrincipalWhoMayInspectProtectedRows:
// a policy bound to a principal realm cannot decide any request of a caller whose
// principal has no subject, whatever the table and the row. An inspection of a
// declared table by a principal that the deployment binding allows to inspect
// protected rows is then answered as an inspection of a table the database does
// not declare: the same status and the same body but for the names sent, alone and
// beside another operation, for the owner and for a token granted every table.
func TestTableThePolicyCannotDecideGetsNoLayerDetailForAPrincipalWhoMayInspectProtectedRows(t *testing.T) {
	bindings := []struct {
		name   string
		allows func(capability string) bool
	}{
		{"protected inspection only", func(capability string) bool { return capability == auth.CapAccessInspectProtected }},
		{"every capability", func(string) bool { return true }},
	}
	headers := map[string]string{"Content-Type": "application/json"}
	for _, binding := range bindings {
		t.Run(binding.name, func(t *testing.T) {
			ts := refusedTableServerInRealm(t, "local", server.WithOwnerAuthorization(func(_ context.Context, _ *auth.Principal, _ az.Source, capability string, _ az.Resource) bool {
				return binding.allows(capability)
			}))
			for _, caller := range refusedCallers {
				t.Run(caller.name, func(t *testing.T) {
					for _, c := range []struct{ table, other string }{{"orders", "customers"}, {"customers", "orders"}} {
						swaps := [][2]string{{c.table, "ghost"}, {c.other, "ghost2"}}
						elsewhere := func(table string) api.Operation { return writeGuardProtectedOp("get", "/"+table+"/02", nil) }
						for _, in := range []struct{ name, declared, undeclared string }{
							{"inspection", refusedInspect(refusedGet(c.table)), refusedInspect(refusedGet("ghost"))},
							{"inspection of an update", refusedInspect(refusedUpdate(c.table)), refusedInspect(refusedUpdate("ghost"))},
							{"inspection after another operation", refusedInspect(elsewhere("ghost"), refusedGet(c.table)), refusedInspect(elsewhere("ghost"), refusedGet("ghost"))},
							{"inspection before another operation", refusedInspect(refusedGet(c.table), elsewhere("ghost")), refusedInspect(refusedGet("ghost"), elsewhere("ghost"))},
							{"inspection with another declared table", refusedInspect(refusedGet(c.table), refusedGet(c.other)), refusedInspect(refusedGet("ghost"), refusedGet("ghost2"))},
						} {
							declared := hiddenSourceAsk(t, ts, caller.token, "POST", refusedEvaluate, in.declared, headers)
							undeclared := hiddenSourceAsk(t, ts, caller.token, "POST", refusedEvaluate, in.undeclared, headers)
							if declared.status != http.StatusOK || !strings.Contains(declared.body, `"result":"deny"`) {
								t.Errorf("%s of %s: want the redacted deny, got %d %s", in.name, c.table, declared.status, declared.body)
							}
							if got := refusedSwap(declared.body, swaps); declared.status != undeclared.status || got != undeclared.body {
								t.Errorf("%s of %s: declared\n  %d %s\nundeclared\n  %d %s", in.name, c.table, declared.status, got, undeclared.status, undeclared.body)
							}
						}
					}
				})
			}
		})
	}
}
