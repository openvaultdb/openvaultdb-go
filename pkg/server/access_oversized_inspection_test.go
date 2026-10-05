package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	az "github.com/dal-go/dalgo/dtql/authorization"

	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
)

// oversizedRowBytes is the size of the large rows of refusedTableServer (the row
// "big" of customers and of orders). The protected session of the SQLite adapter
// accepts at most 8 MiB of evidence, and 100 operations that each hold one of
// these rows are more than that, though each of them alone is far below it.
const oversizedRowBytes = 128 << 10

// refusedInspectOps is the body of an inspection of ops, numbered op1, op2, ...
// in the order given.
func refusedInspectOps(ops []api.Operation) string {
	for i := range ops {
		ops[i].ID = fmt.Sprintf("op%d", i+1)
	}
	data, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: ops})
	return string(data)
}

// refusedRepeat is count operations built by op.
func refusedRepeat(count int, op func() api.Operation) []api.Operation {
	ops := make([]api.Operation, count)
	for i := range ops {
		ops[i] = op()
	}
	return ops
}

// refusedFacts is what an inspection says of the operation id: its entry, the
// decisions of the layers and its blockers.
func refusedFacts(t *testing.T, body, id string) []any {
	t.Helper()
	var result az.Result
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	var facts []any
	for _, operation := range result.Operations {
		if operation.ID == id {
			facts = append(facts, operation)
		}
	}
	for _, layer := range result.Layers {
		for _, decision := range layer.Decisions {
			if decision.OperationID == id {
				facts = append(facts, layer.LayerID, decision)
			}
		}
	}
	for _, blocker := range result.Blockers {
		if blocker.OperationID == id {
			facts = append(facts, blocker)
		}
	}
	return facts
}

// TestInspectionTooLargeForOneProtectedSessionIsAnsweredLikeAnyOther: an
// inspection of 100 operations on records that are large holds more evidence than
// the protected session of the SQLite adapter accepts at once, although each
// operation alone is accepted. The operations are then assessed one by one and the
// answer is the one the same inspection gets when the session accepts it: for a
// table the policy hides, the same status and the same body but for the names
// sent as for a table the database does not declare and for a record that is not
// there, for the owner and for a token granted every table, and with a record the
// caller reads first, whose facts are those of an inspection of it alone.
func TestInspectionTooLargeForOneProtectedSessionIsAnsweredLikeAnyOther(t *testing.T) {
	ts := refusedTableServer(t)
	headers := map[string]string{"Content-Type": "application/json"}
	update := func(table, row string) func() api.Operation {
		return func() api.Operation {
			return writeGuardProtectedOp("update", "/"+table+"/"+row, writeGuardProtectedSet("total"))
		}
	}
	read := func(table, row string) func() api.Operation {
		return func() api.Operation { return writeGuardProtectedOp("get", "/"+table+"/"+row, nil) }
	}
	swaps := map[string][][2]string{
		"undeclared": {{"/orders/", "/ghost/"}, {`"table":"orders"`, `"table":"ghost"`}},
		"absent":     {{"/big", "/zz"}, {`"rowId":"big"`, `"rowId":"zz"`}},
	}
	for _, caller := range refusedCallers {
		t.Run(caller.name, func(t *testing.T) {
			ask := func(ops []api.Operation) hiddenSourceAnswer {
				return hiddenSourceAsk(t, ts, caller.token, "POST", refusedEvaluate, refusedInspectOps(ops), headers)
			}
			alone := ask([]api.Operation{read("customers", "01")()})
			if alone.status != http.StatusOK || !strings.Contains(alone.body, `"result":"allow"`) {
				t.Fatalf("a record the caller reads: want allow, got %d %s", alone.status, alone.body)
			}
			for _, c := range []struct {
				name   string
				before []api.Operation
				count  int
			}{
				{"alone", nil, 100},
				{"alone, few enough for one session", nil, 40},
				{"after a record the caller reads", []api.Operation{read("customers", "01")()}, 99},
			} {
				t.Run(c.name, func(t *testing.T) {
					build := func(table, row string) []api.Operation {
						return append(append([]api.Operation(nil), c.before...), refusedRepeat(c.count, update(table, row))...)
					}
					hidden := ask(build("orders", "big"))
					if hidden.status != http.StatusOK || !strings.Contains(hidden.body, `"result":"deny"`) {
						t.Fatalf("a table the policy hides: want the redacted deny, got %d %s", hidden.status, hidden.body)
					}
					for name, other := range map[string]hiddenSourceAnswer{
						"undeclared": ask(build("ghost", "big")),
						"absent":     ask(build("orders", "zz")),
					} {
						if got := refusedSwap(hidden.body, swaps[name]); hidden.status != other.status || got != other.body {
							t.Errorf("%s: declared\n  %d %.600s\nanswer for it\n  %d %.600s", name, hidden.status, got, other.status, other.body)
						}
					}
					if len(c.before) > 0 && !reflect.DeepEqual(refusedFacts(t, hidden.body, "op1"), refusedFacts(t, alone.body, "op1")) {
						t.Errorf("the facts of the record the caller reads changed:\n  %v\nalone\n  %v", refusedFacts(t, hidden.body, "op1"), refusedFacts(t, alone.body, "op1"))
					}
				})
			}
			t.Run("records the caller reads", func(t *testing.T) {
				got := ask(refusedRepeat(100, read("customers", "big")))
				if got.status != http.StatusOK || strings.Contains(got.body, `"result":"deny"`) || !strings.Contains(got.body, `"result":"allow"`) {
					t.Errorf("want allow, got %d %.600s", got.status, got.body)
				}
			})
		})
	}
}
