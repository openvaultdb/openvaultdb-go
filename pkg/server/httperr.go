package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// resultBufferHint is the hint of a read of one collection whose result is larger than the
// buffer of the server (core.ErrResultTooLarge).
const resultBufferHint = "The result is larger than one response holds (8 MiB). Narrow the read with a filter or a smaller limit. A DTQL read can also select fewer columns and, on a mount without access policies, read the result in pages: send the OVDB-Page-Size header to the DTQL endpoint."

// codeDatabaseUnavailable and messageDatabaseUnavailable are the error code and the
// message of a request that a mount's database server could not answer because it
// could not be reached (core.ErrDatabaseUnreachable).
const (
	codeDatabaseUnavailable    = "database_unavailable"
	messageDatabaseUnavailable = "the database of this mount cannot be reached"
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
// and answered as a generic 500, so callers can log it. A mount whose database
// cannot be reached is answered 503 and is not a 500, so it is not reported: the
// caller that logs it is Server.writeMappedError (logUnreachable).
func writeMappedError(w http.ResponseWriter, err error) (internal bool) {
	var validationErr *schema.ValidationError
	switch {
	case errors.Is(err, core.ErrDatabaseUnreachable):
		// The database server of the mount cannot be reached: one status and one code
		// whatever the failure, and one fixed message. Nothing of the connection (the
		// host, the port, the user, the password, a driver's text) is in it.
		writeError(w, http.StatusServiceUnavailable, codeDatabaseUnavailable, messageDatabaseUnavailable)
	case errors.Is(err, core.ErrResultTooLarge):
		// The result of a read of one collection is larger than the buffer of the server.
		// It is the caller's request, so it is the answer a relational result over its
		// bound gets (422 query_budget_exceeded, naming the bound), with a hint that says to
		// narrow or to page the read, and it is not logged as an error.
		writeBudgetRefusal(w, &joinexec.BudgetError{Name: joinexec.BudgetResponseBytes, Limit: core.ResultBufferBytes, Route: joinexec.RouteDatabase}, resultBufferHint)
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
	case errors.Is(err, core.ErrQueryDoesNotFit):
		// The database server refused a value or a name of the query: the caller's
		// mistake, answered with a fixed message (the error is built in core and holds
		// nothing of the server's) and, like every refusal here, not logged.
		writeError(w, http.StatusBadRequest, "invalid_dtql", core.ErrQueryDoesNotFit.Error())
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
		return
	}
	s.logUnreachable(r, err)
}

// logUnreachable records, when err says a mount's database server cannot be reached,
// which mount it was and the adapter's fixed sentence for the failure. It holds only
// the method, the route path, the ID of the mount and that sentence: nothing of the
// connection string and nothing of the driver's own text is in the error, so none is
// in the line.
func (s *Server) logUnreachable(r *http.Request, err error) {
	var unreachable *core.UnreachableError
	if !errors.As(err, &unreachable) {
		return
	}
	s.logger.ErrorContext(r.Context(), "database unreachable",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("database", unreachable.Database),
		slog.String("reason", unreachable.Reason))
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
