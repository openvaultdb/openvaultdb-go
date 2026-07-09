package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
)

// tokenCreateRequest is the body for POST /v1/tokens.
type tokenCreateRequest struct {
	Label        string   `json:"label"`
	DatabaseID   string   `json:"databaseId"`
	Capabilities []string `json:"capabilities"`
	ExpiresIn    string   `json:"expiresIn,omitempty"` // Go duration string; omit = never
}

// tokenResponse is the response for POST /v1/tokens (includes secret) and
// individual entries in GET /v1/tokens (secret omitted — Token field is empty).
type tokenResponse struct {
	ID           string     `json:"id"`
	Token        string     `json:"token,omitempty"` // only in create response
	Label        string     `json:"label,omitempty"`
	DatabaseID   string     `json:"databaseId"`
	Capabilities []string   `json:"capabilities"`
	IssuedAt     time.Time  `json:"issuedAt"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
}

func grantToResponse(g *auth.Grant, secret string) tokenResponse {
	caps := make([]string, len(g.Capabilities))
	for i, c := range g.Capabilities {
		caps[i] = c.String()
	}
	r := tokenResponse{
		ID:           g.ID,
		Token:        secret,
		Label:        g.Label,
		DatabaseID:   g.DatabaseID,
		Capabilities: caps,
		IssuedAt:     g.IssuedAt,
		RevokedAt:    g.RevokedAt,
	}
	if !g.ExpiresAt.IsZero() {
		r.ExpiresAt = &g.ExpiresAt
	}
	return r
}

// handleTokensCreate handles POST /v1/tokens (owner only).
func (s *Server) handleTokensCreate(w http.ResponseWriter, r *http.Request) {
	if !s.isOwner(r) {
		writeError(w, http.StatusForbidden, "forbidden", "owner token required")
		return
	}
	var req tokenCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if req.DatabaseID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "databaseId is required")
		return
	}
	if len(req.Capabilities) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "at least one capability is required")
		return
	}
	caps := make([]auth.Capability, 0, len(req.Capabilities))
	for _, cs := range req.Capabilities {
		c, err := auth.ParseCapability(cs)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		caps = append(caps, c)
	}
	var expiresAt time.Time
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid expiresIn duration: "+err.Error())
			return
		}
		expiresAt = time.Now().UTC().Add(d)
	}

	token, err := auth.NewToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to generate token: "+err.Error())
		return
	}
	g := &auth.Grant{
		Label:        req.Label,
		DatabaseID:   req.DatabaseID,
		Capabilities: caps,
		ExpiresAt:    expiresAt,
	}
	if err = s.authCfg.Store.CreateGrant(g, token); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to persist grant: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, grantToResponse(g, token))
}

// handleTokensList handles GET /v1/tokens (owner only).
func (s *Server) handleTokensList(w http.ResponseWriter, r *http.Request) {
	if !s.isOwner(r) {
		writeError(w, http.StatusForbidden, "forbidden", "owner token required")
		return
	}
	grants := s.authCfg.Store.ListGrants()
	tokens := make([]tokenResponse, len(grants))
	for i, g := range grants {
		tokens[i] = grantToResponse(g, "") // no secret
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleTokensRevoke handles DELETE /v1/tokens/{id} (owner only).
func (s *Server) handleTokensRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.isOwner(r) {
		writeError(w, http.StatusForbidden, "forbidden", "owner token required")
		return
	}
	id := r.PathValue("id")
	g, ok := s.authCfg.Store.RevokeGrant(id)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "token not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, grantToResponse(g, ""))
}
