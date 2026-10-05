package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// A relational document is not run on a database with access policies. These tests
// hold that refusal to its contract: it depends on the databases the document
// names and on nothing else, so one document, sent with a collection the policy
// hides, with one the database does not declare, with one the policy lets the
// caller read and with a spelling of it that is not canonical, gets one answer, for
// every shape a relational document takes, on both endpoints, for the owner and for
// a granted token, under a source budget that a read would run into. The executor
// is the real one and the mount is a counting fake: it must see no read, no field
// load and no transaction. The
// helpers of this file all start with relProtected so they cannot clash with the
// others of the package.

// relProtectedFake is the driver of a mount with access policies. It counts every
// call that reads or loads a schema; every other method panics (the nil embedded
// DB), so a request that strays onto another path of the driver fails loudly.
type relProtectedFake struct {
	dal.DB
	mu    sync.Mutex
	calls []string
}

func (f *relProtectedFake) note(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *relProtectedFake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *relProtectedFake) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	f.note("read")
	return nil, io.ErrUnexpectedEOF
}

func (f *relProtectedFake) ExecuteQueryToRecordsetReader(context.Context, dal.Query, ...recordset.Option) (dal.RecordsetReader, error) {
	f.note("recordset read")
	return nil, io.ErrUnexpectedEOF
}

func (f *relProtectedFake) JoinFields(context.Context, dal.RecordsetSource) ([]string, error) {
	f.note("field load")
	return nil, nil
}

func (f *relProtectedFake) Get(context.Context, record.Record) error {
	f.note("get")
	return io.ErrUnexpectedEOF
}

func (f *relProtectedFake) RunReadonlyTransaction(context.Context, dal.ROTxWorker, ...dal.TransactionOption) error {
	f.note("read transaction")
	return io.ErrUnexpectedEOF
}

const relProtectedPolicy = `apiVersion: dtql.org/access/v1
kind: AccessPolicy
metadata:
  name: readers
  visibility: public
target:
  database: crm
composition: dalgo-hierarchical-v1
default: deny
ruleSets:
  reader:
    - path: /customers
      rules:
        - id: read-irish
          effect: allow
          operations: [query, get]
          where:
            op: "=="
            left: {field: country}
            right: {value: IE}
          fields: [id, name, country]
    - path: /orders
      rules:
        - id: read-orders
          effect: allow
          operations: [query, get]
          fields: [id, customer_id, total]
bindings:
  roles:
    reader: [reader]
`

// relProtectedMount mounts the database crm with access policies over a counting
// fake. It declares customers and orders, which the policy reads, and secrets,
// which it does not.
func relProtectedMount(t *testing.T) (*core.Database, *relProtectedFake) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readers.yaml"), []byte(relProtectedPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	policies, err := access.LoadPolicyFiles(dir, access.FilePolicyConfig{Enabled: true, Database: "crm", Policies: []string{"readers.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	collection := schema.Collection{Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "crm", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"customers": collection, "orders": collection, "secrets": collection}},
	}
	fake := &relProtectedFake{}
	db, err := core.Open(m, fake, []schema.Mode{schema.ModeStrict}, "", policies...)
	if err != nil {
		t.Fatal(err)
	}
	if !db.HasAccessPolicies() {
		t.Fatal("the mount has no access policies")
	}
	return db, fake
}

// relProtectedServer serves the protected mount to a principal that holds role,
// with a source budget of one row and the real executor. Three callers can read: the
// owner, a token granted the read capability on the whole database, and a token
// granted it on the collection customers only.
func relProtectedServer(t *testing.T, role string) (*httptest.Server, *relProtectedFake) {
	t.Helper()
	db, fake := relProtectedMount(t)
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateGrant(&auth.Grant{DatabaseID: "crm", Capabilities: []auth.Capability{{Action: auth.CapRecordsRead}}}, relProtectedGranted); err != nil {
		t.Fatal(err)
	}
	// A grant of the read capability on one collection of the database only.
	scoped := &auth.Grant{DatabaseID: "crm", Capabilities: []auth.Capability{{Action: auth.CapRecordsRead, Collection: "customers"}}}
	if err := store.CreateGrant(scoped, relProtectedScoped); err != nil {
		t.Fatal(err)
	}
	service := New("test", map[string]*core.Database{"crm": db},
		WithQueryLimits(QueryLimits{MaxSourceRows: 1}),
		WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{role}}, nil
		}),
		WithAuth(&auth.Config{OwnerToken: relProtectedOwner, Store: store}))
	t.Cleanup(service.CloseSnapshots)
	host := httptest.NewServer(service.Handler())
	t.Cleanup(host.Close)
	return host, fake
}

const (
	relProtectedOwner   = "owner-of-crm"
	relProtectedGranted = "token-for-crm"
	relProtectedScoped  = "token-for-customers"
)

