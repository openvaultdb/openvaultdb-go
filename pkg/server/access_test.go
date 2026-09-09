package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func accessPlanRequest(query string) string {
	input := api.Request{APIVersion: az.APIVersion, Mode: az.ModePlan, DiagnosticLevel: "references", Operations: []api.Operation{{ID: "q1", Action: "query", Resource: az.Resource{DatabaseID: "crm", Path: "/customers"}, ExecutionClass: az.ExecutionDTQL, Query: &api.Query{Format: "dtql-yaml", Text: query}}}}
	data, _ := json.Marshal(input)
	return string(data)
}

func TestAccessPlanLayersAndPrivateDisclosure(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb"} {
		t.Run(engine, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "db.yaml")
			storage := "data"
			if engine == "sqlite" {
				storage = "data.sqlite"
			}
			manifest := fmt.Sprintf("database: {id: crm, schema_mode: strict}\nstorage: {engine: %s, path: %s}\nschemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n        country: {type: string}\n        tenant: {type: string}\n", engine, storage)
			aclWriteFile(t, path, manifest)
			if _, err := mount.File(path); err != nil {
				t.Fatal(err)
			}
			aclWriteFile(t, filepath.Join(dir, "upper.yaml"), strings.Replace(aclPolicy("public-upper", "country", "IE"), "visibility: private", "visibility: public", 1))
			aclWriteFile(t, path, manifest+"acl: {enabled: true, policies: [upper.yaml]}\n")
			if engine == "ingitdb" {
				owner := filepath.Join(dir, "data", ".ingitdb", "access")
				aclWriteFile(t, filepath.Join(owner, "manifest.yaml"), "enabled: true\ndatabase: crm\npolicies: [lower.yaml]\n")
				aclWriteFile(t, filepath.Join(owner, "lower.yaml"), aclPolicy("hidden-lower-policy", "tenant", "secret-tenant"))
			}
			db, err := mount.File(path)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(server.New("test", map[string]*core.Database{"crm": db}, server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
				return access.Principal{Roles: []string{"reader"}}, nil
			}), server.WithAuth(&auth.Config{OwnerToken: ownerToken})).Handler())
			defer ts.Close()
			status, body := request(t, ts, http.MethodPost, "/v1/databases/crm/access/evaluate", ownerToken, accessPlanRequest("from: {name: customers}\ncolumns: [{field: name}]\n"))
			if status != 200 {
				t.Fatalf("status %d: %s", status, body)
			}
			result, err := az.ParseResult([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if result.Result != az.OutcomeConditional || result.Allowed || result.Mode != az.ModePlan || result.Coverage.Evaluation != az.EvaluationComplete {
				t.Fatalf("unexpected result: %s", body)
			}
			wantLayers := 1
			if engine == "ingitdb" {
				wantLayers = 2
			}
			if len(result.Layers) != wantLayers {
				t.Fatalf("missing layers: %s", body)
			}
			if !strings.Contains(body, "public-upper") || strings.Contains(body, "hidden-lower-policy") || strings.Contains(body, "secret-tenant") || strings.Contains(body, "policyRevision") {
				t.Fatalf("incorrect disclosure: %s", body)
			}
			for _, restriction := range result.Restrictions {
				if restriction.Enforced {
					t.Fatal("dry run marked enforced")
				}
			}
			policyRequest := strings.Replace(accessPlanRequest("from: {name: customers}\ncolumns: [{field: name}]\n"), `"diagnosticLevel":"references"`, `"diagnosticLevel":"policy"`, 1)
			status, body = request(t, ts, http.MethodPost, "/v1/databases/crm/access/evaluate", ownerToken, policyRequest)
			if status != 200 || !strings.Contains(body, `"representation":"expression"`) || !strings.Contains(body, `"field":"country"`) || strings.Contains(body, "secret-tenant") {
				t.Fatalf("source condition disclosure: %d %s", status, body)
			}
			status, body = request(t, ts, http.MethodGet, "/v1/databases/crm/access/policies?layerId=openvaultdb:local:crm", ownerToken, "")
			if status != 200 || !strings.Contains(body, "public-upper") || !strings.Contains(body, `"editable":false`) || strings.Contains(body, "policyRevision") {
				t.Fatalf("policy metadata: %d %s", status, body)
			}
			status, body = request(t, ts, http.MethodGet, "/v1/databases/crm/access/policies?layerId=ingitdb:local:crm", ownerToken, "")
			if status != 200 || strings.Contains(body, "hidden-lower-policy") {
				t.Fatalf("upper admin crossed lower owner: %d %s", status, body)
			}
			// The original query fields, including a hidden predicate, are assessed
			// by the same field guard used for real queries.
			status, body = request(t, ts, http.MethodPost, "/v1/databases/crm/access/evaluate", ownerToken, accessPlanRequest("from: {name: customers}\ncolumns: [{field: country}]\n"))
			if status != 200 {
				t.Fatalf("hidden field status %d: %s", status, body)
			}
			result, err = az.ParseResult([]byte(body))
			if err != nil || result.Result != az.OutcomeDeny || result.Allowed {
				t.Fatalf("hidden field assessment: %v %s", err, body)
			}
		})
	}
}

func TestDiagnosticCapabilitiesParse(t *testing.T) {
	for _, name := range []string{auth.CapPoliciesDiscover, auth.CapPoliciesAdmin, auth.CapAccessExplain, auth.CapAccessSimulate, auth.CapAccessDiagnostics, auth.CapAccessInspectProtected} {
		capability, err := auth.ParseCapability(name + ":customers")
		if err != nil || capability.Action != name || capability.Collection != "customers" {
			t.Fatalf("capability %s: %+v %v", name, capability, err)
		}
	}
}
