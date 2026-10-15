package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"gopkg.in/yaml.v3"
)

// --- 1. querylimits.go noRelease ---

func TestQueryLimitsNoRelease(t *testing.T) {
	t.Parallel()
	noRelease()
}

// --- 2. access_discovery.go ---

type mockAccessPolicy struct{ name string }

func (m mockAccessPolicy) Name() string { return m.name }
func (m mockAccessPolicy) Decide(context.Context, access.Request) access.Decision {
	return access.Decision{Allowed: true}
}
func (m mockAccessPolicy) Authorize(context.Context, access.Request) error { return nil }

func TestAccessDiscoveryBranches(t *testing.T) {
	t.Parallel()

	// 1. handleAccessLayers with db == nil
	srv := New("test", nil)
	recNil := httptest.NewRecorder()
	reqNil := httptest.NewRequest(http.MethodGet, "/v1/databases/missing/access/layers", nil)
	srv.handleAccessLayers(recNil, reqNil)
	if recNil.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing db, got %d", recNil.Code)
	}

	// 2. handleAccessPolicies with db == nil
	recNilPol := httptest.NewRecorder()
	srv.handleAccessPolicies(recNilPol, reqNil)
	if recNilPol.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing db, got %d", recNilPol.Code)
	}

	// Setup db with access policies
	polPub, err := access.WithPolicyMetadata(mockAccessPolicy{name: "p_pub"}, access.PolicyMetadata{ID: "pol1", Revision: "rev1", Visibility: access.PolicyVisibilityPublic})
	if err != nil {
		t.Fatal(err)
	}
	polNoRev, err := access.WithPolicyMetadata(mockAccessPolicy{name: "p_norev"}, access.PolicyMetadata{ID: "pol2", Revision: "", Visibility: access.PolicyVisibilityPublic})
	if err != nil {
		t.Fatal(err)
	}
	polPriv, err := access.WithPolicyMetadata(mockAccessPolicy{name: "p_priv"}, access.PolicyMetadata{ID: "pol3", Revision: "rev2", Visibility: access.PolicyVisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{
		Database: manifest.Database{ID: "disco", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"items": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}},
		}},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "", polPub, polNoRev, polPriv)
	if err != nil {
		t.Fatal(err)
	}

	srvDisco := New("test", map[string]*core.Database{"disco": db})

	// 3. handleAccessLayers: calls db.PolicyLayers()
	reqLayers := httptest.NewRequest(http.MethodGet, "/v1/databases/disco/access/layers", nil)
	reqLayers.SetPathValue("db", "disco")
	recLayers := httptest.NewRecorder()
	srvDisco.handleAccessLayers(recLayers, reqLayers)
	if recLayers.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recLayers.Code)
	}
	var layerBody map[string]any
	_ = json.Unmarshal(recLayers.Body.Bytes(), &layerBody)
	items, _ := layerBody["items"].([]any)
	if len(items) == 0 {
		t.Fatal("expected layers returned")
	}
	firstLayer, _ := items[0].(map[string]any)
	layerID, _ := firstLayer["layerId"].(string)

	// 4. handleAccessPolicies: layer not matched or no query param
	reqPolMiss := httptest.NewRequest(http.MethodGet, "/v1/databases/disco/access/policies?layerId=nonexistent", nil)
	reqPolMiss.SetPathValue("db", "disco")
	recPolMiss := httptest.NewRecorder()
	srvDisco.handleAccessPolicies(recPolMiss, reqPolMiss)
	if recPolMiss.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recPolMiss.Code)
	}

	// 5. handleAccessPolicies: matching layerID
	reqPolHit := httptest.NewRequest(http.MethodGet, "/v1/databases/disco/access/policies?layerId="+layerID, nil)
	reqPolHit.SetPathValue("db", "disco")
	recPolHit := httptest.NewRecorder()
	srvDisco.handleAccessPolicies(recPolHit, reqPolHit)
	if recPolHit.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recPolHit.Code)
	}
}

// --- 3. stream_json.go ---

type shortWriteRecorder struct {
	header http.Header
	code   int
	writes int
}

func (s *shortWriteRecorder) Header() http.Header {
	if s.header == nil {
		s.header = make(http.Header)
	}
	return s.header
}
func (s *shortWriteRecorder) Write(p []byte) (int, error) {
	s.writes++
	if s.writes == 1 {
		return len(p) / 2, nil // short write!
	}
	return 0, errors.New("simulated write error")
}
func (s *shortWriteRecorder) WriteHeader(statusCode int) {
	s.code = statusCode
}

