package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

const (
	nestedScopeCustomersToken = "ovdb_test_scope_customers"
	nestedScopeBothToken      = "ovdb_test_scope_both"
)

// nestedScopeStore holds two grants on database db: one that reads customers
// only, one that reads customers and orders.
func nestedScopeStore(t *testing.T, db string) *auth.Store {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	read := func(collections ...string) []auth.Capability {
		var out []auth.Capability
		for _, collection := range collections {
			out = append(out, auth.Capability{Action: auth.CapRecordsRead, Collection: collection})
		}
		return out
	}
	for token, capabilities := range map[string][]auth.Capability{
		nestedScopeCustomersToken: read("customers"),
		nestedScopeBothToken:      read("customers", "orders"),
	} {
		if err := store.CreateGrant(&auth.Grant{PrincipalID: token, DatabaseID: db, Capabilities: capabilities}, token); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// nestedScopeServer serves a fake-backed database that declares customers and
// orders (as engine's adapter would run them: the fake counts every read).
func nestedScopeServer(t *testing.T, engine string) (*httptest.Server, *guardFakeDB) {
	t.Helper()
	fake := &guardFakeDB{}
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "guarded", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: engine},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
			"orders":    {Fields: map[string]schema.Field{"total": {Type: schema.TypeString}}},
		}},
	}
	db, err := core.Open(m, fake, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	service := server.New("test", map[string]*core.Database{"guarded": db},
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: nestedScopeStore(t, "guarded")}))
	t.Cleanup(service.CloseSnapshots)
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	return ts, fake
}

// nestedScopeDocuments are DTQL documents of the collection customers that read
// the collection orders as well.
var nestedScopeDocuments = map[string]string{
	"exists":             "from: {name: customers}\nwhere: {exists: {query: {from: {name: orders}}}}\n",
	"not exists":         "from: {name: customers}\nwhere: {notExists: {query: {from: {name: orders}}}}\n",
	"scalar":             "from: {name: customers}\nwhere: {op: '==', left: {field: name}, right: {query: {from: {name: orders}, columns: [{field: total}], limit: 1}}}\n",
	"exists with a join": "from: {name: customers}\nwhere: {exists: {query: {from: {name: customers, alias: c2, joins: [{from: {name: orders, alias: o}, on: [{op: '==', left: {field: id, source: c2}, right: {field: id, source: o}}]}]}}}}\n",
}

// TestDTQLReadsOfAnotherCollectionAreAuthorizedPerCollection: a token's read
// capability is scoped to a collection. A DTQL document of one collection can
// read another inside a subquery, so the capability is checked for every
// collection the document reads, before anything is read, on every engine, for a
// plain request, a request that pages a snapshot and a parameterised one.
func TestDTQLReadsOfAnotherCollectionAreAuthorizedPerCollection(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb", "firestore"} {
		t.Run(engine, func(t *testing.T) {
			ts, fake := nestedScopeServer(t, engine)
			const dtql = "/v1/databases/guarded/dtql"
			for label, doc := range nestedScopeDocuments {
				parameterised, _ := json.Marshal(map[string]any{"query": doc, "parameters": map[string]any{}})
				for _, call := range []guardCall{
					{name: "GET", method: "GET", path: dtql + "?q=" + url.QueryEscape(doc)},
					{name: "POST", method: "POST", path: dtql, body: doc},
					{name: "POST parameters", method: "POST", path: dtql, body: string(parameterised), headers: map[string]string{"Content-Type": "application/json"}},
					{name: "snapshot page", method: "POST", path: dtql, body: doc, headers: map[string]string{"OVDB-Page-Size": "10"}},
				} {
					with := func(token string) guardCall {
						withToken := call
						withToken.headers = map[string]string{"Authorization": "Bearer " + token}
						for name, value := range call.headers {
							withToken.headers[name] = value
						}
						return withToken
					}
					queries, gets := fake.queries, fake.gets
					status, body := send(t, ts, with(nestedScopeCustomersToken))
					detail, _ := body["error"].(map[string]any)
					if status != http.StatusForbidden || detail["code"] != "forbidden" {
						t.Errorf("%s %s, read of orders by a token for customers: %d %v", label, call.name, status, body)
					}
					if fake.queries != queries || fake.gets != gets {
						t.Fatalf("%s %s: the adapter was read (%d queries, %d gets)", label, call.name, fake.queries-queries, fake.gets-gets)
					}
					// A token for both collections, and the owner, are not stopped.
					for _, token := range []string{nestedScopeBothToken, ownerToken} {
						before := fake.queries
						status, body := send(t, ts, with(token))
						if status == http.StatusForbidden || fake.queries == before {
							t.Errorf("%s %s, token for both: %d %v (%d reads)", label, call.name, status, body, fake.queries-before)
						}
					}
				}
			}
			// A document of the one collection the token reads is not stopped.
			before := fake.queries
			status, body := send(t, ts, guardCall{method: "POST", path: dtql, body: "from: {name: customers}\n", headers: map[string]string{"Authorization": "Bearer " + nestedScopeCustomersToken}})
			if status == http.StatusForbidden || fake.queries == before {
				t.Errorf("a document of customers: %d %v", status, body)
			}
		})
	}
}

