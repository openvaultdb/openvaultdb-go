package joinexec

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dal-go/dalgo/dal"
)

// DALgo enforces its own bounds in dal/join_execute.go and
// dal/aggregation_execute.go and reports them as errors whose only stable
// parts are a category and a message (joins) or the message text alone
// (aggregation). MapDalgoError turns those into BudgetError so a caller can
// answer 422 with the name of the limit.
//
// The mapping is pinned to the text shared by dalgo v0.88.0 and v0.89.1 (the
// two files were compared; only IS NULL handling differs). The tests drive the
// real engine to the bounds that are cheap to reach and check every message
// literal against the dalgo version in go.mod, so a dalgo bump that rewords a
// bound fails here instead of turning a 422 into a 500.

// DALgo's join limits (maxJoinRows, maxJoinBytes and ten candidate
// evaluations per row), mirrored here because DALgo does not export them.
// Messages that carry no number are reported with these values.
const (
	dalgoJoinRows       = 10000
	dalgoJoinBytes      = 16 << 20
	dalgoJoinCandidates = dalgoJoinRows * 10
	// dalgoAggregationGroups is the most groups DALgo keeps for one aggregation
	// (defaultMaxAggregationGroups); a message that carries it states it, and the
	// pin test checks it against the DALgo in go.mod.
	dalgoAggregationGroups = 100_000
)

// The bounds of the in-memory route, which the server states to a client in its
// discovery document and in the answer to a request that reaches one: the most
// rows and bytes a join holds, and the most groups an aggregation keeps.
const (
	MaxInMemoryJoinRows  = dalgoJoinRows
	MaxInMemoryJoinBytes = dalgoJoinBytes
	MaxInMemoryGroups    = dalgoAggregationGroups
)

type dalgoBound struct {
	name  string
	limit int64
}

// joinPlanBounds maps the message of a *dal.JoinValidationError with category
// join_plan.
var joinPlanBounds = map[string]dalgoBound{
	"joined row bound exceeded":               {BudgetJoinRows, dalgoJoinRows},
	"joined byte bound exceeded":              {BudgetJoinRetainedBytes, dalgoJoinBytes},
	"relation scan exceeds row or byte bound": {BudgetJoinScan, 0},
	"candidate evaluation bound exceeded":     {BudgetJoinCandidateEvaluations, dalgoJoinCandidates},
}

// queryLimitBounds maps the message of a *dal.QueryValidationError with
// category query_limit, raised when a join runs inside a recursive query.
var queryLimitBounds = map[string]dalgoBound{
	"result_rows":           {BudgetJoinResultRows, dalgoJoinRows},
	"retained_bytes":        {BudgetJoinRetainedBytes, dalgoJoinBytes},
	"fetched_rows":          {BudgetJoinFetchedRows, dalgoJoinRows},
	"candidate_evaluations": {BudgetJoinCandidateEvaluations, dalgoJoinCandidates},
}

// aggregationBounds match DALgo's aggregation limit errors, which are plain
// fmt.Errorf values. The limit is read from the message. Each pattern is
// anchored the way DALgo emits its message: after the start of the text or a
// ": " that DALgo's own wrapping adds, and up to the end of the text. DALgo
// echoes request-supplied names inside quotes in other errors (an unavailable
// join field, for one), and a name that spells a bound message is not a bound.
var aggregationBounds = []struct {
	name string
	re   *regexp.Regexp
}{
	{BudgetAggregationGroups, regexp.MustCompile(`(?:^|: )dalgo aggregation: group limit (\d{1,18}) exceeded$`)},
	{BudgetAggregationStates, regexp.MustCompile(`(?:^|: )dalgo aggregation: aggregate-state limit (\d{1,18}) exceeded$`)},
	{BudgetAggregationBytes, regexp.MustCompile(`(?:^|: )dalgo aggregation: retained aggregation byte limit (\d{1,18}) exceeded$`)},
	{BudgetAggregationDistinctValues, regexp.MustCompile(`(?s)(?:^|: )dalgo aggregation: distinct-value limit (\d{1,18}) exceeded for .+$`)},
	{BudgetAggregationTotalDistinct, regexp.MustCompile(`(?:^|: )dalgo aggregation: total distinct-value limit (\d{1,18}) exceeded$`)},
}

