package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func aclWriteFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func aclPolicy(name, field, value string) string {
	return fmt.Sprintf(`apiVersion: dtql.org/access/v1
kind: AccessPolicy
metadata:
  name: %s
  visibility: private
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
          operations: [query, get]
          where:
            op: "=="
            left: {field: %s}
            right: {value: %s}
          fields: [id, name]
bindings:
  roles:
    reader: [reader]
`, name, field, value)
}

// A real HTTP server, token store, file-backed engines, and owner policy files.
// The first two rows distinguish upper/lower restrictions and pagination order.
func TestLayeredACL_DTQL(t *testing.T) {
	for _, profile := range []struct {
		engine string
		masks  bool
	}{{"ingitdb", false}, {"sqlite", false}, {"ingitdb", true}, {"sqlite", true}} {
		engine := profile.engine
		t.Run(fmt.Sprintf("%s/masks=%t", engine, profile.masks), func(t *testing.T) {
			dir := t.TempDir()
			manifestPath := filepath.Join(dir, "db.yaml")
			storagePath := "data"
			if engine == "sqlite" {
				storagePath = "data.sqlite"
			}
			manifestText := fmt.Sprintf(`database:
  id: crm
  schema_mode: strict
storage:
  engine: %s
  path: %s
schemas:
  collections:
    customers:
      fields:
        name: {type: string}
        tenant: {type: string}
        country: {type: string}
        secret: {type: string}
`, engine, storagePath)
			aclWriteFile(t, manifestPath, manifestText)
			bootstrap, err := mount.File(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range []struct{ id, tenant, country string }{{"01", "B", "IE"}, {"02", "A", "US"}, {"03", "A", "IE"}} {
				_, err := bootstrap.Apply(context.Background(), []core.Op{{Op: "insert", Key: record.NewKeyWithID("customers", row.id), Data: map[string]any{"name": "Customer " + row.id, "tenant": row.tenant, "country": row.country, "secret": "private-value"}}}, "seed")
				if err != nil {
					t.Fatal(err)
				}
			}
			upper := aclPolicy("upper-private", "country", "IE")
			if profile.masks {
				upper = strings.Replace(upper, "fields: [id, name]", "fieldMask: {stages: [{include: ['*']}, {exclude: [tenant, country, secret]}]}", 1)
				upper += "collectionMask: {stages: [{include: ['*']}, {exclude: ['sys_*']}]}\nexecution: {allow: [{class: dtql}]}\n"
			}
			aclWriteFile(t, filepath.Join(dir, "upper.yaml"), upper)
			manifestText += "acl:\n  enabled: true\n  policies: [upper.yaml]\n"
			aclWriteFile(t, manifestPath, manifestText)
			if engine == "ingitdb" {
				ownerRoot := filepath.Join(dir, "data", ".ingitdb", "access")
				aclWriteFile(t, filepath.Join(ownerRoot, "lower.yaml"), aclPolicy("lower-private", "tenant", "A"))
				aclWriteFile(t, filepath.Join(ownerRoot, "manifest.yaml"), "enabled: true\ndatabase: crm\npolicies: [lower.yaml]\n")
			}
			store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			token := "test-scoped-reader"
			if err := store.CreateGrant(&auth.Grant{PrincipalID: "app-reader", DatabaseID: "crm", Capabilities: []auth.Capability{{Action: auth.CapRecordsRead, Collection: "customers"}}}, token); err != nil {
				t.Fatal(err)
			}
			for reload := 0; reload < 2; reload++ {
				db, err := mount.File(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				ts := httptest.NewServer(server.New("test", map[string]*core.Database{"crm": db}, server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}), server.WithPrincipalResolver(func(_ context.Context, actor *auth.Principal) (access.Principal, error) {
					if actor.Grant != nil && actor.Grant.PrincipalID == "app-reader" {
						return access.Principal{Roles: []string{"reader"}}, nil
					}
					return access.Principal{}, nil
				})).Handler())
				query := "from: {name: customers}\norderBy: [{field: name}]\nlimit: 1\n"
				status, body := request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", token, query)
				if status != http.StatusOK {
					ts.Close()
					t.Fatalf("reload %d: status %d: %s", reload, status, body)
				}
				var result struct {
					Records []struct {
						Key  string         `json:"key"`
						Data map[string]any `json:"data"`
					} `json:"records"`
				}
				if err := json.Unmarshal([]byte(body), &result); err != nil {
					t.Fatal(err)
				}
				want := "01"
				if engine == "ingitdb" {
					want = "03"
				}
				if len(result.Records) != 1 || !strings.HasSuffix(result.Records[0].Key, "/"+want) || result.Records[0].Data["name"] != "Customer "+want {
					t.Fatalf("wrong filtered/paged data: %s", body)
				}

				projectedStatus, projectedBody := request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", token, query+"columns: [{field: name}]\n")
				if projectedStatus != http.StatusOK {
					t.Fatalf("explicit projection: %d %s", projectedStatus, projectedBody)
				}
				var projected struct {
					Records []struct {
						Key  string         `json:"key"`
						Data map[string]any `json:"data"`
					} `json:"records"`
				}
				if err := json.Unmarshal([]byte(projectedBody), &projected); err != nil {
					t.Fatal(err)
				}
				if len(projected.Records) != 1 || !strings.HasSuffix(projected.Records[0].Key, "/"+want) || len(projected.Records[0].Data) != 1 || projected.Records[0].Data["name"] != "Customer "+want {
					t.Fatalf("projection lost key or leaked fields: %s", projectedBody)
				}
				for _, hidden := range []string{"secret", "tenant", "country"} {
					if _, exists := result.Records[0].Data[hidden]; exists {
						t.Fatalf("leaked %s: %s", hidden, body)
					}
				}
				for _, suffix := range []string{
					"where: {op: '==', left: {field: secret}, right: {value: private-value}}\n",
					"orderBy: [{field: secret}]\n",
					"columns: [{field: secret}]\n",
				} {
					status, body = request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", token, "from: {name: customers}\n"+suffix)
					if status != http.StatusForbidden || !strings.Contains(body, "ACCESS_DENIED") {
						t.Fatalf("hidden field probe: %d %s", status, body)
					}
					for _, sensitive := range []string{"upper-private", "lower-private", "private-value", "upper.yaml", "lower.yaml"} {
						if strings.Contains(body, sensitive) {
							t.Fatalf("diagnostic leaked %s: %s", sensitive, body)
						}
					}
				}
				status, body = request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", ownerToken, query)
				if status != http.StatusForbidden {
					t.Fatalf("owner bypassed policies: %d %s", status, body)
				}
				status, body = request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", token, "from: {name: other}\n")
				if status != http.StatusForbidden {
					t.Fatalf("token collection scope bypass: %d %s", status, body)
				}
				ts.Close()
			}
		})
	}
}

