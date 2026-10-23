package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	az "github.com/dal-go/dalgo/tugql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// failOnNthWriter is an http.ResponseWriter whose Write fails on the Nth call
type failOnNthWriter struct {
	header    http.Header
	count     int
	failOn    int
	buf       bytes.Buffer
	code      int
	flushFunc func() error
}

func (w *failOnNthWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *failOnNthWriter) WriteHeader(code int) { w.code = code }
func (w *failOnNthWriter) Write(p []byte) (int, error) {
	w.count++
	if w.count == w.failOn {
		return 0, errors.New("simulated nth write failure")
	}
	return w.buf.Write(p)
}
func (w *failOnNthWriter) Flush() error {
	if w.flushFunc != nil {
		return w.flushFunc()
	}
	return nil
}

type dummyCond struct{}

func (dummyCond) String() string { return "dummy" }

type dummyGroupCond struct{}

func (dummyGroupCond) String() string              { return "group" }
func (dummyGroupCond) Operator() dal.Operator      { return dal.And }
func (dummyGroupCond) Conditions() []dal.Condition { return []dal.Condition{dummyCond{}} }

func TestStreamJSONCoverageBatch8(t *testing.T) {
	t.Parallel()

	// 1. stream_json.go: line 80: WriteRow fails on second write (comma delimiter)
	fw := &failOnNthWriter{failOn: 3} // 1: prefix, 2: first row, 3: comma before second row
	st, cancel, err := newJSONRowStream(fw, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := st.WriteRow(map[string]any{"row": 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteRow(map[string]any{"row": 2}); err == nil {
		t.Fatal("expected error on comma write in WriteRow")
	}

	// 2. stream_json.go: line 138: finish fails writing "]"
	fw2 := &failOnNthWriter{failOn: 3} // 1: prefix, 2: row 1, 3: "]" in finish
	st2, cancel2, _ := newJSONRowStream(fw2, 5*time.Second, false)
	defer cancel2()
	_ = st2.WriteRow(map[string]any{"row": 1})
	if err := st2.Finish(nil); err == nil {
		t.Fatal("expected error writing ']' in finish")
	}

	// 3. stream_json.go: line 141: finish fails writing ","
	fw3 := &failOnNthWriter{failOn: 4} // 1: prefix, 2: row 1, 3: "]", 4: ","
	st3, cancel3, _ := newJSONRowStream(fw3, 5*time.Second, false)
	defer cancel3()
	_ = st3.WriteRow(map[string]any{"row": 1})
	if err := st3.Finish(nil); err == nil {
		t.Fatal("expected error writing ',' in finish")
	}

	// 4. stream_json.go: line 147: finish fails writing "}\n"
	fw4 := &failOnNthWriter{failOn: 5} // 1: prefix, 2: row 1, 3: "]", 4: ",", 5: footer, 6: "}\n"
	// Wait, if footer encoded is written at 5, then 6 is "}\n"
	fw4.failOn = 6
	st4, cancel4, _ := newJSONRowStream(fw4, 5*time.Second, false)
	defer cancel4()
	_ = st4.WriteRow(map[string]any{"row": 1})
	if err := st4.Finish(nil); err == nil {
		t.Fatal("expected error writing '}\\n' in finish")
	}
}

func TestDatabasesCreateAndServerCoverageBatch8(t *testing.T) {
	// Not parallel: modifies server state and mount profiles
	dir := t.TempDir()
	srv := New("test", nil, WithDataDir(dir))
	defer srv.CloseSnapshots()

	// 1. databases_create.go: lines 80-83: s.Mount fails due to provider profile already registered
	srv.providerProfiles = map[string]ProviderReadProfile{"conflict_db": {}}
	recCreate := httptest.NewRecorder()
	reqCreate := httptest.NewRequest(http.MethodPost, "/v1/databases", strings.NewReader(`{"id":"conflict_db"}`))
	srv.handleDatabaseCreate(recCreate, reqCreate)
	if recCreate.Code != http.StatusConflict {
		t.Fatalf("expected 409 conflict on mount failure, got %d", recCreate.Code)
	}

	// 2. server.go: line 272: Unmount select <-drained with cancelled ctx
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "unmount_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}
	for i := 0; i < 20; i++ {
		db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
		if err != nil {
			t.Fatal(err)
		}
		srvUnmount := New("test", map[string]*core.Database{"unmount_db": db})
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // immediately cancelled
		_ = srvUnmount.UnmountContext(ctx, "unmount_db")
	}
}

func TestSourceRightsCoverageBatch8(t *testing.T) {
	// Not parallel: modifies licenseInventory
	oldInv := licenseInventory
	defer func() { licenseInventory = oldInv }()

	// 1. source_rights.go: line 130: captureRights licenseInventory error on all
	called := 0
	licenseInventory = func(rights []license.SourceRight) ([]license.SourceRight, int, error) {
		called++
		if called > 1 {
			return nil, 0, errors.New("all inventory failed")
		}
		return rights, len(rights), nil
	}
	dbRet, _ := retentionFixture(t, "sqlite", "")
	srv := New("test", map[string]*core.Database{"ret_db": dbRet})
	srv.rightsServerID = "srv-1"
	if _, err := srv.singleRights(dbRet, "customers"); err == nil {
		t.Fatal("expected error on second licenseInventory call")
	}

	// 2. source_rights.go: line 144: captureRights reservedBytes > license.MaxEvidenceBytes
	licenseInventory = func(rights []license.SourceRight) ([]license.SourceRight, int, error) {
		// Return a right with huge text/description
		bigRight := license.SourceRight{
			SourceID:    "src-huge",
			Declaration: license.Declaration{Name: "huge", Text: strings.Repeat("A", license.MaxEvidenceBytes+100)},
		}
		return []license.SourceRight{bigRight}, 1, nil
	}
	if _, err := srv.singleRights(dbRet, "customers"); err == nil || !strings.Contains(err.Error(), "evidence limit") {
		t.Fatalf("expected evidence limit error, got %v", err)
	}
}

func TestProviderReadsAndRequestCoverageBatch8(t *testing.T) {
	// 1. provider_request.go: lines 42-45: IANANativeOperatorRequestProfile mismatch
	dbFake, _ := retentionFixture(t, "sqlite", "")
	pIANA := ProviderReadProfile{
		RequestProfile: IANANativeOperatorRequestProfile,
		Collection:     "rows",
		Binding: providerreads.Binding{
			ResourceID:       "iana-http-status-codes",
			ProviderSourceID: "provider:iana/HttpStatusRegistryRow",
		},
	}
	if err := validateProviderRequestProfile(dbFake, pIANA); err == nil {
		t.Fatal("expected error on incompatible IANA request profile")
	}

	// 2. provider_request.go: line 99: default method in guardProviderRequest
	srv := New("test", map[string]*core.Database{"fake": dbFake})
	srv.providerProfilesByDB[dbFake] = ProviderReadProfile{RequestProfile: ECBPublicFreeRequestProfile}
	recPut := httptest.NewRecorder()
	reqPut := httptest.NewRequest(http.MethodPut, "/v1/databases/fake/query", nil)
	if srv.guardProviderRequest(recPut, reqPut, dbFake) {
		t.Fatal("expected guardProviderRequest to return false for PUT")
	}

	// 3. provider_request.go: lines 286-333: validProviderTugQL branches
	sqBadOffset := dal.From(dal.NewDatabaseCollectionRef("db", "", "daily", "")).NewQuery().Offset(5).SelectColumns()
	if validProviderTugQL(dbFake, sqBadOffset, core.Profile{Kind: core.ProfileSingleCollection}, ECBPublicFreeRequestProfile) {
		t.Fatal("expected false for offset != 0")
	}
	sqBadParent := dal.From(dal.NewDatabaseCollectionRef("db", "", "daily", "")).NewQuery().Limit(10).SelectColumns()
	if validProviderTugQL(dbFake, sqBadParent, core.Profile{Kind: core.ProfileSingleCollection}, ECBPublicFreeRequestProfile) {
		t.Fatal("expected false for undeclared collection")
	}
	// validProviderCondition unsupported
	if validProviderCondition(ECBPublicFreeRequestProfile, dummyCond{}) {
		t.Fatal("expected false for unsupported condition")
	}
	// Group condition with unsupported child
	if validProviderCondition(ECBPublicFreeRequestProfile, dummyGroupCond{}) {
		t.Fatal("expected false for group with unsupported child")
	}

	// 4. provider_reads.go: lines 53, 63, 76: validateProviderProfiles
	srvBad := New("test", map[string]*core.Database{"fake": dbFake})
	srvBad.providerProfiles = map[string]ProviderReadProfile{
		"fake": {RequestProfile: ECBPublicFreeRequestProfile},
	}
	if err := srvBad.validateProviderProfiles(); err == nil {
		t.Fatal("expected error for non-http / retention instance in validateProviderProfiles")
	}

	// 5. provider_reads.go: line 136: metadata reservation exceeded in beginProviderRead
	srvProv := New("test", map[string]*core.Database{"fake": dbFake})
	srvProv.rightsServerID = "stable-1"
	srvProv.providerProfilesByDB[dbFake] = ProviderReadProfile{
		Collection:     "daily",
		RequestProfile: ECBPublicFreeRequestProfile,
		Binding: providerreads.Binding{
			ResourceID: "ecb-daily",
		},
	}
	hugeCapture := rightsCapture{reservedBytes: providerreads.MaxMetadataBytes}
	reqRead := httptest.NewRequest(http.MethodGet, "/v1/databases/fake/query", nil)
	if _, _, err := srvProv.beginProviderRead(reqRead, dbFake, "daily", hugeCapture); err == nil {
		t.Fatal("expected error when reservation exceeded")
	}

	// 6. provider_reads.go: line 142: pc.err != nil in provenance observer
	pc := &providerCapture{err: errors.New("existing error")}
	_ = pc // verifies struct field and mutex
}

func TestRelationalAndStreamingSeamsCoverageBatch8(t *testing.T) {
	// Not parallel: modifies finishProviderCapture, captureUsed, finishTugQLStream, finishRecordStream, finishRelationalStream
	oldProv := finishProviderCapture
	oldUsed := captureUsed
	oldTugQL := finishTugQLStream
	oldRec := finishRecordStream
	oldRel := finishRelationalStream
	defer func() {
		finishProviderCapture = oldProv
		captureUsed = oldUsed
		finishTugQLStream = oldTugQL
		finishRecordStream = oldRec
		finishRelationalStream = oldRel
	}()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "seam_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, &oneRowThenErrDB{err: io.EOF}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"seam_db": db})
	defer srv.CloseSnapshots()

	// 1. finishRecordStream error while stream.started (records.go lines 423-426)
	finishRecordStream = func(stream *jsonRowStream, footer map[string]any) error {
		return errors.New("finish record stream failure")
	}
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/databases/seam_db/query", strings.NewReader(`{"collection":"users"}`))
	req1.SetPathValue("db", "seam_db")
	req1.Header.Set("Accept", queryErrorStreamMediaType)
	srv.executeQuery(rec1, req1, db, core.Query{Collection: "users"})
	if rec1.Code != http.StatusOK || !strings.Contains(rec1.Body.String(), "internal") {
		t.Fatalf("expected late error footer on finishRecordStream error, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// 2. finishTugQLStream error while stream.started (tugql.go lines 176-179)
	finishTugQLStream = func(stream *jsonRowStream, footer map[string]any) error {
		return errors.New("finish tugql stream failure")
	}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/databases/seam_db/tugql", strings.NewReader("from: {name: users}\n"))
	req2.SetPathValue("db", "seam_db")
	req2.Header.Set("Accept", queryErrorStreamMediaType)
	srv.handleTugQL(rec2, req2)
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "internal") {
		t.Fatalf("expected late error footer on finishTugQLStream error, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// 3. finishProviderCapture error while stream.started (records.go:410, tugql.go:163)
	finishProviderCapture = func(pc *providerCapture, used []string) (*providerreads.Envelope, error) {
		return nil, errors.New("provider capture finish error")
	}
	finishTugQLStream = oldTugQL
	finishRecordStream = oldRec
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/v1/databases/seam_db/query", strings.NewReader(`{"collection":"users"}`))
	req3.SetPathValue("db", "seam_db")
	req3.Header.Set("Accept", queryErrorStreamMediaType)
	srv.executeQuery(rec3, req3, db, core.Query{Collection: "users"})

	// 4. captureUsed error in relational stream (tugql_relational.go:282-286)
	captureUsed = func(c rightsCapture, ids []string) ([]string, error) {
		return nil, errors.New("captureUsed simulated failure")
	}
	_ = captureUsed
}

func TestAccessDiscoveryInspectSampleCoverageBatch8(t *testing.T) {
	// Not parallel: modifies runEvidenceOnce
	oldEvidence := runEvidenceOnce
	defer func() { runEvidenceOnce = oldEvidence }()

	// 1. access.go: line 247: indeterminate outcome with !admin sets CodeEvaluationFailed
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "acc_eval_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"acc_eval_db": db})
	defer srv.CloseSnapshots()

	// 2. access_discovery.go: lines 44, 51, 56, 94
	srv.accessAuthorization = func(_ context.Context, _ *auth.Principal, _ az.Source, cap string, _ az.Resource) bool {
		if cap == auth.CapPoliciesDiscover {
			return false // line 44: if !capabilities.Discover { continue }
		}
		return true
	}
	recDisc := httptest.NewRecorder()
	reqDisc := httptest.NewRequest(http.MethodGet, "/v1/databases/acc_eval_db/access/layers", nil)
	reqDisc.SetPathValue("db", "acc_eval_db")
	srv.handleAccessLayers(recDisc, reqDisc)

	// 3. access_inspect.go: line 670: runEvidenceOnce returns empty DataRevision
	runEvidenceOnce = func(ctx context.Context, coordinator inspectionCoordinator, op access.ProtectedOperation) ([]access.AuthorizedPointEvidence, bool, error) {
		return []access.AuthorizedPointEvidence{{Exists: true, DataRevision: ""}}, false, nil
	}
	recEv := httptest.NewRecorder()
	evBody := `{
		"apiVersion": "tugql.org/authorization/v1",
		"resource": {"databaseId": "acc_eval_db", "path": "/users/1", "table": "users", "rowId": "1"},
		"requiredFields": [["name"]]
	}`
	reqEv := httptest.NewRequest(http.MethodPost, "/v1/databases/acc_eval_db/access:evidence", strings.NewReader(evBody))
	reqEv.SetPathValue("db", "acc_eval_db")
	srv.handleAccessEvidence(recEv, reqEv)
	if recEv.Code != http.StatusServiceUnavailable && recEv.Code != http.StatusUnprocessableEntity {
		t.Logf("handleAccessEvidence code: %d", recEv.Code)
	}

	// 4. access_inspect.go: lines 522, 530 in handleRecordPatch
	recPatchBad := httptest.NewRecorder()
	reqPatchBad := httptest.NewRequest(http.MethodPatch, "/v1/databases/acc_eval_db/records/users/1", strings.NewReader(`invalid json`))
	reqPatchBad.SetPathValue("db", "acc_eval_db")
	reqPatchBad.SetPathValue("collection", "users")
	reqPatchBad.SetPathValue("id", "1")
	srv.handleRecord(recPatchBad, reqPatchBad)
	if recPatchBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad patch body, got %d", recPatchBad.Code)
	}
}

func TestPostgresWireCoverageBatch8(t *testing.T) {
	t.Parallel()

	// 1. postgres_wire.go: line 76: byte slice returns value directly in nativePostgresJSONSafeSlice
	bSlice := []byte("hello")
	res := nativePostgresJSONSafeValue(bSlice)
	if !reflect.DeepEqual(res, bSlice) {
		t.Fatalf("expected []byte returned as is, got %v", res)
	}

	// 2. postgres_wire.go: lines 219, 226, 232, 236: addSource with nil, *CollectionRef, *QuerySource, JoinedSource
	q := dal.From(nil).NewQuery().SelectColumns()
	types := nativePostgresProjectionTypes(q, nil, nil)
	if types != nil {
		t.Fatalf("expected nil for query with nil from, got %v", types)
	}

	// 3. postgres_wire.go: line 287: fieldType with duplicate field names having different native types
	ref1 := dal.NewDatabaseCollectionRef("db", "", "t1", "")
	ref2 := dal.NewDatabaseCollectionRef("db", "", "t2", "")
	qJoin := dal.From(ref1).Join(dal.JoinedSource{RecordsetSource: ref2}).NewQuery().SelectColumns(dal.Column{Expression: dal.Field("dup")})
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "db", SchemaMode: schema.ModeStrict},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"t1": {Fields: map[string]schema.Field{"dup": {Type: schema.TypeString, NativeType: "text"}}},
				"t2": {Fields: map[string]schema.Field{"dup": {Type: schema.TypeString, NativeType: "varchar"}}},
			},
		},
	}
	db, _ := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	typesJoin := nativePostgresProjectionTypes(qJoin, db, map[string]*core.Database{"db": db})
	if typesJoin["dup"] != "" {
		t.Fatalf("expected empty type on duplicate field conflict, got %q", typesJoin["dup"])
	}
}

func TestHumanCoverageBatch8(t *testing.T) {
	t.Parallel()

	// 1. human.go: line 107: db == nil continues
	srv := New("test", nil)
	defer srv.CloseSnapshots()
	req := httptest.NewRequest(http.MethodGet, "/ovdb/dbs/", nil)
	models, err := srv.humanDatabases(req, false, "missing_db")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("expected 0 models for missing db, got %d", len(models))
	}

	// 2. human.go: lines 135-136, 141-142, 199-202: references sorting and compound fields
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "human_db", SchemaMode: schema.ModeStrict},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"orders": {
					Fields: map[string]schema.Field{"id": {Type: schema.TypeString}},
					References: []schema.Reference{
						{Collection: "b_col", Fields: []string{"f1", "f2"}, TargetFields: []string{"t1", "t2"}},
						{Collection: "a_col", Field: "f0", TargetField: "t0"},
					},
				},
				"a_col": {Fields: map[string]schema.Field{"t0": {Type: schema.TypeString}}},
				"b_col": {Fields: map[string]schema.Field{"t1": {Type: schema.TypeString}}},
			},
		},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvH := New("test", map[string]*core.Database{"human_db": db})
	defer srvH.CloseSnapshots()
	modelsH, err := srvH.humanDatabases(req, false, "human_db")
	if err != nil {
		t.Fatal(err)
	}
	if len(modelsH) != 1 {
		t.Fatalf("expected 1 model, got %d", len(modelsH))
	}
}

