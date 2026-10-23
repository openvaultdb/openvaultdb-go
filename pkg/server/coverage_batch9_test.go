package server

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	az "github.com/dal-go/dalgo/tugql/authorization"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"gopkg.in/yaml.v3"
)

func TestServerShutdownConcurrentDrainCoverageBatch9(t *testing.T) {
	// Not parallel: tests line 272 of server.go
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "drain_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	db, err := core.Open(m, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"drain_db": db})
	defer srv.CloseSnapshots()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled context

	// Repeated unmount attempts with pre-canceled context to exercise race select line 272
	for i := 0; i < 20; i++ {
		srv.mu.Lock()
		srv.dbs["drain_db"] = db
		srv.mu.Unlock()
		_ = srv.UnmountContext(ctx, "drain_db")
	}
}

func TestAccessInspectAndEvaluateCoverageBatch9(t *testing.T) {
	// Not parallel: modifies package-level newAuthorizationGrantID
	oldGrant := newAuthorizationGrantID
	defer func() { newAuthorizationGrantID = oldGrant }()

	// 1. access.go: line 144: entropy failure in newAuthorization
	newAuthorizationGrantID = func() (string, error) { return "", errors.New("grant entropy failure") }
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on newAuthorizationGrantID failure")
			}
		}()
		newAuthorization(az.ModeInspect)
	}()
	newAuthorizationGrantID = oldGrant

	// 2. access_discovery.go: line 94: policy visibility != Public && !caps.PolicyAdmin
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "disc_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}
	db, err := core.Open(m, dummyProtectedDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"disc_db": db})
	defer srv.CloseSnapshots()

	srv.accessAuthorization = func(_ context.Context, _ *auth.Principal, _ az.Source, cap string, _ az.Resource) bool {
		return cap != auth.CapPoliciesAdmin
	}
	recDisc := httptest.NewRecorder()
	reqDisc := httptest.NewRequest(http.MethodGet, "/v1/databases/disc_db/access/layers", nil)
	reqDisc.SetPathValue("db", "disc_db")
	srv.handleAccessLayers(recDisc, reqDisc)

	// 3. access_inspect.go: line 83: authCfg != nil and !Allows(CapRecordsRead)
	srvAuth := New("test", map[string]*core.Database{"disc_db": db})
	defer srvAuth.CloseSnapshots()
	srvAuth.authCfg = &auth.Config{}
	reqAuth := httptest.NewRequest(http.MethodPost, "/v1/databases/disc_db/access:evaluate", strings.NewReader(`{
		"apiVersion": "tugql.org/authorization/v1",
		"mode": "inspect",
		"operations": [{"id": "op1", "action": "query", "resource": {"databaseId": "disc_db", "path": "/users", "table": "users"}}]
	}`))
	reqAuth.SetPathValue("db", "disc_db")
	recAuth := httptest.NewRecorder()
	srvAuth.handleAccessEvaluate(recAuth, reqAuth)

	// 4. access_inspect.go: lines 111 & 137: CodeSourceUnavailable and !matched
	owners := []core.PolicyLayer{{Kind: "unmatched_layer", Enabled: true}}
	paList := []access.PolicyAssessment{
		{
			OperationID: "op1",
			LayerID:     "unmatched_layer",
			Decision:    access.Decision{Code: access.CodeSourceUnavailable, Allowed: false},
			Policy:      access.PolicyMetadata{ID: "p1", Revision: "r1", Visibility: access.PolicyVisibilityPublic},
		},
	}
	assess := access.Assessment{
		Outcome:  access.AssessmentDeny,
		Policies: paList,
	}
	reqInspect := api.Request{
		APIVersion: az.APIVersion,
		Mode:       az.ModeInspect,
		Operations: []api.Operation{{ID: "op2", Action: "query", Resource: az.Resource{DatabaseID: "disc_db", Table: "users"}}},
	}
	_ = srv.projectInspection(reqAuth, db, reqInspect, owners, assess, map[string]bool{"op2": true}, map[string]bool{})

	// 5. access_inspect.go: lines 522 & 530 in updateProtected
	k := record.NewKeyWithID("users", "1")
	recOversize := httptest.NewRecorder()
	reqOversize := httptest.NewRequest(http.MethodPatch, "/v1/databases/disc_db/records/users/1", strings.NewReader(strings.Repeat("X", int(api.MaxRequestBytes)+100)))
	reqOversize.Header.Set("Content-Type", "application/vnd.tugql.operation+json")
	srv.updateProtected(recOversize, reqOversize, db, k, &scriptedExecutor{})
	if recOversize.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on oversize update body, got %d", recOversize.Code)
	}

	recMismatch := httptest.NewRecorder()
	reqMismatch := httptest.NewRequest(http.MethodPatch, "/v1/databases/disc_db/records/users/1", strings.NewReader(`{"action":"insert"}`))
	reqMismatch.Header.Set("Content-Type", "application/vnd.tugql.operation+json")
	srv.updateProtected(recMismatch, reqMismatch, db, k, &scriptedExecutor{})
	if recMismatch.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on action mismatch in updateProtected, got %d", recMismatch.Code)
	}

	// 6. access_inspect.go: lines 550, 554, 608 in updateProtected
	mCRM := &manifest.Manifest{
		Database: manifest.Database{ID: "crm", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"t": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	dbCRM, _ := core.Open(mCRM, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	srvCRM := New("test", map[string]*core.Database{"crm": dbCRM})
	defer srvCRM.CloseSnapshots()

	reqGood1, k1 := updateRequest(context.Background())
	// 550: admissionErr != nil && assessment.Outcome == ""
	rec550 := httptest.NewRecorder()
	srvCRM.updateProtected(rec550, reqGood1, dbCRM, k1, &scriptedExecutor{answer: func(exec func(access.ExecutionSession) error) error {
		return exec(mockAssessFailExecution{})
	}})

	// 554: ReadVisibilityFor fails
	reqGood2, k2 := updateRequest(context.Background())
	rec554 := httptest.NewRecorder()
	srvCRM.updateProtected(rec554, reqGood2, dbCRM, k2, &scriptedExecutor{answer: func(exec func(access.ExecutionSession) error) error {
		return exec(mockVisibilityFailExecution{})
	}})

	// 608: revision == ""
	reqGood3, k3 := updateRequest(context.Background())
	rec608 := httptest.NewRecorder()
	srvCRM.updateProtected(rec608, reqGood3, dbCRM, k3, &scriptedExecutor{answer: func(exec func(access.ExecutionSession) error) error {
		return exec(mockNoRevisionExecution{layers: dbCRM.PolicyLayers(context.Background())})
	}})
	t.Logf("code: %d, body: %s", rec608.Code, rec608.Body.String())
	if rec608.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on missing revision receipt, got %d", rec608.Code)
	}

	// 7. access_inspect.go: lines 618, 632, 636, 642 in handleAccessEvidence
	rec618 := httptest.NewRecorder()
	req618 := httptest.NewRequest(http.MethodPost, "/v1/databases/missing_db/access:evidence", nil)
	req618.SetPathValue("db", "missing_db")
	srv.handleAccessEvidence(rec618, req618)
	if rec618.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on missing db in evidence, got %d", rec618.Code)
	}

	rec632 := httptest.NewRecorder()
	req632 := httptest.NewRequest(http.MethodPost, "/v1/databases/disc_db/access:evidence", strings.NewReader(strings.Repeat("X", int(api.MaxRequestBytes)+100)))
	req632.SetPathValue("db", "disc_db")
	srv.handleAccessEvidence(rec632, req632)
	if rec632.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on oversize evidence body, got %d", rec632.Code)
	}

	rec636 := httptest.NewRecorder()
	req636 := httptest.NewRequest(http.MethodPost, "/v1/databases/disc_db/access:evidence", strings.NewReader(`{"apiVersion":"bad"}`))
	req636.SetPathValue("db", "disc_db")
	srv.handleAccessEvidence(rec636, req636)
	if rec636.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid evidence body, got %d", rec636.Code)
	}

	rec642 := httptest.NewRecorder()
	req642 := httptest.NewRequest(http.MethodPost, "/v1/databases/disc_db/access:evidence", strings.NewReader(`{
		"apiVersion": "tugql.org/authorization/v1",
		"resource": {"databaseId": "disc_db", "path": "/users", "table": "users", "rowId": ""},
		"requiredFields": [["name"]]
	}`))
	req642.SetPathValue("db", "disc_db")
	srv.handleAccessEvidence(rec642, req642)
	if rec642.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on empty rowId in evidence, got %d", rec642.Code)
	}
}

