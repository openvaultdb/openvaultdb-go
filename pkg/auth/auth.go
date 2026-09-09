// Package auth implements the OpenVaultDB auth MVP: an OAuth-style connect
// flow (consent → one-time code → scoped bearer token), capability grants
// persisted with hashed tokens, and HTTP middleware enforcing them.
//
// The capability model follows the draft taxonomy in the spec hub
// (openvaultdb/openvaultdb spec/api/auth.md): namespaced capability strings
// such as "records:read", optionally collection-scoped as
// "records:read:contacts" (the spec's open question answered pragmatically —
// scoping is per collection, evaluated at enforcement time). Tokens are
// opaque bearer strings; only their SHA-256 lands on disk. The full JWT
// claim structure, refresh tokens, OIDC and passkeys stay with the spec
// (pending its architecture/security review) — this MVP keeps the wire
// surface small enough to swap those in without breaking applications.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Actions an application capability can name (subset of the spec taxonomy
// that is meaningful against the MVP API surface).
const (
	CapRecordsRead     = "records:read"   // GET/HEAD records, /query, /dtql
	CapRecordsWrite    = "records:write"  // PUT/POST/PATCH records, batch set/insert/update
	CapRecordsDelete   = "records:delete" // DELETE records, batch delete
	CapCollectionsRead = "collections:read"
	CapSchemaRead      = "schema:read"      // inferred-schema endpoint
	CapDatabasesCreate = "databases:create" // POST /v1/databases (server-level)
)

var knownCapabilities = map[string]bool{
	CapRecordsRead:     true,
	CapRecordsWrite:    true,
	CapRecordsDelete:   true,
	CapCollectionsRead: true,
	CapSchemaRead:      true,
	CapDatabasesCreate: true,
}

// Capability is one parsed grant entry: an action, optionally scoped to a
// single collection ("" = all collections of the granted database).
type Capability struct {
	Action     string `json:"action"`
	Collection string `json:"collection,omitempty"`
}

// ParseCapability parses "records:read" or "records:read:contacts".
func ParseCapability(s string) (Capability, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return Capability{}, fmt.Errorf("invalid capability %q (want action or action:collection)", s)
	}
	action := parts[0] + ":" + parts[1]
	if !knownCapabilities[action] {
		return Capability{}, fmt.Errorf("unknown capability %q", action)
	}
	c := Capability{Action: action}
	if len(parts) == 3 {
		if parts[2] == "" {
			return Capability{}, fmt.Errorf("invalid capability %q: empty collection scope", s)
		}
		c.Collection = parts[2]
	}
	return c, nil
}

// ParseCapabilities parses a comma-separated capability list.
func ParseCapabilities(csv string) ([]Capability, error) {
	var caps []Capability
	for _, s := range strings.Split(csv, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		c, err := ParseCapability(s)
		if err != nil {
			return nil, err
		}
		caps = append(caps, c)
	}
	if len(caps) == 0 {
		return nil, fmt.Errorf("at least one capability is required")
	}
	return caps, nil
}

// String renders the capability back to its wire form.
func (c Capability) String() string {
	if c.Collection == "" {
		return c.Action
	}
	return c.Action + ":" + c.Collection
}

// Grant is one issued application token: the token itself is NOT stored —
// only its SHA-256 hex — so the grants file never holds usable credentials.
type Grant struct {
	ID            string       `json:"id"`              // short random identifier (8 hex bytes)
	Label         string       `json:"label,omitempty"` // human display name
	TokenHash     string       `json:"tokenHash"`
	PrincipalType string       `json:"principalType"`        // "application" (MVP)
	PrincipalID   string       `json:"principalId"`          // client_id
	DatabaseID    string       `json:"databaseId,omitempty"` // "" = server-level grant (e.g. databases:create)
	Capabilities  []Capability `json:"capabilities"`
	IssuedAt      time.Time    `json:"issuedAt"`
	ExpiresAt     time.Time    `json:"expiresAt,omitempty"` // zero = never expires
	RevokedAt     *time.Time   `json:"revokedAt,omitempty"`
}

// Expired reports whether the grant has reached its expiry. A zero ExpiresAt never expires.
func (g *Grant) Expired(now time.Time) bool {
	return !g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt)
}

// Revoked reports whether the grant has been revoked.
func (g *Grant) Revoked() bool { return g.RevokedAt != nil }

// NewGrantID generates a short random grant identifier (8 random hex bytes = 16 hex chars).
func NewGrantID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate grant id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Principal is the authenticated caller attached to a request context.
type Principal struct {
	Owner bool   // owner token: full access to everything
	Grant *Grant // application grant when Owner is false
}

// Allows reports whether the principal may perform action on the collection
// of the database. Owner allows everything. An empty collection means the
// action targets the database as a whole (listing collections, queries whose
// target collection cannot be determined) — that requires an UNSCOPED grant
// entry, so collection-scoped grants never leak beyond their collection.
//
// Database matching is on the GRANT side only: a grant with an empty
// DatabaseID is SERVER-LEVEL and matches any requested database (including
// the empty requested id used for server-level checks such as
// databases:create). A grant with a concrete DatabaseID matches only that
// exact database — it never matches a server-level check, because the
// requested "" differs from its concrete id.
func (p *Principal) Allows(databaseID, action, collection string) bool {
	if p == nil {
		return false
	}
	if p.Owner {
		return true
	}
	g := p.Grant
	if g == nil || (g.DatabaseID != "" && g.DatabaseID != databaseID) {
		return false
	}
	for _, c := range g.Capabilities {
		if c.Action != action {
			continue
		}
		if c.Collection == "" || c.Collection == collection {
			return true
		}
	}
	return false
}

// NewToken mints a cryptographically random opaque bearer token.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}
	return "ovdb_" + hex.EncodeToString(b), nil
}

// HashToken returns the SHA-256 hex of a token — the only form persisted.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
