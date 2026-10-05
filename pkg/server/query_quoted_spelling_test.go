package server_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// TestQueryRoutesNameADeclaredCollectionByItsPublicNameOnly: the manifest of the
// SQLite fixture keys a collection by its quoted identifier, "Order Details", and
// the file holds a table whose name carries the quote characters, which the
// manifest does not declare. A query route gives the adapter the collection as
// written, so it reads the declared table for the public name and refuses the
// quoted spelling with 404 not_found, for the owner and for a token whose grant
// names the quoted spelling, at the root of a document and inside a subquery. The
// table named with quotes is never read.
func TestQueryRoutesNameADeclaredCollectionByItsPublicNameOnly(t *testing.T) {
	const publicName, quotedName = "Order Details", `"Order Details"`
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := startSQLNamesWithQuotedTable(t, server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}))
	const quotedGrantToken = "ovdb_test_quoted_spelling_grant"
	grantToken(t, store, "dev", quotedGrantToken,
		auth.Capability{Action: auth.CapRecordsRead, Collection: quotedName},
		auth.Capability{Action: auth.CapRecordsRead, Collection: publicName})

	wire := func(collection string) string {
		out, _ := json.Marshal(map[string]string{"collection": collection})
		return string(out)
	}
	dtql := func(collection string) string { return "from: {name: '" + collection + "'}\n" }
	inSubquery := func(collection string) string {
		return "from: {name: '" + publicName + "'}\nwhere: {exists: {query: {from: {name: '" + collection + "'}}}}\n"
	}
	type route struct {
		name string
		call func(collection string) guardCall
	}
	documents := func(document func(string) string) []route {
		return []route{
			{"GET", func(c string) guardCall {
				return guardCall{method: "GET", path: "/v1/databases/dev/dtql?q=" + url.QueryEscape(document(c))}
			}},
			{"POST", func(c string) guardCall {
				return guardCall{method: "POST", path: "/v1/databases/dev/dtql", body: document(c)}
			}},
			{"snapshot page", func(c string) guardCall {
				return guardCall{method: "POST", path: "/v1/databases/dev/dtql", body: document(c), headers: map[string]string{"OVDB-Page-Size": "10"}}
			}},
		}
	}
	routes := append([]route{
		{"query GET", func(c string) guardCall {
			return guardCall{method: "GET", path: "/v1/databases/dev/query?q=" + url.QueryEscape(wire(c))}
		}},
		{"query POST", func(c string) guardCall {
			return guardCall{method: "POST", path: "/v1/databases/dev/query", body: wire(c)}
		}},
	}, documents(dtql)...)
	for _, token := range []string{ownerToken, quotedGrantToken} {
		withToken := func(call guardCall) guardCall {
			headers := map[string]string{"Authorization": "Bearer " + token}
			for name, value := range call.headers {
				headers[name] = value
			}
			call.headers = headers
			return call
		}
		for _, r := range routes {
			// A snapshot page that succeeds holds one of the server's snapshot slots
			// until it expires, so the public name is not read through it here.
			if r.name != "snapshot page" {
				status, body := send(t, f.ts, withToken(r.call(publicName)))
				answer, _ := json.Marshal(body)
				if status != http.StatusOK || !strings.Contains(string(answer), "row of Order Details") || strings.Contains(string(answer), sqlNamesQuotedTable.marker) {
					t.Errorf("%s, public name: %d %s", r.name, status, answer)
				}
			}
			status, body := send(t, f.ts, withToken(r.call(quotedName)))
			answer, _ := json.Marshal(body)
			if status != http.StatusNotFound || errorCodeOf(body) != "not_found" || strings.Contains(string(answer), sqlNamesQuotedTable.marker) {
				t.Errorf("%s, quoted spelling: %d %s", r.name, status, answer)
			}
		}
		for _, r := range documents(inSubquery) {
			status, body := send(t, f.ts, withToken(r.call(quotedName)))
			answer, _ := json.Marshal(body)
			if status != http.StatusNotFound || errorCodeOf(body) != "not_found" || strings.Contains(string(answer), sqlNamesQuotedTable.marker) {
				t.Errorf("subquery %s, quoted spelling: %d %s", r.name, status, answer)
			}
		}
	}
	f.untouched(t)
	f.quotedTableUntouched(t)
}

// errorCodeOf is the code of the error body a route answered with.
func errorCodeOf(body map[string]any) string {
	detail, _ := body["error"].(map[string]any)
	code, _ := detail["code"].(string)
	return code
}
