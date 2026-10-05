package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

const hiddenTableGrantedToken = "ovdb_test_hidden_table_granted"

// hiddenTableServer serves a real SQLite mount behind an owner policy. It
// declares customers, which the policy admits for the rows of country IE, orders,
// which the policy has no rule for (so it hides it), and returns, declared by the
// quoted SQL identifier of its key and hidden by the policy as well. The file
// also holds a table named with the quote characters of that spelling, which no
// request may read. Two callers may read and write: the owner token, and a
// token granted every collection a case below names, in the spelling it names.
func hiddenTableServer(t *testing.T, extra ...server.Option) (*httptest.Server, sqlNamesFixture) {
	t.Helper()
	return hiddenTableServerInRealm(t, "", extra...)
}

// hiddenTableServerInRealm is hiddenTableServer with the owner ACL bound to a
// principal realm (none when realm is empty). A caller whose principal has no
// subject is not in a realm, so the policy cannot decide any request of it.
func hiddenTableServerInRealm(t *testing.T, realm string, extra ...server.Option) (*httptest.Server, sqlNamesFixture) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: crm, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n" +
		"    customers:\n      fields:\n        name: {type: string}\n        country: {type: string}\n" +
		"    orders:\n      fields:\n        total: {type: string}\n" +
		"    '\"returns\"':\n      fields:\n        name: {type: string}\n"
	aclWriteFile(t, path, manifest)
	file := sqlNamesFixture{path: filepath.Join(dir, "data.sqlite")}
	raw, err := sql.Open("sqlite", file.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE customers (id TEXT PRIMARY KEY, name TEXT, country TEXT)`,
		`INSERT INTO customers VALUES ('01', 'Ada', 'IE')`,
		`CREATE TABLE ` + sqlIdent(`"returns"`) + ` (id TEXT PRIMARY KEY, name TEXT)`,
		`INSERT INTO ` + sqlIdent(`"returns"`) + ` VALUES ('01', 'Quoted')`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	policy := strings.Replace(aclPolicy("upper", "country", "IE"), "operations: [query, get]", "operations: [query, get, update]", 1)
	aclWriteFile(t, filepath.Join(dir, "upper.yaml"), policy)
	acl := "acl: {enabled: true, policies: [upper.yaml]}\n"
	if realm != "" {
		acl = "acl: {enabled: true, realm: " + realm + ", policies: [upper.yaml]}\n"
	}
	aclWriteFile(t, path, manifest+acl)
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reads []auth.Capability
	for _, collection := range []string{"customers", "orders", "ghost", `"returns"`, "returns"} {
		reads = append(reads,
			auth.Capability{Action: auth.CapRecordsRead, Collection: collection},
			auth.Capability{Action: auth.CapRecordsWrite, Collection: collection})
	}
	grantToken(t, store, "crm", hiddenTableGrantedToken, reads...)
	options := append([]server.Option{
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}),
	}, extra...)
	service := server.New("test", map[string]*core.Database{"crm": db}, options...)
	t.Cleanup(service.CloseSnapshots)
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	return ts, file
}

// TestUndeclaredTableIsAnsweredAsAHiddenOne: a mount with access policies does
// not say which collections it declares. On /access/evaluate, in sampling and in
// inspection, a table the database does not declare, and the quoted spelling of a
// declared key (which is not the canonical name), get the answer a declared table
// the policy hides gets: the same status and the same body, the redacted deny,
// with no word of why and no name but the one the caller sent. Each pair puts the
// two names in the same place of the same request, for the owner and for a token
// granted both, and the sample pairs carry the same orderings.
func TestUndeclaredTableIsAnsweredAsAHiddenOne(t *testing.T) {
	ts, file := hiddenTableServer(t)
	const evaluate = "/v1/databases/crm/access/evaluate"
	asJSON := func(v any) string {
		data, _ := json.Marshal(v)
		return string(data)
	}
	sample := func(table, order string) string {
		doc := "from: {name: '" + table + "'}\n" + order
		op := writeGuardProtectedOp("update", "/"+table, writeGuardProtectedSet("name"))
		return asJSON(api.Request{APIVersion: az.APIVersion, Mode: az.ModeSample, DiagnosticLevel: "ordinary", Operations: []api.Operation{op},
			Sample: &api.Sample{Limit: 1, Query: api.Query{Format: "dtql-yaml", Text: doc}}})
	}
	inspectOps := func(ops ...api.Operation) string {
		for i := range ops {
			ops[i].ID = "op" + string(rune('1'+i))
		}
		return asJSON(api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: ops})
	}
	inspect := func(tables ...string) string {
		var ops []api.Operation
		for _, table := range tables {
			ops = append(ops, writeGuardProtectedOp("get", "/"+table+"/01", nil))
		}
		return inspectOps(ops...)
	}
	// mixed asks about a record the caller reads and, in the other place, one more
	// operation on table that the protected session cannot prepare: an update whose
	// change path has two segments, and an insert of a field the table does not
	// have. The same facts are given whether the table is declared or not.
	mixed := func(table string, insertFirst bool, kind string) string {
		read := writeGuardProtectedOp("get", "/customers/01", nil)
		var other api.Operation
		switch kind {
		case "update":
			other = writeGuardProtectedOp("update", "/"+table+"/01", writeGuardProtectedSet("total", "x"))
		default:
			other = writeGuardProtectedOp("insert", "/"+table+"/zz", &api.Mutation{Data: map[string]any{"nofield": "x"}})
		}
		if insertFirst {
			return inspectOps(other, read)
		}
		return inspectOps(read, other)
	}
	type pair struct {
		name         string
		hidden, deny string
		// swaps names what the answer for the undeclared table repeats (the
		// caller sent it) as the denied table is named, so the two compare.
		swaps []string
	}
	ghost := []string{"ghost", "orders"}
	quoted := []string{`\"returns\"`, "orders"}
	pairs := []pair{
		{"sample, undeclared root", sample("ghost", ""), sample("orders", ""), ghost},
		{"sample, undeclared root with an order", sample("ghost", "orderBy: [{field: name}]\n"), sample("orders", "orderBy: [{field: name}]\n"), ghost},
		{"sample, undeclared root ordered by id", sample("ghost", "orderBy: [{field: id, desc: true}]\n"), sample("orders", "orderBy: [{field: id, desc: true}]\n"), ghost},
		{"sample, quoted spelling of a declared key", sample(`"returns"`, ""), sample("orders", ""), quoted},
		{"sample, quoted spelling with an order", sample(`"returns"`, "orderBy: [{field: name}]\n"), sample("orders", "orderBy: [{field: name}]\n"), quoted},
		{"inspection, undeclared table", inspect("ghost"), inspect("orders"), ghost},
		{"inspection, quoted spelling of a declared key", inspect(`"returns"`), inspect("orders"), quoted},
		{"inspection, undeclared table after a visible one", inspect("customers", "ghost"), inspect("customers", "orders"), ghost},
		{"inspection, undeclared table before a visible one", inspect("ghost", "customers"), inspect("orders", "customers"), ghost},
		{"inspection, quoted spelling after a visible one", inspect("customers", `"returns"`), inspect("customers", "orders"), quoted},
	}
	for _, kind := range []string{"update", "insert"} {
		what := map[string]string{"update": "an update of a path of two segments", "insert": "an insert of a field the table lacks"}[kind]
		for _, first := range []bool{false, true} {
			order := "after a visible one"
			if first {
				order = "before a visible one"
			}
			pairs = append(pairs,
				pair{"inspection, " + what + " on an undeclared table " + order, mixed("ghost", first, kind), mixed("orders", first, kind), ghost},
				pair{"inspection, " + what + " on the quoted spelling " + order, mixed(`"returns"`, first, kind), mixed("orders", first, kind), quoted})
		}
	}
	for _, caller := range []struct{ name, token string }{{"owner", ownerToken}, {"granted token", hiddenTableGrantedToken}} {
		t.Run(caller.name, func(t *testing.T) {
			headers := map[string]string{"Content-Type": "application/json"}
			for _, p := range pairs {
				hidden := hiddenSourceAsk(t, ts, caller.token, "POST", evaluate, p.hidden, headers)
				deny := hiddenSourceAsk(t, ts, caller.token, "POST", evaluate, p.deny, headers)
				if hidden.status != http.StatusOK || !strings.Contains(hidden.body, `"result":"deny"`) {
					t.Errorf("%s: want the redacted deny, got %d %s", p.name, hidden.status, hidden.body)
				}
				if got := strings.ReplaceAll(hidden.body, p.swaps[0], p.swaps[1]); hidden.status != deny.status || got != deny.body {
					t.Errorf("%s: undeclared\n  %d %s\nhidden by the policy\n  %d %s", p.name, hidden.status, hidden.body, deny.status, deny.body)
				}
				for _, why := range []string{"not declared", "nested under", "record not found", "Quoted"} {
					if strings.Contains(hidden.body, why) {
						t.Errorf("%s: the answer says why: %s", p.name, hidden.body)
					}
				}
			}
		})
	}
	if got := file.rows(t, `"returns"`); got != "01=Quoted" {
		t.Errorf("the table named with quotes holds %q, want 01=Quoted", got)
	}
}