// relProtectedShapes are the shapes a relational document takes. Each is a format
// that puts the collection under test where %[1]s stands, with the database of a
// source written where %[2]s stands (the per-database endpoint takes sources that
// name none, /v1/dtql needs every source to name one). The documents are the same
// for the three names.
var relProtectedShapes = []struct {
	name string
	doc  string
}{
	{"one source, aliased", "from: {%[2]sname: %[1]s, alias: x}\n"},
	{
		"an ordered join, the collection is the root",
		"from: {%[2]sname: %[1]s, alias: x, joins: [{from: {%[2]sname: orders, alias: o}, on: [{left: {field: id, source: x}, op: '==', right: {field: customer_id, source: o}}]}]}\n" +
			"orderBy: [{field: id, source: x}]\ncolumns: [{field: id, source: x}]\n",
	},
	{
		"an ordered join, the collection is the joined source",
		"from: {%[2]sname: orders, alias: o, joins: [{from: {%[2]sname: %[1]s, alias: x}, on: [{left: {field: customer_id, source: o}, op: '==', right: {field: id, source: x}}]}]}\n" +
			"orderBy: [{field: id, source: o}]\ncolumns: [{field: id, source: x}]\n",
	},
	{
		"a streamed join, the collection is the root",
		"from: {%[2]sname: %[1]s, alias: x, joins: [{from: {%[2]sname: orders, alias: o}, on: [{left: {field: id, source: x}, op: '==', right: {field: customer_id, source: o}}]}]}\n" +
			"columns: [{field: id, source: o}]\n",
	},
	{
		"a streamed join, the collection is the joined source",
		"from: {%[2]sname: orders, alias: o, joins: [{from: {%[2]sname: %[1]s, alias: x}, on: [{left: {field: customer_id, source: o}, op: '==', right: {field: id, source: x}}]}]}\n" +
			"columns: [{field: id, source: x}]\n",
	},
	{"a derived source", "from: {query: {as: d, from: {%[2]sname: %[1]s}, columns: [{field: id}]}}\ncolumns: [{field: id, source: d}]\n"},
	{"a subquery that is evaluated", "from: {%[2]sname: orders, alias: o}\nwhere: {exists: {query: {from: {%[2]sname: %[1]s}}}}\n"},
	{
		"a subquery behind a false condition",
		"from: {%[2]sname: orders, alias: o}\nwhere: {and: [{op: '==', left: {field: id, source: o}, right: {value: none}}, {exists: {query: {from: {%[2]sname: %[1]s}}}}]}\n",
	},
	{
		"a scalar subquery",
		"from: {%[2]sname: orders, alias: o}\ncolumns:\n  - {field: id, source: o}\n  - query: {as: n, from: {%[2]sname: %[1]s}, columns: [{aggregate: {function: count, args: [{star: true}]}}]}\n",
	},
	{"an aggregate", "from: {%[2]sname: %[1]s, alias: x}\ncolumns: [{aggregate: {function: count, args: [{star: true}]}, as: n}]\n"},
	{"two columns of one output name", "from: {%[2]sname: %[1]s, alias: x}\ncolumns: [{field: id, source: x}, {field: name, source: x, as: id}]\n"},
	{"a field name only the classifier accepts", "from: {%[2]sname: %[1]s, alias: x}\ncolumns: [{field: 'zip code', source: x}]\n"},
}

// relProtectedNames are what a document names where it carries the collection
// under test: a collection the policy lets the caller read, one the database
// declares and the policy does not allow, one the database does not declare, and a
// spelling of a declared collection that is not its canonical name.
var relProtectedNames = []struct{ what, name string }{
	{"a readable collection", "customers"},
	{"a declared collection the policy hides", "secrets"},
	{"an undeclared collection", "ghost"},
	{"a spelling that is not canonical", `'"customers"'`},
}

func TestARelationalDocumentIsNotRunOnADatabaseWithAccessPolicies(t *testing.T) {
	for _, role := range []string{"reader", "nobody"} {
		host, fake := relProtectedServer(t, role)
		for _, endpoint := range []struct {
			name, path, database string
		}{
			{"per-database endpoint", "/v1/databases/crm/dtql", ""},
			{"/v1/dtql", "/v1/dtql", "database: crm, "},
		} {
			for _, caller := range []struct{ name, token string }{{"owner", relProtectedOwner}, {"granted token", relProtectedGranted}} {
				for _, shape := range relProtectedShapes {
					t.Run(role+", "+endpoint.name+", "+caller.name+", "+shape.name, func(t *testing.T) {
						var first relFakeResponse
						for i, name := range relProtectedNames {
							doc := fmt.Sprintf(shape.doc, name.name, endpoint.database)
							resp := relFakeDo(t, host, http.MethodPost, endpoint.path, caller.token, doc, nil)
							if resp.status != http.StatusUnprocessableEntity || resp.code() != "authorization_unsupported" {
								t.Fatalf("%s: status %d, want a 422 authorization_unsupported: %s", name.what, resp.status, resp.raw)
							}
							if i == 0 {
								first = resp
								continue
							}
							if resp.raw != first.raw {
								t.Fatalf("%s: %s\n%s: %s\nwant one answer for both", relProtectedNames[0].what, first.raw, name.what, resp.raw)
							}
							for _, hidden := range []string{"ghost", "secrets"} {
								if strings.Contains(resp.raw, hidden) {
									t.Fatalf("the answer repeats %q: %s", hidden, resp.raw)
								}
							}
						}
					})
				}
			}
		}
		if calls := fake.seen(); len(calls) != 0 {
			t.Fatalf("role %s: the protected mount saw %v, want no call", role, calls)
		}
	}
}

