package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// hiddenSourceDB opens a real SQLite mount behind an owner policy. It declares
// customers, which the policy admits for the rows of country IE, and orders. The
// policy has no rule for orders (so it denies them) unless admitOrders is set.
// Nothing else is declared.
func hiddenSourceDB(t *testing.T, admitOrders bool) *core.Database {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: crm, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n" +
		"    customers:\n      fields:\n        name: {type: string}\n        country: {type: string}\n" +
		"    orders:\n      fields:\n        total: {type: string}\n"
	aclWriteFile(t, path, manifest)
	seed, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []core.Op{
		{Op: "insert", Key: record.NewKeyWithID("customers", "01"), Data: map[string]any{"name": "Ada", "country": "IE"}},
		{Op: "insert", Key: record.NewKeyWithID("orders", "o1"), Data: map[string]any{"total": "10"}},
	} {
		if _, err = seed.Apply(context.Background(), []core.Op{op}, "seed"); err != nil {
			t.Fatal(err)
		}
	}
	if err = seed.Close(); err != nil {
		t.Fatal(err)
	}
	policy := strings.Replace(aclPolicy("upper", "country", "IE"), "operations: [query, get]", "operations: [query, get, update]", 1)
	if admitOrders {
		policy = strings.Replace(policy, "bindings:", "    - path: /orders\n      rules:\n        - id: read-orders\n          effect: allow\n          operations: [query, get]\n          fields: [id, total]\nbindings:", 1)
	}
	aclWriteFile(t, filepath.Join(dir, "upper.yaml"), policy)
	aclWriteFile(t, path, manifest+"acl: {enabled: true, policies: [upper.yaml]}\n")
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// hiddenSourceServer serves hiddenSourceDB, whose policy denies orders, to the
// owner token.
func hiddenSourceServer(t *testing.T) *httptest.Server {
	t.Helper()
	service := server.New("test", map[string]*core.Database{"crm": hiddenSourceDB(t, false)},
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken}))
	t.Cleanup(service.CloseSnapshots)
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// hiddenSourceAnswer is what a route says: its status and its JSON body without
// the id of the request.
type hiddenSourceAnswer struct {
	status int
	body   string
}

func hiddenSourceAsk(t *testing.T, ts *httptest.Server, method, path, body string, headers map[string]string) hiddenSourceAnswer {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s %s: not JSON (%d): %s", method, path, resp.StatusCode, raw)
	}
	var strip func(any)
	strip = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			delete(v, "requestId")
			for _, child := range v {
				strip(child)
			}
		case []any:
			for _, child := range v {
				strip(child)
			}
		}
	}
	strip(decoded)
	out, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return hiddenSourceAnswer{status: resp.StatusCode, body: string(out)}
}

// hiddenSourceCase pairs a query that reads an undeclared collection with the
// query that reads a declared collection the policy denies in the same place.
type hiddenSourceCase struct {
	name   string
	hidden string
	denied string
	// root is the collection the route names for the whole query. The answer
	// of a route may repeat it (the caller sent it), so it is the one name that
	// is swapped before the two answers are compared.
	hiddenRoot string
	deniedRoot string
}