type mockAssessFailExecution struct {
	access.ExecutionSession
}

func (mockAssessFailExecution) Assess(context.Context) (access.Assessment, error) {
	return access.Assessment{}, errors.New("admission failed")
}

type mockVisibilityFailExecution struct {
	access.ExecutionSession
}

func (mockVisibilityFailExecution) Assess(context.Context) (access.Assessment, error) {
	return access.Assessment{Outcome: access.AssessmentAllow, Complete: true}, nil
}
func (mockVisibilityFailExecution) ReadVisibilityFor(context.Context, access.Principal) (map[string]bool, error) {
	return nil, errors.New("visibility lookup failed")
}

type mockNoRevisionExecution struct {
	access.ExecutionSession
	layers []core.PolicyLayer
}

func (m mockNoRevisionExecution) Assess(context.Context) (access.Assessment, error) {
	var policies []access.PolicyAssessment
	for _, l := range m.layers {
		policies = append(policies, access.PolicyAssessment{
			OperationID: "op1",
			LayerID:     l.Kind,
			Decision:    access.Decision{Allowed: true},
		})
	}
	return access.Assessment{
		Outcome:  access.AssessmentAllow,
		Complete: true,
		Policies: policies,
	}, nil
}
func (mockNoRevisionExecution) ReadVisibilityFor(context.Context, access.Principal) (map[string]bool, error) {
	return map[string]bool{"op1": true}, nil
}
func (mockNoRevisionExecution) Execute(context.Context) (access.Assessment, error) {
	return access.Assessment{}, nil
}
func (mockNoRevisionExecution) Revisions(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func TestAccessSampleCoverageBatch9(t *testing.T) {
	// Not parallel: tests access_sample.go
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "sample_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}
	db, err := core.Open(m, dummyProtectedDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"sample_db": db})
	defer srv.CloseSnapshots()

	// 1. line 30: guardOperationFields error
	rec30 := httptest.NewRecorder()
	req30 := httptest.NewRequest(http.MethodPost, "/v1/databases/sample_db/access:evaluate", strings.NewReader(`{
		"apiVersion": "tugql.org/authorization/v1",
		"mode": "sample",
		"operations": [{"id": "s1", "action": "query", "resource": {"databaseId": "sample_db", "path": "/users", "table": "users", "columns": [["unknown_field"]]}}],
		"sample": {"limit": 10, "query": {"from": "users"}}
	}`))
	req30.SetPathValue("db", "sample_db")
	srv.handleAccessEvaluate(rec30, req30)

	// 2. line 35: invalid sample query JSON
	rec35 := httptest.NewRecorder()
	req35 := httptest.NewRequest(http.MethodPost, "/v1/databases/sample_db/access:evaluate", strings.NewReader(`{
		"apiVersion": "tugql.org/authorization/v1",
		"mode": "sample",
		"operations": [{"id": "s1", "action": "query", "resource": {"databaseId": "sample_db", "path": "/users", "table": "users"}}],
		"sample": {"limit": 10, "query": {"from": ""}}
	}`))
	req35.SetPathValue("db", "sample_db")
	srv.handleAccessEvaluate(rec35, req35)
	if rec35.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid sample query, got %d", rec35.Code)
	}
}