// TestHiddenRecordIsAnsweredAlikeOnTheProtectedPatchAndEvidence: the protected
// PATCH of /records and /access/evidence answer a record the caller may not see
// with 404 resource_unavailable. A table the database does not declare, and the
// quoted spelling of a declared key, get the same status and the same whole body
// as a declared table the policy hides, whichever of the fields of the resource
// the request carries, for the owner and for a token granted both names.
func TestHiddenRecordIsAnsweredAlikeOnTheProtectedPatchAndEvidence(t *testing.T) {
	ts, file := hiddenTableServer(t)
	patch := func(table string) (path, body string) {
		op := writeGuardProtectedOp("update", "/"+table+"/01", writeGuardProtectedSet("total"))
		data, _ := json.Marshal(op)
		return "/v1/databases/crm/records/" + url.PathEscape(table) + "/01", string(data)
	}
	evidence := func(parts bool) func(string) (string, string) {
		return func(table string) (string, string) {
			resource := az.Resource{DatabaseID: "crm", Path: "/" + table + "/01"}
			if parts {
				resource.Table, resource.RowID = table, "01"
			}
			data, _ := json.Marshal(map[string]any{"apiVersion": az.APIVersion, "resource": resource, "requiredFields": [][]string{{"total"}}})
			return "/v1/databases/crm/access/evidence", string(data)
		}
	}
	for _, caller := range []struct{ name, token string }{{"owner", ownerToken}, {"granted token", hiddenTableGrantedToken}} {
		t.Run(caller.name, func(t *testing.T) {
			for _, c := range []struct {
				what, table, swap string
			}{
				{"an undeclared table", "ghost", "ghost"},
				{"the quoted spelling of a declared key", `"returns"`, `\"returns\"`},
			} {
				for _, route := range []struct {
					name, method, content string
					at                    func(table string) (path, body string)
				}{
					{"PATCH", "PATCH", "application/vnd.dtql.operation+json", patch},
					{"evidence with the table and the row", "POST", "application/json", evidence(true)},
					{"evidence with the path only", "POST", "application/json", evidence(false)},
				} {
					name := route.name + ", " + c.what
					headers := map[string]string{"Content-Type": route.content}
					hiddenPath, hiddenBody := route.at(c.table)
					denyPath, denyBody := route.at("orders")
					hidden := hiddenSourceAsk(t, ts, caller.token, route.method, hiddenPath, hiddenBody, headers)
					deny := hiddenSourceAsk(t, ts, caller.token, route.method, denyPath, denyBody, headers)
					if hidden.status != http.StatusNotFound || !strings.Contains(hidden.body, `"code":"resource_unavailable"`) {
						t.Errorf("%s: want 404 resource_unavailable, got %d %s", name, hidden.status, hidden.body)
					}
					if got := strings.ReplaceAll(hidden.body, c.swap, "orders"); hidden.status != deny.status || got != deny.body {
						t.Errorf("%s: undeclared\n  %d %s\nhidden by the policy\n  %d %s", name, hidden.status, hidden.body, deny.status, deny.body)
					}
				}
			}
		})
	}
	if got := file.rows(t, `"returns"`); got != "01=Quoted" {
		t.Errorf("the table named with quotes holds %q, want 01=Quoted", got)
	}
}

