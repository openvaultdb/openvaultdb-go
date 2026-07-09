package auth

import (
	"encoding/json"
	"fmt"
	"os"
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

// TestPrincipalAllows_ServerLevel covers grants with an empty DatabaseID
// (server-level, e.g. databases:create) and the interaction between
// db-scoped grants and server-level checks.
func TestPrincipalAllows_ServerLevel(t *testing.T) {
	createOnly := &Principal{Grant: &Grant{
		DatabaseID:   "", // server-level
		Capabilities: []Capability{{Action: CapDatabasesCreate}},
	}}
	serverWideRead := &Principal{Grant: &Grant{
		DatabaseID:   "", // server-level grant WITH records caps: matches any db (owner's choice)
		Capabilities: []Capability{{Action: CapRecordsRead}},
	}}
	dbScopedCreate := &Principal{Grant: &Grant{
		DatabaseID:   "db1", // concrete db + create cap: must NOT pass a server-level check
		Capabilities: []Capability{{Action: CapDatabasesCreate}},
	}}

	for _, tc := range []struct {
		name                   string
		p                      *Principal
		db, action, collection string
		want                   bool
	}{
		{"create-db token allows server-level create", createOnly, "", CapDatabasesCreate, "", true},
		{"create-db token cannot read records", createOnly, "db1", CapRecordsRead, "notes", false},
		{"create-db token cannot write records", createOnly, "db1", CapRecordsWrite, "notes", false},
		{"create-db token cannot list collections", createOnly, "db1", CapCollectionsRead, "", false},
		{"server-wide read matches any db", serverWideRead, "db1", CapRecordsRead, "notes", true},
		{"server-wide read matches another db", serverWideRead, "db2", CapRecordsRead, "x", true},
		{"server-wide read cannot create", serverWideRead, "", CapDatabasesCreate, "", false},
		{"db-scoped create fails server-level check", dbScopedCreate, "", CapDatabasesCreate, "", false},
		{"db-scoped create matches its own db only", dbScopedCreate, "db1", CapDatabasesCreate, "", true},
	} {
		if got := tc.p.Allows(tc.db, tc.action, tc.collection); got != tc.want {
			t.Errorf("%s: Allows(%q,%q,%q) = %v, want %v", tc.name, tc.db, tc.action, tc.collection, got, tc.want)
		}
	}
}

func TestParseCapability_DatabasesCreate(t *testing.T) {
	c, err := ParseCapability("databases:create")
	if err != nil {
		t.Fatalf("ParseCapability(databases:create): %v", err)
	}
	if c.Action != CapDatabasesCreate || c.Collection != "" {
		t.Fatalf("ParseCapability(databases:create) = %+v", c)
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
	if grant.ID == "" {
		t.Fatal("ExchangeCode must populate grant ID")
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

	// Expired grants are dropped on Lookup.
	g := s2.Lookup(token)
	g.ExpiresAt = time.Now().Add(-time.Minute)
	if s2.Lookup(token) != nil {
		t.Fatal("expired grant must not resolve")
	}
}

// TestStore_BackwardCompatLoad ensures grants persisted without the ID field
// (old format) load correctly: a stable synthetic ID is derived from the hash.
func TestStore_BackwardCompatLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	// Write legacy grant JSON without id/label/revokedAt fields.
	legacy := `[{
		"tokenHash": "abc123def456abc123def456abc123de",
		"principalType": "application",
		"principalId": "old-app",
		"databaseId": "mydb",
		"capabilities": [{"action": "records:read"}],
		"issuedAt": "2026-01-01T00:00:00Z",
		"expiresAt": "2099-01-01T00:00:00Z"
	}]`
	if err := writeFileAtomic(path, []byte(legacy)); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore with legacy JSON: %v", err)
	}
	grants := s.ListGrants()
	if len(grants) != 1 {
		t.Fatalf("want 1 grant, got %d", len(grants))
	}
	if grants[0].ID == "" {
		t.Fatal("legacy grant must get a synthesized ID on load")
	}
	// Synthesized ID = first 12 chars of token hash.
	if want := "abc123def456"; grants[0].ID != want {
		t.Errorf("synthesized ID = %q, want %q", grants[0].ID, want)
	}
}

// writeFileAtomic is a helper used only in tests.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := writeFile(tmp, data); err != nil {
		return err
	}
	return rename(tmp, path)
}

