package server_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// The corpus test is only as strict as its matcher. A fixture that asks for an error names
// the category, the path and the message of the diagnostic, and an answer that has the
// category and the path and says something else (a refusal for want of a field list in
// the place of an ambiguity, for one) is not the fixture's answer. An entry of the
// divergence file that pins a message holds the answer to it as well. The helpers of this
// file all start with relIntMatcher so they cannot clash with the others of the package.

// relIntMatcherAnswer is a 400 invalid_dtql with the message given.
func relIntMatcherAnswer(message string) relHTTPResponse {
	return relHTTPResponse{
		status: http.StatusBadRequest,
		body:   map[string]any{"error": map[string]any{"code": "invalid_dtql", "message": message}},
	}
}

func TestTheCorpusMatcherHoldsAnErrorAnswerToTheMessageOfItsFixture(t *testing.T) {
	dir := filepath.Join(relIntDalgoDir(t), "dtql", "testdata", "subqueries")
	var fixture relIntCase
	for _, c := range relIntLoadCases(t, "subqueries", dir) {
		if c.id == "subqueries/scope-ambiguous" {
			fixture = c
		}
	}
	if fixture.err == nil {
		t.Fatal("the scope fixture asks for no error")
	}
	for name, tc := range map[string]struct {
		message string
		want    bool
	}{
		"the message of the fixture, with the category and the path": {"scope at columns[0]: ambiguous unqualified field CustomerId", true},
		"the category and the path, and another reason":              {"scope at columns[0]: cannot tell which source carries the unqualified field CustomerId, because a source of the query has no field list: qualify the field with its source", false},
		"the message of the fixture, at another path":                {"scope at columns[1]: ambiguous unqualified field CustomerId", false},
		"the message of the fixture about another field":             {"scope at columns[0]: ambiguous unqualified field Total", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := relIntMatches(t, fixture, relIntMatcherAnswer(tc.message)); got != tc.want {
				t.Fatalf("relIntMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

// An entry of the divergence file that pins a message, a route or rows holds the answer to
// each, and says which one differs.
func TestTheCorpusTestHoldsADivergenceEntryToEveryPinItHas(t *testing.T) {
	refused := relIntDivergence{Status: http.StatusBadRequest, Code: "invalid_dtql", Message: "ambiguous unqualified field X"}
	answered := func(route string, rows ...map[string]any) relHTTPResponse {
		records := make([]any, len(rows))
		for i, row := range rows {
			records[i] = map[string]any{"data": row}
		}
		return relHTTPResponse{status: http.StatusOK, body: map[string]any{
			"records": records, "execution": map[string]any{"route": route},
		}}
	}
	for name, tc := range map[string]struct {
		entry relIntDivergence
		resp  relHTTPResponse
		says  string // empty: the answer is the entry's
	}{
		"the status, the code and the message": {refused, relIntMatcherAnswer("scope at columns[0]: ambiguous unqualified field X"), ""},
		"another message":                      {refused, relIntMatcherAnswer("scope at columns[0]: unqualified field Y"), "the message holds"},
		"another status":                       {refused, answered("database"), "says status 400 invalid_dtql"},
		"another code":                         {relIntDivergence{Status: http.StatusBadRequest, Code: "bad_request"}, relIntMatcherAnswer("x"), "says status 400 bad_request"},
		"no code and no message pinned":        {relIntDivergence{Status: http.StatusBadRequest}, relIntMatcherAnswer("anything"), ""},
		"the route":                            {relIntDivergence{Status: http.StatusOK, Route: "database"}, answered("database"), ""},
		"another route":                        {relIntDivergence{Status: http.StatusOK, Route: "database"}, answered("in-memory"), "says the route is"},
		"the rows":                             {relIntDivergence{Status: http.StatusOK, Rows: []map[string]any{{"a": 1.0}}}, answered("database", map[string]any{"a": 1.0}), ""},
		"other rows":                           {relIntDivergence{Status: http.StatusOK, Rows: []map[string]any{{"a": 1.0}}}, answered("database", map[string]any{"a": 2.0}), "lists other rows"},
		"the route and the rows, the rows differing": {relIntDivergence{Status: http.StatusOK, Route: "database", Rows: []map[string]any{{"a": 1.0}}}, answered("database", map[string]any{"a": 2.0}), "lists other rows"},
	} {
		t.Run(name, func(t *testing.T) {
			got := relIntEntryMismatch(t, tc.entry, tc.resp)
			if tc.says == "" && got != "" || tc.says != "" && !strings.Contains(got, tc.says) {
				t.Fatalf("mismatch = %q, want one that says %q", got, tc.says)
			}
		})
	}
}
