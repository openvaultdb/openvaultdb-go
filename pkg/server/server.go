// Package server exposes mounted OpenVaultDB databases over the minimal
// HTTP API documented in docs/api.md.
package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// Server serves one or more mounted databases.
type Server struct {
	version string
	dbs     map[string]*core.Database
	authCfg *auth.Config // nil = auth disabled (local-dev default)
}

// Option configures the Server.
type Option func(*Server)

// WithAuth enables authentication: the connect flow endpoints are served and
// every data/admin request must carry the owner token or a scoped app token.
func WithAuth(cfg *auth.Config) Option {
	return func(s *Server) { s.authCfg = cfg }
}

// New creates a Server over mounted databases keyed by database id.
func New(version string, dbs map[string]*core.Database, opts ...Option) *Server {
	s := &Server{version: version, dbs: dbs}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handler builds the HTTP handler. With auth enabled the mux is wrapped in
// the token-validation middleware (Layer 1); handlers enforce capabilities
// (Layer 2) via authorize().
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openvaultdb", s.handleWellKnown)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/databases", s.handleDatabases)
	mux.HandleFunc("GET /v1/databases/{db}", s.handleDatabase)
	mux.HandleFunc("GET /v1/databases/{db}/inferred-schema", s.handleInferredSchema)
	mux.HandleFunc("/v1/databases/{db}/records/", s.handleRecord) // GET/HEAD/PUT/POST/PATCH/DELETE
	mux.HandleFunc("POST /v1/databases/{db}/batch", s.handleBatch)
	mux.HandleFunc("POST /v1/databases/{db}/query", s.handleQuery)
	mux.HandleFunc("POST /v1/databases/{db}/dtql", s.handleDTQL)
	if s.authCfg == nil {
		return mux
	}
	mux.HandleFunc("GET /authorize", s.handleAuthorizeGet)
	mux.HandleFunc("POST /authorize", s.handleAuthorizePost)
	mux.HandleFunc("POST /token", s.handleToken)
	return s.authCfg.Middleware(mux)
}

// authorize enforces a capability for the request (Layer 2). Always true
// when auth is disabled. Writes the 403 response when denied.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, databaseID, action, collection string) bool {
	if s.authCfg == nil {
		return true
	}
	if auth.FromRequest(r).Allows(databaseID, action, collection) {
		return true
	}
	writeError(w, http.StatusForbidden, "forbidden",
		"token does not grant "+action+" on database "+databaseID)
	return false
}

// isOwner reports whether the request is made with the owner token (or auth
// is disabled, in which case every caller has owner-level access).
func (s *Server) isOwner(r *http.Request) bool {
	if s.authCfg == nil {
		return true
	}
	p := auth.FromRequest(r)
	return p != nil && p.Owner
}

func (s *Server) db(w http.ResponseWriter, r *http.Request) *core.Database {
	id := r.PathValue("db")
	db, ok := s.dbs[id]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "database not found: "+id)
		return nil
	}
	return db
}

func (s *Server) databaseIDs() []string {
	ids := make([]string, 0, len(s.dbs))
	for id := range s.dbs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := map[string]any{
		"name":    "OpenVaultDB",
		"version": s.version,
	}
	// The database list is owner-level information once auth is on.
	if s.isOwner(r) {
		status["databases"] = s.databaseIDs()
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleDatabases(w http.ResponseWriter, r *http.Request) {
	if !s.isOwner(r) {
		writeError(w, http.StatusForbidden, "forbidden", "owner token required")
		return
	}
	type dbInfo struct {
		ID         string `json:"id"`
		Engine     string `json:"engine"`
		SchemaMode string `json:"schemaMode"`
	}
	infos := make([]dbInfo, 0, len(s.dbs))
	for _, id := range s.databaseIDs() {
		db := s.dbs[id]
		infos = append(infos, dbInfo{
			ID:         id,
			Engine:     db.Manifest.Storage.Engine,
			SchemaMode: string(db.Manifest.Database.SchemaMode),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": infos})
}

func (s *Server) handleDatabase(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	if !s.authorize(w, r, db.ID(), auth.CapCollectionsRead, "") {
		return
	}
	collections, err := db.Collections(r.Context())
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          db.ID(),
		"engine":      db.Manifest.Storage.Engine,
		"schemaMode":  string(db.Manifest.Database.SchemaMode),
		"collections": collections,
	})
}

func (s *Server) handleInferredSchema(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	if !s.authorize(w, r, db.ID(), auth.CapSchemaRead, "") {
		return
	}
	snapshot := db.InferredSnapshot()
	if snapshot == nil {
		writeError(w, http.StatusNotFound, "not_found",
			"database "+db.ID()+" is strict; no inferred schema catalogue is maintained")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