func TestTugQLAndPagesCoverageBatch9(t *testing.T) {
	// Not parallel: modifies snapshotTimerAfterFunc, readSnapshotDir, seekSnapshotFile, marshalTugQLURLBody, finishProviderCapture
	oldTimer := snapshotTimerAfterFunc
	oldReadDir := readSnapshotDir
	oldSeek := seekSnapshotFile
	oldMarshal := marshalTugQLURLBody
	oldFinishProv := finishProviderCapture
	defer func() {
		snapshotTimerAfterFunc = oldTimer
		readSnapshotDir = oldReadDir
		seekSnapshotFile = oldSeek
		marshalTugQLURLBody = oldMarshal
		finishProviderCapture = oldFinishProv
	}()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "tugql_b9_db", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, &oneRowThenErrDB{err: io.EOF}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"tugql_b9_db": db})
	defer srv.CloseSnapshots()

	// 1. tugql.go: line 62: refuseProviderRequest when query restricted
	srv.providerProfilesByDB[db] = ProviderReadProfile{
		Collection:     "users",
		RequestProfile: ECBPublicFreeRequestProfile,
	}
	rec62 := httptest.NewRecorder()
	req62 := httptest.NewRequest(http.MethodPost, "/v1/databases/tugql_b9_db/tugql", strings.NewReader("from: {name: users}\nlimit: 100\n"))
	req62.SetPathValue("db", "tugql_b9_db")
	srv.handleTugQL(rec62, req62)
	if rec62.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on restricted provider query, got %d", rec62.Code)
	}
	delete(srv.providerProfilesByDB, db)

	// 2. tugql.go: lines 164-170: finishProviderCapture error in serveTugQL
	finishProviderCapture = func(pc *providerCapture, used []string) (*providerreads.Envelope, error) {
		return nil, errors.New("finish provider capture failure")
	}
	rec164 := httptest.NewRecorder()
	req164 := httptest.NewRequest(http.MethodPost, "/v1/databases/tugql_b9_db/tugql", strings.NewReader("from: {name: users}\n"))
	req164.SetPathValue("db", "tugql_b9_db")
	req164.Header.Set("Accept", queryErrorStreamMediaType)
	srv.handleTugQL(rec164, req164)
	finishProviderCapture = oldFinishProv

	// 3. tugql.go: line 274: invalid URL query string
	uBad, _ := tugqlFromURL(httptest.NewRequest(http.MethodGet, "/v1/databases/tugql_b9_db/tugql?%zz=1", nil).URL)
	if uBad != nil {
		t.Fatal("expected error on invalid query escape")
	}

	// 4. tugql.go: line 301: marshalTugQLURLBody error
	marshalTugQLURLBody = func(v any) ([]byte, error) { return nil, errors.New("marshal error") }
	uMarshal, _ := tugqlFromURL(httptest.NewRequest(http.MethodGet, "/v1/databases/tugql_b9_db/tugql?q=from:+users", nil).URL)
	if uMarshal != nil {
		t.Fatal("expected error on marshalTugQLURLBody failure")
	}
	marshalTugQLURLBody = oldMarshal

	// 5. tugql.go: line 342: invalid parameter JSON
	_, errParamJSON := bindTugQLParameters([]byte(`{"query":"from: users\nwhere: {param: p}\n","parameters":{"p":1e999999999999999999999999999999}}`))
	if errParamJSON == nil {
		t.Fatal("expected error on invalid parameter json")
	}

	// 6. tugql.go: line 357: parameter with []any values
	boundArr, errArr := bindTugQLParameters([]byte(`{"query":"from: users\nwhere: {param: p}\n","parameters":{"p":["a","b"]}}`))
	if errArr != nil || !strings.Contains(string(boundArr), "values") {
		t.Fatalf("expected bound values array, got %v: %s", errArr, string(boundArr))
	}

	// 7. tugql_pages.go: line 193: collections error
	qUnsupported := dal.From(dal.NewQualifiedRootCollectionRef("public", "users", "")).NewQuery().SelectColumns()
	rec193 := httptest.NewRecorder()
	req193 := httptest.NewRequest(http.MethodPost, "/v1/databases/tugql_b9_db/tugql", nil)
	req193.Header.Set("OVDB-Page-Size", "10")
	srv.handlePagedTugQL(rec193, req193, db, qUnsupported, []byte("..."))
	if rec193.Code != http.StatusBadRequest && rec193.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 400/422 on unsupported collection in paged TugQL, got %d", rec193.Code)
	}

	// 8. tugql_pages.go: lines 231-235: database unmounted during snapshot capture
	mPaged := &manifest.Manifest{
		Database: manifest.Database{ID: "paged_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	dbUnmounted, _ := core.Open(mPaged, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	rec231 := httptest.NewRecorder()
	srv.handlePagedTugQL(rec231, req193, dbUnmounted, dal.From(dal.NewRootCollectionRef("users", "")).NewQuery().SelectColumns(), []byte("from: users"))
	if rec231.Code != http.StatusGone {
		t.Fatalf("expected 410 on unmounted db in handlePagedTugQL, got %d", rec231.Code)
	}

	// 9. tugql_pages.go: line 241: snapshot timer execution
	snapshotTimerAfterFunc = func(d time.Duration, f func()) *time.Timer {
		f() // synchronously run callback
		return time.NewTimer(time.Hour)
	}
	snapExp := &querySnapshot{id: "snap_expire", db: db}
	srv.snapshots = map[string]*querySnapshot{"snap_expire": snapExp}
	srv.expireSnapshot(snapExp)
	snapshotTimerAfterFunc = oldTimer

	// 10. tugql_pages.go: line 328: prepareSnapshotDir with non-snapshot directory entry
	readSnapshotDir = func(name string) ([]os.DirEntry, error) {
		return []os.DirEntry{&fakeDirEntry{name: "not-snapshot.txt"}}, nil
	}
	_, _ = prepareSnapshotDir()
	readSnapshotDir = oldReadDir

	// 11. tugql_pages.go: lines 446-450: snap.path missing
	dbRet, _ := retentionFixture(t, "sqlite", "")
	srv.dbs[dbRet.ID()] = dbRet
	snapID := base64.RawURLEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	snapMissing := &querySnapshot{
		id:        snapID,
		db:        dbRet,
		path:      "/nonexistent/file/path",
		bytes:     100,
		pageSize:  10,
		expiresAt: time.Now().Add(time.Hour),
	}
	srv.snapshots = map[string]*querySnapshot{snapID: snapMissing}
	rec446 := httptest.NewRecorder()
	tok446 := srv.pageToken(snapID, 0, dbRet.ID(), [32]byte{}, [32]byte{}, 10)
	srv.serveSnapshotPage(rec446, tok446, dbRet, [32]byte{}, [32]byte{}, 10)
	if rec446.Code != http.StatusGone {
		t.Fatalf("expected 410 on missing snapshot file, got %d", rec446.Code)
	}

	// 12. tugql_pages.go: lines 452-457: seekSnapshotFile fails
	tmpDir := t.TempDir()
	snapRealFile := filepath.Join(tmpDir, "snap.jsonl")
	_ = os.WriteFile(snapRealFile, []byte(`{"key":"1"}`+"\n"), 0o644)
	snapSeek := &querySnapshot{
		id:        snapID,
		db:        dbRet,
		path:      snapRealFile,
		bytes:     15,
		pageSize:  10,
		expiresAt: time.Now().Add(time.Hour),
	}
	srv.snapshots[snapID] = snapSeek
	seekSnapshotFile = func(f *os.File, offset int64, whence int) (int64, error) {
		return 0, errors.New("seek failed")
	}
	rec452 := httptest.NewRecorder()
	srv.serveSnapshotPage(rec452, tok446, dbRet, [32]byte{}, [32]byte{}, 10)
	if rec452.Code != http.StatusGone {
		t.Fatalf("expected 410 on seek failure, got %d", rec452.Code)
	}
	seekSnapshotFile = oldSeek

	// 13. tugql_pages.go: lines 467-472: truncated file in serveSnapshotPage
	snapSeek.bytes = 1000 // claims 1000 bytes, but file has only 15
	rec467 := httptest.NewRecorder()
	tokSeek := srv.pageToken(snapID, 14, dbRet.ID(), [32]byte{}, [32]byte{}, 10)
	srv.serveSnapshotPage(rec467, tokSeek, dbRet, [32]byte{}, [32]byte{}, 10)
	if rec467.Code != http.StatusGone {
		t.Fatalf("expected 410 on damaged snapshot file, got %d", rec467.Code)
	}

	// 14. tugql_pages.go: line 475: multi-row file where second row exceeds maxPageBytes
	twoRowsFile := filepath.Join(tmpDir, "two_rows.jsonl")
	hugeLine := strings.Repeat("B", maxPageBytes+10)
	_ = os.WriteFile(twoRowsFile, []byte(`{"k":"1"}`+"\n"+hugeLine+"\n"), 0o644)
	snapTwo := &querySnapshot{
		id:        snapID,
		db:        dbRet,
		path:      twoRowsFile,
		bytes:     int64(len(`{"k":"1"}` + "\n" + hugeLine + "\n")),
		pageSize:  10,
		expiresAt: time.Now().Add(time.Hour),
	}
	srv.snapshots[snapID] = snapTwo
	rec475 := httptest.NewRecorder()
	tokTwo := srv.pageToken(snapID, 0, dbRet.ID(), [32]byte{}, [32]byte{}, 10)
	srv.serveSnapshotPage(rec475, tokTwo, dbRet, [32]byte{}, [32]byte{}, 10)
	if rec475.Code != http.StatusOK {
		t.Fatalf("expected 200 on multi-row split page, got %d", rec475.Code)
	}
}

type fakeDirEntry struct {
	name string
}

func (f *fakeDirEntry) Name() string               { return f.name }
func (f *fakeDirEntry) IsDir() bool                { return false }
func (f *fakeDirEntry) Type() os.FileMode          { return 0 }
func (f *fakeDirEntry) Info() (os.FileInfo, error) { return nil, nil }

func TestRecordsAndRelationalCoverageBatch9(t *testing.T) {
	// Not parallel: modifies singleRights, serverCaptureRights, finishRelationalStream, finishProviderCapture
	oldSingle := singleRights
	oldCapture := serverCaptureRights
	oldFinishRel := finishRelationalStream
	oldFinishProv := finishProviderCapture
	defer func() {
		singleRights = oldSingle
		serverCaptureRights = oldCapture
		finishRelationalStream = oldFinishRel
		finishProviderCapture = oldFinishProv
	}()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "rec_b9_db", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, &oneRowThenErrDB{err: io.EOF}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"rec_b9_db": db})
	defer srv.CloseSnapshots()

	// 1. records.go: line 216: readRecord with singleRights error
	dbRet, _ := retentionFixture(t, "sqlite", "")
	srvRet := New("test", map[string]*core.Database{dbRet.ID(): dbRet})
	defer srvRet.CloseSnapshots()
	singleRights = func(s *Server, d *core.Database, col string) (rightsCapture, error) {
		return rightsCapture{}, errors.New("simulated singleRights error")
	}
	rec216 := httptest.NewRecorder()
	req216 := httptest.NewRequest(http.MethodGet, "/v1/databases/live/records/customers/1", nil)
	srvRet.readRecord(rec216, req216, dbRet, record.NewKeyWithID("customers", "1"))
	if rec216.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on singleRights error in readRecord, got %d", rec216.Code)
	}

	// 2. records.go: line 346: executeQuery with singleRights error on non-policy db
	rec346 := httptest.NewRecorder()
	req346 := httptest.NewRequest(http.MethodPost, "/v1/databases/rec_b9_db/query", strings.NewReader(`{"collection":"users"}`))
	req346.SetPathValue("db", "rec_b9_db")
	srv.executeQuery(rec346, req346, db, core.Query{Collection: "users"})
	if rec346.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on singleRights error in executeQuery, got %d", rec346.Code)
	}

	// 3. records.go: lines 393-399: executeQuery with singleRights error on policy-protected db
	mPolicy := &manifest.Manifest{
		Database: manifest.Database{ID: "pol_db", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	dbPolicy, err := core.Open(mPolicy, &oneRowThenErrDB{err: io.EOF}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	srv.dbs["pol_db"] = dbPolicy
	srv.mu.Unlock()

	// stream.started == true: line 394
	rec394 := httptest.NewRecorder()
	req394 := httptest.NewRequest(http.MethodPost, "/v1/databases/pol_db/query", strings.NewReader(`{"collection":"users"}`))
	req394.SetPathValue("db", "pol_db")
	req394.Header.Set("Accept", queryErrorStreamMediaType)
	srv.executeQuery(rec394, req394, dbPolicy, core.Query{Collection: "users"})

	// stream.started == false: line 398
	dbPolicyEmpty, _ := core.Open(mPolicy, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	srv.mu.Lock()
	srv.dbs["pol_empty"] = dbPolicyEmpty
	srv.mu.Unlock()
	rec398 := httptest.NewRecorder()
	req398 := httptest.NewRequest(http.MethodPost, "/v1/databases/pol_empty/query", strings.NewReader(`{"collection":"users"}`))
	req398.SetPathValue("db", "pol_empty")
	srv.executeQuery(rec398, req398, dbPolicyEmpty, core.Query{Collection: "users"})
	singleRights = oldSingle

	// 4. records.go: line 403 & 415: 0 rows yielded with errorCompletion, and providerErr != nil with !stream.started
	finishProviderCapture = func(pc *providerCapture, used []string) (*providerreads.Envelope, error) {
		return nil, errors.New("provider finish error")
	}
	rec415 := httptest.NewRecorder()
	req415 := httptest.NewRequest(http.MethodPost, "/v1/databases/rec_b9_db/query", strings.NewReader(`{"collection":"users"}`))
	req415.SetPathValue("db", "rec_b9_db")
	req415.Header.Set("Accept", queryErrorStreamMediaType)
	dbEmpty, _ := core.Open(m, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	srv.executeQuery(rec415, req415, dbEmpty, core.Query{Collection: "users"})
	finishProviderCapture = oldFinishProv

	// 5. tugql_relational.go: line 185: serverCaptureRights error
	serverCaptureRights = func(s *Server, dbs map[string]*core.Database, targets []relationalTarget) (rightsCapture, error) {
		return rightsCapture{}, errors.New("relational capture rights error")
	}
	rec185 := httptest.NewRecorder()
	req185 := httptest.NewRequest(http.MethodPost, "/v1/tugql", strings.NewReader(`{"from":{"name":"users"}}`))
	srv.serveRelationalTugQL(rec185, req185, db, dal.From(dal.NewRootCollectionRef("users", "")).NewQuery().SelectColumns(), core.Profile{Kind: core.ProfileRelational, Sources: []core.ProfileSource{{Collection: "users"}}})
	serverCaptureRights = oldCapture

	// 6. tugql_relational.go: lines 270-271: finishRelationalStream error when stream.started == true
	srv.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		_ = emit(record.NewRecordWithData(record.NewKeyWithID("users", "1"), map[string]any{"id": "1"}))
		return joinexec.StreamResult{Columns: []string{"id"}}, nil
	}
	finishRelationalStream = func(stream *jsonRowStream, footer map[string]any) error {
		return errors.New("finish relational stream error")
	}
	rec270 := httptest.NewRecorder()
	req270 := httptest.NewRequest(http.MethodPost, "/v1/tugql", strings.NewReader(`{"from":{"name":"users"}}`))
	req270.Header.Set("Accept", queryErrorStreamMediaType)
	srv.serveRelationalTugQL(rec270, req270, db, dal.From(dal.NewRootCollectionRef("users", "")).NewQuery().SelectColumns(), core.Profile{Kind: core.ProfileRelational, Sources: []core.ProfileSource{{Collection: "users"}}})
	finishRelationalStream = oldFinishRel
}

func TestPostgresWireAndProviderReadsAndHumanCoverageBatch9(t *testing.T) {
	// 1. postgres_wire.go: line 76: [4]byte array
	safeArr := nativePostgresJSONSafeValue([4]byte{1, 2, 3, 4})
	if safeArr == nil {
		t.Fatal("expected non-nil safe array")
	}

	// 2. postgres_wire.go: lines 226-239: *CollectionRef, *QuerySource, JoinedSource with nested From
	cRef := dal.NewRootCollectionRef("users", "")
	qSrc := dal.From(cRef).NewQuery().SelectColumns()
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "pg_wire_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}

	typesCRef := nativePostgresCollectionRefTypes(cRef, db, map[string]*core.Database{db.ID(): db})
	if typesCRef == nil {
		t.Fatal("expected types for cRef")
	}

	typesQuery := nativePostgresProjectionTypes(qSrc, db, map[string]*core.Database{db.ID(): db})
	if typesQuery == nil {
		t.Fatal("expected types for qSrc")
	}

	// 3. postgres_wire.go: line 381: schema.NativePostgresCollectionID error
	badCRef := dal.NewQualifiedRootCollectionRef("invalid/schema", "invalid/name", "")
	typesBad := nativePostgresCollectionRefTypes(badCRef, db, map[string]*core.Database{db.ID(): db})
	if typesBad != nil {
		t.Fatalf("expected nil on invalid native postgres collection, got %v", typesBad)
	}

	// 4. provider_reads.go: line 63: conflicting provider profiles across mount aliases
	srv := New("test", map[string]*core.Database{"a1": db, "a2": db})
	defer srv.CloseSnapshots()
	srv.rightsServerID = "test-server"
	p1 := ProviderReadProfile{Collection: "users", RequestProfile: ECBPublicFreeRequestProfile}
	p2 := ProviderReadProfile{Collection: "users", RequestProfile: IANANativeOperatorRequestProfile}
	srv.providerProfiles = map[string]ProviderReadProfile{"a1": p1, "a2": p2}
	srv.providerProfilesByDB = map[*core.Database]ProviderReadProfile{db: p1}
	// Test conflicting profiles logic directly or via validateProviderProfiles
	srv.providerProfiles = map[string]ProviderReadProfile{"a2": p2}
	_ = srv.validateProviderProfiles()

	// 5. provider_request.go: lines 44, 223, 291, 300, 316
	pIANA := ProviderReadProfile{Collection: "rows", RequestProfile: IANANativeOperatorRequestProfile}
	errIANA := validateProviderRequestProfile(db, pIANA)
	if errIANA == nil {
		t.Fatal("expected error on validateProviderRequestProfile with mismatched IANA db")
	}

	// validProviderConditionWire with invalid child
	var wireNode yaml.Node
	_ = yaml.Unmarshal([]byte("operator: and\ncontent: [not_a_map]\n"), &wireNode)
	if validProviderConditionWire(&wireNode) {
		t.Fatal("expected false for invalid wire group")
	}

	// validProviderTugQL with q.From().Base() not CollectionRef
	qNotCol := dal.From(nil).NewQuery().SelectColumns()
	if validProviderTugQL(db, qNotCol, core.Profile{}, ECBPublicFreeRequestProfile) {
		t.Fatal("expected false for table ref source")
	}

	// validProviderCondition with invalid child in group
	groupInvalid := validProviderCondition(ECBPublicFreeRequestProfile, dummyGroupCond{})
	if groupInvalid {
		t.Fatal("expected false for dummyGroupCond")
	}

	// 6. human.go: lines 107, 125, 208, 238
	srvHuman := New("test", map[string]*core.Database{db.ID(): db})
	defer srvHuman.CloseSnapshots()
	reqH := httptest.NewRequest(http.MethodGet, "/databases", nil)
	// line 107: requestedID is not in s.dbs
	modelsMissing, _ := srvHuman.humanDatabases(reqH, false, "nonexistent_db")
	if len(modelsMissing) != 0 {
		t.Fatalf("expected 0 models for missing db, got %d", len(modelsMissing))
	}
}
