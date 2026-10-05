package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
// request may read. Two callers hold a read capability: the owner token, and a
// token granted every collection a case below names, in the spelling it names.
func hiddenTableServer(t *testing.T) (*httptest.Server, sqlNamesFixture) {
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
	aclWriteFile(t, path, manifest+"acl: {enabled: true, policies: [upper.yaml]}\n")
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
		reads = append(reads, auth.Capability{Action: auth.CapRecordsRead, Collection: collection})
	}
	grantToken(t, store, "crm", hiddenTableGrantedToken, reads...)
	service := server.New("test", map[string]*core.Database{"crm": db},
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}))
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
	inspect := func(tables ...string) string {
		var ops []api.Operation
		for i, table := range tables {
			op := writeGuardProtectedOp("get", "/"+table+"/01", nil)
			op.ID = "op" + string(rune('1'+i))
			ops = append(ops, op)
		}
		return asJSON(api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: ops})
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