// TestSampleIsAnsweredAlikeWhenThePolicyCannotDecide: a policy bound to a
// principal realm cannot decide any request of a caller whose principal has no
// subject, whatever collection the request names. A sample of a table the
// database does not declare, of the quoted spelling of a declared key and of a
// declared table is then answered with the same status and the same whole body
// (the result the policy gave), for the owner and for a token granted every name.
func TestSampleIsAnsweredAlikeWhenThePolicyCannotDecide(t *testing.T) {
	ts, _ := hiddenTableServerInRealm(t, "local")
	sample := func(table string) string {
		op := writeGuardProtectedOp("update", "/"+table, writeGuardProtectedSet("name"))
		data, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeSample, DiagnosticLevel: "ordinary", Operations: []api.Operation{op},
			Sample: &api.Sample{Limit: 1, Query: api.Query{Format: "dtql-yaml", Text: "from: {name: '" + table + "'}\n"}}})
		return string(data)
	}
	for _, caller := range []struct{ name, token string }{{"owner", ownerToken}, {"granted token", hiddenTableGrantedToken}} {
		t.Run(caller.name, func(t *testing.T) {
			headers := map[string]string{"Content-Type": "application/json"}
			declared := hiddenSourceAsk(t, ts, caller.token, "POST", "/v1/databases/crm/access/evaluate", sample("orders"), headers)
			if declared.status != http.StatusOK || !strings.Contains(declared.body, `"result":"indeterminate"`) {
				t.Fatalf("a declared table: want the policy's indeterminate result, got %d %s", declared.status, declared.body)
			}
			for _, c := range []struct{ what, table, swap string }{
				{"an undeclared table", "ghost", "ghost"},
				{"the quoted spelling of a declared key", `"returns"`, `\"returns\"`},
				{"a declared table the caller reads", "customers", "customers"},
			} {
				got := hiddenSourceAsk(t, ts, caller.token, "POST", "/v1/databases/crm/access/evaluate", sample(c.table), headers)
				if text := strings.ReplaceAll(got.body, c.swap, "orders"); got.status != declared.status || text != declared.body {
					t.Errorf("%s:\n  %d %s\nhidden by the policy\n  %d %s", c.what, got.status, got.body, declared.status, declared.body)
				}
			}
		})
	}
}

