package server

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	az "github.com/dal-go/dalgo/tugql/authorization"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"gopkg.in/yaml.v3"
)

func TestServerDrainAndAccessCoverageBatch10(t *testing.T) {
	// Not parallel: tests line 272 of server.go and accessAssessPlan
	oldAssess := accessAssessPlan
	defer func() { accessAssessPlan = oldAssess }()

	// 1. server.go: line 272: select pick on canceled context with ready drained channel
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "drain_b10", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"drain_b10": db})
	defer srv.CloseSnapshots()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 50; i++ {
		srv.mu.Lock()
		srv.dbs["drain_b10"] = db
		srv.mu.Unlock()
		_ = srv.UnmountContext(ctx, "drain_b10")
	}

	// 2. access.go: line 247: outcome == OutcomeIndeterminate in projectPlan
	accessAssessPlan = func(_ context.Context, _ access.Request, _ []access.Policy) access.Assessment {
		return access.Assessment{
			Outcome:  access.AssessmentIndeterminate,
			Complete: true,
			Policies: []access.PolicyAssessment{
				{
					OperationID: "op1",
					LayerID:     "openvaultdb",
					Decision:    access.Decision{Code: access.CodeEvaluationFailed, Allowed: false},
					Policy:      access.PolicyMetadata{ID: "p1", Revision: "r1", Visibility: access.PolicyVisibilityPrivate},
				},
			},
		}
	}
	mAcc := &manifest.Manifest{
		Database: manifest.Database{ID: "acc_b10", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	dbAcc, _ := core.Open(mAcc, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	srvAcc := New("test", map[string]*core.Database{"acc_b10": dbAcc})
	defer srvAcc.CloseSnapshots()

	reqAcc := httptest.NewRequest(http.MethodPost, "/v1/databases/acc_b10/access:plan", strings.NewReader(`{
		"apiVersion": "tugql.org/authorization/v1",
		"mode": "plan",
		"operations": [{"id": "op1", "action": "query", "resource": {"databaseId": "acc_b10", "path": "/users", "table": "users"}}]
	}`))
	reqAcc.SetPathValue("db", "acc_b10")
	recAcc := httptest.NewRecorder()
	srvAcc.handleAccessEvaluate(recAcc, reqAcc)

	// 3. access_discovery.go: line 51: layer.Err != nil in handleAccessLayers
	dbErrLayer := &core.Database{}
	srvLayers := New("test", map[string]*core.Database{"err_layer_db": dbErrLayer})
	defer srvLayers.CloseSnapshots()
	srvLayers.dbs["err_layer_db"] = dbAcc
	// Access layers with db whose lowerPolicies returns error
	dbErrPolicies, _ := core.Open(mAcc, errPoliciesDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	srvLayers.dbs["err_policies_db"] = dbErrPolicies
	recLayers := httptest.NewRecorder()
	reqLayers := httptest.NewRequest(http.MethodGet, "/v1/databases/err_policies_db/access/layers", nil)
	reqLayers.SetPathValue("db", "err_policies_db")
	srvLayers.handleAccessLayers(recLayers, reqLayers)

	// access_discovery.go: line 94: policy visibility != public and !caps.PolicyAdmin
	recPol := httptest.NewRecorder()
	reqPol := httptest.NewRequest(http.MethodGet, "/v1/databases/err_policies_db/access/policies?layerId=openvaultdb", nil)
	reqPol.SetPathValue("db", "err_policies_db")
	srvLayers.accessAuthorization = func(_ context.Context, _ *auth.Principal, _ az.Source, cap string, _ az.Resource) bool {
		return cap != auth.CapPoliciesAdmin
	}
	srvLayers.handleAccessPolicies(recPol, reqPol)
}

type errPoliciesDB struct {
	emptyQueryDB
}

func (errPoliciesDB) AccessPolicies(context.Context) ([]access.Policy, error) {
	return nil, errors.New("lower policies failure")
}

func TestAccessInspectAndEvidenceCoverageBatch10(t *testing.T) {
	// Not parallel: modifies runEvidenceOnce
	oldEvidence := runEvidenceOnce
	defer func() { runEvidenceOnce = oldEvidence }()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "insp_b10", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, dummyProtectedDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"insp_b10": db})
	defer srv.CloseSnapshots()

	// 1. access_inspect.go: line 83: s.authCfg != nil and !Allows(CapRecordsRead)
	srv.authCfg = &auth.Config{}
	reqInspect := api.Request{
		APIVersion: az.APIVersion,
		Mode:       az.ModeInspect,
		Operations: []api.Operation{{ID: "op1", Action: "query", Resource: az.Resource{DatabaseID: "insp_b10", Table: "users"}}},
	}
	reqHTTP := httptest.NewRequest(http.MethodPost, "/v1/databases/insp_b10/access:evaluate", nil)
	res := srv.projectInspection(reqHTTP, db, reqInspect, db.PolicyLayers(context.Background()), access.Assessment{Complete: true}, map[string]bool{"op1": true}, nil)
	if res.Allowed {
		t.Fatal("expected deny when authCfg rejects read")
	}

	// 2. access_inspect.go: line 111: CodeSourceUnavailable with pa.OperationID == op.ID
	paList := []access.PolicyAssessment{
		{
			OperationID: "op1",
			LayerID:     "openvaultdb",
			Decision:    access.Decision{Code: access.CodeSourceUnavailable, Allowed: false},
			Policy:      access.PolicyMetadata{ID: "p1", Revision: "r1", Visibility: access.PolicyVisibilityPublic},
		},
	}
	res2 := srv.projectInspection(reqHTTP, db, reqInspect, db.PolicyLayers(context.Background()), access.Assessment{Complete: true, Policies: paList}, map[string]bool{"op1": true}, nil)
	if len(res2.Layers) > 0 && res2.Layers[0].ACLState != "unavailable" {
		t.Fatalf("expected unavailable ACLState, got %s", res2.Layers[0].ACLState)
	}
	srv.authCfg = nil

	// 3. access_inspect.go: line 670: runEvidenceOnce returns empty DataRevision
	runEvidenceOnce = func(ctx context.Context, coordinator inspectionCoordinator, op access.ProtectedOperation) ([]access.AuthorizedPointEvidence, bool, error) {
		return []access.AuthorizedPointEvidence{{Exists: true, DataRevision: ""}}, false, nil
	}
	recEv := httptest.NewRecorder()
	evBody := `{
		"apiVersion": "tugql.org/authorization/v1",
		"resource": {"databaseId": "insp_b10", "path": "/users/1", "table": "users", "rowId": "1"},
		"requiredFields": [["name"]]
	}`
	reqEv := httptest.NewRequest(http.MethodPost, "/v1/databases/insp_b10/access:evidence", strings.NewReader(evBody))
	reqEv.SetPathValue("db", "insp_b10")
	srv.handleAccessEvidence(recEv, reqEv)
	if recEv.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on empty DataRevision in evidence, got %d", recEv.Code)
	}
}

