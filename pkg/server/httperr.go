package server

import (
	"errors"
	"net/http"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code          string     `json:"code"`
	Message       string     `json:"message,omitempty"`
	RequestID     string     `json:"requestId,omitempty"`
	Authorization *az.Result `json:"authorization,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// writeMappedError converts core/engine/schema errors into the API error
// shapes documented in docs/api.md.
func writeMappedError(w http.ResponseWriter, err error) {
	var validationErr *schema.ValidationError
	switch {
	case errors.Is(err, access.ErrAccessDenied):
		// Do not reflect evaluator text: it may contain private predicate values,
		// policy paths, or protected row facts.
		writeError(w, http.StatusForbidden, "ACCESS_DENIED", "access denied")
	case errors.Is(err, core.ErrInvalidDTQL):
		writeError(w, http.StatusBadRequest, "invalid_dtql", err.Error())
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrUpdateOfMissingRecord):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, core.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "already_exists", err.Error())
	case errors.As(err, &validationErr):
		writeError(w, http.StatusUnprocessableEntity, "schema_validation", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}
