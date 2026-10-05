package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dtql"
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
// plain request, a request that pages a snapshot and a parameterised one. A
// document with a subquery is a relational document, so a caller who passes the
// capability check meets the refusals every relational document meets before a
// read: the paging headers, and an engine the server does not join (firestore, by
// default). Neither depends on the caller, and neither reads.
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
					// A token for both collections, and the owner, are not stopped by the
					// capability check: they read, or meet the refusal of the request itself.
					refusal := ""
					switch {
					case call.name == "snapshot page":
						refusal = "snapshot_unsupported"
					case engine == "firestore":
						refusal = "join_engine_unsupported"
					}
					for _, token := range []string{nestedScopeBothToken, ownerToken} {
						before := fake.queries
						status, body := send(t, ts, with(token))
						detail, _ := body["error"].(map[string]any)
						if refusal != "" {
							if status != http.StatusUnprocessableEntity || detail["code"] != refusal || fake.queries != before {
								t.Errorf("%s %s, token for both: %d %v (%d reads), want a 422 %s and no read", label, call.name, status, body, fake.queries-before, refusal)
							}
							continue
						}
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
// capability check as the root. The mount has access policies, so a token that
// passes the capability check meets the mount's own refusal of a second source
// (422 authorization_unsupported), never a 403.
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
			if status, body := ask(token, doc); status != http.StatusUnprocessableEntity || errorCode(t, body) != "authorization_unsupported" {
				t.Errorf("%s, token for both: %d %s", label, status, body)
			}
		}
	}
	if status, body := ask(nestedScopeCustomersToken, "from: {name: customers}\n"); status != http.StatusOK {
		t.Errorf("a document of customers: %d %s", status, body)
	}
}

// nestedScopeUnlistableDocument is a document of one root collection with an EXISTS
// subquery whose join reads a derived source, whose WHERE nests groups of AND around
// one comparison. The source walk of core.QueryCollections counts a derived source
// on a join edge one level deeper than the name walk of the classifier does, and
// both stop at 16: with 13 groups the classifier accepts the document (and so does
// the single-collection validator) and QueryCollections refuses it; with 14 both
// refuse it.
func nestedScopeUnlistableDocument(groups int) string {
	where := "{op: '==', left: {field: name}, right: {value: x}}"
	for i := 0; i < groups; i++ {
		where = "{and: [" + where + "]}"
	}
	return "from: {name: customers}\nwhere: {exists: {query: {from: {name: orders, joins: [{from: {query: {as: d, from: {name: customers}, where: " + where +
		"}}, on: [{op: '==', left: {field: id, source: orders}, right: {field: id, source: d}}]}]}}}}\n"
}

// nestedScopeRequireUnlistable checks the premise of the tests that use
// nestedScopeUnlistableDocument: the classifier and the single-collection validator
// accept the document, and QueryCollections cannot list its sources.
func nestedScopeRequireUnlistable(t *testing.T, doc string) {
	t.Helper()
	query, err := dtql.Deserialize([]byte(doc))
	if err != nil {
		t.Fatalf("the document does not parse: %v", err)
	}
	if _, err := core.ClassifyDTQL(query); err != nil {
		t.Fatalf("the classifier refuses the document, so it is not the case under test: %v", err)
	}
	if _, _, err := core.ParseDTQL([]byte(doc)); err != nil {
		t.Fatalf("the single-collection validator refuses the document, so it is not the case under test: %v", err)
	}
	if _, err := core.QueryCollections(query); !errors.Is(err, core.ErrInvalidDTQL) {
		t.Fatalf("QueryCollections lists the sources of the document (%v), so it is not the case under test", err)
	}
}

