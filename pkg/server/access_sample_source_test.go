package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/policystore"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// TestSampleIsAnsweredAlikeWhileThePolicySourceIsUnavailable: while the owner's
// policy layer cannot be read, a sample of a table the database does not declare
// gets the answer a sample of a declared table gets, whichever table that is: the
// layer is read before the tables of the query are looked at.
func TestSampleIsAnsweredAlikeWhileThePolicySourceIsUnavailable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	base := "database: {id: crm, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n" +
		"    customers:\n      fields:\n        name: {type: string}\n        country: {type: string}\n" +
		"    orders:\n      fields:\n        total: {type: string}\n"
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
	if _, err = store.Activate(ctx, "", []access.DTQLDocument{doc}); err != nil {
		t.Fatal(err)
	}
	aclWriteFile(t, path, base+"acl: {enabled: true}\nacl_store: {path: owner}\n")
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := server.New("test", map[string]*core.Database{"crm": db},
		server.WithAuth(&auth.Config{OwnerToken: ownerToken}),
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}))
	t.Cleanup(service.CloseSnapshots)
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)

	sample := func(table string) hiddenSourceAnswer {
		op := writeGuardProtectedOp("update", "/"+table, writeGuardProtectedSet("name"))
		data, _ := json.Marshal(api.Request{APIVersion: az.APIVersion, Mode: az.ModeSample, DiagnosticLevel: "ordinary", Operations: []api.Operation{op},
			Sample: &api.Sample{Limit: 1, Query: api.Query{Format: "dtql-yaml", Text: "from: {name: '" + table + "'}\n"}}})
		return hiddenSourceAsk(t, ts, ownerToken, "POST", "/v1/databases/crm/access/evaluate", string(data), map[string]string{"Content-Type": "application/json"})
	}
	// The policy source is readable: an undeclared table is answered as a declared
	// table the policy hides.
	if ghost, orders := sample("ghost"), sample("orders"); ghost.status != orders.status || strings.ReplaceAll(ghost.body, "ghost", "orders") != orders.body {
		t.Fatalf("policy source readable: undeclared\n  %d %s\nhidden by the policy\n  %d %s", ghost.status, ghost.body, orders.status, orders.body)
	}

	// The committed generation is unreadable from here on.
	if err = os.WriteFile(filepath.Join(dir, "owner", "active.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ReloadPolicies(ctx); err == nil {
		t.Fatal("the unreadable generation was loaded")
	}
	unavailable := sample("customers")
	if unavailable.status != http.StatusOK || strings.Contains(unavailable.body, `"result":"allow"`) {
		t.Fatalf("a sample while the policy source is unavailable: %d %s", unavailable.status, unavailable.body)
	}
	for _, table := range []string{"ghost", "orders"} {
		if got := sample(table); got.status != unavailable.status || strings.ReplaceAll(got.body, table, "customers") != unavailable.body {
			t.Errorf("sample of %s\n  %d %s\nsample of a declared table\n  %d %s", table, got.status, got.body, unavailable.status, unavailable.body)
		}
	}
}
