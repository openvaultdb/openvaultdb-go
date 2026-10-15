package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/dalgo2http"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"gopkg.in/yaml.v3"
)

func TestServerDrainConcurrentCoverageBatch11(t *testing.T) {
	// Not parallel: timing sensitive
	oldDrain := unmountDrainSync
	defer func() { unmountDrainSync = oldDrain }()
	unmountDrainSync = func() { time.Sleep(5 * time.Millisecond) }

	// server.go: line 271-272: select chooses <-ctx.Done() and drained is also ready
	for i := 0; i < 30; i++ {
		m := &manifest.Manifest{
			Database: manifest.Database{ID: "drain_b11", SchemaMode: schema.ModeStrict},
			Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		}
		db, _ := core.Open(m, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
		if db == nil {
			continue
		}
		srv := New("test", map[string]*core.Database{"drain_b11": db})
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // context is already cancelled
		_ = srv.UnmountContext(ctx, "drain_b11")
	}
}

func TestAccessPlanAndDiscoveryCoverageBatch11(t *testing.T) {
	// Not parallel: modifies accessAssessPlan
	oldAssess := accessAssessPlan
	defer func() { accessAssessPlan = oldAssess }()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "plan_b11", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, dummyProtectedDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"plan_b11": db})
	defer srv.CloseSnapshots()

	// 1. access.go: line 247-248: outcome == az.OutcomeIndeterminate -> code = az.CodeEvaluationFailed
	srv.accessAuthorization = func(ctx context.Context, _ *auth.Principal, source az.Source, capability string, resource az.Resource) bool {
		return false
	}
	accessAssessPlan = func(ctx context.Context, r access.Request, policies []access.Policy) access.Assessment {
		return access.Assessment{Outcome: access.AssessmentIndeterminate, Complete: true}
	}
	recPlan := httptest.NewRecorder()
	reqPlan := httptest.NewRequest(http.MethodPost, "/v1/databases/plan_b11/access/evaluate", strings.NewReader(`{
		"apiVersion": "dtql.org/authorization/v1",
		"mode": "plan",
		"diagnosticLevel": "ordinary",
		"operations": [{"id": "op1", "action": "get", "executionClass": "dtql", "resource": {"databaseId": "plan_b11", "path": "/users", "table": "users"}}]
	}`))
	reqPlan.SetPathValue("db", "plan_b11")
	srv.handleAccessEvaluate(recPlan, reqPlan)
	if recPlan.Code != http.StatusOK {
		t.Fatalf("expected 200 on plan indeterminate, got %d, body: %s", recPlan.Code, recPlan.Body.String())
	}

	// 2. access_discovery.go: lines 51-52: state = "unavailable" when layer.Err != nil
	mErr := &manifest.Manifest{
		Database: manifest.Database{ID: "err_b11", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}
	dbErr, _ := core.Open(mErr, errPoliciesDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if dbErr != nil {
		srvErr := New("test", map[string]*core.Database{"err_b11": dbErr})
		defer srvErr.CloseSnapshots()
		srvErr.accessAuthorization = func(ctx context.Context, _ *auth.Principal, source az.Source, capability string, resource az.Resource) bool {
			return true
		}
		recDisc := httptest.NewRecorder()
		reqDisc := httptest.NewRequest(http.MethodGet, "/v1/databases/err_b11/access/layers", nil)
		reqDisc.SetPathValue("db", "err_b11")
		srvErr.handleAccessLayers(recDisc, reqDisc)
		if recDisc.Code != http.StatusOK {
			t.Fatalf("expected 200 on access layers, got %d", recDisc.Code)
		}
	}

	// 3. access_discovery.go: line 94: private policy skipped without PolicyAdmin cap
	dbPriv, _ := core.Open(m, dummyProtectedDB{}, []schema.Mode{schema.ModeStrict}, "", mockPrivatePolicy{})
	srvPriv := New("test", map[string]*core.Database{"plan_b11": dbPriv})
	defer srvPriv.CloseSnapshots()
	srvPriv.accessAuthorization = func(ctx context.Context, _ *auth.Principal, source az.Source, capability string, resource az.Resource) bool {
		return capability != auth.CapPoliciesAdmin
	}
	recPol := httptest.NewRecorder()
	layerID := sourceLayerID(srvPriv.accessSource(dbPriv, "openvaultdb"))
	reqPol := httptest.NewRequest(http.MethodGet, "/v1/databases/plan_b11/access/policies?layerId="+layerID, nil)
	reqPol.SetPathValue("db", "plan_b11")
	srvPriv.handleAccessPolicies(recPol, reqPol)
	if recPol.Code != http.StatusOK {
		t.Fatalf("expected 200 on access policies, got %d", recPol.Code)
	}
}

type mockPrivatePolicy struct {
	mockAllowPolicy
}

func (mockPrivatePolicy) Visibility() access.PolicyVisibility { return access.PolicyVisibilityPrivate }
func (mockPrivatePolicy) Revision() string                    { return "rev1" }
func (mockPrivatePolicy) PolicyMetadata() access.PolicyMetadata {
	return access.PolicyMetadata{
		ID:         "priv1",
		Revision:   "rev1",
		Visibility: access.PolicyVisibilityPrivate,
	}
}

func TestDTQLAndPagesCoverageBatch11(t *testing.T) {
	// Not parallel: modifies singleRights, snapshotTimerAfterFunc, finishRelationalStream, createSnapshotTemp, streamDTQLQuery
	oldSingle := singleRights
	oldTimer := snapshotTimerAfterFunc
	oldRelFinish := finishRelationalStream
	oldTemp := createSnapshotTemp
	oldStreamDTQL := streamDTQLQuery
	defer func() {
		singleRights = oldSingle
		snapshotTimerAfterFunc = oldTimer
		finishRelationalStream = oldRelFinish
		createSnapshotTemp = oldTemp
		streamDTQLQuery = oldStreamDTQL
	}()

	pgUsersID, _ := schema.NativePostgresCollectionID("public", "users")
	mPG := &manifest.Manifest{
		Database: manifest.Database{ID: "dtql_pg_b11", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true}},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				pgUsersID: {
					Source: &schema.NativeCollectionSource{Schema: "public", Name: "users"},
					Fields: map[string]schema.Field{"name": {Type: schema.TypeString}},
				},
			},
		},
	}
	dbPG, err := core.Open(mPG, dummyOneRowSampleDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvPG := New("test", map[string]*core.Database{"dtql_pg_b11": dbPG})
	defer srvPG.CloseSnapshots()

	// 1. dtql.go: line 62-64: providerRequestRestricted with invalid query -> refuseProviderRequest
	srvPG.providerProfilesByDB[dbPG] = ProviderReadProfile{
		Collection:     "orders",
		RequestProfile: ECBPublicFreeRequestProfile,
	}
	rec62 := httptest.NewRecorder()
	req62 := httptest.NewRequest(http.MethodPost, "/v1/databases/dtql_pg_b11/dtql", strings.NewReader("from:\n  name: users\nlimit: 10\n"))
	req62.SetPathValue("db", "dtql_pg_b11")
	srvPG.handleDTQL(rec62, req62)
	if rec62.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on refused provider request, got %d", rec62.Code)
	}
	delete(srvPG.providerProfilesByDB, dbPG)

	// 2. dtql.go: lines 115-117: NativePostgresReadOnly row emission
	streamDTQLQuery = func(db *core.Database, ctx context.Context, query dal.StructuredQuery, emit func(core.Record) error) error {
		return emit(core.Record{
			Key:  record.NewKeyWithID(pgUsersID, "1"),
			Data: map[string]any{"name": "Alice"},
		})
	}
	rec115 := httptest.NewRecorder()
	req115 := httptest.NewRequest(http.MethodPost, "/v1/databases/dtql_pg_b11/dtql", strings.NewReader("from:\n  name: "+pgUsersID+"\n"))
	req115.SetPathValue("db", "dtql_pg_b11")
	srvPG.handleDTQL(rec115, req115)
	if rec115.Code != http.StatusOK {
		t.Fatalf("expected 200 on native postgres dtql query, got %d, body: %s", rec115.Code, rec115.Body.String())
	}
	streamDTQLQuery = oldStreamDTQL

	// 3. dtql.go: lines 146-153: rightsErr with policies
	mPol := &manifest.Manifest{
		Database: manifest.Database{ID: "rights_b11", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	dbPol, _ := core.Open(mPol, dummyOneRowSampleDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	srvPol := New("test", map[string]*core.Database{"rights_b11": dbPol})
	defer srvPol.CloseSnapshots()

	singleRights = func(s *Server, db *core.Database, collection string) (rightsCapture, error) {
		return rightsCapture{}, errors.New("forced rights error")
	}

	// stream.started == true (1 row written before rightsErr)
	rec146 := httptest.NewRecorder()
	req146 := httptest.NewRequest(http.MethodPost, "/v1/databases/rights_b11/dtql", strings.NewReader("from:\n  name: users\n"))
	req146.SetPathValue("db", "rights_b11")
	req146.Header.Set("Accept", queryErrorStreamMediaType)
	srvPol.handleDTQL(rec146, req146)

	// stream.started == false (0 rows written before rightsErr)
	dbEmpty, _ := core.Open(mPol, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	srvEmpty := New("test", map[string]*core.Database{"rights_b11": dbEmpty})
	defer srvEmpty.CloseSnapshots()
	rec150 := httptest.NewRecorder()
	req150 := httptest.NewRequest(http.MethodPost, "/v1/databases/rights_b11/dtql", strings.NewReader("from:\n  name: users\n"))
	req150.SetPathValue("db", "rights_b11")
	srvEmpty.handleDTQL(rec150, req150)
	if rec150.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on unstarted rights error, got %d", rec150.Code)
	}
	singleRights = oldSingle

	// 4. dtql.go: lines 156-157: stream.errorCompletion with 0 rows -> Cache-Control: no-store
	rec156 := httptest.NewRecorder()
	req156 := httptest.NewRequest(http.MethodPost, "/v1/databases/rights_b11/dtql", strings.NewReader("from:\n  name: users\n"))
	req156.SetPathValue("db", "rights_b11")
	req156.Header.Set("Accept", queryErrorStreamMediaType)
	srvEmpty.handleDTQL(rec156, req156)
	if rec156.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected Cache-Control: no-store, got %s", rec156.Header().Get("Cache-Control"))
	}

	// 5. dtql_pages.go: line 193-195: core.QueryCollections error
	dbRet, _ := retentionFixture(t, "sqlite", "")
	srvRet := New("test", map[string]*core.Database{dbRet.ID(): dbRet})
	defer srvRet.CloseSnapshots()
	qBad := dal.From(dal.NewQualifiedRootCollectionRef("public", "users", "")).NewQuery().SelectColumns()
	rec193 := httptest.NewRecorder()
	req193 := httptest.NewRequest(http.MethodPost, "/v1/databases/"+dbRet.ID()+"/dtql", nil)
	req193.Header.Set("OVDB-Page-Size", "10")
	srvRet.handlePagedDTQL(rec193, req193, dbRet, qBad, []byte("query-with-schema"))
	if rec193.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad collection ref in pages, got %d", rec193.Code)
	}

	// 6. dtql_pages.go: line 241: snapshotTimerAfterFunc execution
	var timerExecuted atomic.Bool
	snapshotTimerAfterFunc = func(d time.Duration, f func()) *time.Timer {
		timerExecuted.Store(true)
		go f()
		return time.NewTimer(d)
	}
	qGood, _, _ := core.ParseDTQL([]byte("from:\n  name: orders\n"))
	rec241 := httptest.NewRecorder()
	req241 := httptest.NewRequest(http.MethodPost, "/v1/databases/"+dbRet.ID()+"/dtql", nil)
	req241.Header.Set("OVDB-Page-Size", "10")
	srvRet.handlePagedDTQL(rec241, req241, dbRet, qGood, []byte("from:\n  name: orders\n"))
	if !timerExecuted.Load() {
		t.Fatalf("expected snapshotTimerAfterFunc callback to execute, status %d, body: %s", rec241.Code, rec241.Body.String())
	}

	// 7. dtql_pages.go: lines 291-295 & 304: spoolDTQL write & flush failure
	dbLarge, _ := core.Open(mPol, dummyLargeRowSampleDB{}, []schema.Mode{schema.ModeStrict}, "")
	qUsers, _, _ := core.ParseDTQL([]byte("from:\n  name: users\n"))
	createSnapshotTemp = func(dir string) (snapshotTempFile, error) {
		return &mockSnapshotTempFile{writeErr: errors.New("write failed")}, nil
	}
	_, _, errSpool := spoolDTQL(context.Background(), dbLarge, qUsers, os.TempDir(), SnapshotLimits{Rows: 10, Bytes: 1 << 20}, 0)
	if errSpool == nil {
		t.Fatal("expected error from spoolDTQL on write failure (large row)")
	}
	_, _, errSpoolFlush := spoolDTQL(context.Background(), dbPol, qUsers, os.TempDir(), SnapshotLimits{Rows: 10, Bytes: 1 << 20}, 0)
	if errSpoolFlush == nil {
		t.Fatal("expected error from spoolDTQL on flush failure")
	}

	// dtql_pages.go: lines 294-295: WriteByte flush failure with exact 65536 byte row
	dbExact, _ := core.Open(mPol, dummyExactRowSampleDB{}, []schema.Mode{schema.ModeStrict}, "")
	createSnapshotTemp = func(dir string) (snapshotTempFile, error) {
		return &mockSnapshotByteFailFile{}, nil
	}
	_, _, errSpoolByte := spoolDTQL(context.Background(), dbExact, qUsers, os.TempDir(), SnapshotLimits{Rows: 10, Bytes: 1 << 20}, 0)
	if errSpoolByte == nil {
		t.Fatal("expected error from spoolDTQL on WriteByte failure")
	}
	createSnapshotTemp = oldTemp

	// 8. dtql_pages.go: lines 467-472: damaged snapshot (offset < snap.bytes but file truncated)
	fDamaged, err := os.CreateTemp("", "damaged_snap_*.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(fDamaged.Name()) }()
	_, _ = fDamaged.WriteString("short")
	_ = fDamaged.Close()

	damID, err := newSnapshotIdentity()
	if err != nil {
		t.Fatal(err)
	}
	snapDamaged := &querySnapshot{
		id:        damID,
		db:        dbRet,
		path:      fDamaged.Name(),
		bytes:     1000, // file has only 5 bytes!
		pageSize:  10,
		expiresAt: time.Now().Add(time.Hour),
	}
	srvRet.snapshotMu.Lock()
	srvRet.snapshots[damID] = snapDamaged
	srvRet.snapshotMu.Unlock()
	rec467 := httptest.NewRecorder()
	srvRet.serveSnapshotPage(rec467, srvRet.pageToken(damID, 0, dbRet.ID(), [32]byte{}, [32]byte{}, 10), dbRet, [32]byte{}, [32]byte{}, 10)
	if rec467.Code != http.StatusGone {
		t.Fatalf("expected 410 on damaged snapshot, got %d", rec467.Code)
	}

	// 9. dtql_relational.go: lines 298-299: finishRelationalStream error when stream.started == false and rights present
	finishRelationalStream = func(stream *jsonRowStream, footer map[string]any) error {
		return errors.New("relational stream error")
	}
	oldServerCapture := serverCaptureRights
	defer func() { serverCaptureRights = oldServerCapture }()
	serverCaptureRights = func(s *Server, databases map[string]*core.Database, targets []relationalTarget) (rightsCapture, error) {
		return rightsCapture{
			rights:  []license.SourceRight{{SourceID: "srv1/rel_empty_b11/users"}},
			planned: map[string]bool{"srv1/rel_empty_b11/users": true},
		}, nil
	}
	oldCaptureUsed := captureUsed
	defer func() { captureUsed = oldCaptureUsed }()
	captureUsed = func(c rightsCapture, ids []string) ([]string, error) {
		return ids, nil
	}
	mRel := &manifest.Manifest{
		Database: manifest.Database{ID: "rel_empty_b11", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	dbRelEmpty, _ := core.Open(mRel, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	srvRelEmpty := New("test", map[string]*core.Database{"rel_empty_b11": dbRelEmpty})
	defer srvRelEmpty.CloseSnapshots()
	srvRelEmpty.rightsServerID = "srv1"
	srvRelEmpty.joinStreamExecute = func(_ context.Context, _ dal.StructuredQuery, _ joinexec.Profile, _ string, _ joinexec.Registry, _ joinexec.Authorize, _ joinexec.Limits, emit func(record.Record) error, _ ...joinexec.Option) (joinexec.StreamResult, error) {
		return joinexec.StreamResult{Execution: joinexec.Execution{Sources: []joinexec.ExecutionSource{{Database: "rel_empty_b11", Collection: "users"}}}}, nil
	}
	rec298 := httptest.NewRecorder()
	req298 := httptest.NewRequest(http.MethodPost, "/v1/databases/rel_empty_b11/dtql", nil)
	req298.SetPathValue("db", "rel_empty_b11")
	srvRelEmpty.serveRelationalDTQL(rec298, req298, dbRelEmpty, dal.From(dal.NewRootCollectionRef("users", "")).NewQuery().SelectColumns(), core.Profile{Kind: core.ProfileRelational, Sources: []core.ProfileSource{{Collection: "users"}}})
	if rec298.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on relational finish error unstarted, got %d, body: %s", rec298.Code, rec298.Body.String())
	}

	// dtql_exact_integers.go: lines 30-31: yamlMarshalExact error
	oldYAMLMarshal := yamlMarshalExact
	defer func() { yamlMarshalExact = oldYAMLMarshal }()
	yamlMarshalExact = func(any) ([]byte, error) { return nil, errors.New("forced yaml marshal error") }
	rawDoc := []byte("value: 9007199254740993")
	if got := exactIntegerValues(rawDoc); string(got) != string(rawDoc) {
		t.Fatalf("expected rawDoc on yaml marshal error, got %s", got)
	}
	yamlMarshalExact = oldYAMLMarshal
}

func TestHumanAndPostgresWireCoverageBatch11(t *testing.T) {
	// 1. human.go: line 107: db == nil continue
	srvNil := New("test", nil)
	srvNil.mu.Lock()
	srvNil.dbs["deleted"] = nil
	srvNil.mu.Unlock()
	reqH := httptest.NewRequest(http.MethodGet, "/databases", nil)
	models, err := srvNil.humanDatabases(reqH, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("expected 0 models for deleted db, got %d", len(models))
	}

	// 2. human.go: lines 125-128, 157-160, 208-209: NativePostgresReadOnly
	pgUsersID, _ := schema.NativePostgresCollectionID("public", "users")
	mPG := &manifest.Manifest{
		Database: manifest.Database{ID: "human_pg", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres", Postgres: &manifest.PostgresOptions{ReadOnly: true}},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				pgUsersID: {
					Source: &schema.NativeCollectionSource{Schema: "public", Name: "users"},
					Fields: map[string]schema.Field{"id": {Type: schema.TypeInteger}, "name": {Type: schema.TypeString}},
				},
			},
		},
	}
	dbPG, err := core.Open(mPG, multiFKReaderDB{isPG: true}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvPG := New("test", map[string]*core.Database{"human_pg": dbPG})
	defer srvPG.CloseSnapshots()
	modelsPG, errPG := srvPG.humanDatabases(reqH, true, "human_pg")
	if errPG != nil {
		t.Fatal(errPG)
	}
	if len(modelsPG) == 0 {
		t.Fatal("expected models for native postgres")
	}

	// 3. human.go: lines 135-136: boundedImmutable with undeclared ref.Collection
	mBound := &manifest.Manifest{
		Database: manifest.Database{ID: "bound_human", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"users": {
					Fields: map[string]schema.Field{"name": {Type: schema.TypeString}},
					References: []schema.Reference{
						{Field: "name", Collection: "undeclared_collection", TargetField: "id"},
					},
				},
			},
		},
	}
	dbBound, errB := core.Open(mBound, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if errB != nil {
		t.Fatal(errB)
	}
	srvBound := New("test", map[string]*core.Database{"bound_human": dbBound})
	defer srvBound.CloseSnapshots()
	srvBound.readProfilesByDB = map[*core.Database]ReadProfile{dbBound: {}}
	modelsBound, errB := srvBound.humanDatabases(reqH, false, "bound_human")
	if errB != nil {
		t.Fatal(errB)
	}
	if len(modelsBound) == 0 {
		t.Fatal("expected models for bound_human")
	}

	// 4. human.go: lines 162-163: non-postgres DB with fk.ReferencedNamespace != ""
	mSqlite := &manifest.Manifest{
		Database: manifest.Database{ID: "sqlite_fk", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"id": {Type: schema.TypeInteger}}}}},
	}
	dbSqlite, _ := core.Open(mSqlite, multiFKReaderDB{isPG: false}, []schema.Mode{schema.ModeStrict}, "")
	srvSqlite := New("test", map[string]*core.Database{"sqlite_fk": dbSqlite})
	defer srvSqlite.CloseSnapshots()
	modelsSqlite, errS := srvSqlite.humanDatabases(reqH, true, "sqlite_fk")
	if errS != nil {
		t.Fatal(errS)
	}
	if len(modelsSqlite) == 0 {
		t.Fatal("expected models for sqlite_fk")
	}

	// 5. human.go: lines 238-239: databaseRights error on no-schema db
	mNoSchema := &manifest.Manifest{
		Database: manifest.Database{ID: "no_schema", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	dbNoSchema, _ := core.Open(mNoSchema, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	srvNoSchema := New("test", map[string]*core.Database{"no_schema": dbNoSchema})
	defer srvNoSchema.CloseSnapshots()
	srvNoSchema.rightsServerID = "srv1"
	srvNoSchema.serverLicense = &license.Declaration{SPDX: "invalid spdx"}
	_, errRights := srvNoSchema.humanDatabases(reqH, false, "no_schema")
	if errRights == nil {
		t.Fatal("expected error on humanDatabases with invalid license on no_schema db")
	}

	// 6. postgres_wire.go: lines 226, 232, 236, 268, 381
	cRefVal := dal.NewRootCollectionRef(pgUsersID, "")
	cRefPtr := &cRefVal
	typesPtr := nativePostgresCollectionRefTypes(*cRefPtr, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})
	if typesPtr == nil {
		t.Fatal("expected types for cRefPtr")
	}

	// *dal.CollectionRef passed to addSource
	qWithRefPtr := dal.From(cRefPtr).NewQuery().SelectColumns()
	typesQRefPtr := nativePostgresProjectionTypes(qWithRefPtr, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})
	if typesQRefPtr == nil {
		t.Fatal("expected types for qWithRefPtr")
	}

	// *dal.QuerySource passed to addSource
	qSub := dal.NewQuerySource(qWithRefPtr, "sub")
	qWithQSrcPtr := dal.From(&qSub).NewQuery().SelectColumns()
	typesQSrcPtr := nativePostgresProjectionTypes(qWithQSrcPtr, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})
	if typesQSrcPtr == nil {
		t.Fatal("expected types for qWithQSrcPtr")
	}

	// dal.JoinedSource passed to addSource
	fromNested := dal.From(cRefVal).Join(dal.NewJoinedSource(dal.NewRootCollectionRef(pgUsersID, "u2"), dal.JoinInner))
	nestedJoined := dal.NewNestedJoinedSource(fromNested, dal.JoinInner)
	qWithJoined := dal.From(nestedJoined).NewQuery().SelectColumns()
	_ = nativePostgresProjectionTypes(qWithJoined, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})

	// line 268: put with empty name (collection has field with empty name)
	mEmptyField := &manifest.Manifest{
		Database: manifest.Database{ID: "empty_field", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"empty": {Fields: map[string]schema.Field{"": {Type: schema.TypeString}}}}},
	}
	dbEmptyField, _ := core.Open(mEmptyField, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	cRefEmpty := dal.NewRootCollectionRef("empty", "")
	qEmptyField := dal.From(cRefEmpty).NewQuery().SelectColumns()
	_ = nativePostgresProjectionTypes(qEmptyField, dbEmptyField, map[string]*core.Database{"empty_field": dbEmptyField})

	// line 381: schema.NativePostgresCollectionID error
	badSchemaRef := dal.NewQualifiedRootCollectionRef("public", strings.Repeat("a", 64), "")
	typesBadSchema := nativePostgresCollectionRefTypes(badSchemaRef, dbPG, map[string]*core.Database{dbPG.ID(): dbPG})
	if typesBadSchema != nil {
		t.Fatalf("expected nil for bad schema ref, got %v", typesBadSchema)
	}

	// 7. records.go: line 337-339: executeQuery 501 on native postgres
	recPG := httptest.NewRecorder()
	reqPG := httptest.NewRequest(http.MethodGet, "/v1/databases/human_pg/records/users", nil)
	srvPG.executeQuery(recPG, reqPG, dbPG, core.Query{Collection: "users"})
	if recPG.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 on postgres executeQuery, got %d", recPG.Code)
	}

	// 8. records.go: line 134-136: boundedImmutable db with RecordExists error
	dbBoundErr, _ := core.Open(mBound, errRecordExistsDB{}, []schema.Mode{schema.ModeStrict}, "")
	srvBoundErr := New("test", map[string]*core.Database{"bound_human": dbBoundErr})
	defer srvBoundErr.CloseSnapshots()
	srvBoundErr.readProfilesByDB = map[*core.Database]ReadProfile{dbBoundErr: {}}
	rec134 := httptest.NewRecorder()
	req134 := httptest.NewRequest(http.MethodHead, "/v1/databases/bound_human/records/users/1", nil)
	req134.SetPathValue("db", "bound_human")
	req134.SetPathValue("collection", "users")
	req134.SetPathValue("id", "1")
	srvBoundErr.handleRecord(rec134, req134)
	if rec134.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on bounded immutable record exists error, got %d", rec134.Code)
	}
}