// The refusal does not depend on the request's other properties that a later check
// looks at: a paging header, a collection spelled another way, or a database that
// is read next to it. A caller who may not read a collection is still refused for
// that first, and a database that is not mounted is still a 404.
func TestTheRefusalOfAProtectedDatabaseComesAfterTheCapabilityAndTheLeaseOnly(t *testing.T) {
	host, fake := relProtectedServer(t, "reader")
	const (
		join    = "from: {database: crm, name: orders, alias: o, joins: [{from: {database: %s, name: customers, alias: c}, on: [{left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}]}]}\n"
		inverse = "from: {database: crm, name: customers, alias: c, joins: [{from: {database: crm, name: orders, alias: o}, on: [{left: {field: id, source: c}, op: '==', right: {field: customer_id, source: o}}]}]}\n"
	)
	for _, tc := range []struct {
		name, path, token, doc string
		headers                map[string]string
		status                 int
		code                   string
		names                  string // a collection the message names, when it names one
	}{
		{"a paging header", "/v1/dtql", relProtectedOwner, fmt.Sprintf(join, "crm"), map[string]string{"OVDB-Page-Size": "10"}, 422, "authorization_unsupported", ""},
		{"a protected database next to one that is not mounted", "/v1/dtql", relProtectedOwner, fmt.Sprintf(join, "nowhere"), nil, 404, "not_found", ""},
		{"no token", "/v1/dtql", "", fmt.Sprintf(join, "crm"), nil, 401, "", ""},
		{"a source the per-database endpoint does not take", "/v1/databases/crm/dtql", relProtectedOwner, fmt.Sprintf(join, "other"), nil, 400, "invalid_dtql", ""},
		// A token granted one collection of the database is refused a join of two before
		// the refusal of the database, on both endpoints and whichever of the two the
		// document starts from: the message names the collection it lacks.
		{"a token scoped to customers, a join that starts from orders, on /v1/dtql", "/v1/dtql", relProtectedScoped, fmt.Sprintf(join, "crm"), nil, 403, "forbidden", "orders"},
		{"a token scoped to customers, a join that starts from orders, on the per-database endpoint", "/v1/databases/crm/dtql", relProtectedScoped, fmt.Sprintf(join, "crm"), nil, 403, "forbidden", "orders"},
		{"a token scoped to customers, a join that starts from customers, on /v1/dtql", "/v1/dtql", relProtectedScoped, inverse, nil, 403, "forbidden", "orders"},
		{"a token scoped to customers, a join that starts from customers, on the per-database endpoint", "/v1/databases/crm/dtql", relProtectedScoped, inverse, nil, 403, "forbidden", "orders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := relFakeDo(t, host, http.MethodPost, tc.path, tc.token, tc.doc, tc.headers)
			if resp.status != tc.status || (tc.code != "" && resp.code() != tc.code) {
				t.Fatalf("status %d code %q, want %d %q: %s", resp.status, resp.code(), tc.status, tc.code, resp.raw)
			}
			if message := fmt.Sprint(resp.errorDetail()["message"]); tc.names != "" && !strings.Contains(message, `collection "`+tc.names+`"`) {
				t.Fatalf("the message %q does not name the collection %s", message, tc.names)
			}
		})
	}
	if calls := fake.seen(); len(calls) != 0 {
		t.Fatalf("the protected mount saw %v, want no call", calls)
	}
}

// A database without access policies is not refused, so the refusal above is the
// protected database's and not a refusal of every document: the same shape, with
// the protected database next to it, is refused, and without it is run.
func TestOnlyADatabaseWithAccessPoliciesRefusesARelationalDocument(t *testing.T) {
	protected, _ := relProtectedMount(t)
	open := relFakeMount("pub", "sqlite", "")
	executed := &relFakeExecutor{result: joinexec.Result{Columns: []string{}}}
	_, host := relFakeServer(t, executed, []*core.Database{protected, open})
	const doc = "from: {database: %s, name: orders, alias: o, joins: [{from: {database: %s, name: customers, alias: c}, on: [{left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}]}]}\n"
	for _, tc := range []struct {
		name, first, second string
		status              int
	}{
		{"two open databases", "pub", "pub", 200},
		{"a protected database as the root", "crm", "pub", 422},
		{"a protected database as the joined source", "pub", "crm", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := executed.count()
			resp := relFakeDo(t, host, http.MethodPost, "/v1/dtql", "", fmt.Sprintf(doc, tc.first, tc.second), nil)
			if resp.status != tc.status {
				t.Fatalf("status %d, want %d: %s", resp.status, tc.status, resp.raw)
			}
			if wantCalls := map[bool]int{true: 1, false: 0}[tc.status == 200]; executed.count()-before != wantCalls {
				t.Fatalf("the executor was called %d times, want %d", executed.count()-before, wantCalls)
			}
		})
	}
}
