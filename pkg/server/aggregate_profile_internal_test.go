package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The relational profile has five aggregate functions. A document that uses first
// or last, in any letter case, is refused by the classifier before anything is
// read, on every route. The helpers of this file all start with aggregateProfile
// so they cannot clash with the others of the package.

// aggregateProfileShapes are the four shapes of a document that carries an
// aggregate call: one source on the per-database endpoint, a join inside one
// database on each endpoint, and a join across two databases.
func aggregateProfileShapes(function string) map[string]struct{ path, body string } {
	aggregate := "{aggregate: {function: " + function + ", args: [{field: total, source: o}]}, as: x}"
	join := func(database, other string) string {
		return "from:\n  " + database + "name: orders\n  alias: o\n  joins:\n    - type: left\n      from: {" + other + "name: customers, alias: c}\n      on:\n        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}\ncolumns: [" + aggregate + "]\n"
	}
	return map[string]struct{ path, body string }{
		"one source":                  {"/v1/databases/alpha/dtql", "from: {name: orders, alias: o}\ncolumns: [" + aggregate + "]\n"},
		"a join":                      {"/v1/databases/alpha/dtql", join("", "")},
		"a join on /v1/dtql":          {"/v1/dtql", join("database: alpha\n  ", "database: alpha, ")},
		"a join across two databases": {"/v1/dtql", "from:\n  database: alpha\n  name: orders\n  alias: o\n  joins:\n    - type: left\n      from: {database: beta, name: customers, alias: c}\n      on:\n        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}\ncolumns: [" + aggregate + "]\n"},
	}
}

func TestFirstAndLastAreRefusedByTheClassifierBeforeAnythingIsRead(t *testing.T) {
	for _, function := range []string{"first", "last", "FIRST", "Last"} {
		for name, shape := range aggregateProfileShapes(function) {
			t.Run(function+" "+name, func(t *testing.T) {
				fake := &relFakeExecutor{}
				// The mounts are fakes whose driver is never reached: a read of one is a
				// fault of the test, and the executor is counted.
				_, host := relFakeServer(t, fake, relFakeDefaultMounts())
				resp := relFakeDo(t, host, http.MethodPost, shape.path, "", shape.body, nil)
				want := fmt.Sprintf("%s is not in the relational profile", strings.ToLower(function))
				if resp.status != http.StatusBadRequest || resp.code() != "invalid_dtql" || !strings.Contains(resp.errorDetail()["message"].(string), want) {
					t.Fatalf("status %d, want 400 invalid_dtql naming %q: %s", resp.status, want, resp.raw)
				}
				if fake.count() != 0 {
					t.Fatalf("the executor was called %d times for a refused document", fake.count())
				}
			})
		}
	}
}
