package server

import "net/http"

// handleServerInfo implements GET /.well-known/openvaultdb (ServerInfo).
func (s *Server) handleServerInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":              "openvaultdb-local",
		"protocol":          "openvaultdb/0.1",
		"authorizeEndpoint": s.baseURL + "/authorize",
		"tokenEndpoint":     s.baseURL + "/token",
	})
}

// requireOwner enforces the owner bearer token. Returns false (and writes an
// error) when the token is missing or wrong.
func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request) bool {
	if bearer(r) != s.store.OwnerToken() {
		writeError(w, http.StatusUnauthorized, "unauthorized", "owner token required")
		return false
	}
	return true
}

// handleListVaults implements GET /vaults (owner token).
func (s *Server) handleListVaults(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.store.Vaults())
}

// handleListNamespaces implements GET /vaults/{vaultId}/namespaces (owner token).
func (s *Server) handleListNamespaces(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}
	vaultID := r.PathValue("vaultId")
	if _, ok := s.store.Vault(vaultID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "vault not found")
		return
	}

	type nsOut struct {
		ID          string   `json:"id"`
		Owner       string   `json:"owner"`
		Collections []string `json:"collections"`
	}
	out := []nsOut{}
	for _, ns := range s.store.Namespaces() {
		out = append(out, nsOut{ID: ns.ID, Owner: ns.Owner, Collections: ns.Collections})
	}
	writeJSON(w, http.StatusOK, out)
}