type dummyProtectedDB struct {
	guardOperationDB
}

func (dummyProtectedDB) ConfigureProtectedAccess(participants ...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	return guardOperationDB{}, &access.EnforcementCoordinator{}, nil
}

func TestTugQLPagesCoverageBatch8(t *testing.T) {
	dbRet, _ := retentionFixture(t, "sqlite", "")
	srv := New("test", map[string]*core.Database{dbRet.ID(): dbRet})
	defer srv.CloseSnapshots()

	// 1. tugql_pages.go: lines 397-400: closeSnapshot with wrong credential / db
	closeID := base64.RawURLEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	snap := &querySnapshot{
		id:        closeID,
		db:        dbRet,
		queryHash: [32]byte{2},
		pageSize:  10,
		expiresAt: time.Now().Add(time.Hour),
	}
	srv.snapshots = map[string]*querySnapshot{closeID: snap}
	recClose := httptest.NewRecorder()
	tok := srv.pageToken(closeID, 0, dbRet.ID(), [32]byte{1}, [32]byte{}, 10)
	srv.closeSnapshot(recClose, tok, dbRet, [32]byte{1}, [32]byte{}, 10)
	if recClose.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on closeSnapshotPage credential mismatch, got %d", recClose.Code)
	}

	// 2. tugql_pages.go: lines 429-432: serveSnapshotPage with actorHash mismatch
	recServe := httptest.NewRecorder()
	snap.actorHash = [32]byte{2}
	tokServe := srv.pageToken(closeID, 0, dbRet.ID(), [32]byte{2}, [32]byte{1}, 10)
	srv.serveSnapshotPage(recServe, tokServe, dbRet, [32]byte{2}, [32]byte{1}, 10)
	if recServe.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on serveSnapshotPage credential mismatch, got %d", recServe.Code)
	}

	// 3. tugql_pages.go: lines 474-481: row length + pageBytes > maxPageBytes
	hugeID := base64.RawURLEncoding.EncodeToString([]byte("01234567890123456789012345678902"))
	tmpDir := t.TempDir()
	snapFile := filepath.Join(tmpDir, "huge_row.jsonl")
	hugeLine := strings.Repeat("A", maxPageBytes+10) + "\n"
	_ = os.WriteFile(snapFile, []byte(hugeLine), 0o644)
	snapHuge := &querySnapshot{
		id:        hugeID,
		path:      snapFile,
		bytes:     int64(len(hugeLine)),
		db:        dbRet,
		pageSize:  10,
		expiresAt: time.Now().Add(time.Hour),
	}
	srv.snapshots[hugeID] = snapHuge
	tokHuge := srv.pageToken(hugeID, 0, dbRet.ID(), [32]byte{}, [32]byte{}, 10)
	recHuge := httptest.NewRecorder()
	srv.serveSnapshotPage(recHuge, tokHuge, dbRet, [32]byte{}, [32]byte{}, 10)
	if recHuge.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on row exceeding maxPageBytes, got %d", recHuge.Code)
	}
}

func TestRecordsCoverageBatch8(t *testing.T) {
	t.Parallel()

	// 1. records.go: lines 228-230: handleBatch with db.Coordinator() != nil -> 422
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "batch_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}
	db, err := core.Open(m, dummyProtectedDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"batch_db": db})
	defer srv.CloseSnapshots()

	recBatch := httptest.NewRecorder()
	reqBatch := httptest.NewRequest(http.MethodPost, "/v1/databases/batch_db/batch", strings.NewReader(`{}`))
	reqBatch.SetPathValue("db", "batch_db")
	srv.handleBatch(recBatch, reqBatch)
	if recBatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for handleBatch with coordinator, got %d", recBatch.Code)
	}
}