// TestDTQLDocumentTooDeepForTheClassifierIsRefusedForEveryCaller: a document the
// parser accepts but that nests derived sources inside two nested join trees past
// the depth the classifier allows is refused with 400 invalid_dtql before anything
// is read, for a token and for the owner alike: the classifier's refusal comes before
// the capability check.
func TestDTQLDocumentTooDeepForTheClassifierIsRefusedForEveryCaller(t *testing.T) {
	derived := "{name: customers}"
	for i := 0; i < 14; i++ {
		derived = fmt.Sprintf("{query: {as: d%d, from: %s}}", i, derived)
	}
	doc := "from: {name: customers}\nwhere: {exists: {query: {from: {name: orders, joins: [{from: {name: customers, alias: c2, joins: [{from: " + derived +
		", on: [{op: '==', left: {field: id, source: c2}, right: {field: id, source: d13}}]}]}, on: [{op: '==', left: {field: id, source: orders}, right: {field: id, source: c2}}]}]}}}}\n"
	if _, _, err := core.ParseDTQL([]byte(doc)); err != nil {
		t.Fatalf("the single-collection validator refuses the document: %v", err)
	}
	ts, fake := nestedScopeServer(t, "ingitdb")
	for name, token := range map[string]string{"scoped token": nestedScopeBothToken, "owner": ownerToken} {
		status, body := send(t, ts, guardCall{method: "POST", path: "/v1/databases/guarded/dtql", body: doc, headers: map[string]string{"Authorization": "Bearer " + token}})
		detail, _ := body["error"].(map[string]any)
		if status != http.StatusBadRequest || detail["code"] != "invalid_dtql" || fake.queries != 0 {
			t.Fatalf("%s: %d %v (%d reads)", name, status, body, fake.queries)
		}
	}
}

// TestSampleDocumentWhoseSourcesCannotBeListedIsRefusedForAScopedToken: the sample
// route runs a document through the single-collection validator alone and lists
// the collections it reads with core.QueryCollections to check the capability for
// each. A document the classifier accepts and the listing cannot list is refused with
// 400 invalid_dtql for a token that is not the owner's, before the mount is asked;
// the owner's request is not checked per collection and reaches the mount's own
// refusal of a second source (422 authorization_unsupported).
func TestSampleDocumentWhoseSourcesCannotBeListedIsRefusedForAScopedToken(t *testing.T) {
	doc := nestedScopeUnlistableDocument(13)
	nestedScopeRequireUnlistable(t, doc)
	service := server.New("test", map[string]*core.Database{"crm": hiddenSourceDB(t, true)},
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: nestedScopeStore(t, "crm")}))
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	op := writeGuardProtectedOp("update", "/customers", writeGuardProtectedSet("name"))
	data, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeSample, DiagnosticLevel: "ordinary", Operations: []api.Operation{op},
		Sample: &api.Sample{Limit: 1, Query: api.Query{Format: "dtql-yaml", Text: doc}}})
	status, body := request(t, ts, http.MethodPost, "/v1/databases/crm/access/evaluate", nestedScopeBothToken, string(data))
	if status != http.StatusBadRequest || errorCode(t, body) != "invalid_dtql" {
		t.Fatalf("scoped token: %d %s", status, body)
	}
	if status, body = request(t, ts, http.MethodPost, "/v1/databases/crm/access/evaluate", ownerToken, string(data)); status != http.StatusUnprocessableEntity || errorCode(t, body) != "authorization_unsupported" {
		t.Fatalf("owner: %d %s", status, body)
	}
}

// TestDTQLDocumentWhoseSourcesCannotBeListedIsAuthorizedBySourcesTheClassifierLists:
// the per-database endpoint answers a document with a subquery as a relational
// document, whose capability check reads the sources the classifier found, not the
// listing of core.QueryCollections. A document the classifier accepts and the listing
// cannot list is therefore refused for no caller by the listing: a token that lacks
// one of the collections it reads is a 403 before anything is read, and every caller
// that holds them all (a token for both, the owner) reaches the adapter.
func TestDTQLDocumentWhoseSourcesCannotBeListedIsAuthorizedBySourcesTheClassifierLists(t *testing.T) {
	doc := nestedScopeUnlistableDocument(13)
	nestedScopeRequireUnlistable(t, doc)
	ts, fake := nestedScopeServer(t, "ingitdb")
	call := func(token string) (int, map[string]any) {
		return send(t, ts, guardCall{method: "POST", path: "/v1/databases/guarded/dtql", body: doc, headers: map[string]string{"Authorization": "Bearer " + token}})
	}
	status, body := call(nestedScopeCustomersToken)
	detail, _ := body["error"].(map[string]any)
	if status != http.StatusForbidden || detail["code"] != "forbidden" || fake.queries != 0 {
		t.Fatalf("token for customers: %d %v (%d reads)", status, body, fake.queries)
	}
	for name, token := range map[string]string{"token for both": nestedScopeBothToken, "owner": ownerToken} {
		before := fake.queries
		status, body = call(token)
		if status == http.StatusBadRequest || status == http.StatusForbidden || fake.queries == before {
			t.Fatalf("%s: %d %v (%d reads)", name, status, body, fake.queries-before)
		}
	}
}