// TestStore_ZeroExpiryNeverExpires checks that a grant with zero ExpiresAt
// never expires and is correctly loaded from disk.
func TestStore_ZeroExpiryNeverExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	g := &Grant{
		DatabaseID:   "db1",
		Capabilities: []Capability{{Action: CapRecordsRead}},
		// ExpiresAt left as zero value = never expires
	}
	if err = s.CreateGrant(g, tok); err != nil {
		t.Fatal(err)
	}
	if g.ExpiresAt != (time.Time{}) {
		t.Fatalf("ExpiresAt should remain zero, got %v", g.ExpiresAt)
	}
	// Lookup must succeed now.
	if s.Lookup(tok) == nil {
		t.Fatal("zero-expiry grant must resolve")
	}
	// Simulate time passing far into the future.
	far := time.Now().Add(100 * 365 * 24 * time.Hour)
	if g.Expired(far) {
		t.Fatal("zero-expiry grant must not expire even far in the future")
	}
	// Reload from disk: grant must survive.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Lookup(tok) == nil {
		t.Fatal("zero-expiry grant must survive reload")
	}
}

// TestStore_RevokedLookupRejected checks that a revoked token cannot be used.
func TestStore_RevokedLookupRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	g := &Grant{
		DatabaseID:   "db1",
		Capabilities: []Capability{{Action: CapRecordsRead}},
	}
	if err = s.CreateGrant(g, tok); err != nil {
		t.Fatal(err)
	}
	// Token resolves before revocation.
	if s.Lookup(tok) == nil {
		t.Fatal("must resolve before revocation")
	}
	// Revoke.
	rg, ok := s.RevokeGrant(g.ID)
	if !ok {
		t.Fatal("RevokeGrant: not found")
	}
	if rg.RevokedAt == nil {
		t.Fatal("RevokedAt must be set after revocation")
	}
	// Token must no longer resolve.
	if s.Lookup(tok) != nil {
		t.Fatal("revoked grant must not resolve")
	}
	// Revoking again is idempotent (200 / true, not error).
	if _, ok2 := s.RevokeGrant(g.ID); !ok2 {
		t.Fatal("double revocation must succeed (idempotent)")
	}
}

// TestStore_CreateListRevokePersistence is an end-to-end round-trip.
func TestStore_CreateListRevokePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for i := 0; i < 3; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		g := &Grant{
			Label:        fmt.Sprintf("token-%d", i),
			DatabaseID:   "db1",
			Capabilities: []Capability{{Action: CapRecordsRead}},
		}
		if err = s.CreateGrant(g, tok); err != nil {
			t.Fatalf("CreateGrant %d: %v", i, err)
		}
		tokens = append(tokens, tok)
	}
	grants := s.ListGrants()
	if len(grants) != 3 {
		t.Fatalf("ListGrants: want 3, got %d", len(grants))
	}
	// Revoke the second token.
	id := grants[1].ID
	if _, ok := s.RevokeGrant(id); !ok {
		t.Fatal("revoke failed")
	}
	// Reload and check persistence.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// All 3 grants persisted (revoked one kept for audit).
	grants2 := s2.ListGrants()
	if len(grants2) != 3 {
		t.Fatalf("after reload: want 3 grants (revoked kept), got %d", len(grants2))
	}
	// Token 1 resolves, token 2 (revoked) does not, token 3 resolves.
	if s2.Lookup(tokens[0]) == nil {
		t.Fatal("token 0 must resolve")
	}
	if s2.Lookup(tokens[1]) != nil {
		t.Fatal("revoked token must not resolve after reload")
	}
	if s2.Lookup(tokens[2]) == nil {
		t.Fatal("token 2 must resolve")
	}
	// Unknown id returns false.
	if _, ok := s2.RevokeGrant("nonexistent"); ok {
		t.Fatal("revoking unknown ID must return false")
	}
}

// writeFile / rename are thin wrappers so tests can write files without
// depending on os directly (keeps the test helpers internal).
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}

func rename(old, new string) error { return os.Rename(old, new) }

// ensure imported packages are used
var (
	_ = json.Marshal
	_ = fmt.Sprintf
)
