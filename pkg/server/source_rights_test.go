package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

func rightsMount(t *testing.T, id, declaration, overrides string) *core.Database {
	t.Helper()
	text := fmt.Sprintf("database: {id: %s, schema_mode: strict%s}\nstorage: {engine: ingitdb, path: data}\nschemas: {collections: {items: {fields: {id: {type: string}, name: {type: string}}}, notes: {fields: {id: {type: string}, name: {type: string}}}}}\n%s", id, declaration, overrides)
	path := filepath.Join(t.TempDir(), "db.yaml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for i := 1; i <= 3; i++ {
		key := fmt.Sprintf("i%d", i)
		_, err := db.Apply(context.Background(), []core.Op{{Op: "set", Key: record.NewKeyWithID("items", key), Data: map[string]any{"id": key, "name": "item"}}}, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func rightsDocument(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	var document map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(response.Code, response.Body.String(), err)
	}
	return response, document
}
func rightsEntries(t *testing.T, doc map[string]any) []license.SourceRight {
	t.Helper()
	data, err := json.Marshal(doc["sourceRights"])
	if err != nil {
		t.Fatal(err)
	}
	var rights []license.SourceRight
	if err := json.Unmarshal(data, &rights); err != nil {
		t.Fatal(err)
	}
	return rights
}
func TestSourceRightsDiscoveryReadsAndFrozenPages(t *testing.T) {
	db := rightsMount(t, "rights", ", license: MIT", "recordset_licenses: {items: {url: 'https://example.org/items#reuse'}}\n")
	serverTerms := &license.Declaration{Text: "Server source terms"}
	service, err := server.NewChecked("test", map[string]*core.Database{"rights": db}, server.WithSourceRights("stable-server", serverTerms))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.CloseSnapshots)
	// Mutable caller-owned declarations cannot change an active mount/server.
	serverTerms.Text = "changed server"
	db.Manifest.Database.License.Text = "changed database"
	db.Manifest.RecordsetLicenses["items"] = license.Declaration{Text: "changed recordset"}
	handler := service.Handler()
	response, metadata := rightsDocument(t, handler, "GET", "/v1/databases/rights", "", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, metadata)
	}
	entries := rightsEntries(t, metadata)
	if len(entries) != 3 {
		t.Fatal(entries)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Declaration.Text, "changed") {
			t.Fatal("mutable declaration escaped", entry)
		}
		if entry.Source.Recordset == "items" && (entry.Declaration.URL != "https://example.org/items#reuse" || entry.Declaration.SPDX != "" || entry.DeclarationScope != license.RecordsetScope || entry.DeclaredAt.Recordset != "items") {
			t.Fatal(entry)
		}
		if entry.Source.Recordset == "notes" && (entry.Declaration.SPDX != "MIT" || entry.DeclarationScope != license.DatabaseScope || entry.EvidenceOrigin != "legacy-metadata") {
			t.Fatal(entry)
		}
	}
	for _, path := range []string{"/.well-known/openvaultdb", "/v1/status"} {
		response, doc := rightsDocument(t, handler, "GET", path, "", nil)
		if response.Code != 200 || rightsEntries(t, doc)[0].Declaration.Text != "Server source terms" {
			t.Fatal(response.Code, doc)
		}
	}
	for _, call := range []struct{ method, path, body string }{{"GET", "/v1/databases/rights/read?key=items/i1", ""}, {"GET", "/v1/databases/rights/records/items/i1", ""}, {"POST", "/v1/databases/rights/query", `{"collection":"items"}`}, {"POST", "/v1/databases/rights/dtql", "from: {name: items}\n"}, {"POST", "/v1/databases/rights/dtql", "from: {name: items}\nwhere: {op: '==', left: {field: id}, right: {value: absent}}\n"}} {
		response, doc := rightsDocument(t, handler, call.method, call.path, call.body, nil)
		got := rightsEntries(t, doc)
		if response.Code != 200 || len(got) != 1 || got[0].DeclarationScope != license.RecordsetScope || !reflect.DeepEqual(doc["usedSourceIds"], []any{"ovdb:stable-server/rights/items"}) {
			t.Fatal(response.Code, doc)
		}
	}
	response, first := rightsDocument(t, handler, "POST", "/v1/databases/rights/dtql", "from: {name: items}\n", map[string]string{"OVDB-Page-Size": "1"})
	if response.Code != 200 {
		t.Fatal(response.Code, first)
	}
	response, second := rightsDocument(t, handler, "POST", "/v1/databases/rights/dtql", "from: {name: items}\n", map[string]string{"OVDB-Page-Size": "1", "OVDB-Page-Token": first["nextPageToken"].(string)})
	if response.Code != 200 || !reflect.DeepEqual(first["sourceRights"], second["sourceRights"]) || !reflect.DeepEqual(first["usedSourceIds"], second["usedSourceIds"]) {
		t.Fatal(response.Code, first, second)
	}
	// Runtime metadata does not invent source joins or schema references.
	if len(db.Manifest.Schemas.Collections["items"].References) != 0 {
		t.Fatal("terms added source joins")
	}
}
func TestSourceRightsRelationalPlannedAndUsedInventories(t *testing.T) {
	a := rightsMount(t, "a", ", license: {text: Source A terms}", "")
	b := rightsMount(t, "b", ", license: {url: 'https://example.org/b'}", "")
	unrelated := rightsMount(t, "unrelated", ", license: {text: Private unrelated terms}", "")
	service := server.New("test", map[string]*core.Database{"a": a, "b": b, "unrelated": unrelated}, server.WithSourceRights("stable-server", nil))
	t.Cleanup(service.CloseSnapshots)
	docs := []string{
		"from: {database: a, name: items, alias: a, joins: [{from: {database: b, name: items, alias: b}, on: [{left: {field: id, source: a}, op: '==', right: {field: id, source: b}}]}]}\ncolumns: [{field: id, source: a}]\n",
		"from: {database: a, name: items}\nwhere: {exists: {query: {from: {database: b, name: items}}}}\ncolumns: [{field: id}]\n",
	}
	for _, doc := range docs {
		response, result := rightsDocument(t, service.Handler(), "POST", "/v1/dtql", doc, nil)
		if response.Code != 200 {
			t.Fatal(response.Code, result)
		}
		entries := rightsEntries(t, result)
		if len(entries) != 2 || entries[0].Source.DatabaseID != "a" || entries[1].Source.DatabaseID != "b" || len(entries[0].Pins) != 0 || len(entries[0].Transformations) != 0 {
			t.Fatal(entries)
		}
		if !reflect.DeepEqual(result["usedSourceIds"], []any{"ovdb:stable-server/a/items", "ovdb:stable-server/b/items"}) {
			t.Fatal(result)
		}
		if strings.Contains(response.Body.String(), "unrelated") {
			t.Fatal("unrelated source disclosure", response.Body.String())
		}
	}
	// A source inside an EXISTS expression is planned, but is never read when
	// its root scan is empty. Its terms remain in the complete inventory.
	doc := "from: {database: a, name: notes}\nwhere: {exists: {query: {from: {database: b, name: items}}}}\ncolumns: [{field: id}]\n"
	response, result := rightsDocument(t, service.Handler(), "POST", "/v1/dtql", doc, nil)
	if response.Code != 200 || len(rightsEntries(t, result)) != 2 || !reflect.DeepEqual(result["usedSourceIds"], []any{"ovdb:stable-server/a/notes"}) {
		t.Fatal(response.Code, result)
	}
}
func TestSourceRightsSafeHumanTermsAndLegacyFallback(t *testing.T) {
	db := rightsMount(t, "human", "", "")
	terms := &license.Declaration{Name: "<script>alert(1)</script>", Text: "<img src=x onerror=alert(1)>\nSecond line", URL: "https://example.org/terms#reuse"}
	service := server.New("test", map[string]*core.Database{"human": db}, server.WithSourceRights("stable-server", terms))
	t.Cleanup(service.CloseSnapshots)
	for _, path := range []string{"/ovdb/", "/ovdb/dbs/human", "/ovdb/dbs/human/collections/items"} {
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
		body := response.Body.String()
		if response.Code != 200 || strings.Contains(body, "<script>") || strings.Contains(body, "<img src=x") || !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "Declared at server scope") || !strings.Contains(body, `href="https://example.org/terms#reuse"`) {
			t.Fatal(response.Code, body)
		}
	}
	legacy := server.New("test", map[string]*core.Database{"human": db})
	t.Cleanup(legacy.CloseSnapshots)
	response, doc := rightsDocument(t, legacy.Handler(), "POST", "/v1/databases/human/dtql", "from: {name: items}\n", nil)
	if response.Code != 200 || doc["sourceRights"] != nil || doc["usedSourceIds"] != nil {
		t.Fatal(response.Code, doc)
	}
	response = httptest.NewRecorder()
	legacy.Handler().ServeHTTP(response, httptest.NewRequest("GET", "/ovdb/dbs/human", nil))
	if !strings.Contains(response.Body.String(), "Source data terms not declared") {
		t.Fatal(response.Body.String())
	}
}
func TestSourceRightsDoNotAuthorizeOrDiscloseDeniedReads(t *testing.T) {
	db := rightsMount(t, "denied", ", license: {text: Confidential source terms}", "")
	service := server.New("test", map[string]*core.Database{"denied": db}, server.WithSourceRights("stable-server", nil), server.WithAuth(&auth.Config{OwnerToken: "owner-token"}))
	t.Cleanup(service.CloseSnapshots)
	for _, call := range []struct{ method, path, body string }{{"GET", "/v1/databases/denied", ""}, {"GET", "/v1/databases/denied/records/items/i1", ""}, {"POST", "/v1/databases/denied/dtql", "from: {name: items}\n"}, {"POST", "/v1/dtql", "from: {database: denied, name: items}\n"}} {
		response, doc := rightsDocument(t, service.Handler(), call.method, call.path, call.body, nil)
		if response.Code < 400 || doc["sourceRights"] != nil || strings.Contains(response.Body.String(), "Confidential source terms") {
			t.Fatal(response.Code, doc)
		}
	}
}
func TestSourceRightsEvidenceBudgetRejectsBeforeDataOutput(t *testing.T) {
	// HTML escaping expands each '<' to six JSON bytes, so one legal declaration
	// can exceed the bounded encoded inventory even though its raw text fits.
	decl := ", license: {text: '" + strings.Repeat("<", 65536) + "'}"
	db := rightsMount(t, "large", decl, "")
	service := server.New("test", map[string]*core.Database{"large": db}, server.WithSourceRights("stable-server", nil))
	t.Cleanup(service.CloseSnapshots)
	for _, headers := range []map[string]string{nil, {"OVDB-Page-Size": "1"}} {
		response, result := rightsDocument(t, service.Handler(), "POST", "/v1/databases/large/dtql", "from: {name: items}\n", headers)
		if response.Code != 422 || result["records"] != nil || result["sourceRights"] != nil {
			t.Fatal(response.Code, result)
		}
	}
	if _, err := server.NewChecked("test", map[string]*core.Database{"large": db}); err == nil {
		t.Fatal("missing configured identity accepted")
	}
}

