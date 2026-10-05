package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// grantToken stores a grant of the given capabilities on one database and
// returns the bearer token that holds it.
func grantToken(t *testing.T, store *auth.Store, databaseID, token string, capabilities ...auth.Capability) string {
	t.Helper()
	if err := store.CreateGrant(&auth.Grant{DatabaseID: databaseID, Capabilities: capabilities}, token); err != nil {
		t.Fatal(err)
	}
	return token
}

// quotedTableUntouched asserts that the table named with quote characters still
// holds its one seeded row.
func (f sqlNamesFixture) quotedTableUntouched(t *testing.T) {
	t.Helper()
	if got, want := f.rows(t, sqlNamesQuotedTable.table), "1="+sqlNamesQuotedTable.marker; got != want {
		t.Errorf("table %q holds %q, want %q", sqlNamesQuotedTable.table, got, want)
	}
}

// TestCapabilityOnQueryRoutesIsCheckedOnTheCollectionAsWritten: /query and /dtql
// hand the adapter the collection as written, and on SQLite that name is the
// table of that exact name. A grant is therefore matched against the spelling
// sent: a grant on the public name Order Details does not cover the quoted
// spelling, and a grant on the quoted spelling does not cover the public name.
// The file also holds a table named with the quote characters, which no request
// under the grant on the public name may read.
func TestCapabilityOnQueryRoutesIsCheckedOnTheCollectionAsWritten(t *testing.T) {
	const publicName, quotedName = "Order Details", `"Order Details"`
	queryBody := func(collection string) string {
		out, _ := json.Marshal(map[string]string{"collection": collection})
		return string(out)
	}
	dtql := func(collection string) string { return "from: {name: '" + collection + "'}\n" }
	routes := []struct {
		name string
		call func(collection string) guardCall
	}{
		{"query GET", func(c string) guardCall {
			return guardCall{method: "GET", path: "/v1/databases/dev/query?q=" + url.QueryEscape(queryBody(c))}
		}},
		{"query POST", func(c string) guardCall {
			return guardCall{method: "POST", path: "/v1/databases/dev/query", body: queryBody(c)}
		}},
		{"dtql GET", func(c string) guardCall {
			return guardCall{method: "GET", path: "/v1/databases/dev/dtql?q=" + url.QueryEscape(dtql(c))}
		}},
		{"dtql POST", func(c string) guardCall {
			return guardCall{method: "POST", path: "/v1/databases/dev/dtql", body: dtql(c)}
		}},
	}
	for _, c := range []struct {
		grant, collection string
		status            int
	}{
		{publicName, publicName, http.StatusOK},
		{publicName, quotedName, http.StatusForbidden},
		{quotedName, publicName, http.StatusForbidden},
		{"Orders", publicName, http.StatusForbidden},
		{"Orders", quotedName, http.StatusForbidden},
	} {
		t.Run("grant on "+c.grant+" for "+c.collection, func(t *testing.T) {
			store, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			f := startSQLNamesWithQuotedTable(t, server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}))
			token := grantToken(t, store, "dev", "ovdb_test_query_token", auth.Capability{Action: auth.CapRecordsRead, Collection: c.grant})
			for _, route := range routes {
				call := route.call(c.collection)
				call.headers = map[string]string{"Authorization": "Bearer " + token}
				status, body := send(t, f.ts, call)
				answer, _ := json.Marshal(body)
				if status != c.status {
					t.Errorf("%s: status %d, want %d: %s", route.name, status, c.status, answer)
				}
				if c.status == http.StatusOK && !strings.Contains(string(answer), "row of Order Details") {
					t.Errorf("%s: the answer is not the row of Order Details: %s", route.name, answer)
				}
				if strings.Contains(string(answer), sqlNamesQuotedTable.marker) {
					t.Errorf("%s: the answer carries the row of the table named with quotes: %s", route.name, answer)
				}
			}
			f.untouched(t)
			f.quotedTableUntouched(t)
		})
	}
}

// protectedQuoted is a SQLite mount behind an owner ACL whose only collection,
// customers, is declared by its quoted SQL identifier ('"customers"'). The file
// also holds a table named with the quote characters, and one grant holds the
// token.
type protectedQuoted struct {
	ts   *httptest.Server
	file sqlNamesFixture
}

