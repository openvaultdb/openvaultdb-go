// Package server implements the OpenVaultDB local HTTP API defined by
// interface/main.tsp, with the concrete constants from INTEGRATION.md.
package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/openvaultdb/openvaultdb-go/internal/store"
)

// Server holds dependencies for the HTTP handlers.
type Server struct {
	store   *store.Store
	baseURL string
	auth    *authStore
}

// New constructs a Server. baseURL is the externally reachable base URL, used
// to build absolute connect-flow endpoints in ServerInfo.
func New(st *store.Store, baseURL string) *Server {
	return &Server{
		store:   st,
		baseURL: strings.TrimRight(baseURL, "/"),
		auth:    newAuthStore(),
	}
}

// allowedOrigins are the wallet browser origins permitted by CORS
// (INTEGRATION.md). The demo app reaches the server server-side and needs none.
var allowedOrigins = map[string]bool{
	"http://localhost:5000":   true,
	"http://localhost:8787":   true,
	"https://openvaultdb.com": true,
}

// Handler returns the root http.Handler with routing and CORS applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /.well-known/openvaultdb", s.handleServerInfo)

	mux.HandleFunc("GET /vaults", s.handleListVaults)
	mux.HandleFunc("GET /vaults/{vaultId}/namespaces", s.handleListNamespaces)

	mux.HandleFunc("GET /authorize", s.handleAuthorizeForm)
	mux.HandleFunc("POST /authorize", s.handleAuthorizeDecision)
	mux.HandleFunc("POST /token", s.handleToken)

	const recBase = "/vaults/{vaultId}/ns/{ns}/collections/{collection}/records"
	mux.HandleFunc("GET "+recBase, s.handleListRecords)
	mux.HandleFunc("POST "+recBase, s.handleCreateRecord)
	mux.HandleFunc("PATCH "+recBase+"/{id}", s.handleUpdateRecord)
	mux.HandleFunc("DELETE "+recBase+"/{id}", s.handleDeleteRecord)

	return s.withCORS(mux)
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowedOrigins[origin] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Vary", "Origin")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// JSON / error helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}

// bearer extracts the bearer token from the Authorization header.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}
