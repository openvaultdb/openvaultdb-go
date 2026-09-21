package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/record"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

const maxQueryRequestBytes = 1 << 20

// handleRead is the URL-query form of a record read. key is a complete,
// escaped record key path (for example, "contacts/c1").
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	s.setReadCacheSafety(w, r, db)
	key, err := parseKeyPath(r.URL.Query().Get("key"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return
	}
	if !s.authorize(w, r, db.ID(), auth.CapRecordsRead, core.RootCollection(key)) {
		return
	}
	s.readRecord(w, r, db, key)
}

// parseRecordKey extracts the record key from the escaped URL path so that
// percent-encoded characters inside IDs (dal.EscapeID encodes `. $ # [ ] /`)
// survive as data rather than path separators. Segments are unescaped
// individually and validated after decoding (core.ValidateSegment: no empty,
// "." or ".." segments, no decoded "../" components, no control characters —
// path traversal safety on every engine). Errors wrap core.ErrInvalidKey.
func parseRecordKey(r *http.Request) (*record.Key, error) {
	escaped := r.URL.EscapedPath()
	idx := strings.Index(escaped, "/records/")
	if idx < 0 {
		return nil, fmt.Errorf("%w: record key missing in path", core.ErrInvalidKey)
	}
	raw := strings.TrimSuffix(escaped[idx+len("/records/"):], "/")
	if raw == "" {
		return nil, fmt.Errorf("%w: record key missing in path", core.ErrInvalidKey)
	}
	return parseKeyPath(raw)
}

// parseKeyPath parses a dal-escaped key path ("collection/id[/sub/id...]")
// into a dalgo key.
func parseKeyPath(raw string) (*record.Key, error) {
	return core.ParseKeyPath(raw)
}

