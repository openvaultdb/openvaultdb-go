package server

import (
	"errors"
	"net/http"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// writeMappedError converts core/engine/schema errors into the API error
// shapes documented in docs/api.md.
func writeMappedError(w http.ResponseWriter, err error) {
	var validationErr *schema.ValidationError
	switch {
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrUpdateOfMissingRecord):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, core.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "already_exists", err.Error())
	case errors.As(err, &validationErr):
		writeError(w, http.StatusUnprocessableEntity, "schema_validation", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
