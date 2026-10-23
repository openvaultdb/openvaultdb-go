package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/access"
	az "github.com/dal-go/dalgo/tugql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/joinexec"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/providerreads"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// Not parallel: modifies package-level newDatabaseToken
func TestDatabasesCreateBranches(t *testing.T) {
	dataDir := t.TempDir()
	srv := New("test", nil, WithDataDir(dataDir))

	// 1. Invalid JSON body
	reqBadJSON := httptest.NewRequest(http.MethodPost, "/v1/databases", strings.NewReader("{bad json"))
	recBadJSON := httptest.NewRecorder()
	srv.handleDatabaseCreate(recBadJSON, reqBadJSON)
	if recBadJSON.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad json, got %d", recBadJSON.Code)
	}

	// 2. Provision failure: dataDir is a file, so MkdirAll fails
	blockFile := filepath.Join(t.TempDir(), "blockfile")
	if err := os.WriteFile(blockFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	srvBlocked := New("test", nil, WithDataDir(blockFile))
	reqProvFail := httptest.NewRequest(http.MethodPost, "/v1/databases", strings.NewReader(`{"id":"dbnew"}`))
	recProvFail := httptest.NewRecorder()
	srvBlocked.handleDatabaseCreate(recProvFail, reqProvFail)
	if recProvFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for provision failure, got %d", recProvFail.Code)
	}

	// 3. Mount failure: database already mounted in between
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "mountedalready", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	existingDB, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvDup := New("test", map[string]*core.Database{"mountedalready": existingDB}, WithDataDir(dataDir))
	reqDup := httptest.NewRequest(http.MethodPost, "/v1/databases", strings.NewReader(`{"id":"mountedalready"}`))
	recDup := httptest.NewRecorder()
	srvDup.handleDatabaseCreate(recDup, reqDup)
	if recDup.Code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate db, got %d", recDup.Code)
	}

	// 4. Non-owner creator with mintDatabaseToken error
	storeDir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(filepath.Join(storeDir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Create grant for non-owner app
	appGrant := &auth.Grant{
		PrincipalID:  "app1",
		Label:        "app-label",
		Capabilities: []auth.Capability{{Action: auth.CapDatabasesCreate}},
	}
	if err := store.CreateGrant(appGrant, "app-bearer-token"); err != nil {
		t.Fatal(err)
	}

	srvTokenErr := New("test", nil, WithDataDir(dataDir), WithAuth(&auth.Config{
		OwnerToken: "owner",
		Store:      store,
	}))
	oldNewDBToken := newDatabaseToken
	newDatabaseToken = func() (string, error) { return "", errors.New("token mint failed") }
	defer func() { newDatabaseToken = oldNewDBToken }()

	reqTokenFail := httptest.NewRequest(http.MethodPost, "/v1/databases", strings.NewReader(`{"id":"dbauthtok"}`))
	reqTokenFail.Header.Set("Authorization", "Bearer app-bearer-token")
	recTokenFail := httptest.NewRecorder()
	srvTokenErr.Handler().ServeHTTP(recTokenFail, reqTokenFail)
	if recTokenFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for token mint failure, got %d: %s", recTokenFail.Code, recTokenFail.Body.String())
	}
}