func TestSourceRightsMixedDeclaredAndUndeclaredInputs(t *testing.T) {
	a := rightsMount(t, "a", ", license: MIT", "")
	b := rightsMount(t, "b", "", "")
	service := server.New("test", map[string]*core.Database{"a": a, "b": b}, server.WithSourceRights("stable-server", nil))
	t.Cleanup(service.CloseSnapshots)
	doc := "from: {database: a, name: items, alias: a, joins: [{from: {database: b, name: items, alias: b}, on: [{left: {field: id, source: a}, op: '==', right: {field: id, source: b}}]}]}\ncolumns: [{field: id, source: a}]\n"
	response, result := rightsDocument(t, service.Handler(), "POST", "/v1/dtql", doc, nil)
	rights := rightsEntries(t, result)
	if response.Code != 200 || len(rights) != 1 || rights[0].Source.DatabaseID != "a" || !reflect.DeepEqual(result["usedSourceIds"], []any{"ovdb:stable-server/a/items", "ovdb:stable-server/b/items"}) {
		t.Fatal(response.Code, result)
	}
}

func TestSourceRightsSnapshotMetadataConsumesBudget(t *testing.T) {
	db := rightsMount(t, "small", ", license: {text: Bounded source terms}", "")
	service := server.New("test", map[string]*core.Database{"small": db}, server.WithSourceRights("stable-server", nil), server.WithSnapshotLimits(server.SnapshotLimits{Slots: 1, Bytes: 100, Rows: 10}))
	t.Cleanup(service.CloseSnapshots)
	response, result := rightsDocument(t, service.Handler(), "POST", "/v1/databases/small/dtql", "from: {name: items}\n", map[string]string{"OVDB-Page-Size": "1"})
	if response.Code != 422 || result["records"] != nil || result["snapshotToken"] != nil {
		t.Fatal(response.Code, result)
	}
}