func TestProviderReadsAndRequestCoverageBatch11(t *testing.T) {
	// 1. provider_reads.go: lines 63-64: conflicting provider profiles across mount aliases
	mHTTP := &manifest.Manifest{
		Database: manifest.Database{ID: "ecb", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "daily"}},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"daily": {
					Fields: map[string]schema.Field{
						"time":     {Type: schema.TypeString},
						"currency": {Type: schema.TypeString},
						"rate":     {Type: schema.TypeString},
					},
				},
			},
		},
	}
	dbHTTP, err := core.Open(mHTTP, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	rightProv := license.SourceRight{
		SourceID:         "ovdb:stable/ecb/daily",
		Source:           license.Identity{ServerID: "stable", DatabaseID: "ecb", Recordset: "daily"},
		Declaration:      license.Declaration{Name: "Synthetic terms", URL: "https://example.com/terms"},
		DeclaredAt:       license.Identity{ServerID: "stable", DatabaseID: "ecb"},
		DeclarationScope: license.DatabaseScope,
		EvidenceOrigin:   "publisher-definition-verified",
		Pins:             []license.Pin{},
		Transformations:  []string{"Synthetic XML to rows"},
		Attribution:      &license.Notice{Text: "Synthetic provider"},
		FreeSource:       &license.Notice{Text: "Free original", URL: "https://example.com/original.xml"},
	}
	digestProv, err := providerreads.RightsDigest(rightProv)
	if err != nil {
		t.Fatal(err)
	}
	p1 := ProviderReadProfile{
		Collection:     "daily",
		RequestProfile: ECBPublicFreeRequestProfile,
		Binding: providerreads.Binding{
			ResourceID:       "ecb-daily",
			ProviderSourceID: "provider:ecb/FxReferenceQuote",
			RightsSourceID:   rightProv.SourceID,
			DefinitionDigest: strings.Repeat("a", 64),
			DecoderDigest:    strings.Repeat("b", 64),
			RightsDigest:     digestProv,
		},
	}
	p2 := ProviderReadProfile{
		Collection:     "daily",
		RequestProfile: ECBPublicFreeRequestProfile,
		Binding: providerreads.Binding{
			ResourceID:       "ecb-daily",
			ProviderSourceID: "provider:ecb/FxReferenceQuote",
			RightsSourceID:   rightProv.SourceID,
			DefinitionDigest: strings.Repeat("c", 64),
			DecoderDigest:    strings.Repeat("b", 64),
			RightsDigest:     digestProv,
		},
	}

	oldSingleRights := singleRights
	defer func() { singleRights = oldSingleRights }()
	singleRights = func(s *Server, db *core.Database, collection string) (rightsCapture, error) {
		return rightsCapture{rights: []license.SourceRight{rightProv}}, nil
	}

	srvConflict := New("test", map[string]*core.Database{"alias1": dbHTTP, "alias2": dbHTTP})
	srvConflict.rightsServerID = "stable"
	srvConflict.providerProfiles = map[string]ProviderReadProfile{"alias1": p1, "alias2": p2}
	if errInit := srvConflict.validateProviderProfiles(); errInit == nil {
		t.Fatal("expected error on conflicting provider profiles")
	}

	// 2. provider_reads.go: lines 73-77: singleRights error and rights mismatch
	srvConflict.providerProfiles = map[string]ProviderReadProfile{"alias1": p1}
	singleRights = func(s *Server, db *core.Database, collection string) (rightsCapture, error) {
		return rightsCapture{}, errors.New("forced single rights error")
	}
	if errSingle := srvConflict.validateProviderProfiles(); errSingle == nil {
		t.Fatal("expected error on singleRights failure")
	}

	// rights mismatch: capture.rights SourceID != Binding.RightsSourceID
	singleRights = func(s *Server, db *core.Database, collection string) (rightsCapture, error) {
		return rightsCapture{rights: []license.SourceRight{{SourceID: "mismatched-source-id"}}}, nil
	}
	if errMismatch := srvConflict.validateProviderProfiles(); errMismatch == nil {
		t.Fatal("expected error on rights mismatch")
	}
	singleRights = func(s *Server, db *core.Database, collection string) (rightsCapture, error) {
		return rightsCapture{rights: []license.SourceRight{rightProv}}, nil
	}

	// 3. provider_reads.go: lines 94-95: corsCfg.WithHeaders error
	oldCORSWithHeaders := corsWithHeaders
	defer func() { corsWithHeaders = oldCORSWithHeaders }()
	corsWithHeaders = func(c *CORSConfig, allowed, exposed []string) (*CORSConfig, error) {
		return nil, errors.New("forced cors headers error")
	}
	srvConflict.corsCfg = &CORSConfig{}
	srvConflict.providerProfiles = map[string]ProviderReadProfile{"alias1": p1}
	if errCORS := srvConflict.validateProviderProfiles(); errCORS == nil {
		t.Fatal("expected error on corsWithHeaders failure")
	}
	corsWithHeaders = oldCORSWithHeaders

	// 4. provider_reads.go: lines 148-149: pc.err != nil in provenance observer
	oldObs := contextWithProvenanceObserver
	defer func() { contextWithProvenanceObserver = oldObs }()
	contextWithProvenanceObserver = func(ctx context.Context, obs dalgo2http.Observer) context.Context {
		obs(dalgo2http.Provenance{}) // first call sets pc.err
		obs(dalgo2http.Provenance{}) // second call returns early at lines 148-149 because pc.err != nil
		return ctx
	}
	reqProv := httptest.NewRequest(http.MethodGet, "/v1/databases/ecb/records/daily", nil)
	reqProv.Header.Set(ProviderExecutionIDHeader, strings.Repeat("a", 32))
	srvConflict.providerProfilesByDB[dbHTTP] = p1
	captureProv := rightsCapture{rights: []license.SourceRight{rightProv}}
	_, _, _ = srvConflict.beginProviderRead(reqProv, dbHTTP, "daily", captureProv)
	contextWithProvenanceObserver = oldObs

	// 5. provider_request.go: lines 44-45: IANANativeOperatorRequestProfile on wrong db ID
	pIANA := ProviderReadProfile{
		Collection:     "rows",
		RequestProfile: IANANativeOperatorRequestProfile,
		Binding: providerreads.Binding{
			ResourceID:       "iana-http-status-codes",
			ProviderSourceID: "provider:iana/HttpStatusRegistryRow",
		},
	}
	mIANAWrong := &manifest.Manifest{
		Database: manifest.Database{ID: "wrong-id", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileIANAHTTPStatus, Collection: "rows"}},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"rows": {
					Fields: map[string]schema.Field{
						"Value":       {Type: schema.TypeString},
						"Description": {Type: schema.TypeString},
						"Reference":   {Type: schema.TypeString},
					},
				},
			},
		},
	}
	dbIANAWrong, _ := core.Open(mIANAWrong, emptyQueryDB{}, []schema.Mode{schema.ModeStrict}, "")
	if errProfile := validateProviderRequestProfile(dbIANAWrong, pIANA); errProfile == nil {
		t.Fatal("expected error on IANA profile for non-IANA db ID")
	}

	// 6. provider_request.go: lines 223-224: and/or with invalid child in validProviderConditionWire
	var badNode yaml.Node
	_ = yaml.Unmarshal([]byte("and:\n- invalid: [1, 2]\n"), &badNode)
	if len(badNode.Content) > 0 && validProviderConditionWire(badNode.Content[0]) {
		t.Fatal("expected false for invalid child condition wire")
	}

	// 7. provider_request.go: lines 300-301: column with alias in validProviderDTQL
	cRef := dal.NewRootCollectionRef("daily", "")
	qColAlias := dal.From(cRef).NewQuery().Limit(10).SelectColumns(dal.Column{Expression: dal.Field("rate"), Alias: "aliased_col"})
	if validProviderDTQL(dbHTTP, qColAlias, core.Profile{}, ECBPublicFreeRequestProfile) {
		t.Fatal("expected false for column with alias in validProviderDTQL")
	}
}

