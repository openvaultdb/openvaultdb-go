package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/dal-go/record"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// nonFlusherWriter is an http.ResponseWriter that does not implement http.Flusher
type nonFlusherWriter struct {
	header http.Header
	buf    bytes.Buffer
	code   int
}

func (w *nonFlusherWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *nonFlusherWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.buf.Write(p)
}
func (w *nonFlusherWriter) WriteHeader(statusCode int) {
	w.code = statusCode
}

// nByteFailWriter fails writes after N bytes
type nByteFailWriter struct {
	http.ResponseWriter
	limit int
	wrote int
}

func (w *nByteFailWriter) Write(p []byte) (int, error) {
	if w.wrote+len(p) > w.limit {
		return 0, errors.New("write limit reached")
	}
	n, err := w.ResponseWriter.Write(p)
	w.wrote += n
	return n, err
}

func TestStreamJSONCoverageBatch7(t *testing.T) {
	// 1. flushJSONStream with non-flusher returns nil (line 224)
	wNonFlush := &nonFlusherWriter{}
	if err := flushJSONStream(wNonFlush); err != nil {
		t.Fatalf("expected nil for non-flusher, got %v", err)
	}

	// 2. finish with json.Marshal error (line 116)
	wRec := httptest.NewRecorder()
	stream, _, err := newJSONRowStream(wRec, 10*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	badFooter := map[string]any{"unsupported": make(chan int)}
	if err := stream.Finish(badFooter); err == nil {
		t.Fatal("expected error on unmarshalable footer")
	}

	// 3. Row write error on second row comma (line 80)
	wFail2 := &nByteFailWriter{ResponseWriter: httptest.NewRecorder(), limit: 20}
	s2, _, err := newJSONRowStream(wFail2, 10*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	// first row succeeds
	_ = s2.WriteRow(map[string]any{"a": 1})
	// second row fails on comma write
	if err := s2.WriteRow(map[string]any{"b": 2}); err == nil {
		t.Fatal("expected write error on second row")
	}

	// 4. finish write errors on closing brackets
	for limit := 0; limit < 50; limit += 5 {
		wF := &nByteFailWriter{ResponseWriter: httptest.NewRecorder(), limit: limit}
		sF, _, err := newJSONRowStream(wF, 10*time.Second, false)
		if err != nil {
			continue
		}
		_ = sF.Finish(map[string]any{"status": "ok"})
	}
}

func TestServerAndConnectCoverageBatch7(t *testing.T) {
	// Not parallel: modifies package variables readConnectRand and serverRandRead
	oldRand := readConnectRand
	oldServerRand := serverRandRead
	defer func() {
		readConnectRand = oldRand
		serverRandRead = oldServerRand
	}()

	// 1. connect.go: newAuthCode rand error (lines 168-170)
	readConnectRand = func(b []byte) (int, error) {
		return 0, errors.New("entropy failure")
	}
	authStore, err := auth.OpenStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	mDb1 := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}
	db1, _ := core.Open(mDb1, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	srv := New("test", map[string]*core.Database{"db1": db1}, WithAuth(&auth.Config{Store: authStore}))

	recAuthPost := httptest.NewRecorder()
	form := url.Values{
		"client_id":    {"client1"},
		"redirect_uri": {"http://localhost/cb"},
		"capabilities": {"records:read"},
		"decision":     {"approve"},
		"db":           {"db1"},
	}
	reqAuthPost := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	reqAuthPost.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.handleAuthorizePost(recAuthPost, reqAuthPost)
	if recAuthPost.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on auth code error, got %d", recAuthPost.Code)
	}
	readConnectRand = oldRand

	// 2. connect.go: handleToken invalid form body (lines 181-183)
	recTokenBad := httptest.NewRecorder()
	reqTokenBad := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader("bad%form;body"))
	reqTokenBad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.handleToken(recTokenBad, reqTokenBad)
	if recTokenBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad form body, got %d", recTokenBad.Code)
	}

	// 3. server.go: readProfileErr != nil with corsCfg != nil (line 368)
	srvCors := New("test", nil, WithCORS(ParseCORSOrigins([]string{"*"})))
	srvCors.readProfileErr = errors.New("read profile configuration failed")
	h := srvCors.Handler()
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
	recCors := httptest.NewRecorder()
	reqCors := httptest.NewRequest(http.MethodGet, "/ovdb/", nil)
	h.ServeHTTP(recCors, reqCors)
	if recCors.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on readProfileErr, got %d", recCors.Code)
	}

	// 4. server.go: handleInferredSchema unauthorized (line 730)
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "auth_db", SchemaMode: schema.ModeSchemaless},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	dbSchemaless, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeSchemaless}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvAuth := New("test", map[string]*core.Database{"auth_db": dbSchemaless}, WithAuth(&auth.Config{Store: authStore}))
	recInfAuth := httptest.NewRecorder()
	reqInfAuth := httptest.NewRequest(http.MethodGet, "/v1/databases/auth_db/schema", nil)
	reqInfAuth.SetPathValue("db", "auth_db")
	srvAuth.handleInferredSchema(recInfAuth, reqInfAuth)
	if recInfAuth.Code != http.StatusUnauthorized && recInfAuth.Code != http.StatusForbidden {
		t.Fatalf("expected 401 or 403 on unauthorized schema read, got %d", recInfAuth.Code)
	}

	// 5. serverRandRead in providerExecutionID (line 188)
	serverRandRead = func(b []byte) (int, error) {
		return 0, errors.New("provider rand failed")
	}
	reqNonce := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := providerExecutionID(reqNonce.Header); err == nil {
		t.Fatal("expected error on provider rand failure")
	}
	serverRandRead = oldServerRand
}

