package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TokenTTL is how long an issued application token stays valid. The spec
// drafts 15-minute app tokens with refresh; the MVP has no refresh tokens
// yet, so it uses the spec's API-key tier (1 hour) — apps re-run the connect
// flow after expiry.
const TokenTTL = time.Hour

// CodeTTL is how long a one-time authorization code stays exchangeable.
const CodeTTL = 5 * time.Minute

// authCode is a pending one-time authorization code (in-memory only:
// codes are ephemeral by design and do not survive restarts).
type authCode struct {
	clientID     string
	redirectURI  string
	databaseID   string
	capabilities []Capability
	expires      time.Time
}

// Store holds issued grants (persisted as JSON with hashed tokens) and
// pending one-time codes (in-memory). Safe for concurrent use.
type Store struct {
	filePath string

	mu         sync.Mutex
	grants     map[string]*Grant // by token hash
	grantsByID map[string]*Grant // by grant ID
	codes      map[string]authCode
}

// OpenStore loads (or initializes) the grants file. Truly expired grants
// (non-zero ExpiresAt in the past) are dropped on load; revoked grants are
// kept for the audit trail.
func OpenStore(filePath string) (*Store, error) {
	s := &Store{
		filePath:   filePath,
		grants:     map[string]*Grant{},
		grantsByID: map[string]*Grant{},
		codes:      map[string]authCode{},
	}
	b, err := os.ReadFile(filePath)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read auth store: %w", err)
	}
	var grants []*Grant
	if err = json.Unmarshal(b, &grants); err != nil {
		return nil, fmt.Errorf("failed to parse auth store %s: %w", filePath, err)
	}
	now := time.Now()
	for _, g := range grants {
		// Synthesize a stable ID for legacy grants that pre-date the ID field.
		if g.ID == "" && len(g.TokenHash) >= 12 {
			g.ID = g.TokenHash[:12]
		}
		// Drop truly expired grants (non-zero expiry in the past).
		// Keep revoked grants (audit trail) and zero-expiry (never expire) grants.
		if g.Expired(now) {
			continue
		}
		s.grants[g.TokenHash] = g
		if g.ID != "" {
			s.grantsByID[g.ID] = g
		}
	}
	return s, nil
}

// PutCode registers a one-time authorization code.
func (s *Store) PutCode(code, clientID, redirectURI, databaseID string, caps []Capability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = authCode{
		clientID:     clientID,
		redirectURI:  redirectURI,
		databaseID:   databaseID,
		capabilities: caps,
		expires:      time.Now().Add(CodeTTL),
	}
}

// ExchangeCode consumes a code (one-time use) and, when it is valid,
// unexpired and bound to the same client, mints and persists a grant.
// Returns the raw bearer token — the only time it ever exists outside the
// client.
func (s *Store) ExchangeCode(code, clientID string) (token string, g *Grant, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[code]
	if ok {
		delete(s.codes, code)
	}
	if !ok || time.Now().After(c.expires) || c.clientID != clientID {
		return "", nil, errors.New("invalid, expired or already used authorization code")
	}
	if token, err = NewToken(); err != nil {
		return "", nil, err
	}
	id, err := NewGrantID()
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	g = &Grant{
		ID:            id,
		TokenHash:     HashToken(token),
		PrincipalType: "application",
		PrincipalID:   c.clientID,
		DatabaseID:    c.databaseID,
		Capabilities:  c.capabilities,
		IssuedAt:      now,
		ExpiresAt:     now.Add(TokenTTL),
	}
	s.grants[g.TokenHash] = g
	s.grantsByID[g.ID] = g
	if err = s.saveLocked(); err != nil {
		delete(s.grants, g.TokenHash)
		delete(s.grantsByID, g.ID)
		return "", nil, err
	}
	return token, g, nil
}

// CreateGrant hashes token, sets IssuedAt, stores and persists the grant.
// The caller must populate g.DatabaseID, g.Capabilities, g.Label, g.ExpiresAt
// (zero = never expires) before calling. On success g.ID, g.TokenHash,
// g.IssuedAt, g.PrincipalType are filled in.
func (s *Store) CreateGrant(g *Grant, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := NewGrantID()
	if err != nil {
		return err
	}
	g.ID = id
	g.TokenHash = HashToken(token)
	g.PrincipalType = "application"
	g.IssuedAt = time.Now().UTC()
	s.grants[g.TokenHash] = g
	s.grantsByID[g.ID] = g
	if err = s.saveLocked(); err != nil {
		delete(s.grants, g.TokenHash)
		delete(s.grantsByID, g.ID)
		return err
	}
	return nil
}

// ListGrants returns all grants (including revoked ones) sorted by IssuedAt
// ascending. Never exposes token secrets.
func (s *Store) ListGrants() []*Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Grant, 0, len(s.grants))
	for _, g := range s.grants {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].IssuedAt.Before(out[j].IssuedAt)
	})
	return out
}

// RevokeGrant sets RevokedAt on the grant with the given ID and persists.
// Returns the updated grant and true on success; nil and false when unknown.
// Revoking an already-revoked grant is idempotent (updates RevokedAt if not set).
func (s *Store) RevokeGrant(id string) (*Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grantsByID[id]
	if !ok {
		return nil, false
	}
	if g.RevokedAt == nil {
		now := time.Now().UTC()
		g.RevokedAt = &now
		_ = s.saveLocked() // best-effort; revocation is in-memory even if persist fails
	}
	return g, true
}

// Lookup resolves a bearer token to its grant, or nil when unknown, expired, or revoked.
func (s *Store) Lookup(token string) *Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[HashToken(token)]
	if !ok || g.Expired(time.Now()) || g.Revoked() {
		return nil
	}
	return g
}

// saveLocked persists grants atomically (temp file + rename). Callers hold mu.
// Revoked grants are kept (audit trail). Truly expired non-revoked grants are pruned.
func (s *Store) saveLocked() error {
	grants := make([]*Grant, 0, len(s.grants))
	now := time.Now()
	for _, g := range s.grants {
		// Keep revoked grants always (audit trail).
		// Prune non-revoked grants that have truly expired.
		if !g.Revoked() && g.Expired(now) {
			continue
		}
		grants = append(grants, g)
	}
	b, err := json.MarshalIndent(grants, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal auth store: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(s.filePath), 0o755); err != nil {
		return fmt.Errorf("failed to create auth store directory: %w", err)
	}
	tmp := s.filePath + ".tmp"
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("failed to write auth store: %w", err)
	}
	if err = os.Rename(tmp, s.filePath); err != nil {
		return fmt.Errorf("failed to replace auth store: %w", err)
	}
	return nil
}
