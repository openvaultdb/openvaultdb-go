package server

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/openvaultdb/openvaultdb-go/internal/store"
)

// recordCtx holds the resolved, scope-checked request context for a data-plane
// operation.
type recordCtx struct {
	vault      string
	namespace  string
	collection string
	id         string
}

// authorizeRecordOp validates the app token, decodes the namespace path
// segment, checks the token is bound to the requested vault/namespace, and
// enforces scope for op on the collection. On failure it writes the response
// and returns ok=false.
func (s *Server) authorizeRecordOp(w http.ResponseWriter, r *http.Request, op store.Op) (recordCtx, bool) {
	tok, ok := s.auth.token(bearer(r))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid app token required")
		return recordCtx{}, false
	}

	// {ns} is URL-encoded (contains %2F) — decode it.
	nsDecoded, err := url.PathUnescape(r.PathValue("ns"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "bad namespace segment")
		return recordCtx{}, false
	}

	ctx := recordCtx{
		vault:      r.PathValue("vaultId"),
		namespace:  nsDecoded,
		collection: r.PathValue("collection"),
		id:         r.PathValue("id"),
	}

	// The token is scoped to one vault + namespace; reject anything else.
	if tok.vault != ctx.vault || tok.namespaceID != ctx.namespace {
		writeError(w, http.StatusForbidden, "forbidden", "token not scoped to this vault/namespace")
		return recordCtx{}, false
	}
	if !scopeAllows(tok.scope, ctx.collection, op) {
		writeError(w, http.StatusForbidden, "forbidden", "operation not permitted for this collection")
		return recordCtx{}, false
	}
	return ctx, true
}

func (s *Server) handleListRecords(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.authorizeRecordOp(w, r, store.OpRead)
	if !ok {
		return
	}
	recs, err := s.store.ListRecords(ctx.vault, ctx.namespace, ctx.collection)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

func (s *Server) handleCreateRecord(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.authorizeRecordOp(w, r, store.OpWrite)
	if !ok {
		return
	}
	var rec store.Record
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "bad JSON body")
		return
	}
	if t, _ := rec["title"].(string); t == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "title is required")
		return
	}
	created, err := s.store.CreateRecord(ctx.vault, ctx.namespace, ctx.collection, rec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateRecord(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.authorizeRecordOp(w, r, store.OpWrite)
	if !ok {
		return
	}
	var patch store.Record
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "bad JSON body")
		return
	}
	updated, found, err := s.store.UpdateRecord(ctx.vault, ctx.namespace, ctx.collection, ctx.id, patch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "record not found")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteRecord(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.authorizeRecordOp(w, r, store.OpDelete)
	if !ok {
		return
	}
	found, err := s.store.DeleteRecord(ctx.vault, ctx.namespace, ctx.collection, ctx.id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "record not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
