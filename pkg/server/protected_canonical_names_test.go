package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
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

// canonicalNamesPolicy admits the rows of country IE for reading and updating
// under both spellings of the declared collection: a binding on the public name
// customers and a binding on its quoted spelling.
const canonicalNamesPolicy = `apiVersion: dtql.org/access/v1
kind: AccessPolicy
metadata:
  name: upper
  visibility: public
target:
  database: crm
composition: dalgo-hierarchical-v1
default: deny
ruleSets:
  reader:
    - path: /customers
      rules:
        - id: read-visible
          effect: allow
          operations: [query, get, update]
          where:
            op: "=="
            left: {field: country}
            right: {value: IE}
          fields: [id, name]
    - path: '/"customers"'
      rules:
        - id: read-visible-quoted
          effect: allow
          operations: [query, get, update]
          where:
            op: "=="
            left: {field: country}
            right: {value: IE}
          fields: [id, name]
bindings:
  roles:
    reader: [reader]
`

// canonicalNamesFixture is a SQLite mount behind an owner ACL whose only
// collection, customers, is declared by its quoted SQL identifier. The file also
// holds a table named with the quote characters, so a request that reaches it
// is seen in its rows.
type canonicalNamesFixture struct {
	ts   *httptest.Server
	file sqlNamesFixture
}

func startCanonicalNames(t *testing.T) canonicalNamesFixture {
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
	aclWriteFile(t, filepath.Join(dir, "upper.yaml"), canonicalNamesPolicy)
	aclWriteFile(t, path, manifest+"acl: {enabled: true, policies: [upper.yaml]}\n")
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := server.New("test", map[string]*core.Database{"crm": db},
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
		server.WithAuth(&auth.Config{OwnerToken: ownerToken}),
		server.WithOwnerAuthorization(func(_ context.Context, _ *auth.Principal, _ az.Source, capability string, _ az.Resource) bool {
			return capability == auth.CapAccessDiagnostics
		}))
	t.Cleanup(service.CloseSnapshots)
	ts := httptest.NewServer(service.Handler())
	t.Cleanup(ts.Close)
	return canonicalNamesFixture{ts: ts, file: file}
}

func (f canonicalNamesFixture) quotedTableUntouched(t *testing.T) {
	t.Helper()
	if got := f.file.rows(t, `"customers"`); got != "01=Quoted" {
		t.Errorf("the table named with quotes holds %q, want 01=Quoted", got)
	}
}

// TestByKeyAuthorizationRoutesTakeTheCanonicalCollectionName: the protected
// PATCH of /records, /access/evidence and the inspection of /access/evaluate
// take the canonical name of a collection on an engine that builds SQL. The
// quoted spelling of a SQLite key names a different table of the file (the one
// whose name carries the quote characters), so the owner's request under it
// neither reads nor changes that table, though a policy binding covers the
// quoted path. The canonical name works on the same routes.
func TestByKeyAuthorizationRoutesTakeTheCanonicalCollectionName(t *testing.T) {
	asJSON := func(v any) string {
		data, _ := json.Marshal(v)
		return string(data)
	}
	routes := []struct {
		name string
		do   func(f canonicalNamesFixture, t *testing.T, collection string) (int, string)
	}{
		{"protected PATCH", func(f canonicalNamesFixture, t *testing.T, c string) (int, string) {
			op := writeGuardProtectedOp("update", "/"+c+"/01", writeGuardProtectedSet("name"))
			return writeGuardRequest(t, f.ts, "PATCH", "/v1/databases/crm/records/"+url.PathEscape(c)+"/01", "application/vnd.dtql.operation+json", asJSON(op))
		}},
		{"evidence", func(f canonicalNamesFixture, t *testing.T, c string) (int, string) {
			body := map[string]any{"apiVersion": az.APIVersion, "resource": az.Resource{DatabaseID: "crm", Path: "/" + c + "/01"}, "requiredFields": [][]string{{"name"}}}
			return writeGuardRequest(t, f.ts, "POST", "/v1/databases/crm/access/evidence", "application/json", asJSON(body))
		}},
		{"inspection", func(f canonicalNamesFixture, t *testing.T, c string) (int, string) {
			op := writeGuardProtectedOp("get", "/"+c+"/01", nil)
			request := api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: []api.Operation{op}}
			return writeGuardRequest(t, f.ts, "POST", "/v1/databases/crm/access/evaluate", "application/json", asJSON(request))
		}},
	}
	for _, r := range routes {
		t.Run(r.name+"/quoted spelling", func(t *testing.T) {
			f := startCanonicalNames(t)
			status, body := r.do(f, t, `"customers"`)
			if status == http.StatusOK && strings.Contains(body, `"allowed":true`) || strings.Contains(body, "Quoted") || strings.Contains(body, "dataRevision") || strings.Contains(body, `"state":"present"`) {
				t.Errorf("the quoted spelling was answered as a declared table: status %d: %s", status, body)
			}
			f.quotedTableUntouched(t)
			if got := f.file.rows(t, "customers"); got != "01=Original" {
				t.Errorf("customers holds %q, want 01=Original", got)
			}
		})
		t.Run(r.name+"/canonical name", func(t *testing.T) {
			f := startCanonicalNames(t)
			status, body := r.do(f, t, "customers")
			if status != http.StatusOK {
				t.Fatalf("the canonical name: status %d: %s", status, body)
			}
			f.quotedTableUntouched(t)
		})
	}
}
