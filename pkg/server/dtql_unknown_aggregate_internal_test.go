package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// unknownAggMarker is a function name no profile lists. DALgo's deserializer
// refuses an aggregate it does not know with the name quoted in the message.
const unknownAggMarker = "zzmarkerfn"

// TestAnUnknownAggregateNameIsRefusedWithABuiltMessageThatDoesNotRepeatIt: an
// aggregate function that DALgo does not know, in a column, in HAVING and in ORDER
// BY, is refused by DALgo's deserializer before the classifier looks at the
// document, with the caller's text quoted. The answer is the 400 invalid_dtql it
// always was, with a message built here that names the functions the profile
// has and none of the caller's text, in every spelling of the name, on both
// endpoints and by POST and GET.
func TestAnUnknownAggregateNameIsRefusedWithABuiltMessageThatDoesNotRepeatIt(t *testing.T) {
	positions := map[string]string{
		"a column": "from: {database: alpha, name: orders, alias: o}\ncolumns: [{aggregate: {function: NAME, args: [{field: total, source: o}]}, as: n}]\n",
		"HAVING": "from: {database: alpha, name: orders, alias: o}\ngroupBy: [{field: customer_id, source: o}]\n" +
			"having: {op: '>', left: {aggregate: {function: NAME, args: [{field: total, source: o}]}}, right: {value: 1}}\n" +
			"columns: [{field: customer_id, source: o}]\n",
		"ORDER BY": "from: {database: alpha, name: orders, alias: o}\ngroupBy: [{field: customer_id, source: o}]\n" +
			"orderBy: [{aggregate: {function: NAME, args: [{field: total, source: o}]}}]\n" +
			"columns: [{field: customer_id, source: o}]\n",
	}
	_, host := relFakeServer(t, &relFakeExecutor{}, relFakeDefaultMounts())
	for position, doc := range positions {
		for _, spelling := range []string{unknownAggMarker, strings.ToUpper(unknownAggMarker), "Zz\"Marker", "median"} {
			document := strings.ReplaceAll(doc, "NAME", "\""+strings.ReplaceAll(spelling, `"`, `\"`)+"\"")
			for _, route := range []struct{ name, method, path, body string }{
				{"POST /v1/dtql", http.MethodPost, "/v1/dtql", document},
				{"GET /v1/dtql", http.MethodGet, "/v1/dtql?q=" + url.QueryEscape(document), ""},
				{"POST per-database", http.MethodPost, "/v1/databases/alpha/dtql", strings.ReplaceAll(document, "database: alpha, ", "")},
			} {
				t.Run(position+"/"+spelling+"/"+route.name, func(t *testing.T) {
					resp := relFakeDo(t, host, route.method, route.path, "", route.body, nil)
					if resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" {
						t.Fatalf("status %d: %s", resp.status, resp.raw)
					}
					message, _ := resp.errorDetail()["message"].(string)
					if !strings.Contains(message, "an aggregate function must be one of count, sum, avg, min, max") {
						t.Errorf("message = %q, want the built sentence that lists the functions", message)
					}
					for _, echoed := range []string{unknownAggMarker, strings.ToUpper(unknownAggMarker), "Marker", "MARKER", "MEDIAN", "median"} {
						if strings.Contains(resp.raw, echoed) {
							t.Errorf("the answer repeats %q: %s", echoed, resp.raw)
						}
					}
				})
			}
		}
	}
}