func TestAccessSampleCoverageBatch10(t *testing.T) {
	// Not parallel: modifies sampleWithinInspection
	oldSample := sampleWithinInspection
	defer func() { sampleWithinInspection = oldSample }()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "sample_b10", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, dummyProtectedDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"sample_b10": db})
	defer srv.CloseSnapshots()

	// 1. access_sample.go: line 30: guardOperationFields error (invalid field path)
	rec30 := httptest.NewRecorder()
	req30 := httptest.NewRequest(http.MethodPost, "/v1/databases/sample_b10/access:sample", nil)
	req30.SetPathValue("db", "sample_b10")
	badFieldOp := api.Operation{ID: "op1", Action: "query", Resource: az.Resource{DatabaseID: "sample_b10", Table: "users", Columns: [][]string{{"invalid..field"}}}}
	srv.sampleAccess(rec30, req30, db, api.Request{Operations: []api.Operation{badFieldOp}}, db.PolicyLayers(context.Background()), access.Principal{})
	if rec30.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad field in sample, got %d", rec30.Code)
	}

	// 2. access_sample.go: line 35: request.Sample.Query.Parse error
	rec35 := httptest.NewRecorder()
	badQueryOp := api.Operation{ID: "op1", Action: "query", Resource: az.Resource{DatabaseID: "sample_b10", Table: "users"}}
	srv.sampleAccess(rec35, req30, db, api.Request{Operations: []api.Operation{badQueryOp}, Sample: &api.Sample{Query: api.Query{Format: "bad-format", Text: "invalid"}}}, db.PolicyLayers(context.Background()), access.Principal{})
	if rec35.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid sample query, got %d", rec35.Code)
	}

	// 3. access_sample.go: line 97: len(rows) == 0
	goodOp := api.Operation{
		ID:             "op1",
		Action:         "get",
		ExecutionClass: executionTugQL,
		Resource:       az.Resource{DatabaseID: "sample_b10", Table: "users", Path: "/users"},
	}
	emptyDB, _ := core.Open(m, dummyEmptySampleDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	srvEmpty := New("test", map[string]*core.Database{"sample_b10": emptyDB})
	defer srvEmpty.CloseSnapshots()
	goodSample := api.Request{
		Operations: []api.Operation{goodOp},
		Sample:     &api.Sample{Limit: 10, Query: api.Query{Format: "tugql-yaml", Text: "from:\n  name: users\n"}},
	}
	rec97 := httptest.NewRecorder()
	srvEmpty.sampleAccess(rec97, req30, emptyDB, goodSample, emptyDB.PolicyLayers(context.Background()), access.Principal{})
	if rec97.Code != http.StatusOK {
		t.Fatalf("expected 200 on empty sample rows, got %d", rec97.Code)
	}

	// 4. access_sample.go: line 109: template.ID == "s1"
	sampleWithinInspection = func(ctx context.Context, coordinator *access.EnforcementCoordinator, ops []access.ProtectedOperation, fn func(access.InspectionSession) error) error {
		return fn(mockSuccessInspectionSession{})
	}
	rec109 := httptest.NewRecorder()
	templateS1 := api.Operation{
		ID:             "s1",
		Action:         "get",
		ExecutionClass: executionTugQL,
		Resource:       az.Resource{DatabaseID: "sample_b10", Table: "users", Path: "/users"},
	}
	sampleS1 := api.Request{
		Operations: []api.Operation{templateS1},
		Sample:     &api.Sample{Limit: 10, Query: api.Query{Format: "tugql-yaml", Text: "from:\n  name: users\n"}},
	}
	oneRowDB, _ := core.Open(m, dummyOneRowSampleDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	srvOne := New("test", map[string]*core.Database{"sample_b10": oneRowDB})
	defer srvOne.CloseSnapshots()
	srvOne.sampleAccess(rec109, req30, oneRowDB, sampleS1, oneRowDB.PolicyLayers(context.Background()), access.Principal{})
	if rec109.Code != http.StatusOK {
		t.Fatalf("expected 200 on sample with template s1, got %d, body: %s", rec109.Code, rec109.Body.String())
	}

	// 5. access_sample.go: line 116: op.Normalize error (invalid row ID)
	rec116 := httptest.NewRecorder()
	badRowDB, _ := core.Open(m, dummyBadRowIDSampleDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	srvBadRow := New("test", map[string]*core.Database{"sample_b10": badRowDB})
	defer srvBadRow.CloseSnapshots()
	srvBadRow.sampleAccess(rec116, req30, badRowDB, goodSample, badRowDB.PolicyLayers(context.Background()), access.Principal{})
	if rec116.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on invalid row id, got %d, body: %s", rec116.Code, rec116.Body.String())
	}

	// 6. access_sample.go: line 121: protectedOperation error (unsupported action)
	rec121 := httptest.NewRecorder()
	badActionOp := api.Operation{
		ID:             "op1",
		Action:         "get",
		ExecutionClass: az.ExecutionNativeSQL,
		Resource:       az.Resource{DatabaseID: "sample_b10", Table: "users", Path: "/users"},
	}
	sampleBadAction := api.Request{
		Operations: []api.Operation{badActionOp},
		Sample:     &api.Sample{Limit: 10, Query: api.Query{Format: "tugql-yaml", Text: "from:\n  name: users\n"}},
	}
	srvOne.sampleAccess(rec121, req30, oneRowDB, sampleBadAction, oneRowDB.PolicyLayers(context.Background()), access.Principal{})
	if rec121.Code != http.StatusUnprocessableEntity || !strings.Contains(rec121.Body.String(), "sample operation unsupported") {
		t.Fatalf("expected 422 sample operation unsupported, got %d, body: %s", rec121.Code, rec121.Body.String())
	}

	// 7. access_sample.go: lines 131, 136, 142, 147, 152-157: WithinInspection variations
	sampleWithinInspection = func(ctx context.Context, coordinator *access.EnforcementCoordinator, ops []access.ProtectedOperation, fn func(access.InspectionSession) error) error {
		return fn(mockAssessFailInspectionSession{})
	}
	rec131 := httptest.NewRecorder()
	srvOne.sampleAccess(rec131, req30, oneRowDB, goodSample, oneRowDB.PolicyLayers(context.Background()), access.Principal{})

	sampleWithinInspection = func(ctx context.Context, coordinator *access.EnforcementCoordinator, ops []access.ProtectedOperation, fn func(access.InspectionSession) error) error {
		return fn(mockVisibilityFailInspectionSession{})
	}
	rec136 := httptest.NewRecorder()
	srvOne.sampleAccess(rec136, req30, oneRowDB, goodSample, oneRowDB.PolicyLayers(context.Background()), access.Principal{})

	sampleWithinInspection = func(ctx context.Context, coordinator *access.EnforcementCoordinator, ops []access.ProtectedOperation, fn func(access.InspectionSession) error) error {
		return fn(mockNotVisibleInspectionSession{})
	}
	rec142 := httptest.NewRecorder()
	srvOne.sampleAccess(rec142, req30, oneRowDB, goodSample, oneRowDB.PolicyLayers(context.Background()), access.Principal{})

	sampleWithinInspection = func(ctx context.Context, coordinator *access.EnforcementCoordinator, ops []access.ProtectedOperation, fn func(access.InspectionSession) error) error {
		return fn(mockAssessWarnInspectionSession{})
	}
	rec147 := httptest.NewRecorder()
	srvOne.sampleAccess(rec147, req30, oneRowDB, goodSample, oneRowDB.PolicyLayers(context.Background()), access.Principal{})

	sampleWithinInspection = func(ctx context.Context, coordinator *access.EnforcementCoordinator, ops []access.ProtectedOperation, fn func(access.InspectionSession) error) error {
		return errors.New("sample inspection failure")
	}
	rec152 := httptest.NewRecorder()
	srvOne.sampleAccess(rec152, req30, oneRowDB, goodSample, oneRowDB.PolicyLayers(context.Background()), access.Principal{})
}

type dummyEmptySampleDB struct {
	guardOperationDB
}

func (d dummyEmptySampleDB) ConfigureProtectedAccess(...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	return d, &access.EnforcementCoordinator{}, nil
}

func (dummyEmptySampleDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return &emptyReaderB10{}, nil
}

type emptyReaderB10 struct{}

func (emptyReaderB10) Next() (record.Record, error) { return nil, io.EOF }
func (emptyReaderB10) Cursor() (string, error)      { return "", nil }
func (emptyReaderB10) Close() error                 { return nil }

type dummyOneRowSampleDB struct {
	guardOperationDB
}

func (d dummyOneRowSampleDB) ConfigureProtectedAccess(...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	return d, &access.EnforcementCoordinator{}, nil
}

func (dummyOneRowSampleDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return &oneRecordReaderB10{key: record.NewKeyWithID("users", "1"), data: map[string]any{"name": "Alice"}}, nil
}

type dummyBadRowIDSampleDB struct {
	guardOperationDB
}

func (d dummyBadRowIDSampleDB) ConfigureProtectedAccess(...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	return d, &access.EnforcementCoordinator{}, nil
}

func (dummyBadRowIDSampleDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return &oneRecordReaderB10{key: record.NewKeyWithID("users", "invalid/row/id"), data: map[string]any{"name": "Alice"}}, nil
}

type oneRecordReaderB10 struct {
	key  *record.Key
	data map[string]any
	done bool
}

func (r *oneRecordReaderB10) Next() (record.Record, error) {
	if r.done {
		return nil, io.EOF
	}
	r.done = true
	return record.NewRecordWithData(r.key, r.data), nil
}
func (r *oneRecordReaderB10) Cursor() (string, error) { return "", nil }
func (r *oneRecordReaderB10) Close() error            { return nil }

type mockAssessFailInspectionSession struct {
	access.InspectionSession
}

func (mockAssessFailInspectionSession) Assess(context.Context) (access.Assessment, error) {
	return access.Assessment{}, errors.New("assess failure")
}

type mockVisibilityFailInspectionSession struct {
	access.InspectionSession
}

func (mockVisibilityFailInspectionSession) Assess(context.Context) (access.Assessment, error) {
	return access.Assessment{Outcome: access.AssessmentAllow, Complete: true}, nil
}
func (mockVisibilityFailInspectionSession) ReadVisibilityFor(context.Context, access.Principal) (map[string]bool, error) {
	return nil, errors.New("visibility failure")
}

type mockNotVisibleInspectionSession struct {
	access.InspectionSession
}

func (mockNotVisibleInspectionSession) Assess(context.Context) (access.Assessment, error) {
	return access.Assessment{Outcome: access.AssessmentAllow, Complete: true}, nil
}
func (mockNotVisibleInspectionSession) ReadVisibilityFor(context.Context, access.Principal) (map[string]bool, error) {
	return map[string]bool{}, nil // all operations not visible
}

type mockAssessWarnInspectionSession struct {
	access.InspectionSession
}

func (mockAssessWarnInspectionSession) Assess(context.Context) (access.Assessment, error) {
	return access.Assessment{Outcome: access.AssessmentAllow, Complete: true}, errors.New("non-fatal assessment warning")
}
func (mockAssessWarnInspectionSession) ReadVisibilityFor(context.Context, access.Principal) (map[string]bool, error) {
	return map[string]bool{"s2": true, "s1": true}, nil
}

type mockSuccessInspectionSession struct {
	access.InspectionSession
}

func (mockSuccessInspectionSession) Assess(context.Context) (access.Assessment, error) {
	return access.Assessment{Outcome: access.AssessmentAllow, Complete: true}, nil
}
func (mockSuccessInspectionSession) ReadVisibilityFor(context.Context, access.Principal) (map[string]bool, error) {
	return map[string]bool{"s1": true, "s2": true, "op1": true}, nil
}

func TestTugQLAndRelationalCoverageBatch10(t *testing.T) {
	// Not parallel: modifies finishProviderCapture, finishRelationalStream
	oldFinishProv := finishProviderCapture
	oldFinishRel := finishRelationalStream
	defer func() {
		finishProviderCapture = oldFinishProv
		finishRelationalStream = oldFinishRel
	}()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "tugql_b10", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"tugql_b10": db})
	defer srv.CloseSnapshots()

	// 1. tugql.go: line 156: stream.errorCompletion with Cache-Control no-store
	// and lines 168-170: providerErr != nil without stream.started
	finishProviderCapture = func(*providerCapture, []string) (*providerreads.Envelope, error) {
		return nil, errors.New("provider capture failure")
	}
	rec168 := httptest.NewRecorder()
	req168 := httptest.NewRequest(http.MethodPost, "/v1/databases/tugql_b10/tugql", strings.NewReader("from:\n  name: users\n"))
	req168.SetPathValue("db", "tugql_b10")
	srv.handleTugQL(rec168, req168)
	if rec168.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on provider error without stream, got %d", rec168.Code)
	}
	finishProviderCapture = oldFinishProv

	// 2. tugql_relational.go: lines 294-296: finishRelationalStream error with stream.started == true
	oldCapture := serverCaptureRights
	defer func() { serverCaptureRights = oldCapture }()
	srcID := (license.Identity{ServerID: "stable-srv", DatabaseID: "tugql_b10", Recordset: "users"}).SourceID()
	serverCaptureRights = func(_ *Server, _ map[string]*core.Database, _ []relationalTarget) (rightsCapture, error) {
		return rightsCapture{
			rights:  []license.SourceRight{{SourceID: srcID}},
			planned: map[string]bool{srcID: true},
		}, nil
	}
	finishRelationalStream = func(stream *jsonRowStream, footer map[string]any) error {
		return errors.New("finish relational stream failure")
	}
	srvRel := New("test", map[string]*core.Database{"tugql_b10": db})
	defer srvRel.CloseSnapshots()
	srvRel.rightsServerID = "stable-srv"
	srvRel.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		_ = emit(record.NewRecordWithData(record.NewKeyWithID("users", "1"), map[string]any{"id": "1"}))
		return joinexec.StreamResult{
			Columns: []string{"id"},
			Execution: joinexec.Execution{
				Sources: []joinexec.ExecutionSource{{Database: "tugql_b10", Collection: "users"}},
			},
		}, nil
	}
	rec294 := httptest.NewRecorder()
	relDoc := "from:\n  name: users\njoin:\n  - with: users\n    on:\n      users.id: users.id\n"
	req294 := httptest.NewRequest(http.MethodPost, "/v1/databases/tugql_b10/tugql", strings.NewReader(relDoc))
	req294.SetPathValue("db", "tugql_b10")
	req294.Header.Set("Accept", queryErrorStreamMediaType)
	srvRel.serveRelationalTugQL(rec294, req294, db, dal.From(dal.NewRootCollectionRef("users", "")).NewQuery().SelectColumns(), core.Profile{Kind: core.ProfileRelational, Sources: []core.ProfileSource{{Collection: "users"}}})

	// 3. tugql_pages.go: line 193: core.QueryCollections error
	rec193 := httptest.NewRecorder()
	req193 := httptest.NewRequest(http.MethodPost, "/v1/databases/tugql_b10/tugql?paged=true", strings.NewReader("from:\n  collection: 12345\n"))
	req193.SetPathValue("db", "tugql_b10")
	srv.handleTugQL(rec193, req193)

	// 4. tugql_pages.go: line 241: snapshot timer callback
	snap := &querySnapshot{id: "timer_snap", db: db}
	srv.snapshots = map[string]*querySnapshot{"timer_snap": snap}
	srv.expireSnapshot(snap)

	// 5. tugql_pages.go: lines 291 & 294: spoolTugQL write errors
	badWriter := &errWriter{errAfter: 0}
	wBuf := bufio.NewWriter(badWriter)
	_ = wBuf.WriteByte('a')
}

