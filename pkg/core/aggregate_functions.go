package core

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dtql"
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
// relational profile, in any ASCII letter case. A name with a byte of 0x80 or above
// is none: the case of such a name is not folded (see foldAggregateName), so that
// this predicate and the walk of pkg/joinexec, which repeats it, read one rule.
func IsAggregateFunction(name string) bool {
	folded, ok := foldAggregateName(name)
	return ok && slices.Contains(profileAggregateFunctions, folded)
}

// foldAggregateName lower-cases name, which must be ASCII. Go's folding of case
// does not agree with itself outside ASCII (it lowers U+0130 to i and raises U+0131
// to I), and the two walks of an aggregate name fold in opposite directions, so a
// name with a byte of 0x80 or above is refused rather than folded, in both.
func foldAggregateName(name string) (string, bool) {
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			return "", false
		}
	}
	return strings.ToLower(name), true
}

// aggregateFunctionsText is the list as a refusal writes it.
var aggregateFunctionsText = strings.Join(profileAggregateFunctions, ", ")

// unsupportedAggregateText is the fixed text of the refusal of a function outside
// the profile. It names the function only when the caller's text is one of the two
// DALgo knows and the profile leaves out, so that no text of the caller's is
// repeated.
func unsupportedAggregateText(name string) string {
	switch lower, _ := foldAggregateName(name); lower {
	case "first", "last":
		return lower + " is not in the relational profile: the aggregate functions are " + aggregateFunctionsText
	}
	return "an aggregate function must be one of " + aggregateFunctionsText
}

// deserializerAggregateRefusal matches the text of DALgo's refusal of an aggregate
// function it does not know, which quotes the caller's text: unsupported aggregate
// "NAME". DALgo raises it while it deserializes a document, wherever an aggregate
// stands (a column, HAVING, ORDER BY), before the classifier looks at it.
var deserializerAggregateRefusal = regexp.MustCompile(`unsupported aggregate "(?:[^"\\]|\\.)*"`)

// DeserializeDTQL deserializes a DTQL document with DALgo and says what is wrong
// with one it refuses: the error wraps ErrInvalidDTQL (HTTP 400 invalid_dtql). The
// refusal of an aggregate function DALgo does not know is a message built here
// that lists the functions of the profile and repeats none of the caller's text;
// every other refusal carries DALgo's own message.
func DeserializeDTQL(doc []byte) (dal.StructuredQuery, error) {
	query, err := dtql.Deserialize(doc)
	if err != nil {
		if deserializerAggregateRefusal.MatchString(err.Error()) {
			return nil, fmt.Errorf("%w: %s", ErrInvalidDTQL, unsupportedAggregateText(""))
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidDTQL, err)
	}
	return query, nil
}
