// Package server exposes mounted OpenVaultDB databases over the minimal
// HTTP API documented in docs/api.md.
package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// Server serves one or more mounted databases.
type Server struct {
	version string

	mu  sync.RWMutex // guards dbs — databases can be mounted at runtime
	dbs map[string]*core.Database

	createMu sync.Mutex // serializes runtime database creation end-to-end

	authCfg *auth.Config // nil = auth disabled (local-dev default)
	corsCfg *CORSConfig  // nil = CORS disabled (no headers added)
	dataDir string       // "" = runtime database creation disabled
}

// Option configures the Server.
type Option func(*Server)

// WithAuth enables authentication: the connect flow endpoints are served and
// every data/admin request must carry the owner token or a scoped app token.
func WithAuth(cfg *auth.Config) Option {
	return func(s *Server) { s.authCfg = cfg }
}

// WithCORS configures CORS header injection for browser clients.
// A nil cfg disables CORS entirely (zero behavior change, the default).
func WithCORS(cfg *CORSConfig) Option {
	return func(s *Server) { s.corsCfg = cfg }
}

// WithDataDir enables runtime database creation (POST /v1/databases):
// each created database gets a manifest YAML plus an inGitDB data directory
// under dir, so a restart rescan (mount.Dir) remounts them.
func WithDataDir(dir string) Option {
	return func(s *Server) { s.dataDir = dir }
}

// New creates a Server over mounted databases keyed by database id.
func New(version string, dbs map[string]*core.Database, opts ...Option) *Server {
	s := &Server{version: version, dbs: dbs}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handler builds the HTTP handler. Middleware order (outermost first):
//  1. CORS (when --cors is set) — short-circuits preflight OPTIONS before auth
//  2. Auth (when --auth is set) — Layer-1 token validation
//  3. Per-handler capability checks — Layer-2
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openvaultdb", s.handleWellKnown)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/databases", s.handleDatabases)
	mux.HandleFunc("POST /v1/databases", s.handleDatabaseCreate)
	mux.HandleFunc("GET /v1/databases/{db}", s.handleDatabase)
	mux.HandleFunc("GET /v1/databases/{db}/inferred-schema", s.handleInferredSchema)
	mux.HandleFunc("/v1/databases/{db}/records/", s.handleRecord) // GET/HEAD/PUT/POST/PATCH/DELETE
	mux.HandleFunc("POST /v1/databases/{db}/batch", s.handleBatch)
	mux.HandleFunc("POST /v1/databases/{db}/query", s.handleQuery)
	mux.HandleFunc("POST /v1/databases/{db}/dtql", s.handleDTQL)
	if s.authCfg != nil {
		mux.HandleFunc("GET /authorize", s.handleAuthorizeGet)
		mux.HandleFunc("POST /authorize", s.handleAuthorizePost)
		mux.HandleFunc("POST /token", s.handleToken)
		mux.HandleFunc("POST /v1/tokens", s.handleTokensCreate)
		mux.HandleFunc("GET /v1/tokens", s.handleTokensList)
		mux.HandleFunc("DELETE /v1/tokens/{id}", s.handleTokensRevoke)
	}

	var h http.Handler = mux
	if s.authCfg != nil {
		h = s.authCfg.Middleware(h)
	}
	if s.corsCfg != nil {
		h = corsMiddleware(s.corsCfg, h)
	}
	return h
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
	db := s.getDB(id)
	if db == nil {
		writeError(w, http.StatusNotFound, "not_found", "database not found: "+id)
		return nil
	}
	return db
}

// getDB returns the mounted database with the given id, or nil.
func (s *Server) getDB(id string) *core.Database {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dbs[id]
}

func (s *Server) databaseIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	ids := s.databaseIDs()
	infos := make([]dbInfo, 0, len(ids))
	for _, id := range ids {
		db := s.getDB(id)
		if db == nil {
			continue
		}
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
