package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// handleDTQL authenticates a bounded DTQL query and executes it through the
// mounted database's secured DALgo handle.
func (s *Server) handleDTQL(w http.ResponseWriter, r *http.Request) {
	db := s.db(w, r)
	if db == nil {
		return
	}
	doc, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read body: "+err.Error())
		return
	}
	if len(doc) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "body must contain a DTQL YAML document")
		return
	}
	query, collection, err := core.ParseDTQL(doc)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !s.authorize(w, r, db.ID(), auth.CapRecordsRead, collection) {
		return
	}
	records, err := db.ExecuteDTQLQuery(r.Context(), query)
	if err != nil {
		if errors.Is(err, access.ErrAccessDenied) {
			op := api.Operation{ID: "q1", Action: "query", Resource: az.Resource{DatabaseID: db.ID(), Path: "/" + collection, Table: collection}, ExecutionClass: az.ExecutionDTQL, Query: &api.Query{Format: "dtql-yaml", Text: string(doc)}}
			s.writeQueryAuthorizationError(w, r, db, op, err)
			return
		}
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