func TestProvisionDatabaseDirectErrors(t *testing.T) {
	t.Parallel()
	// MkdirAll fails
	blockFile := filepath.Join(t.TempDir(), "blockfile")
	if err := os.WriteFile(blockFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provisionDatabase(blockFile, "sub"); err == nil {
		t.Fatal("expected error provisioning database in file path")
	}

	// WriteFile fails (manifest path is blocked)
	dir := t.TempDir()
	manifestBlock := filepath.Join(dir, "blocked.yaml")
	if err := os.Mkdir(manifestBlock, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := provisionDatabase(dir, "blocked"); err == nil {
		t.Fatal("expected error writing manifest")
	}
}

// Not parallel: modifies package-level serverRandRead
func TestServerRandReadFailure(t *testing.T) {
	oldRandRead := serverRandRead
	serverRandRead = func(p []byte) (int, error) { return 0, errors.New("rand failed") }
	defer func() { serverRandRead = oldRandRead }()

	s := New("test", nil)
	if s.snapshotDirErr == nil || !strings.Contains(s.snapshotDirErr.Error(), "query snapshot token key") {
		t.Fatalf("expected snapshotDirErr from rand failure, got %v", s.snapshotDirErr)
	}
}

type dummyPolicy struct{}

func (dummyPolicy) Name() string { return "dummy" }
func (dummyPolicy) Decide(context.Context, access.Request) access.Decision {
	return access.Decision{Allowed: true}
}
func (dummyPolicy) Authorize(context.Context, access.Request) error { return nil }

func TestServerMountAndUnmountMore(t *testing.T) {
	t.Parallel()
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", nil)

	// 1. Mount with provider profile already configured for ID
	srv.providerProfiles = map[string]ProviderReadProfile{"db1": {}}
	if err := srv.Mount(db); err == nil || !strings.Contains(err.Error(), "provider profile requires startup admission") {
		t.Fatalf("expected provider profile error on Mount, got %v", err)
	}
	delete(srv.providerProfiles, "db1")

	// 2. Mount with license declarations when rightsServerID is empty
	mWithLic := &manifest.Manifest{
		Database: manifest.Database{ID: "dblic", SchemaMode: schema.ModeStrict, License: &license.Declaration{SPDX: "Apache-2.0"}},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	dbLic, err := core.Open(mWithLic, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Mount(dbLic); err == nil || !strings.Contains(err.Error(), "database terms require WithSourceRights") {
		t.Fatalf("expected terms error on Mount, got %v", err)
	}

	// 3. UnmountContext with cancelled context
	if err := srv.Mount(db); err != nil {
		t.Fatal(err)
	}
	cancCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.UnmountContext(cancCtx, "db1"); err == nil {
		t.Fatal("expected error unmounting with cancelled context")
	}

	// 4. handleDatabases nil db in list
	srvNilDB := New("test", nil)
	srvNilDB.dbs = map[string]*core.Database{"deleted": nil}
	reqDBs := httptest.NewRequest(http.MethodGet, "/v1/databases", nil)
	recDBs := httptest.NewRecorder()
	srvNilDB.handleDatabases(recDBs, reqDBs)
	if recDBs.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recDBs.Code)
	}

	// 5. handleInferredSchema on strict database (InferredSnapshot == nil)
	srvStrict := New("test", map[string]*core.Database{"db1": db})
	reqInf := httptest.NewRequest(http.MethodGet, "/v1/databases/db1/inferred-schema", nil)
	reqInf.SetPathValue("db", "db1")
	recInf := httptest.NewRecorder()
	srvStrict.handleInferredSchema(recInf, reqInf)
	if recInf.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for strict db inferred-schema, got %d", recInf.Code)
	}

	// 6. handleInferredSchema with access policies
	mAcl := &manifest.Manifest{
		Database: manifest.Database{ID: "dbacl", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		ACL:      &access.FilePolicyConfig{Enabled: true, Realm: "realm1"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	dbAcl, err := core.Open(mAcl, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "", dummyPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srvAcl := New("test", map[string]*core.Database{"dbacl": dbAcl})
	reqInfAcl := httptest.NewRequest(http.MethodGet, "/v1/databases/dbacl/inferred-schema", nil)
	reqInfAcl.SetPathValue("db", "dbacl")
	recInfAcl := httptest.NewRecorder()
	srvAcl.handleInferredSchema(recInfAcl, reqInfAcl)
	if recInfAcl.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for acl db inferred-schema, got %d", recInfAcl.Code)
	}
}

func TestAccessSampleMoreBranches(t *testing.T) {
	t.Parallel()
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"orders": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}, "total": {Type: schema.TypeNumber}}},
			},
		},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"db1": db})

	req := api.Request{
		Mode:       az.ModeSample,
		Operations: []api.Operation{{ID: "s1", Resource: az.Resource{Table: "orders"}}},
		Sample:     &api.Sample{Limit: 5, Query: api.Query{}},
	}

	// 1. db.Coordinator() == nil -> 422
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/databases/db1/access/sample", nil)
	rec := httptest.NewRecorder()
	srv.sampleAccess(rec, httpReq, db, req, nil, access.Principal{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for uncoordinated db sample, got %d", rec.Code)
	}

	// 2. guardOperationFields error (unknown field)
	badFieldReq := api.Request{
		Mode:       az.ModeSample,
		Operations: []api.Operation{{ID: "s1", Resource: az.Resource{Table: "orders", Columns: [][]string{{"bad\x00col"}}}}},
		Sample:     &api.Sample{Limit: 5},
	}
	recField := httptest.NewRecorder()
	srvCoordinator := New("test", map[string]*core.Database{"db1": db})
	errBadField := guardOperationFields(db, badFieldReq.Operations[0])
	srvCoordinator.refuseOperation(recField, httpReq, az.ModeSample, badFieldReq.Operations[0], errBadField)
	if recField.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for refuseOperation, got %d", recField.Code)
	}
}

