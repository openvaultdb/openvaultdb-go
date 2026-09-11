package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/policystore"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func TestGenerationPoliciesReloadThroughHTTP(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	base := `database: {id: crm, schema_mode: strict}
storage: {engine: sqlite, path: data.sqlite}
schemas:
  collections:
    customers:
      fields:
        name: {type: string}
        country: {type: string}
`
	aclWriteFile(t, path, base)
	if _, err := mount.File(path); err != nil {
		t.Fatal(err)
	}
	store, err := policystore.Open(filepath.Join(dir, "owner"), policystore.Owner{Enabled: true, Database: "crm"})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := access.ParseDTQLPolicy([]byte(aclPolicy("upper", "country", "IE")))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Activate(ctx, "", []access.DTQLDocument{doc})
	if err != nil {
		t.Fatal(err)
	}
	aclWriteFile(t, path, base+"acl: {enabled: true}\nacl_store: {path: owner}\n")
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(db *core.Database) *httptest.Server {
		return httptest.NewServer(server.New("test", map[string]*core.Database{"crm": db},
			server.WithAuth(&auth.Config{OwnerToken: ownerToken}),
			server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
				return access.Principal{Roles: []string{"reader"}}, nil
			}),
		).Handler())
	}
	ts := serve(db)
	defer ts.Close()
	check := func(ts *httptest.Server, want int) {
		t.Helper()
		status, body := request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", ownerToken, "from: {name: customers}\ncolumns: [{field: name}]\n")
		if status != want {
			t.Fatalf("wanted %d got %d: %s", want, status, body)
		}
	}
	check(ts, 200)
	doc.Execution = &access.ExecutionGate{Allow: []access.ExecutionEntry{}}
	second, err := db.PublishPolicies(ctx, first.Revision, []access.DTQLDocument{doc})
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision == first.Revision {
		t.Fatal("revision did not change")
	}
	check(ts, 403) // Existing handle uses the new owner snapshot.
	remounted, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	ts2 := serve(remounted)
	defer ts2.Close()
	check(ts2, 403)
	doc.Execution = nil
	if _, err := store.Activate(ctx, second.Revision, []access.DTQLDocument{doc}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReloadPolicies(ctx); err != nil {
		t.Fatal(err)
	}
	check(ts, 200)
}