type dummyLargeRowSampleDB struct {
	guardOperationDB
}

func (d dummyLargeRowSampleDB) ConfigureProtectedAccess(...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	return d, &access.EnforcementCoordinator{}, nil
}

func (dummyLargeRowSampleDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return &oneRecordReaderB10{key: record.NewKeyWithID("users", "1"), data: map[string]any{"name": strings.Repeat("x", 70000)}}, nil
}

type dummyExactRowSampleDB struct {
	guardOperationDB
}

func (d dummyExactRowSampleDB) ConfigureProtectedAccess(...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	return d, &access.EnforcementCoordinator{}, nil
}

func (dummyExactRowSampleDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return &oneRecordReaderB10{key: record.NewKeyWithID("users", "1"), data: map[string]any{"name": strings.Repeat("x", 65500)}}, nil
}

type mockSnapshotByteFailFile struct{}

func (m *mockSnapshotByteFailFile) Write(p []byte) (int, error) {
	return 0, errors.New("byte flush failed")
}
func (m *mockSnapshotByteFailFile) Close() error { return nil }
func (m *mockSnapshotByteFailFile) Name() string { return "mock-byte-fail" }

type multiFKReaderDB struct {
	guardOperationDB
	isPG bool
}

func (multiFKReaderDB) ListCollections(ctx context.Context, parent *record.Key) ([]dal.CollectionRef, error) {
	return nil, nil
}

func (m multiFKReaderDB) DescribeCollection(ctx context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	ns := "public"
	target := "users"
	if !m.isPG {
		ns = "other_schema"
		target = "roles"
	}
	fks := []dbschema.ForeignKeyDef{
		{
			Name:                 "fk_multi",
			ReferencedNamespace:  ns,
			ReferencedCollection: target,
			Fields:               []dal.FieldName{"user_id", "dept_id"},
			ReferencedFields:     []dal.FieldName{"id", "dept_id"},
			Enforcement:          "Enforced",
		},
	}
	if m.isPG {
		fks = append(fks, dbschema.ForeignKeyDef{
			Name:                 "fk_same_schema",
			ReferencedNamespace:  "",
			ReferencedCollection: target,
			Fields:               []dal.FieldName{"user_id"},
			ReferencedFields:     []dal.FieldName{"id"},
			Enforcement:          "Enforced",
		})
	}
	return &dbschema.CollectionDef{
		Name:        ref.Name(),
		ForeignKeys: fks,
	}, nil
}

func (multiFKReaderDB) ListIndexes(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	return nil, nil
}

func (multiFKReaderDB) ListConstraints(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	return nil, nil
}

func (multiFKReaderDB) ListReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	return nil, nil
}