func TestTugqlRelationalProfileMismatchLogInternalFalse(t *testing.T) {
	t.Parallel()
	srv := New("test", nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/databases/db1/tugql", nil)
	srv.writeRelationalErrorMode(rec, req, joinexec.ErrProfileMismatch, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for profile mismatch, got %d", rec.Code)
	}
}

func TestProviderReadsMoreBranches(t *testing.T) {
	t.Parallel()
	// 1. validateProviderProfiles: collection undeclared
	m := &manifest.Manifest{
		Database: manifest.Database{
			ID: "p1", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone,
			License: &license.Declaration{URL: "https://example.org/synthetic-terms#rights", Text: "Synthetic\nmultiline\tterms"},
		},
		Storage: manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "rates"}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"rates": {Fields: map[string]schema.Field{
				"time":     {Type: schema.TypeString},
				"currency": {Type: schema.TypeString},
				"rate":     {Type: schema.TypeString},
			}},
		}},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvUndeclared := New("test", map[string]*core.Database{"p1": db}, WithSourceRights("server1", nil))
	srvUndeclared.providerProfiles = map[string]ProviderReadProfile{
		"p1": {Collection: "unknown_collection"},
	}
	if err := srvUndeclared.validateProviderProfiles(); err == nil || !strings.Contains(err.Error(), "provider profile collection is undeclared") {
		t.Fatalf("expected undeclared collection error, got %v", err)
	}

	// 2. beginProviderRead: unplanned provider recordset
	right, err := db.SourceRight("server1", nil, "rates")
	if err != nil || right == nil {
		t.Fatal(right, err)
	}
	right.EvidenceOrigin = "publisher-definition-verified"
	right.Pins = []license.Pin{{Role: "provider", Repository: "https://github.com/synthetic/provider", Revision: strings.Repeat("c", 40), Path: "ovdb.yaml", SHA256: strings.Repeat("a", 64), Bytes: 42}}
	right.Attribution = &license.Notice{Text: "Synthetic provider", URL: "https://example.org/"}
	right.FreeSource = &license.Notice{Text: "Synthetic original is free", URL: manifest.ECBDailyURL}
	right.Transformations = []string{"Synthetic XML restructured into rows"}
	rd, err := providerreads.RightsDigest(*right)
	if err != nil {
		t.Fatal(err)
	}
	profile := ProviderReadProfile{
		Collection:  "rates",
		SourceRight: right,
		Binding: providerreads.Binding{
			ProviderSourceID: "provider:synthetic/FxReferenceQuote",
			RightsSourceID:   right.SourceID,
			ResourceID:       "ecb-daily",
			DefinitionDigest: strings.Repeat("a", 64),
			DecoderDigest:    strings.Repeat("b", 64),
			RightsDigest:     rd,
		},
	}
	srvPlan := New("test", nil, WithSourceRights("server1", nil))
	srvPlan.providerProfilesByDB = map[*core.Database]ProviderReadProfile{
		db: profile,
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/databases/p1/query", nil)
	if _, _, err := srvPlan.beginProviderRead(req, db, "mismatched", rightsCapture{}); err == nil || !strings.Contains(err.Error(), "unplanned provider recordset") {
		t.Fatalf("expected unplanned provider recordset error, got %v", err)
	}

	// 3. beginProviderRead: reservedBytes exceeded
	bigCapture := rightsCapture{rights: []license.SourceRight{*right}, reservedBytes: providerreads.MaxMetadataBytes}
	reqWithHdr := httptest.NewRequest(http.MethodGet, "/v1/databases/p1/query", nil)
	reqWithHdr.Header.Set("OVDB-Execution-ID", strings.Repeat("a", 32))
	if _, _, err := srvPlan.beginProviderRead(reqWithHdr, db, "rates", bigCapture); err == nil || !strings.Contains(err.Error(), "provider metadata reservation exceeded") {
		t.Fatalf("expected reservation exceeded error, got %v", err)
	}

	// 4. pc.finish with pc.err != nil
	pcErr := &providerCapture{err: errors.New("previous capture error")}
	if _, err := pcErr.finish(nil); err == nil {
		t.Fatal("expected error in finish when pc.err is set")
	}

	// 5. providerReadError non-execution-id
	recReadErr := httptest.NewRecorder()
	srvPlan.providerReadError(recReadErr, errors.New("some other rights error"))
	if recReadErr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for rightsError, got %d", recReadErr.Code)
	}
}

