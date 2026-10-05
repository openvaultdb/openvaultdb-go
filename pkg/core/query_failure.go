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
// adapter's: a cancellation and a deadline keep their identity, and any other
// failure is a fixed sentence, because the text of a database server's error can
// repeat a value, a name or a hint of the request, and it would reach a log. The
// other engines keep the adapter's error in the chain.
func (d *Database) queryError(prefix string, err error) error {
	switch {
	case isQueryNotRunnable(err):
		return fmt.Errorf("%s: %w", prefix, ErrQueryNotRunnable)
	case !serverEngines[d.queryEngine()]:
		return fmt.Errorf("%s: %w", prefix, err)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", prefix, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %w", prefix, context.DeadlineExceeded)
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
	db *Database
}

func (r builtReader) Next() (record.Record, error) {
	rec, err := r.RecordsReader.Next()
	if err == nil || err == io.EOF || errors.Is(err, dal.ErrNoMoreRecords) {
		return rec, err
	}
	return nil, r.db.queryError("failed reading query results", err)
}

func (r builtReader) Close() error {
	if err := r.RecordsReader.Close(); err != nil {
		return r.db.queryError("failed to close the query results", err)
	}
	return nil
}