// An upper-only policy must not expose a computed field derived from a denied
// stored field merely because the backend has no owner ACL manifest.
func TestLayeredACL_UpperOnlySuppressesInGitDBDerivedValues(t *testing.T) {
	dir := t.TempDir()
	aclWriteFile(t, filepath.Join(dir, "data", ".ingitdb", "root-collections.yaml"), "customers: customers\n")
	aclWriteFile(t, filepath.Join(dir, "data", "customers", ".collection", "definition.yaml"), `id: customers
record_file:
  name: "{key}.yaml"
  format: yaml
  type: map[string]any
columns:
  country: {type: string}
  secret: {type: string}
  name:
    type: string
    formula: 'secret + "-derived"'
`)
	aclWriteFile(t, filepath.Join(dir, "data", "customers", "$records", "01.yaml"), "country: IE\nsecret: protected-input\n")
	aclWriteFile(t, filepath.Join(dir, "upper.yaml"), aclPolicy("upper", "country", "IE"))
	path := filepath.Join(dir, "db.yaml")
	aclWriteFile(t, path, `database:
  id: crm
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: data
acl:
  enabled: true
  policies: [upper.yaml]
`)
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New("test", map[string]*core.Database{"crm": db},
		server.WithAuth(&auth.Config{OwnerToken: ownerToken}),
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
	).Handler())
	defer ts.Close()
	status, body := request(t, ts, http.MethodPost, "/v1/databases/crm/dtql", ownerToken, "from: {name: customers}\ncolumns: [{field: name}]\n")
	if status != http.StatusOK {
		t.Fatalf("%d: %s", status, body)
	}
	var result struct {
		Records []struct{ Data map[string]any }
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 1 || result.Records[0].Data["name"] != nil || strings.Contains(body, "protected-input") {
		t.Fatalf("computed value escaped upper policy: %s", body)
	}
	aclWriteFile(t, filepath.Join(dir, "upper.yaml"), aclPolicy("upper", "country", "IE")+"execution: {allow: []}\n")
	deniedDB, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	deniedServer := httptest.NewServer(server.New("test", map[string]*core.Database{"crm": deniedDB},
		server.WithAuth(&auth.Config{OwnerToken: ownerToken}),
		server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
			return access.Principal{Roles: []string{"reader"}}, nil
		}),
	).Handler())
	defer deniedServer.Close()
	status, body = request(t, deniedServer, http.MethodPost, "/v1/databases/crm/dtql", ownerToken, "from: {name: customers}\n")
	if status != http.StatusForbidden {
		t.Fatalf("execution gate: %d %s", status, body)
	}

}
