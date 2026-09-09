package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func TestProtectedHTTPReadInspectUpdate(t *testing.T) {
	for _, engine := range []string{"sqlite", "ingitdb"} {
		t.Run(engine, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "db.yaml")
			storage := "data"
			if engine == "sqlite" {
				storage = "data.sqlite"
			}
			manifest := fmt.Sprintf("database: {id: crm, schema_mode: strict}\nstorage: {engine: %s, path: %s}\nschemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n        tenant: {type: string}\n        country: {type: string}\n        secret: {type: string}\n", engine, storage)
			aclWriteFile(t, path, manifest)
			seed, err := mount.File(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"01", "02"} {
				country := "IE"
				if id == "02" {
					country = "US"
				}
				_, err = seed.Apply(context.Background(), []core.Op{{Op: "insert", Key: record.NewKeyWithID("customers", id), Data: map[string]any{"name": "Original", "tenant": "A", "country": country, "secret": "hidden"}}}, "seed")
				if err != nil {
					t.Fatal(err)
				}
			}
			policy := func(name, field, value string) string {
				text := strings.Replace(aclPolicy(name, field, value), "visibility: private", "visibility: public", 1)
				text = strings.Replace(text, "operations: [query, get]", "operations: [query, get, update]", 1)
				if engine == "ingitdb" {
					text = strings.Replace(text, "fields: [id, name]", "fields: [id, name, $id]", 1)
				}
				return text
			}
			aclWriteFile(t, filepath.Join(dir, "upper.yaml"), policy("upper", "country", "IE"))
			aclWriteFile(t, path, manifest+"acl: {enabled: true, policies: [upper.yaml]}\n")
			if engine == "ingitdb" {
				root := filepath.Join(dir, "data", ".ingitdb", "access")
				aclWriteFile(t, filepath.Join(root, "manifest.yaml"), "enabled: true\ndatabase: crm\npolicies: [lower.yaml]\n")
				aclWriteFile(t, filepath.Join(root, "lower.yaml"), policy("lower", "tenant", "A"))
			}
			db, err := mount.File(path)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(server.New("test", map[string]*core.Database{"crm": db}, server.WithPrincipalResolver(func(context.Context, *auth.Principal) (access.Principal, error) {
				return access.Principal{Roles: []string{"reader"}}, nil
			}), server.WithAuth(&auth.Config{OwnerToken: ownerToken}), server.WithOwnerAuthorization(func(_ context.Context, _ *auth.Principal, _ az.Source, capability string, _ az.Resource) bool {
				return capability == auth.CapAccessDiagnostics
			})).Handler())
			defer ts.Close()
			// The browser's initial query has no projection; engine-specific
			// key-order grants must not become nonexistent SQL data columns.
			initialStatus, initialBody := request(t, ts, "POST", "/v1/databases/crm/dtql", ownerToken, "from: {name: customers}\nlimit: 20\n")
			if initialStatus != 200 || !strings.Contains(initialBody, "Original") {
				t.Fatalf("browser query %d %s", initialStatus, initialBody)
			}

			updateOp := api.Operation{ID: "u1", Action: "update", Resource: az.Resource{DatabaseID: "crm", Path: "/customers/01"}, ExecutionClass: az.ExecutionDTQL, Mutation: &api.Mutation{Changes: []api.Change{{Op: "set", Path: []string{"name"}, Value: json.RawMessage(`"Changed"`)}}}}
			call := func(method, path, content string, body any) (int, string) {
				t.Helper()
				data, _ := json.Marshal(body)
				req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(string(data)))
				req.Header.Set("Authorization", "Bearer "+ownerToken)
				req.Header.Set("Content-Type", content)
				resp, err := ts.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				raw, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, string(raw)
			}
			inspect := api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: []api.Operation{updateOp}}
			status, body := call("POST", "/v1/databases/crm/access/evaluate", "application/json", inspect)
			if status != 200 {
				t.Fatalf("inspect %d %s", status, body)
			}
			result, err := az.ParseResult([]byte(body))
			if err != nil || !result.Allowed {
				t.Fatalf("inspect %v %s", err, body)
			}
			status, body = request(t, ts, "POST", "/v1/databases/crm/dtql", ownerToken, "from: {name: customers}\ncolumns: [{field: name}]\n")
			if status != 200 || strings.Contains(body, "Changed") || !strings.Contains(body, "Original") {
				t.Fatalf("inspect mutated %d %s", status, body)
			}
			evidenceRequest := map[string]any{"apiVersion": az.APIVersion, "resource": updateOp.Resource, "requiredFields": [][]string{{"name"}}}
			status, body = call("POST", "/v1/databases/crm/access/evidence", "application/json", evidenceRequest)
			if status != 200 {
				t.Fatalf("evidence %d %s", status, body)
			}
			var evidence struct {
				DataRevision string `json:"dataRevision"`
			}
			if err = json.Unmarshal([]byte(body), &evidence); err != nil || evidence.DataRevision == "" {
				t.Fatal(body)
			}
			updateOp.Mutation.IfDataRevision = evidence.DataRevision
			status, body = call("PATCH", "/v1/databases/crm/records/customers/01", "application/vnd.dtql.operation+json", updateOp)
			if status != 200 {
				t.Fatalf("write %d %s", status, body)
			}
			var success struct {
				Authorization az.Result `json:"authorization"`
				DataRevision  string    `json:"dataRevision"`
			}
			if err = json.Unmarshal([]byte(body), &success); err != nil || !success.Authorization.Allowed || success.DataRevision == "" || success.DataRevision == evidence.DataRevision {
				t.Fatal(body)
			}
			status, body = call("PATCH", "/v1/databases/crm/records/customers/01", "application/vnd.dtql.operation+json", updateOp)
			if status != 409 {
				t.Fatalf("stale revision %d %s", status, body)
			}
			status, body = request(t, ts, "POST", "/v1/databases/crm/dtql", ownerToken, "from: {name: customers}\ncolumns: [{field: name}]\n")
			if status != 200 || !strings.Contains(body, "Changed") || strings.Contains(body, "Original") {
				t.Fatalf("read after write %d %s", status, body)
			}
			sampleOp := updateOp
			sampleOp.Resource = az.Resource{DatabaseID: "crm", Path: "/customers"}
			sampleOp.Mutation = &api.Mutation{Changes: []api.Change{{Op: "set", Path: []string{"name"}, Value: json.RawMessage(`"Sample must not write"`)}}}
			sample := api.Request{APIVersion: az.APIVersion, Mode: az.ModeSample, DiagnosticLevel: "ordinary", Operations: []api.Operation{sampleOp}, Sample: &api.Sample{Limit: 1, Query: api.Query{Format: "dtql-yaml", Text: "from: {name: customers}\ncolumns: [{field: name}]\n"}}}
			status, body = call("POST", "/v1/databases/crm/access/evaluate", "application/json", sample)
			if status != 200 {
				t.Fatalf("sample %d %s", status, body)
			}
			result, err = az.ParseResult([]byte(body))
			if err != nil || result.Allowed || result.Result != az.OutcomeConditional || result.Sample == nil || result.Sample.EvaluatedCount != 1 || result.Operations[0].Resource.RowID != "01" {
				t.Fatalf("sample %v %s", err, body)
			}

			blockedOp := updateOp
			blockedOp.Mutation = &api.Mutation{Changes: []api.Change{{Op: "set", Path: []string{"secret"}, Value: json.RawMessage(`"Forbidden"`)}}}
			blocked := api.Request{APIVersion: az.APIVersion, Mode: az.ModeInspect, DiagnosticLevel: "references", Operations: []api.Operation{blockedOp}}
			status, body = call("POST", "/v1/databases/crm/access/evaluate", "application/json", blocked)
			result, err = az.ParseResult([]byte(body))
			if status != 200 || err != nil || result.Result != az.OutcomeDeny {
				t.Fatalf("blocked inspect %d %v %s", status, err, body)
			}
			refs := map[string]bool{}
			for _, blocker := range result.Blockers {
				if blocker.PolicyRef != nil {
					refs[blocker.PolicyRef.PolicyID] = true
					if len(blocker.Columns) != 1 || blocker.Columns[0][0] != "secret" {
						t.Fatalf("column facts missing: %s", body)
					}
				}
			}
			if !refs["upper"] || engine == "ingitdb" && !refs["lower"] {
				t.Fatalf("owner blockers missing %s", body)
			}
			status, body = call("PATCH", "/v1/databases/crm/records/customers/01", "application/vnd.dtql.operation+json", blockedOp)
			if status != 403 || !strings.Contains(body, `"policyId":"upper"`) || engine == "ingitdb" && !strings.Contains(body, `"policyId":"lower"`) {
				t.Fatalf("real write blockers %d %s", status, body)
			}

			// Hidden and nonexistent row probes have identical safe denial shape.
			for _, id := range []string{"02", "missing"} {
				getStatus, getBody := request(t, ts, "GET", "/v1/databases/crm/records/customers/"+id, ownerToken, "")
				if getStatus != 404 || !strings.Contains(getBody, `"code":"resource_unavailable"`) {
					t.Fatalf("get probe %s %d %s", id, getStatus, getBody)
				}
				evRequest := map[string]any{"apiVersion": az.APIVersion, "resource": az.Resource{DatabaseID: "crm", Path: "/customers/" + id}, "requiredFields": [][]string{{"name"}}}
				evStatus, evBody := call("POST", "/v1/databases/crm/access/evidence", "application/json", evRequest)
				if evStatus != 404 || !strings.Contains(evBody, `"code":"resource_unavailable"`) {
					t.Fatalf("evidence probe %s %d %s", id, evStatus, evBody)
				}

				inspect.Operations[0].Resource.Path = "/customers/" + id
				status, body = call("POST", "/v1/databases/crm/access/evaluate", "application/json", inspect)
				if status != 200 {
					t.Fatalf("probe %s %d %s", id, status, body)
				}
				result, err = az.ParseResult([]byte(body))
				if err != nil || result.Result != az.OutcomeDeny || len(result.Blockers) != 1 || result.Blockers[0].Code != az.CodeAccessDenied {
					t.Fatalf("probe %s %v %s", id, err, body)
				}
			}
			if engine == "sqlite" {
				unsupportedStatus, unsupportedBody := request(t, ts, "POST", "/v1/databases/crm/dtql", ownerToken, "from: {name: customers}\ncolumns: [{field: name}]\nwhere: {op: '==', left: {field: name}, right: {value: true}}\n")
				if unsupportedStatus != 422 || !strings.Contains(unsupportedBody, `"code":"authorization_unsupported"`) {
					t.Fatalf("unsupported typed query %d %s", unsupportedStatus, unsupportedBody)
				}
			}
			reconnected, err := mount.File(path)
			if err != nil {
				t.Fatal(err)
			}
			readCtx := access.WithPrincipal(context.Background(), access.Principal{Roles: []string{"reader"}})
			persisted, err := reconnected.ExecuteDTQL(readCtx, []byte("from: {name: customers}\ncolumns: [{field: name}]\n"))
			if err != nil || len(persisted) != 1 || persisted[0].Data["name"] != "Changed" {
				t.Fatalf("remounted protected update %v %v", persisted, err)
			}

		})
	}
}
