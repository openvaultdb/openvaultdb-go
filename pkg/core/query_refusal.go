package core

import (
	"errors"
	"regexp"
	"strings"

	"github.com/dal-go/dalgo/dal"
)

// DALgo stands between the guarded executor of a mount and its adapter (dal.NewDB). Its
// planner validates the relation tree of a join before it chooses an engine, and when the
// adapter declines a join it reads each table and joins the rows itself, inside the
// transaction, under bounds of its own. What it refuses there it refuses with a typed error,
// a *dal.JoinValidationError or a *dal.QueryValidationError, whose category and message say
// what the document or the request ran into: a source alias used twice, a field a table does
// not hold, the bound of a scan. The relational route of the server answers those as the
// refusals they are (400, or 422 for a bound), whichever engine raised them.
//
// On a mount that reaches a database server queryError built every error of the adapter, so
// that no text of the server reaches a log, and the typed refusal DALgo raised was lost with
// the rest: a caller's mistake was a failure of the server. The error is kept here, rebuilt
// from its category, path and message, and only where those are what DALgo says about the
// query and nothing of a read that failed: the categories that report a document it cannot
// run, and, of the categories that mix refusals, bounds and failed reads, the messages that
// are one of the first two. A join_plan message that carries the text of a failed scan, of a
// close or of a field load (cannot scan, scan, close scan, cannot load fields, an encoding
// fault) is not kept, and neither is the cause that came with it, so the fixed sentence is
// all that is logged for it.
//
// pkg/joinexec holds the same tables to map what the engine raises above the mounts
// (MapDalgoError, IsJoinPlanRefusal). It is not imported here, because it is the planner and
// this package does not depend on it; the drift test of this file runs the messages of
// DALgo's join through both and requires the two to agree.

// dalgoRefusalCategories are the categories of DALgo's validation errors that report a
// document it cannot run, whatever the data. They are the categories of both error types but
// join_plan and query_limit, whose messages are a mix of refusals, bounds and failed reads.
var dalgoRefusalCategories = map[string]bool{
	"scope": true, "shape": true, "cardinality": true, "query_shape": true,
	"join_shape": true, "join_scope": true, "join_key_type": true, "join_field": true,
	"join_cycle": true, "join_algorithm": true, "join_type": true, "join_operator": true,
}

// dalgoJoinPlanMessages are the messages of the join_plan category that are no failed read:
// the four bounds of the join (rows, bytes, the scan of a relation, candidate evaluations)
// and the refusals of a document.
var dalgoJoinPlanMessages = map[string]bool{
	"joined row bound exceeded":                           true,
	"joined byte bound exceeded":                          true,
	"relation scan exceeds row or byte bound":             true,
	"candidate evaluation bound exceeded":                 true,
	"wildcard expansion requires ordered schema metadata": true,
	"generic JOIN does not support provider cursors":      true,
	"IN or NOT IN requires an array":                      true,
	"IS NULL requires an operand":                         true,
}

// dalgoOperatorRefusal starts the message of a comparison operator DALgo's join does not
// evaluate; the rest of it is the operator the document wrote.
const dalgoOperatorRefusal = "unsupported operator "

// dalgoTypeRefusal matches the whole message of DALgo's join when it meets an expression or
// a condition of a type it does not evaluate: the text after the words is the name of a Go
// type, never a word of the request.
var dalgoTypeRefusal = regexp.MustCompile(`^unsupported (?:expression|condition) \S+$`)

// dalgoQueryLimits are the messages of the query_limit category: the bounds of a join that
// runs inside a recursive query.
var dalgoQueryLimits = map[string]bool{
	"result_rows": true, "retained_bytes": true, "fetched_rows": true, "candidate_evaluations": true,
}

// keptJoinDiagnostic reports whether a *dal.JoinValidationError says something about the
// query and not about a failed read.
func keptJoinDiagnostic(err *dal.JoinValidationError) bool {
	if err.Category != "join_plan" {
		return dalgoRefusalCategories[err.Category]
	}
	return dalgoJoinPlanMessages[err.Message] ||
		strings.HasPrefix(err.Message, dalgoOperatorRefusal) ||
		dalgoTypeRefusal.MatchString(err.Message)
}

// keptQueryDiagnostic reports whether a *dal.QueryValidationError says something about the
// query and not about a failed read.
func keptQueryDiagnostic(err *dal.QueryValidationError) bool {
	if err.Category == "query_limit" {
		return dalgoQueryLimits[err.Message]
	}
	return dalgoRefusalCategories[err.Category]
}

// dalgoRefusal returns the typed refusal of DALgo that err is or wraps, rebuilt from its
// category, path and message and holding nothing else (the diagnostic of a failed scan
// carries the error that failed as its cause, which is not copied), or nil when err is no
// such refusal.
func dalgoRefusal(err error) error {
	var join *dal.JoinValidationError
	if errors.As(err, &join) && keptJoinDiagnostic(join) {
		return &dal.JoinValidationError{Category: join.Category, Path: join.Path, Message: join.Message}
	}
	var query *dal.QueryValidationError
	if errors.As(err, &query) && keptQueryDiagnostic(query) {
		return &dal.QueryValidationError{Category: query.Category, Path: query.Path, Message: query.Message}
	}
	return nil
}
