package joinexec

import (
	"errors"
	"fmt"
)

// Execution routes a BudgetError can name. They are the same labels the
// HTTP response reports as execution.route.
const (
	RouteInMemory = "in-memory"
	RouteDatabase = "database"
)

// Budget names carried by BudgetError.Name. The source_* names are this
// package's own request budget; the join_* and aggregation_* names are the
// bounds DALgo enforces itself (see MapDalgoError).
const (
	BudgetSourceRows  = "source_rows"
	BudgetSourceBytes = "source_bytes"

	BudgetJoinRows                 = "join_rows"
	BudgetJoinResultRows           = "join_result_rows"
	BudgetJoinFetchedRows          = "join_fetched_rows"
	BudgetJoinRetainedBytes        = "join_retained_bytes"
	BudgetJoinScan                 = "join_scan"
	BudgetJoinCandidateEvaluations = "join_candidate_evaluations"

	BudgetAggregationGroups         = "aggregation_groups"
	BudgetAggregationStates         = "aggregation_states"
	BudgetAggregationBytes          = "aggregation_bytes"
	BudgetAggregationDistinctValues = "aggregation_distinct_values"
	BudgetAggregationTotalDistinct  = "aggregation_total_distinct_values"
)

var (
	// ErrNotSingleSource is returned for a read that is not a plain structured
	// query over exactly one collection: a join, a derived source, a query
	// holding a subquery, or a text query. Only single-source reads reach a
	// source, because only those can be authorised one collection at a time.
	ErrNotSingleSource = errors.New("joinexec: a source read must be a plain query over one collection")

	// ErrUnsupportedSource is returned for a schema-qualified collection on a
	// source that does not explicitly support native PostgreSQL reads, or a
	// collection nested under a parent key. Native PostgreSQL refs are converted
	// to the same logical collection IDs used by request profiles and grants.
	ErrUnsupportedSource = errors.New("joinexec: schema-qualified and nested collections are not supported")

	// ErrRecordsetUnsupported is returned by the recordset read path, which is
	// not guarded and therefore not offered.
	ErrRecordsetUnsupported = errors.New("joinexec: recordset readers are not supported")

	// ErrNoExecutor is returned when a Source has no executor to read through.
	ErrNoExecutor = errors.New("joinexec: source has no executor")

	// ErrRowNotEncodable is returned when a row cannot be encoded to measure it
	// against the byte budget. The read fails closed.
	ErrRowNotEncodable = errors.New("joinexec: source row cannot be encoded")
)

// BudgetError reports that a request exceeded a resource bound. It names the
// bound and its limit, never the observed figure.
//
// A BudgetError ends the request, but rows may already have been read when it
// arrives: DALgo's streaming join (a flat equality join without ORDER BY)
// returns its reader first and the error comes from a later Next. Only a caller
// that reads to the end before answering returns no rows; Guard.Collect does
// exactly that.
type BudgetError struct {
	// Name is one of the Budget* constants.
	Name string
	// Limit is the bound that was exceeded. It is 0 when DALgo's message does
	// not identify which of several limits applied.
	Limit int64
	// Route is RouteInMemory or RouteDatabase.
	Route string
	// Path locates the bound: "database.collection" for a source read, or the
	// join tree path DALgo reports (for example "from.joins[0]"). It can be
	// empty when DALgo gives no path.
	Path string
}

func (e *BudgetError) Error() string {
	limit := ""
	if e.Limit != 0 {
		limit = fmt.Sprintf(" limit %d", e.Limit)
	}
	where := ""
	if e.Path != "" {
		where = fmt.Sprintf(" at %q", e.Path)
	}
	return fmt.Sprintf("query budget exceeded: %s%s (%s route)%s", e.Name, limit, e.Route, where)
}

// SourceDeniedError reports a read of a collection the caller may not read.
// The collection was never read.
type SourceDeniedError struct {
	Database   string
	Collection string
}

func (e *SourceDeniedError) Error() string {
	return fmt.Sprintf("access to collection %q in database %q is not allowed", e.Collection, e.Database)
}

// SourceError reports that a source failed while it was being read: its
// executor returned an error, a reader failed part way, or the request context
// ended during a read. Err is the source's own error with its chain intact, so
// errors.Is finds an access denial, a deadline or a backend sentinel through it.
// DALgo rewraps such an error as text; the guard records this one so that
// Guard.Classify can return it instead.
type SourceError struct {
	Database   string
	Collection string
	Err        error
}

func (e *SourceError) Error() string {
	return fmt.Sprintf("read of collection %q in database %q failed: %v", e.Collection, e.Database, e.Err)
}

func (e *SourceError) Unwrap() error { return e.Err }