// DALgo evaluates a derived source (a subquery in FROM) by running the inner
// query and flattening the error it gets with %v into a join_plan error whose
// Message is "cannot scan <alias>: " (or "scan <alias>: " from a reader) plus
// the inner error's text. A bound raised inside the derived source therefore
// arrives as text. These two patterns recognise the inner text at the end of
// such a Message, built from the same tables as the exact lookups so the two
// cannot drift apart. The path of the inner error is captured.
var (
	flattenedQueryLimit = flattenedBoundPattern("query_limit", queryLimitBounds)
	flattenedJoinPlan   = flattenedBoundPattern("join_plan", joinPlanBounds)
)

func flattenedBoundPattern(category string, bounds map[string]dalgoBound) *regexp.Regexp {
	messages := make([]string, 0, len(bounds))
	for message := range bounds {
		messages = append(messages, regexp.QuoteMeta(message))
	}
	sort.Strings(messages)
	return regexp.MustCompile(`(?s)^(?:cannot )?scan .+: ` + category + `(?: at (\S+))?: (` + strings.Join(messages, "|") + `)$`)
}

// MapDalgoError returns a *BudgetError when err is, or wraps, one of DALgo's
// own bound errors, and err unchanged otherwise (including nil). route is the
// route the engine ran on (RouteInMemory or RouteDatabase). An error that is
// already a *BudgetError or *SourceDeniedError is returned as is.
//
// DALgo reports a leaf's error as text inside its own error, which drops the
// type; use Guard.Classify to recover a leaf's BudgetError, SourceDeniedError
// or SourceError as well.
func MapDalgoError(err error, route string) error {
	if err == nil {
		return nil
	}
	var budget *BudgetError
	var denied *SourceDeniedError
	if errors.As(err, &budget) || errors.As(err, &denied) {
		return err
	}
	var query *dal.QueryValidationError
	if errors.As(err, &query) && query.Category == "query_limit" {
		if bound, ok := queryLimitBounds[query.Message]; ok {
			return &BudgetError{Name: bound.name, Limit: bound.limit, Route: route, Path: query.Path}
		}
	}
	var join *dal.JoinValidationError
	if errors.As(err, &join) && join.Category == "join_plan" {
		if bound, ok := joinPlanBounds[join.Message]; ok {
			return &BudgetError{Name: bound.name, Limit: bound.limit, Route: route, Path: join.Path}
		}
		if mapped := flattenedBound(join.Message, route); mapped != nil {
			return mapped
		}
	}
	text := err.Error()
	for _, bound := range aggregationBounds {
		if m := bound.re.FindStringSubmatch(text); m != nil {
			limit, _ := strconv.ParseInt(m[1], 10, 64) // at most 18 digits: always fits
			return &BudgetError{Name: bound.name, Limit: limit, Route: route}
		}
	}
	return err
}

// flattenedBound maps the Message of a join_plan error that DALgo built from a
// bound raised inside a derived source, or returns nil.
func flattenedBound(message, route string) *BudgetError {
	if m := flattenedQueryLimit.FindStringSubmatch(message); m != nil {
		bound := queryLimitBounds[m[2]]
		return &BudgetError{Name: bound.name, Limit: bound.limit, Route: route, Path: m[1]}
	}
	if m := flattenedJoinPlan.FindStringSubmatch(message); m != nil {
		bound := joinPlanBounds[m[2]]
		return &BudgetError{Name: bound.name, Limit: bound.limit, Route: route, Path: m[1]}
	}
	return nil
}
