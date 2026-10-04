package joinexec

import (
	"errors"
	"fmt"
)

// Budget names of the result bound, carried by BudgetError.Name beside the
// ones of errors.go. They bound what is answered, not what is read.
const (
	BudgetResponseRows  = "response_rows"
	BudgetResponseBytes = "response_bytes"
)

var (
	// ErrInvalidDocument is returned when the document is not one this package
	// executes: a shape the walk does not know, a name that is not plain, or a
	// profile that does not describe the query. The document was not run, and no
	// mount was resolved for it.
	ErrInvalidDocument = errors.New("joinexec: invalid document")

	// ErrSourceWithoutDatabase is returned when a source names no database and
	// there is no single default one to read it from: the request has no default
	// database, or the document reads several databases and so must name the
	// database of every source.
	ErrSourceWithoutDatabase = errors.New("joinexec: a source names no database")

	// ErrScanOnProtectedSource is returned for a scan clause on a source with
	// access policies. DALgo applies the scan's limit and order to the source's
	// own read, where the policy filter could run after the limit, so the clause
	// is refused until DALgo applies policies before scan bounds (dalgo pull
	// request 197).
	ErrScanOnProtectedSource = errors.New("joinexec: scan clauses are not supported on a source with access policies")

	// ErrReadTxSkipped is returned when a source's ReadTx returned without
	// running the function it was given, which would otherwise read as an empty
	// result.
	ErrReadTxSkipped = errors.New("joinexec: the read transaction did not run the query")

	// ErrNoReader is returned when an executor returned neither a reader nor an
	// error.
	ErrNoReader = errors.New("joinexec: the executor returned no reader")

	// ErrRegistryMismatch is returned when the registry answers a database id
	// with a source of another id. The leaf authorises a read under the source's
	// own id, so such a source would be authorised under the wrong name.
	ErrRegistryMismatch = errors.New("joinexec: the registry returned a source for another database")
)

// UnknownDatabaseError reports a database id no source is registered under.
// It is returned only after every source of the document was authorised, so it
// does not tell a caller who may not read a database whether it exists.
type UnknownDatabaseError struct {
	Database string
}

func (e *UnknownDatabaseError) Error() string {
	return fmt.Sprintf("database %q is not registered", e.Database)
}

// EngineNotQueryableError reports a database whose storage engine cannot run
// structured queries at all. Callers answer it as the engine guard of the
// single-collection path does (HTTP 501 query_unsupported). It takes
// precedence over EngineNotJoinableError, and no operator setting lifts it.
type EngineNotQueryableError struct {
	Database string
	Engine   string
}

func (e *EngineNotQueryableError) Error() string {
	return fmt.Sprintf("the %q storage engine of database %q is not yet supported for queries", e.Engine, e.Database)
}

// EngineNotJoinableError reports a database whose storage engine is not in the
// configured set of engines that may take part in relational queries.
type EngineNotJoinableError struct {
	Database string
	Engine   string
}

func (e *EngineNotJoinableError) Error() string {
	return fmt.Sprintf("the %q storage engine of database %q is not enabled for joins and aggregation", e.Engine, e.Database)
}
