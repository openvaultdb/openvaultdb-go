package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func TestTypedGrantPoliciesAndCurrentMembership(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := `database: {id: crm, schema_mode: strict}
storage: {engine: sqlite, path: data.sqlite}
schemas:
  collections:
    customers:
      fields:
        name: {type: string}
`
	aclWriteFile(t, path, manifest)
	if _, err := mount.File(path); err != nil {
		t.Fatal(err)
	}
	policy := `apiVersion: dtql.org/access/v1
kind: AccessPolicy
metadata: {name: humans}
target: {database: crm}
composition: dalgo-hierarchical-v1
default: deny
ruleSets:
  read:
    - path: /customers
      rules: [{id: read, effect: allow, operations: [query], fields: [name]}]
bindings:
  users: {same-id: [read]}
`
	aclWriteFile(t, filepath.Join(dir, "policy.yaml"), policy)
	aclWriteFile(t, filepath.Join(dir, "membership.yaml"), strings.Replace(strings.Replace(policy, "name: humans", "name: memberships", 1), "users: {same-id: [read]}", "roles: {reader: [read]}", 1))
	aclWriteFile(t, path, manifest+"acl: {enabled: true, realm: local, policies: [policy.yaml, membership.yaml]}\n")
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(filepath.Join(dir, "grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	actor := access.PrincipalRef{Realm: "local", Kind: access.PrincipalKindApplication, ID: "datatug"}
	for _, item := range []struct {
		token, realm string
		kind         access.PrincipalKind
		cap          string
	}{
		{"human", "local", access.PrincipalKindUser, auth.CapRecordsRead},
		{"service", "local", access.PrincipalKindService, auth.CapRecordsRead},
		{"wrong-realm", "other", access.PrincipalKindUser, auth.CapRecordsRead},
		{"narrow", "local", access.PrincipalKindUser, auth.CapSchemaRead},
		{"second-provider", "local", access.PrincipalKindUser, auth.CapRecordsRead},
	} {
		subject := access.PrincipalRef{Realm: item.realm, Kind: item.kind, ID: "same-id"}
		g := &auth.Grant{Subject: &subject, Actor: &actor, DatabaseID: "crm", Capabilities: []auth.Capability{{Action: item.cap, Collection: "customers"}}}
		if err := store.CreateGrant(g, item.token); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	unavailable := false
	revoked := false
	resolve := func(context.Context, access.PrincipalRef) (server.Membership, error) {
		mu.Lock()
		defer mu.Unlock()
		if unavailable {
			return server.Membership{}, errors.New("directory unavailable")
		}
		if revoked {
			return server.Membership{Revision: "2"}, nil
		}
		return server.Membership{Roles: []string{"reader"}, Revision: "1"}, nil
	}
	ts := httptest.NewServer(server.New("test", map[string]*core.Database{"crm": db},
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}),
		server.WithGrantIdentity(server.GrantIdentityConfig{Bootstrap: access.PrincipalRef{Realm: "local", Kind: access.PrincipalKindService, ID: "bootstrap"}, Resolve: resolve}),
	).Handler())
	defer ts.Close()
	query := "from: {name: customers}\ncolumns: [{field: name}]\n"
	provision := `{"databaseId":"crm","capabilities":["records:read:customers"],"subject":{"realm":"local","kind":"user","id":"same-id"},"actor":{"realm":"local","kind":"application","id":"datatug"}}`
	code, payload := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken, provision)
	if code != http.StatusCreated {
		t.Fatalf("owner provision: %d %s", code, payload)
	}
	var issued struct {
		Token   string
		Subject *access.PrincipalRef
		Actor   *access.PrincipalRef
	}
	if err := json.Unmarshal([]byte(payload), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Subject == nil || issued.Actor == nil || issued.Subject.ID != "same-id" || issued.Actor.ID != "datatug" {
		t.Fatalf("binding missing: %s", payload)
	}
	code, payload = request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", issued.Token, query)
	if code != http.StatusOK {
		t.Fatalf("issued delegation: %d %s", code, payload)
	}
	code, _ = request(t, ts, http.MethodPost, "/v1/tokens", "human", provision)
	if code != http.StatusForbidden {
		t.Fatal("ordinary user could provision another subject")
	}

	for token, want := range map[string]int{"human": 200, "second-provider": 200, "service": 403, "wrong-realm": 403, "narrow": 403, ownerToken: 403} {
		status, body := request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", token, query)
		if status != want {
			t.Fatalf("%s: %d %s", token, status, body)
		}
	}
	status, body := request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", "human", query+"roles: [admin]\n")
	if status != http.StatusBadRequest {
		t.Fatalf("caller role injection: %d %s", status, body)
	}
	mu.Lock()
	revoked = true
	mu.Unlock()
	status, _ = request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", "human", query)
	if status != http.StatusForbidden {
		t.Fatal("membership revocation did not take effect on next request")
	}
	mu.Lock()
	unavailable = true
	mu.Unlock()
	status, _ = request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", "human", query)
	if status != http.StatusForbidden {
		t.Fatal("resolver failure did not fail closed")
	}
}