func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	key, err := parseRecordKey(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
		return
	}
	ctx := r.Context()
	if db.HasAccessPolicies() {
		w.Header().Set("Cache-Control", "no-store")
	}
	// The root collection of the validated key scopes the capability: it is
	// the same key the driver writes, so an id cannot redirect the write.
	collection := core.RootCollection(key)
	action := ""
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		action = auth.CapRecordsRead
	case http.MethodPut, http.MethodPost, http.MethodPatch:
		action = auth.CapRecordsWrite
	case http.MethodDelete:
		action = auth.CapRecordsDelete
	}
	if action != "" && !s.authorize(w, r, db.ID(), action, collection) {
		return
	}
	if db.Coordinator() != nil && (r.Method == http.MethodPut || r.Method == http.MethodPost || r.Method == http.MethodDelete || r.Method == http.MethodPatch && strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/vnd.dtql.operation+json") {
		writeError(w, 422, "authorization_unsupported", "this mount requires normalized protected operations")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.readRecord(w, r, db, key)
	case http.MethodHead:
		exists, err := db.Exists(ctx, key)
		if err != nil || !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodPut, http.MethodPost:
		var body struct {
			Data map[string]any `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
			return
		}
		if body.Data == nil {
			writeError(w, http.StatusBadRequest, "bad_request", `body must contain a "data" object`)
			return
		}
		opName := "set"
		okStatus := http.StatusNoContent
		if r.Method == http.MethodPost {
			opName = "insert"
			okStatus = http.StatusCreated
		}
		if _, err := db.Apply(ctx, []core.Op{{Op: opName, Key: key, Data: body.Data}}, ""); err != nil {
			s.writeMappedError(w, r, err)
			return
		}
		w.WriteHeader(okStatus)
	case http.MethodPatch:
		if strings.Split(r.Header.Get("Content-Type"), ";")[0] == "application/vnd.dtql.operation+json" {
			s.handleProtectedUpdate(w, r, db, key)
			return
		}
		var body struct {
			Updates []core.UpdateOp `json:"updates"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
			return
		}
		if len(body.Updates) == 0 {
			writeError(w, http.StatusBadRequest, "bad_request", `body must contain a non-empty "updates" array`)
			return
		}
		if _, err := db.Apply(ctx, []core.Op{{Op: "update", Key: key, Updates: body.Updates}}, ""); err != nil {
			s.writeMappedError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if _, err := db.Apply(ctx, []core.Op{{Op: "delete", Key: key}}, ""); err != nil {
			s.writeMappedError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed: "+r.Method)
	}
}

func (s *Server) readRecord(w http.ResponseWriter, r *http.Request, db *core.Database, key *record.Key) {
	data, err := db.Get(r.Context(), key)
	if err != nil {
		if db.HasAccessPolicies() && (errors.Is(err, access.ErrAccessDenied) || errors.Is(err, core.ErrNotFound)) {
			writeUnavailablePoint(w, db, key, "get")
			return
		}
		s.writeMappedError(w, r, err)
		return
	}
	s.cacheReadResponse(w, r, db)
	writeJSON(w, http.StatusOK, map[string]any{"key": key.String(), "data": data})
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	if db.Coordinator() != nil {
		writeError(w, 422, "authorization_unsupported", "legacy batches are unavailable on this protected profile")
		return
	}
	var body struct {
		Message string    `json:"message"`
		Ops     []core.Op `json:"ops"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if len(body.Ops) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", `body must contain a non-empty "ops" array`)
		return
	}
	for i := range body.Ops {
		key, err := parseKeyPath(body.Ops[i].KeyPath)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_key",
				fmt.Sprintf("op %d: invalid key %q: %v", i, body.Ops[i].KeyPath, err))
			return
		}
		body.Ops[i].Key = key
	}
	// Layer-2 capability check per op: writes and deletes are scoped to each
	// op's root collection.
	for i := range body.Ops {
		action := auth.CapRecordsWrite
		if body.Ops[i].Op == "delete" {
			action = auth.CapRecordsDelete
		}
		if !s.authorize(w, r, db.ID(), action, core.RootCollection(body.Ops[i].Key)) {
			return
		}
	}
	applied, err := db.Apply(r.Context(), body.Ops, body.Message)
	if err != nil {
		s.writeMappedError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": applied})
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.Header().Set("Allow", "GET, POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	db := s.db(w, r)
	if db == nil {
		return
	}
	s.setReadCacheSafety(w, r, db)
	var q core.Query
	if err := decodeQuery(r, &q); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.executeQuery(w, r, db, q)
}

func decodeQuery(r *http.Request, q *core.Query) error {
	var data []byte
	if r.Method == http.MethodGet {
		raw, ok := r.URL.Query()["q"]
		if !ok || len(raw) != 1 || raw[0] == "" {
			return errors.New(`query parameter "q" is required`)
		}
		if len(raw[0]) > maxQueryRequestBytes {
			return errors.New("query parameter exceeds 1 MiB limit")
		}
		data = []byte(raw[0])
	} else {
		if err := json.NewDecoder(r.Body).Decode(q); err != nil {
			return fmt.Errorf("invalid JSON body: %w", err)
		}
		return nil
	}
	if err := json.Unmarshal(data, q); err != nil {
		return fmt.Errorf("invalid JSON query: %w", err)
	}
	return nil
}

func (s *Server) executeQuery(w http.ResponseWriter, r *http.Request, db *core.Database, q core.Query) {
	// A subcollection query is scoped by its parent's root collection, not by
	// the (possibly same-named) leaf collection.
	_, scope, err := q.Target()
	if err != nil {
		s.writeMappedError(w, r, err)
		return
	}
	if !s.authorize(w, r, db.ID(), auth.CapRecordsRead, scope) {
		return
	}
	records, err := db.Execute(r.Context(), q)
	if err != nil {
		s.writeMappedError(w, r, err)
		return
	}
	type recordOut struct {
		Key  string         `json:"key"`
		Data map[string]any `json:"data,omitempty"`
	}
	out := make([]recordOut, 0, len(records))
	for _, rec := range records {
		ro := recordOut{Key: rec.Key.String()}
		if !q.KeysOnly {
			ro.Data = rec.Data
		}
		out = append(out, ro)
	}
	s.cacheReadResponse(w, r, db)
	writeJSON(w, http.StatusOK, map[string]any{"records": out})
}

func (s *Server) cacheReadResponse(w http.ResponseWriter, r *http.Request, db *core.Database) {
	if s.readOnly && s.readCacheTTL > 0 && r.Method == http.MethodGet &&
		(strings.HasSuffix(r.URL.Path, "/read") || strings.HasSuffix(r.URL.Path, "/query")) &&
		s.authCfg == nil && !db.HasAccessPolicies() {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(s.readCacheTTL.Seconds())))
	}
}

// setReadCacheSafety prevents a shared or private cache from retaining a
// response whose contents or visibility can vary by caller. It runs before
// parsing, authorization, or fetching so errors are covered too.
func (s *Server) setReadCacheSafety(w http.ResponseWriter, r *http.Request, db *core.Database) {
	if r.Method == http.MethodGet &&
		(strings.HasSuffix(r.URL.Path, "/read") || strings.HasSuffix(r.URL.Path, "/query")) &&
		(s.authCfg != nil || db.HasAccessPolicies()) {
		w.Header().Set("Cache-Control", "no-store")
	}
}