func TestSourceRightsProtectedAuthorizationPrecedesMetadataRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.yaml")
	// A legal raw declaration whose encoded inventory exceeds the evidence limit.
	// Mandatory access failure still wins over its metadata refusal.
	manifest := `database: {id: crm, schema_mode: strict, license: {text: '` + strings.Repeat("<", 65536) + `'}}
storage: {engine: ingitdb, path: data}
schemas: {collections: {customers: {fields: {id: {type: string}, name: {type: string}, country: {type: string}}}}}
acl: {enabled: true, policies: [policy.yaml]}
`
	aclWriteFile(t, path, manifest)
	aclWriteFile(t, filepath.Join(dir, "policy.yaml"), aclPolicy("deny-unbound", "country", "IE"))
	db, err := mount.File(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := server.New("test", map[string]*core.Database{"crm": db}, server.WithSourceRights("stable-server", nil))
	t.Cleanup(service.CloseSnapshots)
	for _, call := range []struct{ method, path, body string }{{"GET", "/v1/databases/crm/records/customers/secret", ""}, {"POST", "/v1/databases/crm/dtql", "from: {name: customers}\n"}, {"GET", "/v1/databases/crm", ""}} {
		response, result := rightsDocument(t, service.Handler(), call.method, call.path, call.body, nil)
		if response.Code < 400 || result["sourceRights"] != nil || strings.Contains(response.Body.String(), "source_rights_invalid") {
			t.Fatal(response.Code, result)
		}
	}
}
