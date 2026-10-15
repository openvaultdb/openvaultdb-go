package auth

import (
	"path/filepath"
	"testing"

	"github.com/dal-go/dalgo/access"
)

func TestTypedGrantPersistenceAndIsolation(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "grants.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	subject := access.PrincipalRef{Realm: "local", Kind: access.PrincipalKindUser, ID: "stable-user"}
	actor := access.PrincipalRef{Realm: "local", Kind: access.PrincipalKindApplication, ID: "datatug"}
	g := &Grant{Subject: &subject, Actor: &actor, DatabaseID: "crm", Capabilities: []Capability{{Action: CapRecordsRead}}}
	if err := store.CreateGrant(g, "token"); err != nil {
		t.Fatal(err)
	}
	subject.ID = "mutated"
	g.Capabilities[0].Action = CapRecordsWrite
	got := store.Lookup("token")
	if got.Subject.ID != "stable-user" || got.Capabilities[0].Action != CapRecordsRead {
		t.Fatal("caller mutated stored delegation")
	}
	got.Actor.ID = "mutated"
	if store.Lookup("token").Actor.ID != "datatug" {
		t.Fatal("lookup leaked mutable store")
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Lookup("token").Subject.ID != "stable-user" {
		t.Fatal("identity not persisted")
	}
	if err := reopened.CreateGrant(&Grant{Subject: &actor}, "partial"); err == nil {
		t.Fatal("partial delegation accepted")
	}
	if _, ok := reopened.RevokeGrant(g.ID); !ok {
		t.Fatal("revoke failed")
	}
	if reopened.Lookup("token") != nil {
		t.Fatal("revoked identity credential usable")
	}
}

func TestGrantValidateIdentityEdgeCases(t *testing.T) {
	t.Parallel()
	var nilGrant *Grant
	if err := nilGrant.ValidateIdentity(); err == nil {
		t.Fatal("expected error for nil grant")
	}
	validUser := access.PrincipalRef{Realm: "local", Kind: access.PrincipalKindUser, ID: "u1"}
	validApp := access.PrincipalRef{Realm: "local", Kind: access.PrincipalKindApplication, ID: "a1"}
	invalidRef := access.PrincipalRef{Realm: "", Kind: access.PrincipalKindUser, ID: "u1"}

	if err := (&Grant{Subject: &invalidRef, Actor: &validApp}).ValidateIdentity(); err == nil {
		t.Fatal("expected error for invalid subject")
	}
	if err := (&Grant{Subject: &validUser, Actor: &invalidRef}).ValidateIdentity(); err == nil {
		t.Fatal("expected error for invalid actor")
	}
	if err := (&Grant{Subject: &validUser, Actor: &validUser}).ValidateIdentity(); err == nil {
		t.Fatal("expected error when actor is a user")
	}
	if cloneGrant(nil) != nil {
		t.Fatal("expected nil from cloneGrant(nil)")
	}
}