func TestSourceRightsPanicsAndBranches(t *testing.T) {
	t.Parallel()
	// 1. WithSourceRights panics on empty or too long serverID
	assertPanic := func(fn func()) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic")
			}
		}()
		fn()
	}
	s := &Server{}
	assertPanic(func() { WithSourceRights("", nil)(s) })
	assertPanic(func() { WithSourceRights(strings.Repeat("x", 257), nil)(s) })
	assertPanic(func() { WithSourceRights("bad\x00ctrl", nil)(s) })
	assertPanic(func() { WithSourceRights("server1", &license.Declaration{SPDX: "INVALID!!!"})(s) })

	// 2. captureRights target database not in databases map
	srv := New("test", nil, WithSourceRights("server1", nil))
	targets := []relationalTarget{{database: "missing", collection: "orders"}}
	if _, err := srv.captureRights(map[string]*core.Database{}, targets); err == nil || !strings.Contains(err.Error(), "source rights target is unavailable") {
		t.Fatalf("expected unavailable target error, got %v", err)
	}

	// 3. rightsCapture.used with empty rights
	emptyCapture := rightsCapture{rights: nil}
	if used, err := emptyCapture.used([]string{"s1"}); err != nil || used != nil {
		t.Fatalf("expected nil used and nil err for empty rights, got %v, %v", used, err)
	}

	// 4. writeRightsResult with unmarshalable JSON value
	recMarshalFail := httptest.NewRecorder()
	srv.writeRightsResult(recMarshalFail, nil, map[string]any{"bad": func() {}}, rightsCapture{rights: []license.SourceRight{{}}}, nil, 1000)
	if recMarshalFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on json marshal failure, got %d", recMarshalFail.Code)
	}
	recReqMarshalFail := httptest.NewRecorder()
	reqHdr := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.writeRightsResult(recReqMarshalFail, reqHdr, map[string]any{"bad": func() {}}, rightsCapture{rights: []license.SourceRight{{}}}, nil, 1000)
	if recReqMarshalFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on json marshal failure with request, got %d", recReqMarshalFail.Code)
	}
}