func startProtectedQuoted(t *testing.T, token string, capabilities ...auth.Capability) protectedQuoted {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	manifest := "database: {id: crm, schema_mode: strict}\nstorage: {engine: sqlite, path: data.sqlite}\nschemas:\n  collections:\n    '\"customers\"':\n      fields:\n        name: {type: string}\n        country: {type: string}\n"
	aclWriteFile(t, path, manifest)
	file := sqlNamesFixture{path: filepath.Join(dir, "data.sqlite")}
	raw, err := sql.Open("sqlite", file.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE customers (id TEXT PRIMARY KEY, name TEXT, country TEXT)`,
		`INSERT INTO customers VALUES ('01', 'Original', 'IE')`,
		`CREATE TABLE ` + sqlIdent(`"customers"`) + ` (id TEXT PRIMARY KEY, name TEXT, country TEXT)`,
		`INSERT INTO ` + sqlIdent(`"customers"`) + ` VALUES ('01', 'Quoted', 'IE')`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	policy := strings.Replace(aclPolicy("upper", "country", "IE"), "visibility: private", "visibility: public", 1)
	policy = strings.Replace(policy, "operations: [query, get]", "operations: [query, get, update]", 1)
	aclWriteFile(t, filepath.Join(dir, "upper.yaml"), policy)
	aclWriteFile(t, path, manifest+"acl: {enabled: true, policies: [upper.yaml]}\n")
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	grantToken(t, store, "crm", token, capabilities...)
	service := server.New("test", map[string]*core.Database{"crm": db},
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}),
		server.WithOwnerAuthorization(func(_ context.Context, _ *auth.Principal, _ az.Source, capability string, _ az.Resource) bool {
			return capability == auth.CapAccessDiagnostics
		}))
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	return protectedQuoted{ts: ts, file: file}
}

// do sends a request as the holder of token and returns the status and body.
func (p protectedQuoted) do(t *testing.T, token, method, path, content, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, p.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", content)
	resp, err := p.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestCapabilityOnProtectedRoutesIsCheckedOnTheCollectionAsWritten: the protected
// update, the sampling and evidence endpoints of the authorization API, and
// /query and /dtql on a mount with access policies, address the collection as
// written. A grant on the public name customers covers a request that names it
// and is refused for the quoted spelling, whose name is also the name of a table
// of the file that no request under that grant may read or change.
func TestCapabilityOnProtectedRoutesIsCheckedOnTheCollectionAsWritten(t *testing.T) {
	const token = "ovdb_test_protected_token"
	type call struct{ method, path, content, body string }
	asJSON := func(v any) string {
		data, _ := json.Marshal(v)
		return string(data)
	}
	for _, c := range []struct {
		name string
		call func(collection string) call
	}{
		{"protected update", func(c string) call {
			op := writeGuardProtectedOp("update", "/"+c+"/01", writeGuardProtectedSet("name"))
			return call{"PATCH", "/v1/databases/crm/records/" + url.PathEscape(c) + "/01", "application/vnd.dtql.operation+json", asJSON(op)}
		}},
		{"sample", func(c string) call {
			op := writeGuardProtectedOp("update", "/"+c, writeGuardProtectedSet("name"))
			request := api.Request{APIVersion: az.APIVersion, Mode: az.ModeSample, DiagnosticLevel: "ordinary", Operations: []api.Operation{op},
				Sample: &api.Sample{Limit: 1, Query: api.Query{Format: "dtql-yaml", Text: "from: {name: '" + c + "'}\n"}}}
			return call{"POST", "/v1/databases/crm/access/evaluate", "application/json", asJSON(request)}
		}},
		{"evidence", func(c string) call {
			body := map[string]any{"apiVersion": az.APIVersion, "resource": az.Resource{DatabaseID: "crm", Path: "/" + c + "/01"}, "requiredFields": [][]string{{"name"}}}
			return call{"POST", "/v1/databases/crm/access/evidence", "application/json", asJSON(body)}
		}},
		{"query", func(c string) call {
			return call{"POST", "/v1/databases/crm/query", "application/json", asJSON(map[string]string{"collection": c})}
		}},
		{"dtql", func(c string) call {
			return call{"POST", "/v1/databases/crm/dtql", "application/yaml", "from: {name: '" + c + "'}\n"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := startProtectedQuoted(t, token,
				auth.Capability{Action: auth.CapRecordsRead, Collection: "customers"},
				auth.Capability{Action: auth.CapRecordsWrite, Collection: "customers"})
			sent := c.call("customers")
			// What the mount answers for the public name is its ACL's business; the
			// capability check must not be the one that refuses it.
			if status, body := p.do(t, token, sent.method, sent.path, sent.content, sent.body); status == http.StatusForbidden {
				t.Errorf("the public name: status %d: %s", status, body)
			}
			sent = c.call(`"customers"`)
			status, body := p.do(t, token, sent.method, sent.path, sent.content, sent.body)
			if status != http.StatusForbidden || strings.Contains(body, "Quoted") {
				t.Errorf("the quoted spelling: status %d, want 403: %s", status, body)
			}
			if got := p.file.rows(t, `"customers"`); got != "01=Quoted" {
				t.Errorf("the table named with quotes holds %q, want 01=Quoted", got)
			}
		})
	}
}

// TestProtectedKeyReadsSeeThePublicNameWhicheverSpellingIsSent: on a mount with
// access policies, a key read is evaluated under the name the adapter is given.
// A policy that covers the public name customers therefore decides a read of the
// key written in either spelling, and a grant on the public name covers both.
func TestProtectedKeyReadsSeeThePublicNameWhicheverSpellingIsSent(t *testing.T) {
	const token = "ovdb_test_protected_read_token"
	p := startProtectedQuoted(t, token, auth.Capability{Action: auth.CapRecordsRead, Collection: "customers"})
	for _, spelling := range []string{"customers", `"customers"`} {
		status, body := p.do(t, token, "GET", "/v1/databases/crm/records/"+url.PathEscape(spelling)+"/01", "application/json", "")
		if status != http.StatusOK || !strings.Contains(body, "Original") || strings.Contains(body, "Quoted") {
			t.Errorf("GET as %s: status %d: %s", spelling, status, body)
		}
	}
	if got := p.file.rows(t, `"customers"`); got != "01=Quoted" {
		t.Errorf("the table named with quotes holds %q, want 01=Quoted", got)
	}
}

// TestProtectedMountRefusesPlainWritesAfterTheCapabilityCheck: a mount with
// access policies takes writes as protected operations only. A plain write under
// a grant that covers its collection, in either spelling, is 422
// authorization_unsupported and changes nothing; the protected update is the one
// PATCH it accepts, so the same PATCH with another content type is refused.
func TestProtectedMountRefusesPlainWritesAfterTheCapabilityCheck(t *testing.T) {
	const token = "ovdb_test_protected_write_token"
	p := startProtectedQuoted(t, token,
		auth.Capability{Action: auth.CapRecordsWrite, Collection: "customers"},
		auth.Capability{Action: auth.CapRecordsDelete, Collection: "customers"})
	for _, spelling := range []string{"customers", `"customers"`} {
		path := "/v1/databases/crm/records/" + url.PathEscape(spelling) + "/01"
		for _, c := range []struct{ method, body string }{
			{"PUT", `{"data":{"name":"x"}}`},
			{"POST", `{"data":{"name":"x"}}`},
			{"PATCH", `{"updates":[{"fieldName":"name","value":"x"}]}`},
			{"DELETE", ""},
		} {
			status, body := p.do(t, token, c.method, path, "application/json", c.body)
			if status != http.StatusUnprocessableEntity || !strings.Contains(body, "authorization_unsupported") {
				t.Errorf("%s as %s: status %d: %s", c.method, spelling, status, body)
			}
		}
	}
	if got := p.file.rows(t, `"customers"`); got != "01=Quoted" {
		t.Errorf("the table named with quotes holds %q, want 01=Quoted", got)
	}
	if got := p.file.rows(t, "customers"); got != "01=Original" {
		t.Errorf("customers holds %q, want 01=Original", got)
	}
}