func TestStreamJSONBranches(t *testing.T) {
	t.Parallel()

	// 1. newJSONRowStream with timeout <= 0 and errorCompletion = true
	rec := httptest.NewRecorder()
	st, cancel, err := newJSONRowStream(rec, -1, true)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if st.contentType != queryErrorStreamMediaType {
		t.Fatalf("expected contentType %q, got %q", queryErrorStreamMediaType, st.contentType)
	}

	// 2. WriteRow: write error / short write
	shortW := &shortWriteRecorder{}
	stShort, cancelShort, _ := newJSONRowStream(shortW, 5*time.Second, false)
	defer cancelShort()
	if err := stShort.WriteRow(map[string]any{"a": 1}); err == nil {
		t.Fatal("expected error on short write")
	}

	// 3. WriteRow: marshal error
	stRec, cancelRec, _ := newJSONRowStream(httptest.NewRecorder(), 5*time.Second, false)
	defer cancelRec()
	if err := stRec.WriteRow(func() {}); err == nil {
		t.Fatal("expected error for unmarshalable type")
	}

	// 4. WriteRow: second row (s.started == true)
	rec2 := httptest.NewRecorder()
	st2, cancel2, _ := newJSONRowStream(rec2, 5*time.Second, false)
	defer cancel2()
	if err := st2.WriteRow(map[string]any{"row": 1}); err != nil {
		t.Fatal(err)
	}
	if err := st2.WriteRow(map[string]any{"row": 2}); err != nil {
		t.Fatal(err)
	}

	// 5. Finish: empty result (started = false) with nil footer
	recEmpty := httptest.NewRecorder()
	stEmpty, cancelEmpty, _ := newJSONRowStream(recEmpty, 5*time.Second, false)
	defer cancelEmpty()
	if err := stEmpty.Finish(nil); err != nil {
		t.Fatal(err)
	}

	// 6. Finish: footer too large (> maxStreamFooterBytes)
	recBig := httptest.NewRecorder()
	stBig, cancelBig, _ := newJSONRowStream(recBig, 5*time.Second, false)
	defer cancelBig()
	bigFooter := map[string]any{"large": strings.Repeat("x", maxStreamFooterBytes+10)}
	if err := stBig.Finish(bigFooter); err == nil {
		t.Fatal("expected error for oversize footer")
	}

	// 7. Finish: maxBytes limit exceeded
	recLimit := httptest.NewRecorder()
	stLimit, cancelLimit, _ := newJSONRowStream(recLimit, 5*time.Second, false)
	defer cancelLimit()
	stLimit.maxBytes = 10
	if err := stLimit.Finish(map[string]any{"a": 1}); !errors.Is(err, core.ErrResultTooLarge) {
		t.Fatalf("expected ErrResultTooLarge, got %v", err)
	}

	// 8. FinishFailure: disabled / unstarted / nil / oversize detail / success
	stFail, cancelFail, _ := newJSONRowStream(httptest.NewRecorder(), 5*time.Second, false)
	defer cancelFail()
	if err := stFail.FinishFailure(map[string]any{"err": "x"}); err == nil {
		t.Fatal("expected error when errorCompletion is false")
	}

	recOpt := httptest.NewRecorder()
	stOpt, cancelOpt, _ := newJSONRowStream(recOpt, 5*time.Second, true)
	defer cancelOpt()
	if err := stOpt.FinishFailure(map[string]any{"err": "x"}); err == nil {
		t.Fatal("expected error when stream not started")
	}
	if err := stOpt.WriteRow(map[string]any{"id": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := stOpt.FinishFailure(nil); err == nil {
		t.Fatal("expected error when detail is nil")
	}
	bigDetail := map[string]any{"huge": strings.Repeat("a", maxStreamErrorBytes+10)}
	if err := stOpt.FinishFailure(bigDetail); err == nil {
		t.Fatal("expected error when detail exceeds maxStreamErrorBytes")
	}
	if err := stOpt.FinishFailure(map[string]any{"code": "interrupted"}); err != nil {
		t.Fatalf("expected success on valid FinishFailure, got %v", err)
	}

	// 9. wantsQueryErrorStream: various Accept headers
	reqPlain := httptest.NewRequest(http.MethodGet, "/", nil)
	if wantsQueryErrorStream(reqPlain) {
		t.Fatal("expected false for no Accept header")
	}
	reqBadQ := httptest.NewRequest(http.MethodGet, "/", nil)
	reqBadQ.Header.Set("Accept", queryErrorStreamMediaType+";q=invalid")
	if wantsQueryErrorStream(reqBadQ) {
		t.Fatal("expected false for invalid q")
	}
	reqNaN := httptest.NewRequest(http.MethodGet, "/", nil)
	reqNaN.Header.Set("Accept", queryErrorStreamMediaType+";q=NaN")
	if wantsQueryErrorStream(reqNaN) {
		t.Fatal("expected false for NaN q")
	}
	reqInf := httptest.NewRequest(http.MethodGet, "/", nil)
	reqInf.Header.Set("Accept", queryErrorStreamMediaType+";q=Inf")
	if wantsQueryErrorStream(reqInf) {
		t.Fatal("expected false for Inf q")
	}
	reqNeg := httptest.NewRequest(http.MethodGet, "/", nil)
	reqNeg.Header.Set("Accept", queryErrorStreamMediaType+";q=-0.5")
	if wantsQueryErrorStream(reqNeg) {
		t.Fatal("expected false for negative q")
	}
	reqOver := httptest.NewRequest(http.MethodGet, "/", nil)
	reqOver.Header.Set("Accept", queryErrorStreamMediaType+";q=1.5")
	if wantsQueryErrorStream(reqOver) {
		t.Fatal("expected false for q > 1")
	}
	reqValidQ := httptest.NewRequest(http.MethodGet, "/", nil)
	reqValidQ.Header.Set("Accept", queryErrorStreamMediaType+";q=0.8")
	if !wantsQueryErrorStream(reqValidQ) {
		t.Fatal("expected true for valid q")
	}

	// 10. boundedErrorResponseWriter: WriteHeader duplicate, Write default status, oversize
	bw := &boundedErrorResponseWriter{header: make(http.Header)}
	bw.WriteHeader(http.StatusNotFound)
	bw.WriteHeader(http.StatusInternalServerError)
	if bw.status != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, bw.status)
	}

	bw2 := &boundedErrorResponseWriter{header: make(http.Header)}
	n, err := bw2.Write([]byte("hello"))
	if err != nil || n != 5 || bw2.status != http.StatusOK {
		t.Fatalf("expected status 200 on write, got %d", bw2.status)
	}
	_, err = bw2.Write(bytes.Repeat([]byte("a"), maxStreamErrorBytes+1))
	if err == nil {
		t.Fatal("expected error on oversize error write")
	}

	// 11. finishStreamErrorOrAbort:
	srv := New("test", nil)
	// Subcase A: transport error aborts
	defer func() {
		if r := recover(); r != nil && r != http.ErrAbortHandler {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	stAbort, cAbort, _ := newJSONRowStream(httptest.NewRecorder(), 5*time.Second, true)
	defer cAbort()
	stAbort.transportErr = errors.New("broken pipe")
	srv.finishStreamErrorOrAbort(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), stAbort, func(w http.ResponseWriter) {})
}

func TestFinishStreamErrorOrAbortRecovery(t *testing.T) {
	t.Parallel()
	srv := New("test", nil)

	// Subcase B: error completion finishes cleanly
	rec := httptest.NewRecorder()
	st, cancel, _ := newJSONRowStream(rec, 5*time.Second, true)
	defer cancel()
	_ = st.WriteRow(map[string]any{"id": "first"})
	req := httptest.NewRequest(http.MethodGet, "/query", nil)

	srv.finishStreamErrorOrAbort(rec, req, st, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"bad_request","message":"invalid query"}}`))
	})

	if !strings.Contains(rec.Body.String(), `"code":"bad_request"`) {
		t.Fatalf("expected error detail written, got %s", rec.Body.String())
	}
}

// --- 4. human.go ---

func TestHumanOriginAndRoutes(t *testing.T) {
	t.Parallel()

	// 1. humanOrigin with publicOrigin
	srvCustom := New("test", nil, WithPublicOrigin("https://custom.example.com"))
	req := httptest.NewRequest(http.MethodGet, "http://internal/ovdb/", nil)
	if orig := srvCustom.humanOrigin(req); orig != "https://custom.example.com" {
		t.Fatalf("expected custom origin, got %s", orig)
	}

	// 2. humanOrigin with TLS
	srvDefault := New("test", nil)
	reqTLS := httptest.NewRequest(http.MethodGet, "http://myhost/ovdb/", nil)
	reqTLS.TLS = &tls.ConnectionState{}
	reqTLS.Host = "myhost"
	if orig := srvDefault.humanOrigin(reqTLS); orig != "https://myhost" {
		t.Fatalf("expected https://myhost, got %s", orig)
	}

	// 3. handleHumanServer: path != "/ovdb/" -> 404
	rec404 := httptest.NewRecorder()
	reqWrong := httptest.NewRequest(http.MethodGet, "/ovdb/other", nil)
	srvDefault.handleHumanServer(rec404, reqWrong)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec404.Code)
	}

	// 4. handleHumanDatabases: path != "/ovdb/dbs/" -> 404
	rec404Dbs := httptest.NewRecorder()
	srvDefault.handleHumanDatabases(rec404Dbs, reqWrong)
	if rec404Dbs.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec404Dbs.Code)
	}

	// 5. handleHumanDatabase: database not found -> 404 missing view
	recMissing := httptest.NewRecorder()
	reqMissing := httptest.NewRequest(http.MethodGet, "/ovdb/dbs/unknown", nil)
	reqMissing.SetPathValue("db", "unknown")
	srvDefault.handleHumanDatabase(recMissing, reqMissing)
	if recMissing.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown db, got %d", recMissing.Code)
	}

	// 6. handleHumanDatabase: database with "/" or empty
	recSlash := httptest.NewRecorder()
	reqSlash := httptest.NewRequest(http.MethodGet, "/ovdb/dbs/bad/slash", nil)
	reqSlash.SetPathValue("db", "bad/slash")
	srvDefault.handleHumanDatabase(recSlash, reqSlash)
	if recSlash.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for slashed db, got %d", recSlash.Code)
	}

	// 7. humanDatabases with serverRights error (invalid declaration)
	m := &manifest.Manifest{
		Database: manifest.Database{
			ID: "termsdb", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone,
			License: &license.Declaration{URL: "https://example.com/terms#rights", Text: "Terms text"},
		},
		Storage: manifest.Storage{Engine: "sqlite", Path: ":memory:"},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"col1": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}},
		}},
	}
	dbTerms, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvErrRights := New("test", map[string]*core.Database{"termsdb": dbTerms}, WithSourceRights("server1", nil))
	srvErrRights.serverLicense = &license.Declaration{SPDX: "invalid spdx with spaces"}
	recErrRights := httptest.NewRecorder()
	reqServer := httptest.NewRequest(http.MethodGet, "/ovdb/", nil)
	srvErrRights.handleHumanServer(recErrRights, reqServer)
	if recErrRights.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when rights invalid, got %d", recErrRights.Code)
	}

	// 8. handleHumanStyle
	recStyle := httptest.NewRecorder()
	reqStyle := httptest.NewRequest(http.MethodGet, "/ovdb/style.css", nil)
	handleHumanStyle(recStyle, reqStyle)
	if recStyle.Code != http.StatusOK || recStyle.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("expected 200 text/css for style, got %d", recStyle.Code)
	}

	// 9. handleHumanCollection
	recCol := httptest.NewRecorder()
	reqCol := httptest.NewRequest(http.MethodGet, "/ovdb/dbs/disco/collections/items", nil)
	reqCol.SetPathValue("db", "disco")
	reqCol.SetPathValue("collection", "items")
	srvDefault.handleHumanCollection(recCol, reqCol)
	if recCol.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing db collection in default srv, got %d", recCol.Code)
	}
}

// --- 5. provider_request.go ---

func TestProviderRequestValidationBranches(t *testing.T) {
	t.Parallel()

	// 1. validateProviderRequestProfile branches
	// Storage.HTTP == nil
	mNonHTTP := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite", Path: ":memory:"},
	}
	dbNonHTTP, err := core.Open(mNonHTTP, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProviderRequestProfile(dbNonHTTP, ProviderReadProfile{}); err == nil {
		t.Fatal("expected error for non-HTTP db")
	}

	// HTTP db: ECBPublicFreeRequestProfile with wrong collection
	mHTTP := &manifest.Manifest{
		Database: manifest.Database{ID: "ecb", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "http", HTTP: &manifest.HTTPOptions{Profile: manifest.HTTPProfileECBDaily, Collection: "daily"}},
		Schemas: &schema.Schemas{Collections: map[string]schema.Collection{
			"daily": {Fields: map[string]schema.Field{
				"time":     {Type: schema.TypeString},
				"currency": {Type: schema.TypeString},
				"rate":     {Type: schema.TypeString},
			}},
		}},
	}
	dbHTTP, err := core.Open(mHTTP, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}

	// Mismatched collection
	if err := validateProviderRequestProfile(dbHTTP, ProviderReadProfile{RequestProfile: ECBPublicFreeRequestProfile, Collection: "wrong"}); err == nil {
		t.Fatal("expected error for mismatched collection")
	}

	// Unknown request profile
	if err := validateProviderRequestProfile(dbHTTP, ProviderReadProfile{Collection: "daily", RequestProfile: "unknown/1"}); err == nil {
		t.Fatal("expected error for unknown request profile")
	}

	// 2. guardProviderRequest: refusal for /read, /records/, /batch
	srvRestricted := New("test", map[string]*core.Database{"ecb": dbHTTP}, WithSourceRights("server1", nil))
	srvRestricted.providerProfilesByDB = map[*core.Database]ProviderReadProfile{
		dbHTTP: {RequestProfile: ECBPublicFreeRequestProfile},
	}

	for _, badPath := range []string{"/v1/databases/ecb/read", "/v1/databases/ecb/records/id1", "/v1/databases/ecb/batch"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, badPath, nil)
		if srvRestricted.guardProviderRequest(rec, req, dbHTTP) {
			t.Fatalf("expected guardProviderRequest to refuse %s", badPath)
		}
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 for %s, got %d", badPath, rec.Code)
		}
	}

	// Non-query non-dtql path returns true
	reqMeta := httptest.NewRequest(http.MethodGet, "/v1/databases/ecb/schema", nil)
	if !srvRestricted.guardProviderRequest(httptest.NewRecorder(), reqMeta, dbHTTP) {
		t.Fatal("expected true for schema path")
	}

	// Query with paging headers returns false
	recPaging := httptest.NewRecorder()
	reqPaging := httptest.NewRequest(http.MethodGet, "/v1/databases/ecb/query?q=%7B%7D", nil)
	reqPaging.Header.Set("OVDB-Page-Limit", "10")
	if srvRestricted.guardProviderRequest(recPaging, reqPaging, dbHTTP) {
		t.Fatal("expected guardProviderRequest to refuse paging headers")
	}

	// Query with unknown extra param
	recExtra := httptest.NewRecorder()
	reqExtra := httptest.NewRequest(http.MethodGet, "/v1/databases/ecb/query?q=%7B%7D&extra=1", nil)
	if srvRestricted.guardProviderRequest(recExtra, reqExtra, dbHTTP) {
		t.Fatal("expected guardProviderRequest to refuse extra parameters")
	}

	// DTQL with POST and application/json Content-Type
	recDTQL := httptest.NewRecorder()
	reqDTQL := httptest.NewRequest(http.MethodPost, "/v1/databases/ecb/dtql", strings.NewReader(`{"query":"from daily limit 5"}`))
	reqDTQL.Header.Set("Content-Type", "application/json")
	_ = srvRestricted.guardProviderRequest(recDTQL, reqDTQL, dbHTTP)

	// DTQL with URL parameter
	recDTQLParam := httptest.NewRecorder()
	reqDTQLParam := httptest.NewRequest(http.MethodGet, "/v1/databases/ecb/dtql?q=from+daily+limit+5&parameters=%7B%7D", nil)
	_ = srvRestricted.guardProviderRequest(recDTQLParam, reqDTQLParam, dbHTTP)

	// 3. providerWireNode checks
	if providerWireNode([]byte(`key: *alias`)) != nil {
		t.Fatal("expected nil for alias node")
	}
	if providerWireNode([]byte(`{a: 1, a: 2}`)) != nil {
		t.Fatal("expected nil for duplicate key")
	}
	if providerWireNode([]byte(`{<<: {a: 1}}`)) != nil {
		t.Fatal("expected nil for merge key")
	}
	if providerWireNode([]byte(`1`)) == nil {
		t.Fatal("expected valid scalar document")
	}

	// 4. validProviderJSONQuery
	if validProviderJSONQuery([]byte(`{"collection":"daily","where":"not-a-seq"}`)) {
		t.Fatal("expected false for non-seq where")
	}
	if validProviderJSONQuery([]byte(`{"collection":"daily","where":[{"bad":"key"}]}`)) {
		t.Fatal("expected false for invalid where filter mapping")
	}

	// 5. validProviderDTQLWire
	if validProviderDTQLWire([]byte(`{"from":"not-mapping"}`)) {
		t.Fatal("expected false for non-mapping from")
	}
	if validProviderDTQLWire([]byte(`{"from":{"name":"daily"},"columns":"not-seq"}`)) {
		t.Fatal("expected false for non-seq columns")
	}
	if validProviderDTQLWire([]byte(`{"from":{"name":"daily"},"columns":[{"bad":"key"}]}`)) {
		t.Fatal("expected false for bad column key")
	}
	if validProviderDTQLWire([]byte(`{"from":{"name":"daily"},"limit":100}`)) {
		t.Fatal("expected false for limit > 50")
	}
	if validProviderDTQLWire([]byte(`{"from":{"name":"daily"},"limit":0}`)) {
		t.Fatal("expected false for limit < 1")
	}

	// 6. validProviderConditionWire
	var badAnd yaml.Node
	_ = yaml.Unmarshal([]byte(`{"and": "not-a-seq"}`), &badAnd)
	if validProviderConditionWire(badAnd.Content[0]) {
		t.Fatal("expected false for non-seq and")
	}
	var badLeft yaml.Node
	_ = yaml.Unmarshal([]byte(`{"left": {"bad": "key"}}`), &badLeft)
	if validProviderConditionWire(badLeft.Content[0]) {
		t.Fatal("expected false for bad left")
	}
	var badRight yaml.Node
	_ = yaml.Unmarshal([]byte(`{"right": {"bad": "key"}}`), &badRight)
	if validProviderConditionWire(badRight.Content[0]) {
		t.Fatal("expected false for bad right")
	}

	// 7. providerNativeValue: op == "in"
	if providerNativeValue("in", "not-a-slice") {
		t.Fatal("expected false for non-slice in")
	}
	if providerNativeValue("in", []string{}) {
		t.Fatal("expected false for empty slice in")
	}
	if providerNativeValue("in", []int{1, 2}) {
		t.Fatal("expected false for non-string elements in")
	}
	if !providerNativeValue("in", []string{"USD", "GBP"}) {
		t.Fatal("expected true for valid string slice in")
	}
	// non-in op
	if providerNativeValue("!=", "USD") {
		t.Fatal("expected false for unsupported op !=")
	}
	if providerNativeValue("==", 123) {
		t.Fatal("expected false for non-string value with ==")
	}
	if !providerNativeValue("==", "USD") {
		t.Fatal("expected true for == with string")
	}

	// 8. validProviderQuery
	badQ := core.Query{Collection: "daily", Limit: 5, Where: []core.Filter{{Field: "unknown", Op: "==", Value: "x"}}}
	if validProviderQuery(dbHTTP, badQ, ECBPublicFreeRequestProfile) {
		t.Fatal("expected false for unknown field")
	}

	// 9. validProviderCondition
	if !validProviderCondition(ECBPublicFreeRequestProfile, nil) {
		t.Fatal("expected true for nil condition")
	}
	// Group condition with unsupported operator
	grpCond := dal.NewGroupCondition("NOT_SUPPORTED", dal.Comparison{Operator: dal.Equal, Left: dal.Field("currency"), Right: dal.Constant{Value: "USD"}})
	if validProviderCondition(ECBPublicFreeRequestProfile, grpCond) {
		t.Fatal("expected false for unsupported group operator")
	}
	// Valid group condition
	andCond := dal.NewGroupCondition(dal.And, dal.Comparison{Operator: dal.Equal, Left: dal.Field("currency"), Right: dal.Constant{Value: "USD"}})
	if !validProviderCondition(ECBPublicFreeRequestProfile, andCond) {
		t.Fatal("expected true for valid And condition")
	}
	// Comparison with non-FieldRef Left
	cmpBadLeft := dal.Comparison{Operator: dal.Equal, Left: dal.Constant{Value: "bad"}, Right: dal.Constant{Value: "USD"}}
	if validProviderCondition(ECBPublicFreeRequestProfile, cmpBadLeft) {
		t.Fatal("expected false for non-FieldRef left")
	}
	// Comparison with Array right and in operator
	cmpIn := dal.Comparison{Operator: dal.In, Left: dal.Field("currency"), Right: dal.Array{Value: []string{"USD", "EUR"}}}
	if !validProviderCondition(ECBPublicFreeRequestProfile, cmpIn) {
		t.Fatal("expected true for in array")
	}
}