// TestAccessSampleReadsOfAnotherCollectionAreAuthorizedPerCollection: the sample
// route runs a DTQL document too, so a subquery in its query is held to the same
// capability check as the root.
func TestAccessSampleReadsOfAnotherCollectionAreAuthorizedPerCollection(t *testing.T) {
	service := server.New("test", map[string]*core.Database{"crm": hiddenSourceDB(t, true)},
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: nestedScopeStore(t, "crm")}))
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	sample := func(query string) string {
		op := writeGuardProtectedOp("update", "/customers", writeGuardProtectedSet("name"))
		data, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeSample, DiagnosticLevel: "ordinary", Operations: []api.Operation{op},
			Sample: &api.Sample{Limit: 1, Query: api.Query{Format: "dtql-yaml", Text: query}}})
		return string(data)
	}
	ask := func(token, query string) (int, string) {
		return request(t, ts, http.MethodPost, "/v1/databases/crm/access/evaluate", token, sample(query))
	}
	for label, doc := range nestedScopeDocuments {
		status, body := ask(nestedScopeCustomersToken, doc)
		if status != http.StatusForbidden || errorCode(t, body) != "forbidden" {
			t.Errorf("%s, read of orders by a token for customers: %d %s", label, status, body)
		}
		for _, token := range []string{nestedScopeBothToken, ownerToken} {
			if status, body := ask(token, doc); status != http.StatusOK {
				t.Errorf("%s, token for both: %d %s", label, status, body)
			}
		}
	}
	if status, body := ask(nestedScopeCustomersToken, "from: {name: customers}\n"); status != http.StatusOK {
		t.Errorf("a document of customers: %d %s", status, body)
	}
}

// TestDTQLDocumentWhoseSourcesCannotBeListedIsRefusedForAScopedToken: the
// capability check needs the collections a document reads. A document the
// parser accepts but whose sources cannot be listed (a chain of derived sources
// inside two nested join trees, past the depth the source walk allows) is
// refused with 400 invalid_dtql before anything is read, for a token that is not
// the owner's. The owner's request is not checked per collection.
func TestDTQLDocumentWhoseSourcesCannotBeListedIsRefusedForAScopedToken(t *testing.T) {
	derived := "{name: customers}"
	for i := 0; i < 14; i++ {
		derived = fmt.Sprintf("{query: {as: d%d, from: %s}}", i, derived)
	}
	doc := "from: {name: customers}\nwhere: {exists: {query: {from: {name: orders, joins: [{from: {name: customers, alias: c2, joins: [{from: " + derived +
		", on: [{op: '==', left: {field: id, source: c2}, right: {field: id, source: d13}}]}]}, on: [{op: '==', left: {field: id, source: orders}, right: {field: id, source: c2}}]}]}}}}\n"
	ts, fake := nestedScopeServer(t, "ingitdb")
	call := func(token string) (int, map[string]any) {
		return send(t, ts, guardCall{method: "POST", path: "/v1/databases/guarded/dtql", body: doc, headers: map[string]string{"Authorization": "Bearer " + token}})
	}
	status, body := call(nestedScopeBothToken)
	detail, _ := body["error"].(map[string]any)
	if status != http.StatusBadRequest || detail["code"] != "invalid_dtql" || fake.queries != 0 {
		t.Fatalf("scoped token: %d %v (%d reads)", status, body, fake.queries)
	}
	if status, body = call(ownerToken); status == http.StatusBadRequest || fake.queries == 0 {
		t.Fatalf("owner: %d %v (%d reads)", status, body, fake.queries)
	}
}