// TestUndeclaredSourceIsAnsweredAsADeniedOneOnAMountWithPolicies: a mount with
// access policies does not say which collections it declares. A query that
// reads a collection the database does not declare, as its root or anywhere
// inside it, gets the answer the same query gets for a declared collection the
// policy denies: the same status and the same body, with no word of why and
// no name of the undeclared collection but the one the caller sent as root.
func TestUndeclaredSourceIsAnsweredAsADeniedOneOnAMountWithPolicies(t *testing.T) {
	ts := hiddenSourceServer(t)
	const db = "/v1/databases/crm"
	wire := func(collection, parent string) string {
		out := map[string]any{"collection": collection}
		if parent != "" {
			out["parent"] = parent
		}
		data, _ := json.Marshal(out)
		return string(data)
	}
	dtql := func(from string) string { return "from: {name: " + from + "}\n" }
	nested := func(root, inner string) string {
		return "from: {name: " + root + "}\nwhere: {exists: {query: {from: {name: " + inner + "}}}}\n"
	}
	sample := func(q string) string {
		op := writeGuardProtectedOp("update", "/customers", writeGuardProtectedSet("name"))
		data, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeSample, DiagnosticLevel: "ordinary", Operations: []api.Operation{op},
			Sample: &api.Sample{Limit: 1, Query: api.Query{Format: "dtql-yaml", Text: q}}})
		return string(data)
	}
	type route struct {
		name    string
		method  string
		path    func(q string) string
		body    func(q string) string
		headers map[string]string
		cases   []hiddenSourceCase
	}
	inURL := func(prefix string) func(string) string {
		return func(q string) string { return db + prefix + "?q=" + url.QueryEscape(q) }
	}
	atPath := func(path string) func(string) string { return func(string) string { return db + path } }
	noBody := func(string) string { return "" }
	asBody := func(q string) string { return q }
	wireCases := []hiddenSourceCase{
		{name: "root", hidden: wire("ghost", ""), denied: wire("orders", ""), hiddenRoot: "ghost", deniedRoot: "orders"},
		{name: "parent", hidden: wire("customers", "customers/c1"), denied: wire("orders", "")},
		{name: "root with a parent", hidden: wire("ghost", "customers/c1"), denied: wire("orders", "")},
	}
	dtqlCases := []hiddenSourceCase{
		{name: "root", hidden: dtql("ghost"), denied: dtql("orders"), hiddenRoot: "ghost", deniedRoot: "orders"},
		{name: "subquery", hidden: nested("customers", "ghost"), denied: nested("customers", "orders")},
		{name: "subquery of a denied root", hidden: nested("orders", "ghost"), denied: dtql("orders")},
	}
	sampleCases := []hiddenSourceCase{
		{name: "subquery", hidden: nested("customers", "ghost"), denied: nested("customers", "orders")},
	}
	for _, r := range []route{
		{name: "/query GET", method: "GET", path: inURL("/query"), body: noBody, cases: wireCases},
		{name: "/query POST", method: "POST", path: atPath("/query"), body: asBody, cases: wireCases},
		{name: "/dtql GET", method: "GET", path: inURL("/dtql"), body: noBody, cases: dtqlCases},
		{name: "/dtql POST", method: "POST", path: atPath("/dtql"), body: asBody, cases: dtqlCases},
		{name: "/dtql snapshot page", method: "POST", path: atPath("/dtql"), body: asBody, headers: map[string]string{"OVDB-Page-Size": "10"}, cases: dtqlCases},
		{name: "access sample", method: "POST", path: atPath("/access/evaluate"), body: sample, cases: sampleCases},
	} {
		t.Run(r.name, func(t *testing.T) {
			ask := func(q string) hiddenSourceAnswer {
				return hiddenSourceAsk(t, ts, r.method, r.path(q), r.body(q), r.headers)
			}
			for _, c := range r.cases {
				hidden, denied := ask(c.hidden), ask(c.denied)
				if hidden.status == http.StatusNotFound || hidden.status == http.StatusInternalServerError {
					t.Errorf("%s: %d %s", c.name, hidden.status, hidden.body)
				}
				got := hidden.body
				if c.hiddenRoot != "" {
					got = strings.ReplaceAll(got, c.hiddenRoot, c.deniedRoot)
				}
				if hidden.status != denied.status || got != denied.body {
					t.Errorf("%s: undeclared\n  %d %s\ndenied\n  %d %s", c.name, hidden.status, hidden.body, denied.status, denied.body)
				}
				if strings.Contains(hidden.body, "ghost") && c.hiddenRoot == "" {
					t.Errorf("%s: the answer names the undeclared collection: %s", c.name, hidden.body)
				}
				for _, why := range []string{"not declared", "nested under", "record not found"} {
					if strings.Contains(hidden.body, why) {
						t.Errorf("%s: the answer says why: %s", c.name, hidden.body)
					}
				}
			}
		})
	}
}