// Not parallel: modifies package-level readConnectRand
func TestConnectMoreBranches(t *testing.T) {
	srv := New("test", nil)

	// 1. validateConnect branches
	// empty client_id
	if _, msg := srv.validateConnect(url.Values{}); msg != "client_id is required" {
		t.Fatalf("expected client_id is required, got %s", msg)
	}
	// empty db
	if _, msg := srv.validateConnect(url.Values{"client_id": {"c1"}}); msg != "db is required" {
		t.Fatalf("expected db is required, got %s", msg)
	}
	// unknown db
	if _, msg := srv.validateConnect(url.Values{"client_id": {"c1"}, "db": {"unk"}}); !strings.Contains(msg, "unknown database") {
		t.Fatalf("expected unknown db, got %s", msg)
	}
	// valid db but bad redirect_uri
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvWithDB := New("test", map[string]*core.Database{"db1": db})
	if _, msg := srvWithDB.validateConnect(url.Values{"client_id": {"c1"}, "db": {"db1"}, "redirect_uri": {"not-a-url"}}); msg != "redirect_uri must be an absolute URL" {
		t.Fatalf("expected absolute url error, got %s", msg)
	}
	// bad capabilities
	if _, msg := srvWithDB.validateConnect(url.Values{"client_id": {"c1"}, "db": {"db1"}, "redirect_uri": {"http://localhost/cb"}, "capabilities": {"unknown:cap"}}); msg == "" {
		t.Fatal("expected error for unknown capability")
	}

	// 2. handleAuthorizeGet with invalid params -> 400
	reqAuthGet := httptest.NewRequest(http.MethodGet, "/authorize?client_id=", nil)
	recAuthGet := httptest.NewRecorder()
	srv.handleAuthorizeGet(recAuthGet, reqAuthGet)
	if recAuthGet.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on handleAuthorizeGet, got %d", recAuthGet.Code)
	}

	// 3. handleAuthorizePost with parse form error
	reqAuthPost := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader("bad%form%data"))
	reqAuthPost.Header.Set("Content-Type", "application/x-www-form-urlencoded; bad")
	recAuthPost := httptest.NewRecorder()
	srv.handleAuthorizePost(recAuthPost, reqAuthPost)
	// Or invalid connect params on valid form
	reqAuthPostValidForm := httptest.NewRequest(http.MethodPost, "/authorize", strings.NewReader("client_id="))
	reqAuthPostValidForm.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recAuthPostValidForm := httptest.NewRecorder()
	srv.handleAuthorizePost(recAuthPostValidForm, reqAuthPostValidForm)
	if recAuthPostValidForm.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid params on authorize post, got %d", recAuthPostValidForm.Code)
	}

	// 4. handleToken: unsupported grant_type
	reqTokGT := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=implicit"))
	reqTokGT.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recTokGT := httptest.NewRecorder()
	srv.handleToken(recTokGT, reqTokGT)
	if recTokGT.Code != http.StatusBadRequest || !strings.Contains(recTokGT.Body.String(), "unsupported grant_type") {
		t.Fatalf("expected 400 for unsupported grant_type, got %d: %s", recTokGT.Code, recTokGT.Body.String())
	}

	// 5. handleToken: ExchangeCode error
	dir := t.TempDir()
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	srvAuth := New("test", nil, WithAuth(&auth.Config{Store: store}))
	reqTokBadCode := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("grant_type=authorization_code&code=invalid_code&client_id=c1"))
	reqTokBadCode.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recTokBadCode := httptest.NewRecorder()
	srvAuth.handleToken(recTokBadCode, reqTokBadCode)
	if recTokBadCode.Code != http.StatusBadRequest || !strings.Contains(recTokBadCode.Body.String(), "invalid_grant") {
		t.Fatalf("expected 400 invalid_grant, got %d: %s", recTokBadCode.Code, recTokBadCode.Body.String())
	}

	// 6. newAuthCode random read error
	oldRand := readConnectRand
	readConnectRand = func(p []byte) (int, error) { return 0, errors.New("entropy error") }
	defer func() { readConnectRand = oldRand }()

	if _, err := newAuthCode(); err == nil {
		t.Fatal("expected error from newAuthCode on entropy failure")
	}
}
