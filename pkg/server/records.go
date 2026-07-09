package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/dal-go/dalgo/dal"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// parseRecordKey extracts the record key from the escaped URL path so that
// percent-encoded characters inside IDs (dal.EscapeID encodes `. $ # [ ] /`)
// survive as data rather than path separators. Segments are unescaped
// individually and validated (no empty, "." or ".." segments — path
// traversal safety).
func parseRecordKey(r *http.Request) (*dal.Key, error) {
	escaped := r.URL.EscapedPath()
	idx := strings.Index(escaped, "/records/")
	if idx < 0 {
		return nil, fmt.Errorf("record key missing in path")
	}
	raw := strings.TrimSuffix(escaped[idx+len("/records/"):], "/")
	if raw == "" {
		return nil, fmt.Errorf("record key missing in path")
	}
	return parseKeyPath(raw)
}

// parseKeyPath parses a dal-escaped key path ("collection/id[/sub/id...]")
// into a dalgo key.
func parseKeyPath(raw string) (*dal.Key, error) {
	return core.ParseKeyPath(raw)
}

func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	key, err := parseRecordKey(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	ctx := r.Context()
	collection := key.Collection()
	for cur := key; cur != nil; cur = cur.Parent() {
		collection = cur.Collection() // root collection scopes the capability
	}
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
	switch r.Method {
	case http.MethodGet:
		data, err := db.Get(ctx, key)
		if err != nil {
			writeMappedError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"key": key.String(), "data": data})
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
			writeMappedError(w, err)
			return
		}
		w.WriteHeader(okStatus)
	case http.MethodPatch:
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
			writeMappedError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if _, err := db.Apply(ctx, []core.Op{{Op: "delete", Key: key}}, ""); err != nil {
			writeMappedError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed: "+r.Method)
	}
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
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
			writeError(w, http.StatusBadRequest, "bad_request",
				fmt.Sprintf("op %d: invalid key %q: %v", i, body.Ops[i].KeyPath, err))
			return
		}
		body.Ops[i].Key = key
	}
	// Layer-2 capability check per op: writes and deletes are scoped to each
	// op's root collection.
	for i := range body.Ops {
		root := body.Ops[i].Key
		for cur := root; cur != nil; cur = cur.Parent() {
			root = cur
		}
		action := auth.CapRecordsWrite
		if body.Ops[i].Op == "delete" {
			action = auth.CapRecordsDelete
		}
		if !s.authorize(w, r, db.ID(), action, root.Collection()) {
			return
		}
	}
	applied, err := db.Apply(r.Context(), body.Ops, body.Message)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": applied})
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	var q core.Query
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return
	}
	if !s.authorize(w, r, db.ID(), auth.CapRecordsRead, q.Collection) {
		return
	}
	records, err := db.Execute(r.Context(), q)
	if err != nil {
		writeMappedError(w, err)
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
	writeJSON(w, http.StatusOK, map[string]any{"records": out})
}
