package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestTokenAdmin_OwnerCreateListRevoke exercises the full POST/GET/DELETE
// /v1/tokens flow with the owner token.
func TestTokenAdmin_OwnerCreateListRevoke(t *testing.T) {
	ts := startAuthServer(t)

	// POST /v1/tokens — create a never-expiring read-only token.
	createBody := `{"label":"test","databaseId":"dev","capabilities":["records:read"]}`
	status, body := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken, createBody)
	if status != http.StatusCreated {
		t.Fatalf("create token: %d %s", status, body)
	}
	var created struct {
		ID         string     `json:"id"`
		Token      string     `json:"token"`
		Label      string     `json:"label"`
		DatabaseID string     `json:"databaseId"`
		ExpiresAt  *time.Time `json:"expiresAt"`
		RevokedAt  *time.Time `json:"revokedAt"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("create: ID must be set")
	}
	if !strings.HasPrefix(created.Token, "ovdb_") {
		t.Fatalf("create: token must have ovdb_ prefix, got %q", created.Token)
	}
	if created.Label != "test" {
		t.Fatalf("create: label = %q, want test", created.Label)
	}
	if created.DatabaseID != "dev" {
		t.Fatalf("create: databaseId = %q, want dev", created.DatabaseID)
	}
	if created.ExpiresAt != nil {
		t.Fatalf("create: expiresAt should be nil for never-expiring token, got %v", created.ExpiresAt)
	}
	appToken := created.Token

	// The new token can read records.
	if status, body = request(t, ts, http.MethodGet, "/v1/status", appToken, ""); status != http.StatusOK {
		t.Fatalf("app token on status: %d %s", status, body)
	}

	// GET /v1/tokens — list includes our token.
	status, body = request(t, ts, http.MethodGet, "/v1/tokens", ownerToken, "")
	if status != http.StatusOK {
		t.Fatalf("list tokens: %d %s", status, body)
	}
	var list struct {
		Tokens []struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, tok := range list.Tokens {
		if tok.ID == created.ID {
			found = true
		}
		if tok.Token != "" {
			t.Fatal("list must not expose token secrets")
		}
	}
	if !found {
		t.Fatalf("created token %s not in list: %s", created.ID, body)
	}

	// DELETE /v1/tokens/{id} — revoke.
	status, body = request(t, ts, http.MethodDelete, "/v1/tokens/"+created.ID, ownerToken, "")
	if status != http.StatusOK {
		t.Fatalf("revoke: %d %s", status, body)
	}
	var revoked struct {
		RevokedAt *time.Time `json:"revokedAt"`
	}
	if err := json.Unmarshal([]byte(body), &revoked); err != nil {
		t.Fatalf("decode revoke: %v", err)
	}
	if revoked.RevokedAt == nil {
		t.Fatal("revoke: revokedAt must be set in response")
	}

	// Revoked token now returns 401.
	if status, _ = request(t, ts, http.MethodGet, "/v1/status", appToken, ""); status != http.StatusUnauthorized {
		t.Fatalf("revoked token must get 401, got %d", status)
	}

	// Double-revoke is idempotent (200).
	if status, body = request(t, ts, http.MethodDelete, "/v1/tokens/"+created.ID, ownerToken, ""); status != http.StatusOK {
		t.Fatalf("double revoke: %d %s", status, body)
	}
}

// TestTokenAdmin_WithExpiry checks that a token with expiresIn is created with a non-nil expiresAt.
func TestTokenAdmin_WithExpiry(t *testing.T) {
	ts := startAuthServer(t)
	createBody := `{"label":"short","databaseId":"dev","capabilities":["records:read"],"expiresIn":"720h"}`
	status, body := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken, createBody)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	var resp struct {
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ExpiresAt == nil {
		t.Fatal("expiresAt must be set when expiresIn provided")
	}
}

// TestTokenAdmin_AppTokenForbidden verifies non-owner callers cannot use token admin endpoints.
func TestTokenAdmin_AppTokenForbidden(t *testing.T) {
	ts := startAuthServer(t)
	appToken := connectFlow(t, ts, "records:read")

	// Create — 403 for app token.
	if status, _ := request(t, ts, http.MethodPost, "/v1/tokens", appToken,
		`{"label":"x","databaseId":"dev","capabilities":["records:read"]}`); status != http.StatusForbidden {
		t.Fatalf("app token create: want 403, got %d", status)
	}
	// List — 403.
	if status, _ := request(t, ts, http.MethodGet, "/v1/tokens", appToken, ""); status != http.StatusForbidden {
		t.Fatalf("app token list: want 403, got %d", status)
	}
	// Revoke — 403.
	if status, _ := request(t, ts, http.MethodDelete, "/v1/tokens/abc123", appToken, ""); status != http.StatusForbidden {
		t.Fatalf("app token revoke: want 403, got %d", status)
	}
}

// TestTokenAdmin_Unauthenticated verifies unauthenticated callers get 401.
func TestTokenAdmin_Unauthenticated(t *testing.T) {
	ts := startAuthServer(t)
	if status, _ := request(t, ts, http.MethodPost, "/v1/tokens", "",
		`{"databaseId":"dev","capabilities":["records:read"]}`); status != http.StatusUnauthorized {
		t.Fatalf("unauth create: want 401, got %d", status)
	}
	if status, _ := request(t, ts, http.MethodGet, "/v1/tokens", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("unauth list: want 401, got %d", status)
	}
}

// TestTokenAdmin_BadRequest verifies validation error paths.
func TestTokenAdmin_BadRequest(t *testing.T) {
	ts := startAuthServer(t)

	// Missing databaseId.
	if status, _ := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken,
		`{"capabilities":["records:read"]}`); status != http.StatusBadRequest {
		t.Fatalf("missing db: want 400, got %d", status)
	}
	// Unknown capability.
	if status, _ := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken,
		`{"databaseId":"dev","capabilities":["records:explode"]}`); status != http.StatusBadRequest {
		t.Fatalf("bad cap: want 400, got %d", status)
	}
	// Bad expiresIn.
	if status, _ := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken,
		`{"databaseId":"dev","capabilities":["records:read"],"expiresIn":"notaduration"}`); status != http.StatusBadRequest {
		t.Fatalf("bad duration: want 400, got %d", status)
	}
	// Malformed JSON.
	if status, _ := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken, `{not json`); status != http.StatusBadRequest {
		t.Fatalf("bad json: want 400, got %d", status)
	}
}

// TestTokenAdmin_UnknownID verifies DELETE on an unknown ID returns 404.
func TestTokenAdmin_UnknownID(t *testing.T) {
	ts := startAuthServer(t)
	if status, _ := request(t, ts, http.MethodDelete, "/v1/tokens/doesnotexist", ownerToken, ""); status != http.StatusNotFound {
		t.Fatalf("unknown id: want 404, got %d", status)
	}
}

// TestTokenAdmin_AdminTokenCanAccessData verifies an admin-created token
// (not from connect flow) is also enforced by capability middleware.
func TestTokenAdmin_AdminTokenCanAccessData(t *testing.T) {
	ts := startAuthServer(t)
	// Seed a record as owner.
	if status, _ := request(t, ts, http.MethodPut, "/v1/databases/dev/records/notes/n1", ownerToken,
		`{"data":{"text":"hello"}}`); status != http.StatusNoContent {
		t.Fatal("seed failed")
	}

	// Create a read-only token via admin API.
	status, body := request(t, ts, http.MethodPost, "/v1/tokens", ownerToken,
		`{"databaseId":"dev","capabilities":["records:read"]}`)
	if status != http.StatusCreated {
		t.Fatalf("create admin token: %d %s", status, body)
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	appToken := resp.Token

	// Can read.
	if status, _ = request(t, ts, http.MethodGet, "/v1/databases/dev/records/notes/n1", appToken, ""); status != http.StatusOK {
		t.Fatalf("admin token read: want 200, got %d", status)
	}
	// Cannot write (no records:write capability).
	if status, _ = request(t, ts, http.MethodPut, "/v1/databases/dev/records/notes/n2", appToken,
		`{"data":{"x":1}}`); status != http.StatusForbidden {
		t.Fatalf("admin token write: want 403, got %d", status)
	}
}
