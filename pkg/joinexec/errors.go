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

	// ErrUnsupportedSource is returned for a collection the request profile
	// does not allow: schema-qualified or nested under a parent key. Their
	// names cannot be matched against collection grants or policies.
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
// bound and its limit, never the observed figure. A request that returns it
// returns no rows.
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
	where := ""
	if e.Path != "" {
		where = " at " + e.Path
	}
	return fmt.Sprintf("query budget exceeded: %s limit %d (%s route)%s", e.Name, e.Limit, e.Route, where)
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
