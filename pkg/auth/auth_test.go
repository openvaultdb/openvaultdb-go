package auth

import (
	"path/filepath"
	"testing"
	"time"
)

func TestParseCapability(t *testing.T) {
	for _, tc := range []struct {
		in         string
		wantErr    bool
		action     string
		collection string
	}{
		{in: "records:read", action: "records:read"},
		{in: "records:write:contacts", action: "records:write", collection: "contacts"},
		{in: "records:delete", action: "records:delete"},
		{in: "collections:read", action: "collections:read"},
		{in: "schema:read", action: "schema:read"},
		{in: "records", wantErr: true},
		{in: "records:drop", wantErr: true},
		{in: "records:read:", wantErr: true},
		{in: "records:read:a:b", wantErr: true},
		{in: "grants:manage", wantErr: true}, // not in the MVP surface
	} {
		c, err := ParseCapability(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseCapability(%q): want error, got %+v", tc.in, c)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseCapability(%q): %v", tc.in, err)
			continue
		}
		if c.Action != tc.action || c.Collection != tc.collection {
			t.Errorf("ParseCapability(%q) = %+v", tc.in, c)
		}
		if c.String() != tc.in {
			t.Errorf("round-trip: %q → %q", tc.in, c.String())
		}
	}
}

func TestPrincipalAllows(t *testing.T) {
	grant := &Grant{
		DatabaseID: "db1",
		Capabilities: []Capability{
			{Action: CapRecordsRead}, // unscoped read
			{Action: CapRecordsWrite, Collection: "contacts"},
		},
	}
	app := &Principal{Grant: grant}
	owner := &Principal{Owner: true}

	for _, tc := range []struct {
		name                   string
		p                      *Principal
		db, action, collection string
		want                   bool
	}{
		{"owner anything", owner, "any", CapRecordsDelete, "x", true},
		{"unscoped read any collection", app, "db1", CapRecordsRead, "lists", true},
		{"unscoped read db-level", app, "db1", CapRecordsRead, "", true},
		{"scoped write matching", app, "db1", CapRecordsWrite, "contacts", true},
		{"scoped write other collection", app, "db1", CapRecordsWrite, "lists", false},
		{"scoped write db-level denied", app, "db1", CapRecordsWrite, "", false},
		{"wrong database", app, "db2", CapRecordsRead, "lists", false},
		{"ungranted action", app, "db1", CapRecordsDelete, "contacts", false},
		{"nil principal", nil, "db1", CapRecordsRead, "x", false},
	} {
		if got := tc.p.Allows(tc.db, tc.action, tc.collection); got != tc.want {
			t.Errorf("%s: Allows(%q,%q,%q) = %v, want %v", tc.name, tc.db, tc.action, tc.collection, got, tc.want)
		}
	}
}

func TestStore_CodeExchangeAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	caps := []Capability{{Action: CapRecordsRead}}

	s.PutCode("code1", "app1", "http://localhost/cb", "db1", caps)
	token, grant, err := s.ExchangeCode("code1", "app1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if grant.DatabaseID != "db1" || grant.PrincipalID != "app1" {
		t.Fatalf("grant = %+v", grant)
	}
	if grant.TokenHash == token || grant.TokenHash != HashToken(token) {
		t.Fatal("stored hash must be the SHA-256 of the token, not the token")
	}

	// One-time use.
	if _, _, err = s.ExchangeCode("code1", "app1"); err == nil {
		t.Fatal("code reuse must fail")
	}
	// Wrong client.
	s.PutCode("code2", "app1", "http://localhost/cb", "db1", caps)
	if _, _, err = s.ExchangeCode("code2", "evil"); err == nil {
		t.Fatal("client mismatch must fail")
	}

	// Lookup resolves the raw token; unknown tokens do not.
	if g := s.Lookup(token); g == nil || g.PrincipalID != "app1" {
		t.Fatalf("Lookup = %+v", g)
	}
	if s.Lookup("ovdb_nope") != nil {
		t.Fatal("unknown token must not resolve")
	}

	// Persistence: a fresh store loads the grant from disk.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if g := s2.Lookup(token); g == nil {
		t.Fatal("grant must survive reload")
	}

	// Expired grants are dropped.
	g := s2.Lookup(token)
	g.ExpiresAt = time.Now().Add(-time.Minute)
	if s2.Lookup(token) != nil {
		t.Fatal("expired grant must not resolve")
	}
}
