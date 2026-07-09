package server

import (
	"io"
	"net/http"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
)

// handleDTQL accepts a DTQL-YAML document (dalgo's native lossless
// serialization of dal.StructuredQuery) and passes it through to the
// database's DALgo driver. ovdb's role here is routing (and, in the future,
// authentication) — not query evaluation.
func (s *Server) handleDTQL(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	doc, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read body: "+err.Error())
		return
	}
	if len(doc) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "body must contain a DTQL YAML document")
		return
	}
	// DTQL's target collection is only known after deserialization, which
	// happens inside core — so authorization here requires an UNSCOPED
	// records:read grant (or the owner token); collection-scoped grants must
	// use /query, where the collection is explicit.
	if !s.authorize(w, r, db.ID(), auth.CapRecordsRead, "") {
		return
	}
	records, err := db.ExecuteDTQL(r.Context(), doc)
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
		out = append(out, recordOut{Key: rec.Key.String(), Data: rec.Data})
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": out})
}
