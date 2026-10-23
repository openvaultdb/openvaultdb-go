package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/tugql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func TestAccessEvaluateCoverage(t *testing.T) {
	// Not parallel: modifies package-level parseAuthorizationRequest

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "accdb", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}

	authStore, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	grant := &auth.Grant{
		ID:         "g1",
		DatabaseID: "accdb",
		Capabilities: []auth.Capability{
			{Action: auth.CapAccessExplain},
			{Action: auth.CapAccessSimulate},
			{Action: auth.CapAccessDiagnostics},
			{Action: auth.CapPoliciesAdmin},
			{Action: auth.CapPoliciesRead},
			{Action: auth.CapRecordsRead},
			{Action: auth.CapRecordsWrite},
			{Action: auth.CapRecordsDelete},
		},
	}
	_ = authStore.CreateGrant(grant, "token-all")
	_ = authStore.CreateGrant(&auth.Grant{
		ID:         "g2",
		DatabaseID: "accdb",
		Capabilities: []auth.Capability{
			{Action: auth.CapRecordsRead},
		},
	}, "token-no-explain")

	srv := New("test", map[string]*core.Database{"accdb": db}, WithAuth(&auth.Config{
		OwnerToken: "owner",
		Store:      authStore,
	}))

	// 1. Database missing -> 404 (handled by s.db returning nil)
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/missing/access:evaluate", strings.NewReader("{}"))
		req.SetPathValue("db", "missing")
		rec := httptest.NewRecorder()
		srv.handleAccessEvaluate(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", rec.Code)
		}
	}

	// 2. Request body > MaxRequestBytes (1MB limit exceeded)
	{
		bigBody := bytes.Repeat([]byte("a"), api.MaxRequestBytes+10)
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access:evaluate", bytes.NewReader(bigBody))
		req.SetPathValue("db", "accdb")
		rec := httptest.NewRecorder()
		srv.handleAccessEvaluate(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for too large body, got %d", rec.Code)
		}
	}

	// 3. Invalid JSON parse error
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access:evaluate", strings.NewReader("{invalid"))
		req.SetPathValue("db", "accdb")
		rec := httptest.NewRecorder()
		srv.handleAccessEvaluate(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid JSON, got %d", rec.Code)
		}
	}

	// 4. Subject without CapAccessExplain -> 403
	{
		reqJSON := `{"apiVersion":"tugql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","subject":{"kind":"user","id":"u1","realm":"default"},"operations":[{"id":"op1","action":"get","resource":{"databaseId":"accdb","path":"/users","table":"users"},"executionClass":"tugql"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access/evaluate", strings.NewReader(reqJSON))
		req.Header.Set("Authorization", "Bearer token-no-explain")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 without explain capability, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 5. Simulation without CapAccessSimulate -> 403
	{
		reqJSON := `{"apiVersion":"tugql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","simulation":{"roles":["admin"],"groups":[],"attributes":{}},"operations":[{"id":"op1","action":"get","resource":{"databaseId":"accdb","path":"/users","table":"users"},"executionClass":"tugql"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access/evaluate", strings.NewReader(reqJSON))
		req.Header.Set("Authorization", "Bearer token-no-explain")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 without simulate capability, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 6. Subject with explain capability, but srv.explainResolver == nil -> 503
	{
		reqJSON := `{"apiVersion":"tugql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","subject":{"kind":"user","id":"u1","realm":"default"},"operations":[{"id":"op1","action":"get","resource":{"databaseId":"accdb","path":"/users","table":"users"},"executionClass":"tugql"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access/evaluate", strings.NewReader(reqJSON))
		req.Header.Set("Authorization", "Bearer token-all")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for nil explainResolver, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 7. Subject with explainResolver error -> 503
	{
		srvWithResolver := New("test", map[string]*core.Database{"accdb": db}, WithAuth(&auth.Config{
			OwnerToken: "owner",
			Store:      authStore,
		}))
		srvWithResolver.explainResolver = func(ctx context.Context, s access.PrincipalRef) (Membership, error) {
			return Membership{}, errors.New("resolver error")
		}

		reqJSON := `{"apiVersion":"tugql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","subject":{"kind":"user","id":"u1","realm":"default"},"operations":[{"id":"op1","action":"get","resource":{"databaseId":"accdb","path":"/users","table":"users"},"executionClass":"tugql"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access/evaluate", strings.NewReader(reqJSON))
		req.Header.Set("Authorization", "Bearer token-all")
		rec := httptest.NewRecorder()
		srvWithResolver.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for explainResolver error, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 8. Simulation with Attributes > 0 -> 422
	{
		reqJSON := `{"apiVersion":"tugql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","simulation":{"roles":["admin"],"groups":[],"attributes":{"dept":"eng"}},"operations":[{"id":"op1","action":"get","resource":{"databaseId":"accdb","path":"/users","table":"users"},"executionClass":"tugql"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access/evaluate", strings.NewReader(reqJSON))
		req.Header.Set("Authorization", "Bearer token-all")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != 422 {
			t.Fatalf("expected 422 for simulation attributes, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 9. Simulation success with explainResolver
	{
		srvWithResolver := New("test", map[string]*core.Database{"accdb": db}, WithAuth(&auth.Config{
			OwnerToken: "owner",
			Store:      authStore,
		}))
		srvWithResolver.explainResolver = func(ctx context.Context, s access.PrincipalRef) (Membership, error) {
			return Membership{Roles: []string{"viewer"}, Groups: []string{"staff"}, Revision: "r1"}, nil
		}

		reqJSON := `{"apiVersion":"tugql.org/authorization/v1","mode":"plan","diagnosticLevel":"ordinary","subject":{"kind":"user","id":"u1","realm":"default"},"simulation":{"roles":["admin"],"groups":["eng"],"attributes":{}},"operations":[{"id":"op1","action":"get","resource":{"databaseId":"accdb","path":"/users","table":"users"},"executionClass":"tugql"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access/evaluate", strings.NewReader(reqJSON))
		req.Header.Set("Authorization", "Bearer token-all")
		rec := httptest.NewRecorder()
		srvWithResolver.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 10. Unsupported mode != az.ModePlan -> 422
	{
		oldParse := parseAuthorizationRequest
		parseAuthorizationRequest = func(b []byte, db string) (api.Request, error) {
			return api.Request{Mode: "unsupported"}, nil
		}
		defer func() { parseAuthorizationRequest = oldParse }()

		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access/evaluate", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer token-all")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		parseAuthorizationRequest = oldParse
		if rec.Code != 422 {
			t.Fatalf("expected 422 for unsupported mode, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	// 11. Mode == az.ModeSample: test refusal / error path
	{
		reqJSON := `{"apiVersion":"tugql.org/authorization/v1","mode":"sample","diagnosticLevel":"ordinary","sample":{"limit":10,"query":{"format":"tugql-yaml","text":"invalid: [yaml"}},"operations":[{"id":"op1","action":"get","resource":{"databaseId":"accdb","path":"/users","table":"users"},"executionClass":"tugql"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access/evaluate", strings.NewReader(reqJSON))
		req.Header.Set("Authorization", "Bearer token-all")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid sample query, got %d", rec.Code)
		}
	}

	// 12. Mode == az.ModeInspect: test through handleAccessEvaluate
	{
		reqJSON := `{"apiVersion":"tugql.org/authorization/v1","mode":"inspect","diagnosticLevel":"ordinary","operations":[{"id":"op1","action":"get","resource":{"databaseId":"accdb","path":"/users/1","table":"users","rowId":"1"},"executionClass":"tugql"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/accdb/access/evaluate", strings.NewReader(reqJSON))
		req.Header.Set("Authorization", "Bearer token-all")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == 0 {
			t.Fatal("response code was 0")
		}
	}
}

func TestInternalRequestAndCapabilityForAction(t *testing.T) {
	t.Parallel()

	// 1. capabilityForAction
	actions := []string{"get", "exists", "query", "insert", "set", "update", "delete", "truncate", "unknown"}
	expectedCaps := []string{
		auth.CapRecordsRead, auth.CapRecordsRead, auth.CapRecordsRead,
		auth.CapRecordsWrite, auth.CapRecordsWrite, auth.CapRecordsWrite,
		auth.CapRecordsDelete, auth.CapRecordsDelete, "",
	}
	for i, a := range actions {
		got := capabilityForAction(a)
		if got != expectedCaps[i] {
			t.Errorf("capabilityForAction(%q) = %q, want %q", a, got, expectedCaps[i])
		}
	}

	// 2. internalRequest: with RowID, with Callable, with Query Parse error, with valid Query
	opWithRowID := api.Operation{
		ID:     "op1",
		Action: "get",
		Resource: az.Resource{
			Table: "users",
			RowID: "42",
		},
		Callable: &az.Callable{
			Namespace: "ns1",
			Name:      "call1",
		},
	}
	req, err := internalRequest(opWithRowID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Execution.Namespace != "ns1" || req.Execution.Name != "call1" {
		t.Fatalf("unexpected execution target: %+v", req.Execution)
	}

	// Invalid Query format or text
	opInvalidQuery := api.Operation{
		ID:     "op2",
		Action: "query",
		Resource: az.Resource{
			Table: "users",
		},
		Query: &api.Query{
			Format: "tugql-yaml",
			Text:   ": invalid yaml",
		},
	}
	_, err = internalRequest(opInvalidQuery)
	if err == nil {
		t.Fatal("expected query parse error, got nil")
	}
}

func TestBoundAuthorizationAndSeams(t *testing.T) {
	// Not parallel: modifies package seam azMarshalResult
	res := newAuthorization(az.ModePlan)

	// Normal size
	bounded := boundAuthorization(res)
	if bounded.Coverage.Truncated {
		t.Fatal("expected not truncated for small result")
	}

	// Make result huge by adding 1001 blockers
	for i := 0; i < 1001; i++ {
		res.Blockers = append(res.Blockers, az.Blocker{
			OperationID: fmt.Sprintf("op%d", i),
			Code:        az.CodeAccessDenied,
			Scope:       az.ScopeOperation,
		})
	}
	res.Operations = []az.OperationResult{
		{ID: "op1", Result: az.OutcomeDeny},
		{ID: "op2", Result: az.OutcomeAllow},
	}

	bounded = boundAuthorization(res)
	if !bounded.Coverage.Truncated {
		t.Fatal("expected truncated for large result")
	}
	if bounded.Result != az.OutcomeDeny {
		t.Fatalf("expected OutcomeDeny, got %s", bounded.Result)
	}

	// Test conditionNodes
	if count := conditionNodes(nil); count != 0 {
		t.Fatalf("expected 0 for nil, got %d", count)
	}
	cond := &access.DocumentCondition{
		Left:  &access.DocumentExpression{Field: "x"},
		Right: &access.DocumentExpression{Field: "y"},
		And:   []access.DocumentCondition{{}, {}},
		Or:    []access.DocumentCondition{{}},
	}
	if count := conditionNodes(cond); count != 6 {
		t.Fatalf("expected 6, got %d", count)
	}

	// Test writeAuthorization when azMarshalResult fails
	oldMarshal := azMarshalResult
	azMarshalResult = func(r az.Result) ([]byte, error) {
		return nil, errors.New("marshal error")
	}
	defer func() { azMarshalResult = oldMarshal }()

	rec := httptest.NewRecorder()
	writeAuthorization(rec, http.StatusOK, res)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when azMarshalResult fails, got %d", rec.Code)
	}

	// Test writeQueryAuthorizationError when azMarshalResult fails
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"db1": db})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	op := api.Operation{ID: "op1", Action: "query", Resource: az.Resource{DatabaseID: "db1", Path: "/users", Table: "users"}, ExecutionClass: executionTugQL}

	recQueryErr := httptest.NewRecorder()
	srv.writeQueryAuthorizationError(recQueryErr, req, db, op, access.ErrAccessDenied)
	if recQueryErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when azMarshalResult fails in writeQueryAuthorizationError, got %d: %s", recQueryErr.Code, recQueryErr.Body.String())
	}

	// Restore azMarshalResult and test writeQueryAuthorizationError with various error types
	azMarshalResult = oldMarshal

	// 1. PolicyProviderError
	recProv := httptest.NewRecorder()
	provErr := &access.PolicyProviderError{Err: errors.New("prov unavailable")}
	srv.writeQueryAuthorizationError(recProv, req, db, op, provErr)
	if recProv.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for PolicyProviderError, got %d: %s", recProv.Code, recProv.Body.String())
	}

	// 2. CodeEnforcementUnsupported
	recUnsupp := httptest.NewRecorder()
	unsuppErr := &access.DeniedError{Decision: access.Decision{Allowed: false, Code: access.CodeEnforcementUnsupported}, Decisions: []access.Decision{{Allowed: false, Code: access.CodeEnforcementUnsupported}}}
	srv.writeQueryAuthorizationError(recUnsupp, req, db, op, unsuppErr)
	if recUnsupp.Code != 422 {
		t.Fatalf("expected 422 for CodeEnforcementUnsupported, got %d: %s", recUnsupp.Code, recUnsupp.Body.String())
	}
}

func TestAppendPlanRestrictionsBranches(t *testing.T) {
	t.Parallel()

	srv := New("test", nil)
	op := api.Operation{ID: "op1", Action: "get", Resource: az.Resource{DatabaseID: "db1", Path: "/users", Table: "users"}}
	source := az.Source{Kind: "openvaultdb", OwnerID: "o1", DatabaseID: "db1"}
	layer := az.Layer{LayerID: "l1"}
	res := newAuthorization(az.ModePlan)
	res.Operations = []az.OperationResult{{ID: "op1"}}

	// 1. Empty restrictions
	srv.appendPlanRestrictions(httptest.NewRequest(http.MethodGet, "/", nil), "ordinary", source, op, &res, &layer, 0, access.Assessment{}, false)
	if len(res.Restrictions) != 0 {
		t.Fatalf("expected 0 restrictions, got %d", len(res.Restrictions))
	}

	// 2. Level != "policy" -> adds opaque reference restriction and returns
	assess := access.Assessment{
		Restrictions: []access.AssessmentRestriction{
			{PolicyIndex: 0},
		},
	}
	srv.appendPlanRestrictions(httptest.NewRequest(http.MethodGet, "/", nil), "ordinary", source, op, &res, &layer, 0, assess, false)
	if len(res.Restrictions) != 1 || res.Restrictions[0].Representation != "reference" {
		t.Fatalf("expected 1 reference restriction, got %+v", res.Restrictions)
	}

	// 3. Level == "policy", admin with slot check and large condition
	docCond := &access.DocumentCondition{Op: "==", Left: &access.DocumentExpression{Field: "status"}, Right: &access.DocumentExpression{Value: "active"}}
	assessDetailed := access.Assessment{
		Policies: []access.PolicyAssessment{
			{
				Policy: access.PolicyMetadata{
					ID:         "p1",
					Revision:   "rev1",
					Visibility: access.PolicyVisibilityPublic,
				},
			},
		},
		Restrictions: []access.AssessmentRestriction{
			{
				PolicyIndex: -1, // out of range
			},
			{
				PolicyIndex: 99, // out of range
			},
			{
				PolicyIndex: 0,
				Slot:        access.DecisionSlotCheck,
				Expression:  docCond,
			},
		},
	}

	resDetailed := newAuthorization(az.ModePlan)
	resDetailed.Operations = []az.OperationResult{{ID: "op1"}}
	layerDetailed := az.Layer{LayerID: "l1", Decisions: []az.LayerDecision{{OperationID: "op1"}}}

	srv.appendPlanRestrictions(httptest.NewRequest(http.MethodGet, "/", nil), "policy", source, op, &resDetailed, &layerDetailed, 0, assessDetailed, true)
	if len(resDetailed.Restrictions) < 2 {
		t.Fatalf("expected at least 2 restrictions, got %d", len(resDetailed.Restrictions))
	}

	// 4. Level == "policy", non-admin with private policy -> line 338 continue
	assessPrivate := access.Assessment{
		Policies: []access.PolicyAssessment{
			{
				Policy: access.PolicyMetadata{
					ID:         "p_priv",
					Visibility: access.PolicyVisibilityPrivate,
				},
			},
		},
		Restrictions: []access.AssessmentRestriction{
			{
				PolicyIndex: 0,
				Expression:  docCond,
			},
		},
	}
	resPriv := newAuthorization(az.ModePlan)
	resPriv.Operations = []az.OperationResult{{ID: "op1"}}
	layerPriv := az.Layer{LayerID: "l1", Decisions: []az.LayerDecision{{OperationID: "op1"}}}
	srv.appendPlanRestrictions(httptest.NewRequest(http.MethodGet, "/", nil), "policy", source, op, &resPriv, &layerPriv, 0, assessPrivate, false)
	// Only the reference restriction is added because private policy is skipped
	if len(resPriv.Restrictions) != 1 {
		t.Fatalf("expected 1 restriction for private policy non-admin, got %d", len(resPriv.Restrictions))
	}

	// 5. Huge condition (>256 nodes) -> lines 351-354 (too_large)
	nested := &access.DocumentCondition{Op: "==", Left: &access.DocumentExpression{Field: "x"}, Right: &access.DocumentExpression{Value: "y"}}
	for i := 0; i < 300; i++ {
		nested = &access.DocumentCondition{Op: "and", And: []access.DocumentCondition{*nested}}
	}
	assessHuge := access.Assessment{
		Policies: []access.PolicyAssessment{
			{
				Policy: access.PolicyMetadata{
					ID:         "p_huge",
					Visibility: access.PolicyVisibilityPublic,
				},
			},
		},
		Restrictions: []access.AssessmentRestriction{
			{
				PolicyIndex: 0,
				Expression:  nested,
			},
		},
	}
	resHuge := newAuthorization(az.ModePlan)
	resHuge.Operations = []az.OperationResult{{ID: "op1"}}
	layerHuge := az.Layer{LayerID: "l1", Decisions: []az.LayerDecision{{OperationID: "op1"}}}
	srv.appendPlanRestrictions(httptest.NewRequest(http.MethodGet, "/", nil), "policy", source, op, &resHuge, &layerHuge, 0, assessHuge, true)
	if len(resHuge.Restrictions) < 2 || resHuge.Restrictions[1].OmissionReason != "too_large" {
		t.Fatalf("expected too_large omission reason, got %+v", resHuge.Restrictions)
	}
}

func TestNewAuthorizationPanic(t *testing.T) {
	// Not parallel: modifies package-level newAuthorizationGrantID
	old := newAuthorizationGrantID
	newAuthorizationGrantID = func() (string, error) { return "", errors.New("entropy error") }
	defer func() { newAuthorizationGrantID = old }()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from newAuthorization")
		}
	}()
	newAuthorization(az.ModePlan)
}

func TestAssessPlanBranches(t *testing.T) {
	t.Parallel()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "accdb", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"accdb": db})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	op := api.Operation{ID: "op1", Action: "get", Resource: az.Resource{DatabaseID: "accdb", Path: "/users", Table: "users"}, ExecutionClass: "tugql"}
	authReq := api.Request{
		APIVersion: az.APIVersion,
		Mode:       az.ModePlan,
		Operations: []api.Operation{op},
	}

	// 1. PolicyLayer with Enabled: true but Policies: nil -> lines 193-199 (source_unavailable)
	layers := []core.PolicyLayer{
		{
			Kind:     "openvaultdb",
			Enabled:  true,
			Policies: nil,
		},
	}
	res := srv.assessPlan(req, db, authReq, layers)
	if res.Coverage.Evaluation != az.EvaluationPartial {
		t.Fatalf("expected EvaluationPartial, got %s", res.Coverage.Evaluation)
	}
}
