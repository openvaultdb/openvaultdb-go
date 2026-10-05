package server

import (
	"errors"
	"log/slog"
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
// shapes documented in docs/api.md. It reports whether err was unrecognised
// and answered as a generic 500, so callers can log it.
func writeMappedError(w http.ResponseWriter, err error) (internal bool) {
	var validationErr *schema.ValidationError
	switch {
	case errors.Is(err, access.ErrAccessDenied):
		// Do not reflect evaluator text: it may contain private predicate values,
		// policy paths, or protected row facts.
		decisions := access.DecisionsFromError(err)
		unsupported := len(decisions) > 0
		for _, decision := range decisions {
			if decision.Code != access.CodeEnforcementUnsupported {
				unsupported = false
			}
		}
		if unsupported {
			writeError(w, http.StatusUnprocessableEntity, "authorization_unsupported", "operation is unsupported by the enforcement profile")
			return
		}
		writeError(w, http.StatusForbidden, "ACCESS_DENIED", "access denied")
	case errors.Is(err, core.ErrInvalidKey):
		writeError(w, http.StatusBadRequest, "invalid_key", err.Error())
	case errors.Is(err, core.ErrInvalidQuery), errors.Is(err, core.ErrInvalidFieldName), errors.Is(err, core.ErrEmptyWrite):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, core.ErrInvalidDTQL):
		writeError(w, http.StatusBadRequest, "invalid_dtql", err.Error())
	case errors.Is(err, core.ErrQueryNotRunnable):
		// The adapter cannot run this query. The message is fixed: the error is built
		// in core and holds no text of the adapter's, and none is added here.
		writeError(w, http.StatusUnprocessableEntity, "query_unsupported", core.ErrQueryNotRunnable.Error())
	case errors.Is(err, core.ErrQueryUnsupported):
		writeError(w, http.StatusNotImplemented, "query_unsupported", err.Error())
	case errors.Is(err, core.ErrProtectedSingleSource):
		// The shape of the query decides this, not the collections it names, so
		// the message is the error's own: it names none.
		writeError(w, http.StatusUnprocessableEntity, "authorization_unsupported", core.ErrProtectedSingleSource.Error())
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrUpdateOfMissingRecord):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, core.ErrAlreadyExists):
		writeError(w, http.StatusConflict, "already_exists", err.Error())
	case errors.As(err, &validationErr):
		writeError(w, http.StatusUnprocessableEntity, "schema_validation", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "internal server error")
		return true
	}
	return false
}

// hiddenAsDenied returns the error a structured read is answered with. A mount
// with access policies does not say which collections it declares: a read of a
// collection the database does not declare (an error wrapping core.ErrNotFound)
// is answered as the read of a declared collection the policy denies, as
// readRecord answers a key. Any other error is returned as it is. A document that
// reads a second source never reaches this on such a mount: core refuses it for
// its shape (core.ErrProtectedSingleSource) before it looks at a name.
func hiddenAsDenied(db *core.Database, err error) error {
	if db.HasAccessPolicies() && errors.Is(err, core.ErrNotFound) {
		return access.ErrAccessDenied
	}
	return err
}

// writeMappedError maps err like the package-level writeMappedError and logs
// errors answered as 500 through the server logger.
func (s *Server) writeMappedError(w http.ResponseWriter, r *http.Request, err error) {
	if writeMappedError(w, err) {
		s.logInternal(r, err)
	}
}

// writeInternalError answers 500 with message and logs err.
func (s *Server) writeInternalError(w http.ResponseWriter, r *http.Request, message string, err error) {
	s.logInternal(r, err)
	writeError(w, http.StatusInternalServerError, "internal", message)
}

// logInternal records an internal error. It is redaction-safe by
// construction: only the method, the route path (never the query string,
// headers or body, which may carry tokens or record data) and the error.
func (s *Server) logInternal(r *http.Request, err error) {
	s.logger.ErrorContext(r.Context(), "internal server error",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("error", err))
}
