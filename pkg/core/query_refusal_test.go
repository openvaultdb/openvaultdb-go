package core

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dal-go/dalgo/dal"

	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
)

// TestOnlyWhatDalgoSaysAboutTheQueryIsKeptAsATypedRefusal: of the typed errors of DALgo, the
// categories that report a document it cannot run are kept, a bound of its join is kept by
// its message, and a join_plan error that reports a failed read (a scan, a close, the load of
// the fields of a table, an encoding fault) is not, nor is a message nobody listed. A kept
// error is a copy of its category, path and message, with nothing it wrapped.
func TestOnlyWhatDalgoSaysAboutTheQueryIsKeptAsATypedRefusal(t *testing.T) {
	join := func(category, message string) error {
		return &dal.JoinValidationError{Category: category, Path: "from.joins[0]", Message: message}
	}
	query := func(category, message string) error {
		return &dal.QueryValidationError{Category: category, Path: "where", Message: message}
	}
	for _, c := range []struct {
		name string
		err  error
		kept bool
	}{
		{"join shape", join("join_shape", "on must contain at least one predicate"), true},
		{"join scope", join("join_scope", `duplicate alias "o"`), true},
		{"join field", join("join_field", `field "x" is unavailable in "c"`), true},
		{"join key type", join("join_key_type", "unsupported key type []uint8"), true},
		{"join cycle", join("join_cycle", "recursive from reference"), true},
		{"join algorithm", join("join_algorithm", `unknown algorithm "x"`), true},
		{"join type", join("join_type", `unsupported join type "x"`), true},
		{"join operator", join("join_operator", "only == is supported"), true},
		{"a category nobody listed", join("join_other", "anything"), false},
		{"the bound of rows", join("join_plan", "joined row bound exceeded"), true},
		{"the bound of bytes", join("join_plan", "joined byte bound exceeded"), true},
		{"the bound of a scan", join("join_plan", "relation scan exceeds row or byte bound"), true},
		{"the bound of candidates", join("join_plan", "candidate evaluation bound exceeded"), true},
		{"a wildcard with no field list", join("join_plan", "wildcard expansion requires ordered schema metadata"), true},
		{"a cursor", join("join_plan", "generic JOIN does not support provider cursors"), true},
		{"an array that is none", join("join_plan", "IN or NOT IN requires an array"), true},
		{"a null test of nothing", join("join_plan", "IS NULL requires an operand"), true},
		{"an operator", join("join_plan", "unsupported operator ~"), true},
		{"an expression of a type", join("join_plan", "unsupported expression dal.Param"), true},
		{"a condition of a type", join("join_plan", "unsupported condition dal.Param"), true},
		{"an expression with more after the type", join("join_plan", "unsupported expression dal.Param "+failureMarker), false},
		{"a scan that failed", join("join_plan", "cannot scan o: "+failureMarker), false},
		{"a scan that failed reading", join("join_plan", "scan o: "+failureMarker), false},
		{"a close that failed", join("join_plan", "close scan o: "+failureMarker), false},
		{"a field load that failed", join("join_plan", "cannot load fields for o: "+failureMarker), false},
		{"an encoding fault", join("join_plan", "output is not JSON serializable: "+failureMarker), false},
		{"a message nobody listed", join("join_plan", "something else"), false},
		{"query scope", query("scope", "ambiguous unqualified field x"), true},
		{"query shape", query("shape", `field "x" is unavailable`), true},
		{"query cardinality", query("cardinality", "scalar query returned more than one row"), true},
		{"query query_shape", query("query_shape", "query is required"), true},
		{"query limit result rows", query("query_limit", "result_rows"), true},
		{"query limit retained bytes", query("query_limit", "retained_bytes"), true},
		{"query limit fetched rows", query("query_limit", "fetched_rows"), true},
		{"query limit candidate evaluations", query("query_limit", "candidate_evaluations"), true},
		{"query limit nobody listed", query("query_limit", failureMarker), false},
		{"a join_plan category on a query error, which carries the text of a derived source", query("join_plan", "cannot scan d: "+failureMarker), false},
		{"a plain error", errors.New("plain " + failureMarker), false},
		{"nothing", nil, false},
	} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped %v", c.name, wrapped), func(t *testing.T) {
				err := c.err
				if wrapped && err != nil {
					err = fmt.Errorf("failed to scan: %w", err)
				}
				got := dalgoRefusal(err)
				if (got != nil) != c.kept {
					t.Fatalf("kept = %v, want %v (%v)", got != nil, c.kept, got)
				}
				if got == nil {
					return
				}
				if errors.Unwrap(got) != nil {
					t.Errorf("the kept error wraps %v", errors.Unwrap(got))
				}
				if got.Error() != c.err.Error() {
					t.Errorf("kept %q, want %q", got.Error(), c.err.Error())
				}
				if fmt.Sprintf("%T", got) != fmt.Sprintf("%T", c.err) {
					t.Errorf("kept a %T, want a %T", got, c.err)
				}
			})
		}
	}
}