func TestDatabasesCreateCoverageBatch7(t *testing.T) {
	// Not parallel: modifies mountFile
	oldMountFile := mountFile
	defer func() { mountFile = oldMountFile }()

	dir := t.TempDir()
	dbExisting, err := core.Open(&manifest.Manifest{
		Database: manifest.Database{ID: "existing_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"existing_db": dbExisting}, WithDataDir(dir))

	// 1. database already exists (lines 80-83)
	mountFile = func(path string) (*core.Database, error) {
		return core.Open(&manifest.Manifest{
			Database: manifest.Database{ID: "existing_db", SchemaMode: schema.ModeSchemaless},
			Storage:  manifest.Storage{Engine: "sqlite"},
		}, guardOperationDB{}, []schema.Mode{schema.ModeSchemaless}, "")
	}
	recDup := httptest.NewRecorder()
	reqDup := httptest.NewRequest(http.MethodPost, "/v1/databases", strings.NewReader(`{"id":"existing_db"}`))
	srv.handleDatabaseCreate(recDup, reqDup)
	if recDup.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for existing database, got %d", recDup.Code)
	}

	// 2. mountFile returns error (lines 173-175)
	mountFile = func(path string) (*core.Database, error) {
		return nil, errors.New("mount failed")
	}
	recMountErr := httptest.NewRecorder()
	reqMountErr := httptest.NewRequest(http.MethodPost, "/v1/databases", strings.NewReader(`{"id":"fresh_db"}`))
	srv.handleDatabaseCreate(recMountErr, reqMountErr)
	if recMountErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on mount failure, got %d", recMountErr.Code)
	}
	mountFile = oldMountFile

	// 3. authCfg.Store.CreateGrant returns error (lines 139-140)
	storeDir := filepath.Join(t.TempDir(), "ro_dir")
	_ = os.MkdirAll(storeDir, 0o755)
	storeFile := filepath.Join(storeDir, "auth.json")
	_ = os.WriteFile(storeFile, []byte("[]"), 0o600)
	failStore, err := auth.OpenStore(storeFile)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(storeDir, 0o500)
	defer func() { _ = os.Chmod(storeDir, 0o755) }()
	srvGrantFail := New("test", nil, WithDataDir(t.TempDir()), WithAuth(&auth.Config{Store: failStore}))
	reqGrantErr := httptest.NewRequest(http.MethodPost, "/v1/databases", strings.NewReader(`{"id":"mint_db"}`))
	if _, _, err = srvGrantFail.mintDatabaseToken(reqGrantErr, "mint_db", "test-label"); err == nil {
		t.Fatal("expected error on CreateGrant error")
	}
}

type mockAllowPolicy struct{}

func (mockAllowPolicy) Name() string { return "mock-allow" }
func (mockAllowPolicy) Decide(_ context.Context, r access.Request) access.Decision {
	return access.Decision{Allowed: true}
}
func (mockAllowPolicy) Authorize(context.Context, access.Request) error { return nil }
func (mockAllowPolicy) InspectionPure() bool                            { return true }

func TestSourceRightsAndAccessCoverageBatch7(t *testing.T) {
	// Not parallel: modifies accessAssessPlan and licenseInventory
	oldAssess := accessAssessPlan
	oldInventory := licenseInventory
	defer func() {
		accessAssessPlan = oldAssess
		licenseInventory = oldInventory
	}()

	// 1. source_rights.go: serverRights with invalid serverLicense (line 56)
	srvInvalidLic := New("test", nil)
	srvInvalidLic.rightsServerID = "stable-srv"
	srvInvalidLic.serverLicense = &license.Declaration{SPDX: "???"}
	if rights := srvInvalidLic.serverRights(); rights != nil {
		t.Fatalf("expected nil for invalid server license, got %v", rights)
	}

	// 2. source_rights.go: licenseInventory returns error (line 128)
	licenseInventory = func(rights []license.SourceRight) ([]license.SourceRight, int, error) {
		return nil, 0, errors.New("inventory failed")
	}
	dbRet, _ := retentionFixture(t, "sqlite", "")
	srvInv := New("test", map[string]*core.Database{"ret_db": dbRet})
	srvInv.rightsServerID = "stable-srv"
	if _, err := srvInv.singleRights(dbRet, "customers"); err == nil {
		t.Fatal("expected error on licenseInventory failure")
	}

	// 3. connect.go: databaseRights error during /.well-known/openvaultdb (lines 119-121)
	recWellKnown := httptest.NewRecorder()
	reqWellKnown := httptest.NewRequest(http.MethodGet, "/.well-known/openvaultdb", nil)
	srvInv.handleWellKnown(recWellKnown, reqWellKnown)
	if recWellKnown.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on well-known database rights error, got %d", recWellKnown.Code)
	}

	// 4. server.go: handleDatabases and handleDatabase rightsError (lines 648, 717)
	recDbs := httptest.NewRecorder()
	reqDbs := httptest.NewRequest(http.MethodGet, "/v1/databases", nil)
	srvInv.handleDatabases(recDbs, reqDbs)
	if recDbs.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on handleDatabases rights error, got %d", recDbs.Code)
	}

	recDb := httptest.NewRecorder()
	reqDb := httptest.NewRequest(http.MethodGet, "/v1/databases/ret_db", nil)
	reqDb.SetPathValue("db", "ret_db")
	srvInv.handleDatabase(recDb, reqDb)
	if recDb.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on handleDatabase rights error, got %d", recDb.Code)
	}

	// 5. human.go: handleHumanDatabases and handleHumanDatabase with error (lines 296, 307)
	recHumanDbs := httptest.NewRecorder()
	reqHumanDbs := httptest.NewRequest(http.MethodGet, "/ovdb/dbs/", nil)
	srvInv.handleHumanDatabases(recHumanDbs, reqHumanDbs)
	if recHumanDbs.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on human databases rights error, got %d", recHumanDbs.Code)
	}

	recHumanDb := httptest.NewRecorder()
	reqHumanDb := httptest.NewRequest(http.MethodGet, "/ovdb/dbs/ret_db", nil)
	reqHumanDb.SetPathValue("db", "ret_db")
	srvInv.handleHumanDatabase(recHumanDb, reqHumanDb)
	if recHumanDb.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on human database rights error, got %d", recHumanDb.Code)
	}
	licenseInventory = oldInventory

	// 6. access.go: assessPlan returns incomplete and indeterminate (lines 206, 220, 246)
	accessAssessPlan = func(ctx context.Context, req access.Request, policies []access.Policy) access.Assessment {
		return access.Assessment{
			Outcome:  access.AssessmentIndeterminate,
			Complete: false,
			Policies: []access.PolicyAssessment{
				{
					Policy: access.PolicyMetadata{ID: "pol1", Revision: "r1", Visibility: access.PolicyVisibilityPublic},
					Decision: access.Decision{
						Allowed: false,
						Code:    access.CodeEvaluationFailed, // IsIndeterminate() == true
					},
				},
			},
		}
	}
	mAcc := &manifest.Manifest{
		Database: manifest.Database{ID: "acc_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"items": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}}}},
	}
	dbAcc, err := core.Open(mAcc, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "", mockAllowPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	srvAcc := New("test", map[string]*core.Database{"acc_db": dbAcc})
	srvAcc.explainResolver = func(ctx context.Context, s access.PrincipalRef) (Membership, error) {
		return Membership{}, nil
	}
	recAcc := httptest.NewRecorder()
	evalBody := `{
		"apiVersion": "dtql.org/authorization/v1",
		"mode": "plan",
		"diagnosticLevel": "references",
		"operations": [{
			"id": "op1",
			"action": "get",
			"executionClass": "dtql",
			"resource": {"databaseId": "acc_db", "path": "/items/1"}
		}],
		"subject": {"kind": "user", "id": "u1", "realm": "default"},
		"simulation": {"roles": [], "groups": [], "attributes": {}}
	}`
	reqAcc := httptest.NewRequest(http.MethodPost, "/v1/databases/acc_db/access:evaluate", strings.NewReader(evalBody))
	reqAcc.SetPathValue("db", "acc_db")
	srvAcc.handleAccessEvaluate(recAcc, reqAcc)
	if recAcc.Code != http.StatusOK {
		t.Fatalf("expected 200 on evaluate with indeterminate outcome, got %d: %s", recAcc.Code, recAcc.Body.String())
	}
	accessAssessPlan = oldAssess
}

type oneRowThenErrReader struct {
	done bool
	err  error
}

func (r *oneRowThenErrReader) Next() (record.Record, error) {
	if !r.done {
		r.done = true
		return record.NewRecordWithData(record.NewKeyWithID("users", "1"), map[string]any{"name": "Alice"}), nil
	}
	return nil, r.err
}
func (r *oneRowThenErrReader) Cursor() (string, error) { return "", nil }
func (r *oneRowThenErrReader) Close() error            { return nil }

type oneRowThenErrDB struct {
	dal.DB
	err error
}

func (d *oneRowThenErrDB) ExecuteQueryToRecordsReader(context.Context, dal.Query) (dal.RecordsReader, error) {
	return &oneRowThenErrReader{err: d.err}, nil
}

func TestStreamingStartedErrorBranchesCoverageBatch7(t *testing.T) {
	// Tests dtql.go (lines 128-179) and records.go (lines 384-426)
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "stream_db", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}

	for _, tc := range []struct {
		name       string
		queryErr   error
		terminal   bool
		wantCode   int
		expectCode string
	}{
		{"access denied with terminal", access.ErrAccessDenied, true, http.StatusOK, "access_denied"},
		{"db error with terminal", errors.New("read failed"), true, http.StatusOK, "internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := core.Open(m, &oneRowThenErrDB{err: tc.queryErr}, []schema.Mode{schema.ModeStrict}, "")
			if err != nil {
				t.Fatal(err)
			}
			srv := New("test", map[string]*core.Database{"stream_db": db})

			// 1. handleDTQL
			recDTQL := httptest.NewRecorder()
			reqDTQL := httptest.NewRequest(http.MethodPost, "/v1/databases/stream_db/dtql", strings.NewReader("from: {name: users}\n"))
			reqDTQL.SetPathValue("db", "stream_db")
			if tc.terminal {
				reqDTQL.Header.Set("Accept", queryErrorStreamMediaType)
			}
			srv.handleDTQL(recDTQL, reqDTQL)
			if recDTQL.Code != tc.wantCode {
				t.Fatalf("handleDTQL expected %d, got %d", tc.wantCode, recDTQL.Code)
			}
			if tc.expectCode != "" && !strings.Contains(recDTQL.Body.String(), tc.expectCode) {
				t.Fatalf("expected error code %s in body: %s", tc.expectCode, recDTQL.Body.String())
			}

			// 2. executeQuery (records.go)
			recRec := httptest.NewRecorder()
			reqRec := httptest.NewRequest(http.MethodPost, "/v1/databases/stream_db/query", strings.NewReader(`{"collection":"users"}`))
			reqRec.SetPathValue("db", "stream_db")
			if tc.terminal {
				reqRec.Header.Set("Accept", queryErrorStreamMediaType)
			}
			srv.executeQuery(recRec, reqRec, db, core.Query{Collection: "users"})
			if recRec.Code != tc.wantCode {
				t.Fatalf("executeQuery expected %d, got %d", tc.wantCode, recRec.Code)
			}
		})
	}

	// Non-terminal abort branches (abortJSONStream panics with http.ErrAbortHandler)
	t.Run("abortJSONStream panic", func(t *testing.T) {
		db, err := core.Open(m, &oneRowThenErrDB{err: access.ErrAccessDenied}, []schema.Mode{schema.ModeStrict}, "")
		if err != nil {
			t.Fatal(err)
		}
		srv := New("test", map[string]*core.Database{"stream_db": db})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/stream_db/dtql", strings.NewReader("from: {name: users}\n"))
		req.SetPathValue("db", "stream_db")
		defer func() {
			r := recover()
			if r == nil || !errors.Is(r.(error), http.ErrAbortHandler) {
				t.Fatalf("expected http.ErrAbortHandler panic, got %v", r)
			}
		}()
		srv.handleDTQL(rec, req)
	})
}

