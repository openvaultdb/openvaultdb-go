package core

import (
	"slices"
	"strings"
)

// profileAggregateFunctions is the one list of aggregate functions of the
// relational profile, as a document writes them. Three things read it: the
// classifier (a document that names any other function is refused before
// anything is read), the name walk of the query guard, and the server's
// discovery document, which advertises exactly this list. DALgo also knows first
// and last, but its adapters evaluate them only over a stable row order, which no
// engine the server runs declares, so they are not in the profile.
//
// pkg/joinexec repeats the list because it does not import this package; the test
// that compares the two walks (joinexec_names_drift_test.go) fails if they differ.
var profileAggregateFunctions = []string{"count", "sum", "avg", "min", "max"}

// AggregateFunctions returns the aggregate functions of the relational profile in
// lower case, in the order discovery lists them. The caller owns the slice.
func AggregateFunctions() []string { return slices.Clone(profileAggregateFunctions) }

// IsAggregateFunction reports whether name is an aggregate function of the
// relational profile, in any letter case.
func IsAggregateFunction(name string) bool {
	return slices.Contains(profileAggregateFunctions, strings.ToLower(name))
}

// aggregateFunctionsText is the list as a refusal writes it.
var aggregateFunctionsText = strings.Join(profileAggregateFunctions, ", ")

// unsupportedAggregateText is the fixed text of the refusal of a function outside
// the profile. It names the function only when the caller's text is one of the two
// DALgo knows and the profile leaves out, so that no text of the caller's is
// repeated.
func unsupportedAggregateText(name string) string {
	switch lower := strings.ToLower(name); lower {
	case "first", "last":
		return lower + " is not in the relational profile: the aggregate functions are " + aggregateFunctionsText
	}
	return "an aggregate function must be one of " + aggregateFunctionsText
}