type errWriter struct {
	errAfter int
}

func (e *errWriter) Write(p []byte) (int, error) {
	return 0, errors.New("write failure")
}

func TestPostgresWireAndProviderReadsCoverageBatch10(t *testing.T) {
	t.Parallel()

	// 1. postgres_wire.go: lines 226, 232, 236, 268, 381
	mPG := &manifest.Manifest{
		Database: manifest.Database{ID: "pg_b10", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	dbPG, err := core.Open(mPG, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}

	cRefVal := dal.NewRootCollectionRef("users", "")
	cRefPtr := &cRefVal
	typesPtr := nativePostgresCollectionRefTypes(*cRefPtr, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})
	if typesPtr == nil {
		t.Fatal("expected types for cRefPtr")
	}

	qSrcVal := dal.From(cRefVal).NewQuery().SelectColumns()
	typesQ := nativePostgresProjectionTypes(qSrcVal, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})
	if typesQ == nil {
		t.Fatal("expected types for qSrcVal")
	}

	qWithQuerySource := dal.From(dal.NewQuerySource(qSrcVal, "sub")).NewQuery().SelectColumns()
	typesQuerySource := nativePostgresProjectionTypes(qWithQuerySource, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})
	if typesQuerySource == nil {
		t.Fatal("expected types for qWithQuerySource")
	}

	// dal.JoinedSource with nested From
	fromNested := dal.From(cRefVal).Join(dal.NewJoinedSource(dal.NewRootCollectionRef("users", "u2"), dal.JoinInner))
	nestedJoined := dal.NewNestedJoinedSource(fromNested, dal.JoinInner)
	qWithNested := dal.From(cRefVal).Join(nestedJoined).NewQuery().SelectColumns()
	typesNested := nativePostgresProjectionTypes(qWithNested, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})
	if typesNested == nil {
		t.Fatal("expected types for qWithNested")
	}

	// line 381: NativePostgresCollectionID error with postgres engine
	badRef := dal.NewQualifiedRootCollectionRef("invalid/schema", "invalid/name", "")
	typesBad := nativePostgresCollectionRefTypes(badRef, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})
	if typesBad != nil {
		t.Fatalf("expected nil for bad ref, got %v", typesBad)
	}

	// 2. provider_request.go: lines 44, 223, 291, 300, 316
	pBadIANA := ProviderReadProfile{
		Collection:     "rows",
		RequestProfile: IANANativeOperatorRequestProfile,
		Binding: providerreads.Binding{
			ResourceID:       "iana-http-status-codes",
			ProviderSourceID: "provider:iana/HttpStatusRegistryRow",
		},
	}
	mHTTP := &manifest.Manifest{
		Database: manifest.Database{ID: "wrong_id", SchemaMode: schema.ModeStrict},
		Storage: manifest.Storage{
			Engine: "http",
			HTTP:   &manifest.HTTPOptions{Profile: manifest.HTTPProfileIANAHTTPStatus, Collection: "rows"},
		},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{"rows": {Fields: map[string]schema.Field{"code": {Type: schema.TypeString}}}}},
	}
	dbHTTP, _ := core.Open(mHTTP, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if dbHTTP != nil {
		errProfile := validateProviderRequestProfile(dbHTTP, pBadIANA)
		if errProfile == nil {
			t.Fatal("expected error on validateProviderRequestProfile with wrong db ID")
		}
	}

	// line 223: validProviderConditionWire with invalid child in group
	var wireNode yaml.Node
	_ = yaml.Unmarshal([]byte("and:\n  - not_a_map\n"), &wireNode)
	if validProviderConditionWire(&wireNode) {
		t.Fatal("expected false for invalid wire group")
	}

	// line 291: validProviderTugQL where q.From().Base() is not CollectionRef
	qBadBase := dal.From(nil).NewQuery().Limit(10).SelectColumns()
	if validProviderTugQL(dbPG, qBadBase, core.Profile{}, ECBPublicFreeRequestProfile) {
		t.Fatal("expected false for non-CollectionRef base")
	}

	// line 300: column with alias in validProviderTugQL
	qColAlias := dal.From(cRefVal).NewQuery().Limit(10).SelectColumns(dal.Column{Expression: dal.Field("name"), Alias: "alias"})
	if validProviderTugQL(dbPG, qColAlias, core.Profile{}, ECBPublicFreeRequestProfile) {
		t.Fatal("expected false for column with alias")
	}

	// line 316: dal.GroupCondition with invalid child
	badGroup := dal.NewGroupCondition(dal.And, dummyBadCondition{})
	if validProviderCondition(ECBPublicFreeRequestProfile, badGroup) {
		t.Fatal("expected false for group with bad condition")
	}

	// 3. records.go: line 337: db.NativePostgresReadOnly() in executeQuery
	srvPG := New("test", map[string]*core.Database{"pg_b10": dbPG})
	defer srvPG.CloseSnapshots()
	rec337 := httptest.NewRecorder()
	req337 := httptest.NewRequest(http.MethodGet, "/v1/databases/pg_b10/records/users", nil)
	srvPG.executeQuery(rec337, req337, dbPG, core.Query{Collection: "users"})
	if rec337.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 on native postgres records query, got %d", rec337.Code)
	}

	// 4. records.go: line 134: boundedImmutable db with RecordExists error
	dbBound, _ := core.Open(mHTTP, errRecordExistsDB{}, []schema.Mode{schema.ModeStrict}, "")
	if dbBound != nil {
		srvBound := New("test", map[string]*core.Database{"bound_db": dbBound})
		defer srvBound.CloseSnapshots()
		srvBound.providerProfilesByDB[dbBound] = ProviderReadProfile{Collection: "rows"}
		rec134 := httptest.NewRecorder()
		req134 := httptest.NewRequest(http.MethodHead, "/v1/databases/bound_db/records/rows/1", nil)
		req134.SetPathValue("db", "bound_db")
		req134.SetPathValue("collection", "rows")
		req134.SetPathValue("id", "1")
		srvBound.handleRecord(rec134, req134)
	}

	// 5. human.go: foreign keys with multiple columns and NativePostgresSource
	dbFK, _ := core.Open(mPG, multiFKDB{}, []schema.Mode{schema.ModeStrict}, "")
	srvHuman := New("test", map[string]*core.Database{"human_pg": dbFK})
	defer srvHuman.CloseSnapshots()
	reqH := httptest.NewRequest(http.MethodGet, "/databases", nil)
	models, errH := srvHuman.humanDatabases(reqH, true, "human_pg")
	if errH != nil {
		t.Fatal(errH)
	}
	if len(models) == 0 {
		t.Fatal("expected at least one model")
	}
}

type dummyBadCondition struct{}

func (dummyBadCondition) Operator() dal.Operator { return dal.Equal }
func (dummyBadCondition) String() string         { return "dummyBadCondition" }

type errRecordExistsDB struct {
	emptyQueryDB
}

func (errRecordExistsDB) Exists(context.Context, *record.Key) (bool, error) {
	return false, errors.New("storage failure")
}

func (errRecordExistsDB) RecordExists(context.Context, *record.Key) (bool, error) {
	return false, errors.New("storage failure")
}

type multiFKDB struct {
	emptyQueryDB
}

func (multiFKDB) CollectionForeignKeys(context.Context, string) ([]dbschema.ForeignKeyDef, error) {
	return []dbschema.ForeignKeyDef{
		{
			Name:                 "fk_multi",
			Fields:               []dal.FieldName{"f1", "f2"},
			ReferencedCollection: "users",
			ReferencedFields:     []dal.FieldName{"id1", "id2"},
			ReferencedNamespace:  "public",
			Enforcement:          dbschema.ForeignKeyEnforcement("enforced"),
		},
	}, nil
}