func TestDTQLPagesCoverageBatch7(t *testing.T) {
	// Not parallel: modifies readSnapshotRand
	oldRand := readSnapshotRand
	defer func() { readSnapshotRand = oldRand }()

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "pages_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	db, err := core.Open(m, &oneRowThenErrDB{err: io.EOF}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"pages_db": db})
	defer srv.CloseSnapshots()

	// 1. OVDB-Page-Size invalid (0 or 1001) (line 150)
	for _, size := range []string{"0", "1001", "abc"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/databases/pages_db/dtql", strings.NewReader("from: {name: users}\n"))
		req.SetPathValue("db", "pages_db")
		req.Header.Set("OVDB-Page-Size", size)
		srv.handleDTQL(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for bad page size %s, got %d", size, rec.Code)
		}
	}

	// 2. Query with Limit/Offset != 0 in paged DTQL (lines 154-156)
	recLimit := httptest.NewRecorder()
	reqLimit := httptest.NewRequest(http.MethodPost, "/v1/databases/pages_db/dtql", strings.NewReader("from: {name: users}\nlimit: 10\n"))
	reqLimit.SetPathValue("db", "pages_db")
	reqLimit.Header.Set("OVDB-Page-Size", "10")
	srv.handleDTQL(recLimit, reqLimit)
	if recLimit.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for paged DTQL with limit, got %d", recLimit.Code)
	}

	// 3. OVDB-Page-Close invalid value (lines 161-163)
	recCloseBad := httptest.NewRecorder()
	reqCloseBad := httptest.NewRequest(http.MethodPost, "/v1/databases/pages_db/dtql", strings.NewReader("from: {name: users}\n"))
	reqCloseBad.SetPathValue("db", "pages_db")
	reqCloseBad.Header.Set("OVDB-Page-Size", "10")
	reqCloseBad.Header.Set("OVDB-Page-Close", "maybe")
	srv.handleDTQL(recCloseBad, reqCloseBad)
	if recCloseBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for OVDB-Page-Close maybe, got %d", recCloseBad.Code)
	}

	// 4. OVDB-Page-Close true with missing token (lines 167-169)
	recCloseNoTok := httptest.NewRecorder()
	reqCloseNoTok := httptest.NewRequest(http.MethodPost, "/v1/databases/pages_db/dtql", strings.NewReader("from: {name: users}\n"))
	reqCloseNoTok.SetPathValue("db", "pages_db")
	reqCloseNoTok.Header.Set("OVDB-Page-Size", "10")
	reqCloseNoTok.Header.Set("OVDB-Page-Close", "true")
	srv.handleDTQL(recCloseNoTok, reqCloseNoTok)
	if recCloseNoTok.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for OVDB-Page-Close without token, got %d", recCloseNoTok.Code)
	}

	// 5. snapshotDirErr != nil (lines 188-190)
	srvDirErr := New("test", map[string]*core.Database{"pages_db": db})
	defer srvDirErr.CloseSnapshots()
	srvDirErr.snapshotDirErr = errors.New("dir error")
	recDirErr := httptest.NewRecorder()
	reqDirErr := httptest.NewRequest(http.MethodPost, "/v1/databases/pages_db/dtql", strings.NewReader("from: {name: users}\n"))
	reqDirErr.SetPathValue("db", "pages_db")
	reqDirErr.Header.Set("OVDB-Page-Size", "10")
	srvDirErr.handleDTQL(recDirErr, reqDirErr)
	if recDirErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on snapshotDirErr, got %d", recDirErr.Code)
	}

	// 6. newSnapshotIdentity rand error (lines 223-226)
	readSnapshotRand = func(b []byte) (int, error) {
		return 0, errors.New("rand id error")
	}
	recIdErr := httptest.NewRecorder()
	reqIdErr := httptest.NewRequest(http.MethodPost, "/v1/databases/pages_db/dtql", strings.NewReader("from: {name: users}\n"))
	reqIdErr.SetPathValue("db", "pages_db")
	reqIdErr.Header.Set("OVDB-Page-Size", "10")
	srv.handleDTQL(recIdErr, reqIdErr)
	if recIdErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on newSnapshotIdentity error, got %d", recIdErr.Code)
	}
	readSnapshotRand = oldRand

	// 7. serveSnapshotPage with missing file, seek error, read error (lines 446-472)
	tmpDir := t.TempDir()
	snapFile := filepath.Join(tmpDir, "snap.jsonl")
	_ = os.WriteFile(snapFile, []byte("{\"key\":\"users/1\",\"data\":{\"name\":\"Alice\"}}\n"), 0o644)
	snap := &querySnapshot{
		id:        "snap1",
		path:      snapFile,
		bytes:     40,
		db:        db,
		pageSize:  10,
		expiresAt: time.Now().Add(time.Hour),
	}
	srv.snapshots = map[string]*querySnapshot{"snap1": snap}
	tok := srv.pageToken("snap1", 0, db.ID(), [32]byte{}, [32]byte{}, 10)

	// File missing -> 410
	_ = os.Remove(snapFile)
	recMissing := httptest.NewRecorder()
	srv.serveSnapshotPage(recMissing, tok, db, [32]byte{}, [32]byte{}, 10)
	if recMissing.Code != http.StatusGone {
		t.Fatalf("expected 410 for missing snapshot file, got %d", recMissing.Code)
	}
}