// TestUnsupportedOperationIsRefusedAlikeForEveryTable: an operation of the
// authorization API that the protected session does not support (an execution
// class other than dtql) is refused before the table is looked at, so the
// protected PATCH of /records and the inspection of /access/evaluate give the
// same status and the same whole body for a table the database does not declare,
// the quoted spelling of a declared key, a declared table the policy hides and a
// declared table the caller reads, for the owner and for a token granted every
// name.
func TestUnsupportedOperationIsRefusedAlikeForEveryTable(t *testing.T) {
	ts, file := hiddenTableServer(t)
	classes := []struct {
		name  string
		apply func(*api.Operation)
	}{
		{"native_sql", func(op *api.Operation) { op.ExecutionClass = az.ExecutionNativeSQL }},
		{"native_graphql", func(op *api.Operation) { op.ExecutionClass = az.ExecutionNativeGraphQL }},
		{"stored_procedure", func(op *api.Operation) {
			op.ExecutionClass = az.ExecutionStoredProcedure
			op.Callable = &az.Callable{Namespace: "ns", Name: "proc"}
		}},
	}
	routes := []struct {
		name, method, content string
		at                    func(table string, apply func(*api.Operation)) (path, body string)
	}{
		{"PATCH", "PATCH", "application/vnd.dtql.operation+json", func(table string, apply func(*api.Operation)) (string, string) {
			op := writeGuardProtectedOp("update", "/"+table+"/01", writeGuardProtectedSet("total"))
			apply(&op)
			data, _ := json.Marshal(op)
			return "/v1/databases/crm/records/" + url.PathEscape(table) + "/01", string(data)
		}},
		{"inspection", "POST", "application/json", func(table string, apply func(*api.Operation)) (string, string) {
			op := writeGuardProtectedOp("get", "/"+table+"/01", nil)
			apply(&op)
			data, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: []api.Operation{op}})
			return "/v1/databases/crm/access/evaluate", string(data)
		}},
	}
	tables := []struct{ what, table, swap string }{
		{"an undeclared table", "ghost", "ghost"},
		{"the quoted spelling of a declared key", `"returns"`, `\"returns\"`},
		{"a declared table the caller reads", "customers", "customers"},
	}
	for _, caller := range []struct{ name, token string }{{"owner", ownerToken}, {"granted token", hiddenTableGrantedToken}} {
		t.Run(caller.name, func(t *testing.T) {
			for _, class := range classes {
				for _, route := range routes {
					headers := map[string]string{"Content-Type": route.content}
					denyPath, denyBody := route.at("orders", class.apply)
					deny := hiddenSourceAsk(t, ts, caller.token, route.method, denyPath, denyBody, headers)
					if deny.status != http.StatusUnprocessableEntity || !strings.Contains(deny.body, `"code":"authorization_unsupported"`) {
						t.Errorf("%s, %s, a declared table the policy hides: want 422 authorization_unsupported, got %d %s", route.name, class.name, deny.status, deny.body)
					}
					for _, c := range tables {
						name := route.name + ", " + class.name + ", " + c.what
						path, body := route.at(c.table, class.apply)
						got := hiddenSourceAsk(t, ts, caller.token, route.method, path, body, headers)
						if text := strings.ReplaceAll(got.body, c.swap, "orders"); got.status != deny.status || text != deny.body {
							t.Errorf("%s:\n  %d %s\nhidden by the policy\n  %d %s", name, got.status, got.body, deny.status, deny.body)
						}
					}
				}
			}
		})
	}
	if got := file.rows(t, `"returns"`); got != "01=Quoted" {
		t.Errorf("the table named with quotes holds %q, want 01=Quoted", got)
	}
}

