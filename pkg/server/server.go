// Package server exposes mounted OpenVaultDB databases over the minimal
// HTTP API documented in docs/api.md.
package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// Server serves one or more mounted databases.
type Server struct {
	version string
	dbs     map[string]*core.Database
}

// New creates a Server over mounted databases keyed by database id.
func New(version string, dbs map[string]*core.Database) *Server {
	return &Server{version: version, dbs: dbs}
}

// Handler builds the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/databases", s.handleDatabases)
	mux.HandleFunc("GET /v1/databases/{db}", s.handleDatabase)
	mux.HandleFunc("GET /v1/databases/{db}/inferred-schema", s.handleInferredSchema)
	mux.HandleFunc("/v1/databases/{db}/records/", s.handleRecord) // GET/HEAD/PUT/POST/PATCH/DELETE
	mux.HandleFunc("POST /v1/databases/{db}/batch", s.handleBatch)
	mux.HandleFunc("POST /v1/databases/{db}/query", s.handleQuery)
	mux.HandleFunc("POST /v1/databases/{db}/dtql", s.handleDTQL)
	return mux
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
	writeJSON(w, http.StatusOK, map[string]any{
		"name":      "OpenVaultDB",
		"version":   s.version,
		"databases": s.databaseIDs(),
	})
}

func (s *Server) handleDatabases(w http.ResponseWriter, r *http.Request) {
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