type errorPolicyDriver struct {
	guardOperationDB
}

func (errorPolicyDriver) AccessPolicies(context.Context) ([]access.Policy, error) {
	return nil, errors.New("policy store unavailable")
}

func TestAccessDiscoveryAndInspectCoverageBatch7(t *testing.T) {
	// 1. access_discovery.go: layer.Err != nil returns 503 (lines 88-90)
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "disc_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
	}
	dbErr, err := core.Open(m, errorPolicyDriver{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"disc_db": dbErr})
	srv.accessAuthorization = func(_ context.Context, _ *auth.Principal, _ az.Source, _ string, _ az.Resource) bool { return true }
	layerID := sourceLayerID(srv.accessSource(dbErr, "ingitdb"))

	recPoliciesErr := httptest.NewRecorder()
	reqPoliciesErr := httptest.NewRequest(http.MethodGet, "/v1/databases/disc_db/access/policies?layerId="+url.QueryEscape(layerID), nil)
	reqPoliciesErr.SetPathValue("db", "disc_db")
	srv.handleAccessPolicies(recPoliciesErr, reqPoliciesErr)
	if recPoliciesErr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on unavailable policy source, got %d", recPoliciesErr.Code)
	}

	// 2. access_inspect.go: protectedOperation with action == "exists" (lines 35-37)
	opExists := api.Operation{
		ID:             "e1",
		Action:         "exists",
		ExecutionClass: az.ExecutionDTQL,
		Resource: az.Resource{
			DatabaseID: "disc_db",
			Table:      "users",
			RowID:      "1",
		},
	}
	pOp, err := protectedOperation(opExists)
	if err != nil {
		t.Fatalf("expected protectedOperation success for exists, got %v", err)
	}
	if pOp.Action() != access.Exists {
		t.Fatalf("expected access.Exists, got %v", pOp.Action())
	}

	// 3. access_inspect.go: handleAccessEvidence with nil coordinator (lines 620-624)
	recEvNoCoord := httptest.NewRecorder()
	reqEvNoCoord := httptest.NewRequest(http.MethodPost, "/v1/databases/disc_db/access/evidence", strings.NewReader(`{}`))
	reqEvNoCoord.SetPathValue("db", "disc_db")
	srv.handleAccessEvidence(recEvNoCoord, reqEvNoCoord)
	if recEvNoCoord.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for evidence without coordinator, got %d", recEvNoCoord.Code)
	}

	// 4. access_sample.go: sampleAccess without coordinator (lines 18-21)
	recSampleNoCoord := httptest.NewRecorder()
	reqSampleNoCoord := httptest.NewRequest(http.MethodPost, "/v1/databases/disc_db/access:evaluate", strings.NewReader(`{}`))
	reqSampleNoCoord.SetPathValue("db", "disc_db")
	srv.sampleAccess(recSampleNoCoord, reqSampleNoCoord, dbErr, api.Request{Operations: []api.Operation{opExists}}, nil, access.Principal{})
	if recSampleNoCoord.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for sample without coordinator, got %d", recSampleNoCoord.Code)
	}
}

func TestPostgresWireWildcardWithoutSourceCoverageBatch7(t *testing.T) {
	// postgres_wire.go: lines 338-345: Wildcard without Source
	ref := dal.NewDatabaseCollectionRef("pg_db", "", "users", "")
	q := dal.From(ref).NewQuery().SelectColumns(dal.AllColumnsExcept())
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "pg_db", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "postgres"},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"users": {
					Fields: map[string]schema.Field{
						"id":   {Type: schema.TypeString, NativeType: "text"},
						"name": {Type: schema.TypeString, NativeType: "text"},
					},
				},
			},
		},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	types := nativePostgresProjectionTypes(q, db, map[string]*core.Database{"pg_db": db})
	if len(types) != 2 {
		t.Fatalf("expected 2 columns from wildcard without source, got %v", types)
	}
}
