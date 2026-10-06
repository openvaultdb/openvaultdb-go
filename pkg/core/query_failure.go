package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// ErrQueryNotRunnable identifies a structured query that the adapter of the
// storage engine refused to run: it reports dal.ErrNotSupported (a condition, an
// aggregation or a join it cannot compile), or it does not know the
// structured-query dialect its mount asked for (mapped to HTTP 422
// query_unsupported with a fixed message). The error is built here: it holds no
// text of the adapter's.
var ErrQueryNotRunnable = errors.New("the storage engine cannot run this query")

// ErrQueryDoesNotFit identifies a structured query that the database server of the mount
// refused because a value or a name of it does not fit the table: a value that is no
// value of the type of its field (a word compared with an integer), a number compared
// with text, a name the table does not have. It is the caller's mistake, mapped to HTTP
// 400 invalid_dtql with a fixed message and not logged. The error is built here: it
// holds no text of the server's, which can repeat the value.
var ErrQueryDoesNotFit = errors.New("a value or a name of the query does not fit the field it is used with")

// sqlStated is an error that carries the SQLSTATE of a database server's refusal, as
// the driver of PostgreSQL's does (*pgconn.PgError.SQLState).
type sqlStated interface{ SQLState() string }

// isQueryThatDoesNotFit reports whether err is the refusal of a database server
// because of what the query holds, by the SQLSTATE the error carries and never by its
// text: class 22 (a data exception: a value that is no value of its type, out of its
// range or of the wrong format), 42883 (no operator for the types the query compares),
// 42804 (datatype mismatch), 42703 (no such column) and 42P18 (a value nothing gives a
// type). Any other code, and an error with none, is a failure of the server.
func isQueryThatDoesNotFit(err error) bool {
	var stated sqlStated
	if !errors.As(err, &stated) {
		return false
	}
	switch code := stated.SQLState(); code {
	case "42883", "42804", "42703", "42P18":
		return true
	default:
		return len(code) == 5 && code[:2] == "22"
	}
}

// unknownDialectPrefix starts the text of the adapter's refusal of a dialect it
// does not know (dalgo2sql: unsupported structured query dialect "x"). It is a
// plain error with no type to ask for, and it is only ever read here, to choose the
// answer; the text is never repeated.
const unknownDialectPrefix = "unsupported structured query dialect"

// isQueryNotRunnable reports whether err says the adapter cannot run the query: by
// type (dal.ErrNotSupported), or by the one refusal that has no type.
func isQueryNotRunnable(err error) bool {
	if errors.Is(err, dal.ErrNotSupported) {
		return true
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.HasPrefix(e.Error(), unknownDialectPrefix) {
			return true
		}
	}
	return false
}

// queryError is the error of a structured read that failed in the adapter, under
// the sentence prefix says (it names the step and the collection). A query the
// adapter cannot run is ErrQueryNotRunnable, on every engine. An engine reached
// through a connection string (serverEngines) gets a built error and nothing of the
// adapter's: a connection that cannot be made or that failed, while the request's own
// context is still alive, is an *UnreachableError (ErrDatabaseUnreachable); a
// cancellation and a deadline keep their identity, a refusal of the
// server because of what the query holds (isQueryThatDoesNotFit) is
// ErrQueryDoesNotFit, a refusal that DALgo raised above the adapter, in its planner or in
// the join it evaluates itself (dalgoRefusal), is kept as the typed error it is, rebuilt from
// its parts, and any other failure is a fixed sentence, because the text of a
// database server's error can repeat a value, a name or a hint of the request, and it
// would reach a log. The other engines keep the adapter's error in the chain.
func (d *Database) queryError(ctx context.Context, prefix string, err error) error {
	if d.readOnlyHTTP {
		switch {
		case errors.Is(err, dal.ErrNotSupported):
			return ErrHTTPOperationUnsupported
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			return ErrDatabaseUnreachable
		}
	}
	switch {
	case isQueryNotRunnable(err):
		return fmt.Errorf("%s: %w", prefix, ErrQueryNotRunnable)
	case !serverEngines[d.queryEngine()]:
		return fmt.Errorf("%s: %w", prefix, err)
	case ctx.Err() == nil && d.unreachableOf(err) != nil:
		return fmt.Errorf("%s: %w", prefix, d.unreachableOf(err))
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", prefix, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %w", prefix, context.DeadlineExceeded)
	case isQueryThatDoesNotFit(err):
		return fmt.Errorf("%s: %w", prefix, ErrQueryDoesNotFit)
	}
	if refusal := dalgoRefusal(err); refusal != nil {
		return fmt.Errorf("%s: %w", prefix, refusal)
	}
	return fmt.Errorf("%s: the database server could not run the query", prefix)
}

// builtReader is a records reader whose failures are the built errors of queryError.
// It is put around the reader of an engine reached through a connection string, so
// that what the driver says while rows are read or the reader is closed stays out
// of the errors the server answers with and logs. The end of a result is passed on
// untouched.
type builtReader struct {
	dal.RecordsReader
	db  *Database
	ctx context.Context
}

func (r builtReader) Next() (record.Record, error) {
	rec, err := r.RecordsReader.Next()
	if err == nil || err == io.EOF || errors.Is(err, dal.ErrNoMoreRecords) {
		return rec, err
	}
	return nil, r.db.queryError(r.ctx, "failed reading query results", err)
}

func (r builtReader) Close() error {
	if err := r.RecordsReader.Close(); err != nil {
		return r.db.queryError(r.ctx, "failed to close the query results", err)
	}
	return nil
}