// TestWhatCoreKeepsOfDalgosRefusalsIsWhatTheServerAnswersAsOne: the messages of the join of
// DALgo (dal/join_execute.go, dal/q_join_validate.go of v0.89.6) are run through the table of
// this package and through the mapping of pkg/joinexec, which the relational route answers
// with, and the two must agree: a message that joinexec maps to a bound or knows as a refusal
// is kept here, and a message that reports a failed read is in neither. A change of one
// table without the other fails here.
func TestWhatCoreKeepsOfDalgosRefusalsIsWhatTheServerAnswersAsOne(t *testing.T) {
	messages := []string{
		// the bounds
		"joined row bound exceeded", "joined byte bound exceeded", "relation scan exceeds row or byte bound", "candidate evaluation bound exceeded",
		// the refusals of a document
		"wildcard expansion requires ordered schema metadata", "generic JOIN does not support provider cursors",
		"IN or NOT IN requires an array", "IS NULL requires an operand", "unsupported operator ~",
		"unsupported expression dal.Param", "unsupported condition dal.Param",
		// the failed reads and the encoding fault
		"cannot scan o: " + failureMarker, "scan o: " + failureMarker, "close scan o: " + failureMarker,
		"cannot load fields for o: " + failureMarker, "output is not JSON serializable: " + failureMarker,
	}
	for _, message := range messages {
		t.Run(message, func(t *testing.T) {
			err := &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: message}
			var budget *joinexec.BudgetError
			answered := errors.As(joinexec.MapDalgoError(err, joinexec.RouteDatabase), &budget) || joinexec.IsJoinPlanRefusal(message)
			if kept := dalgoRefusal(err) != nil; kept != answered {
				t.Fatalf("core keeps it = %v, the server answers it as a refusal or a bound = %v", kept, answered)
			}
		})
	}
	for message := range dalgoQueryLimits {
		err := &dal.QueryValidationError{Category: "query_limit", Path: "from", Message: message}
		var budget *joinexec.BudgetError
		if !errors.As(joinexec.MapDalgoError(err, joinexec.RouteDatabase), &budget) {
			t.Errorf("query_limit %q is kept here and is no bound to the server", message)
		}
	}
	// The categories: joinexec reads a refusal of a derived source back out of the text it was
	// flattened into, by the categories it lists, and answers the typed error it rebuilds. It is
	// the one place where it lists them, so it is asked about each category DALgo has.
	for _, category := range []string{
		"scope", "shape", "cardinality", "query_shape", "join_shape", "join_scope", "join_key_type", "join_field",
		"join_cycle", "join_algorithm", "join_type", "join_operator", "join_plan", "query_limit",
	} {
		if category == "join_plan" || category == "query_limit" {
			if dalgoRefusalCategories[category] {
				t.Errorf("category %q mixes refusals, bounds and failed reads and is kept by its message", category)
			}
			continue
		}
		var flattened error = &dal.JoinValidationError{Category: "join_plan", Path: "from", Message: "cannot scan d: " + category + " at p: m"}
		refusedBy := joinexec.MapDalgoError(flattened, joinexec.RouteDatabase) != flattened
		if refusedBy != dalgoRefusalCategories[category] {
			t.Errorf("category %q: joinexec reads it as a refusal = %v, core keeps it = %v", category, refusedBy, dalgoRefusalCategories[category])
		}
	}
}