// TestInspectionAsksForRowEvidenceOnlyForTheOperationsItCouldNotDecide: of the
// operations of one inspection, the ones the protected session could not prepare
// (an update of a path of two segments) are listed as needing row evidence, for a
// caller who may inspect protected rows, and an operation that was decided is not,
// whichever place it has in the request.
func TestInspectionAsksForRowEvidenceOnlyForTheOperationsItCouldNotDecide(t *testing.T) {
	ts, _ := hiddenTableServer(t, server.WithOwnerAuthorization(func(_ context.Context, _ *auth.Principal, _ az.Source, capability string, _ az.Resource) bool {
		return capability == auth.CapAccessInspectProtected
	}))
	read := writeGuardProtectedOp("get", "/customers/01", nil)
	read.ID = "read"
	undecided := writeGuardProtectedOp("update", "/customers/01", writeGuardProtectedSet("name", "x"))
	undecided.ID = "undecided"
	for _, c := range []struct {
		name string
		ops  []api.Operation
		want []string
	}{
		{"a decided read", []api.Operation{read}, nil},
		{"a decided read before an undecided update", []api.Operation{read, undecided}, []string{"undecided:row_evidence_required"}},
		{"an undecided update before a decided read", []api.Operation{undecided, read}, []string{"undecided:row_evidence_required"}},
		{"an undecided update", []api.Operation{undecided}, []string{"undecided:row_evidence_required"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			data, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: c.ops})
			answer := hiddenSourceAsk(t, ts, ownerToken, "POST", "/v1/databases/crm/access/evaluate", string(data), map[string]string{"Content-Type": "application/json"})
			var body struct {
				Coverage struct {
					Unevaluated []struct{ OperationID, Reason string }
				}
			}
			if err := json.Unmarshal([]byte(answer.body), &body); err != nil || answer.status != http.StatusOK {
				t.Fatalf("%d %s: %v", answer.status, answer.body, err)
			}
			var got []string
			for _, fact := range body.Coverage.Unevaluated {
				got = append(got, fact.OperationID+":"+fact.Reason)
			}
			sort.Strings(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("unevaluated %q, want %q: %s", got, c.want, answer.body)
			}
		})
	}
}
