package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"gopkg.in/yaml.v3"
)

type fakeRecordKeysDB struct {
	dal.DB
}

func (fakeRecordKeysDB) SQLiteRecordKeys() (map[string]string, time.Duration) {
	return map[string]string{"orders": "id"}, 0
}

func TestCORSNilWithHeaders(t *testing.T) {
	t.Parallel()
	var c *CORSConfig
	if _, err := c.WithHeaders([]string{"X-Test"}, nil); err == nil {
		t.Fatal("expected error calling WithHeaders on nil CORSConfig")
	}
}

func TestValidateIANAPublisherHTMLNilProfile(t *testing.T) {
	t.Parallel()
	// Dynamic rights requires IANA profile and non-nil PublisherHTMLDefinition
	if err := validateIANAPublisherHTML(ProviderReadProfile{SourceRight: &license.SourceRight{}}, license.Pin{}); err == nil {
		t.Fatal("expected error for empty profile in validateIANAPublisherHTML")
	}
}

func TestHttpErrBranches(t *testing.T) {
	t.Parallel()
	// 1. writeMappedError for ErrNativePostgresCollectionQueryUnsupported
	rec := httptest.NewRecorder()
	if handled := writeMappedError(rec, core.ErrNativePostgresCollectionQueryUnsupported); handled {
		t.Fatal("expected false for mapped non-500 error")
	}
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rec.Code)
	}

	// 2. Server.writeStreamMappedError
	srv := New("test", nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/databases/db1/query", nil)

	// Stream mapped error with 500 error (unhandled error -> logStreamFailure)
	rec500 := httptest.NewRecorder()
	srv.writeStreamMappedError(rec500, req, errors.New("underlying driver died"))
	if rec500.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec500.Code)
	}

	// Stream mapped error with non-500 error (e.g. ErrNotFound -> logUnreachable)
	rec404 := httptest.NewRecorder()
	srv.writeStreamMappedError(rec404, req, core.ErrNotFound)
	if rec404.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec404.Code)
	}
}

func TestReadProfilesMore(t *testing.T) {
	t.Parallel()
	// 1. servingOrderQuery.String()
	q, _, err := core.ParseTugQL([]byte("from: {name: orders}\n"))
	if err != nil {
		t.Fatal(err)
	}
	soq := servingOrderQuery{StructuredQuery: q, key: "orders_id"}
	if !strings.Contains(soq.String(), "ORDER BY serving key \"orders_id\" ASC") {
		t.Fatalf("unexpected String(): %s", soq.String())
	}

	// 2. prepareImmutableQuery when ServingKey is not found
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "db1", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas: &schema.Schemas{
			Collections: map[string]schema.Collection{
				"orders": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}},
			},
		},
	}
	db, err := core.Open(m, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"db1": db}, WithReadOnly(true))
	// Configure read profile on db1
	srv.readProfilesByDB[db] = ReadProfile{AllowOrdinaryQuery: true, PublishedQuery: true}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/databases/db1/tugql", nil)
	// prepareImmutableQuery on a collection without serving key ("orders" has no SQLiteRecordKeys on db)
	if _, ok := srv.prepareImmutableQuery(rec, req, db, q, "orders"); ok {
		t.Fatal("expected prepareImmutableQuery to fail when serving key unavailable")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}

	// 3. validateReadProfiles conflicting remount profile (lines 68 and 75)
	dbKeys1, err := core.Open(m, fakeRecordKeysDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	dbKeys2, err := core.Open(m, fakeRecordKeysDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvConf := New("test", map[string]*core.Database{"alias1": dbKeys1, "alias2": dbKeys2}, WithReadOnly(true))
	p1 := ReadProfile{Kind: BoundedImmutable, AllowOrdinaryQuery: true, PublishedQuery: true}
	p2 := ReadProfile{Kind: BoundedImmutable, AllowOrdinaryQuery: false, PublishedQuery: false}
	srvConf.readProfiles = map[string]ReadProfile{"alias1": p1, "alias2": p2}
	if err := srvConf.validateReadProfiles(); err == nil || !strings.Contains(err.Error(), "conflicting read profiles for remount identity") {
		t.Fatalf("expected conflicting read profiles for remount identity error, got %v", err)
	}

	// Line 75: remount != profile
	mA := &manifest.Manifest{
		Database: manifest.Database{ID: "shared", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"orders": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}}}},
	}
	mB := &manifest.Manifest{
		Database: manifest.Database{ID: "mountA", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"orders": {Fields: map[string]schema.Field{"id": {Type: schema.TypeString}}}}},
	}
	dbA, err := core.Open(mA, fakeRecordKeysDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	dbB, err := core.Open(mB, fakeRecordKeysDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srvConf2 := New("test", map[string]*core.Database{"mountA": dbA, "mountB": dbB}, WithReadOnly(true))
	srvConf2.readProfiles = map[string]ReadProfile{"mountA": p1, "mountB": p2}
	if err := srvConf2.validateReadProfiles(); err == nil || !strings.Contains(err.Error(), "conflicting mount and remount profiles") {
		t.Fatalf("expected conflicting mount and remount profiles error, got %v", err)
	}
}

func TestTokensCreateValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", nil, WithAuth(&auth.Config{
		OwnerToken: "owner-token",
		Store:      store,
	}))

	// 1. empty capabilities
	reqBody := `{"subject":{"realm":"default","kind":"user","id":"u1"},"actor":{"realm":"default","kind":"application","id":"a1"},"capabilities":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/tokens", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer owner-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at least one capability is required") {
		t.Fatalf("expected 400 for empty caps, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. ValidateIdentity failure on Grant
	badReqBody := `{"subject":{"realm":"","kind":"user","id":"u1"},"actor":{"realm":"default","kind":"application","id":"a1"},"capabilities":["databases:create"]}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/tokens", strings.NewReader(badReqBody))
	req2.Header.Set("Authorization", "Bearer owner-token")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest || !strings.Contains(rec2.Body.String(), "invalid subject or actor binding") {
		t.Fatalf("expected 400 for bad identity, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// 3. Store save error (block directory so saveLocked fails)
	storeDirFail := filepath.Join(t.TempDir(), "storedir")
	if err := os.Mkdir(storeDirFail, 0o755); err != nil {
		t.Fatal(err)
	}
	failStore, err := auth.OpenStore(filepath.Join(storeDirFail, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(storeDirFail); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storeDirFail, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	srvFailStore := New("test", nil, WithAuth(&auth.Config{
		OwnerToken: "owner-token",
		Store:      failStore,
	}))
	validBody := `{"subject":{"realm":"default","kind":"user","id":"u1"},"actor":{"realm":"default","kind":"application","id":"a1"},"capabilities":["databases:create"]}`
	req3 := httptest.NewRequest(http.MethodPost, "/v1/tokens", strings.NewReader(validBody))
	req3.Header.Set("Authorization", "Bearer owner-token")
	rec3 := httptest.NewRecorder()
	srvFailStore.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusInternalServerError || !strings.Contains(rec3.Body.String(), "failed to persist grant") {
		t.Fatalf("expected 500 for failed persist, got %d: %s", rec3.Code, rec3.Body.String())
	}
}

// Not parallel: modifies package-level newAuthToken function
func TestTokensCreateEntropyFailure(t *testing.T) {
	oldNewAuthToken := newAuthToken
	newAuthToken = func() (string, error) { return "", errors.New("mock entropy error") }
	defer func() { newAuthToken = oldNewAuthToken }()

	dir := t.TempDir()
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", nil, WithAuth(&auth.Config{
		OwnerToken: "owner-token",
		Store:      store,
	}))
	validBody := `{"subject":{"realm":"default","kind":"user","id":"u1"},"actor":{"realm":"default","kind":"application","id":"a1"},"capabilities":["databases:create"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/tokens", strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer owner-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "failed to generate token") {
		t.Fatalf("expected 500 for generate token failure, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWithGrantIdentityBranches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	validRef := access.PrincipalRef{Realm: "default", Kind: access.PrincipalKindUser, ID: "u1"}
	dummyResolve := func(ctx context.Context, p access.PrincipalRef) (Membership, error) {
		return Membership{Roles: []string{"admin"}}, nil
	}

	// 1. Bootstrap.Validate() error (empty realm)
	s1 := New("test", nil, WithGrantIdentity(GrantIdentityConfig{
		Bootstrap: access.PrincipalRef{},
		Resolve:   dummyResolve,
	}))
	if _, err := s1.principalResolver(ctx, &auth.Principal{Owner: true}); err == nil {
		t.Fatal("expected error for invalid bootstrap")
	}

	// 2. Resolve == nil or authenticated == nil
	s2 := New("test", nil, WithGrantIdentity(GrantIdentityConfig{
		Bootstrap: validRef,
		Resolve:   nil,
	}))
	if _, err := s2.principalResolver(ctx, nil); err == nil {
		t.Fatal("expected error for nil resolve / nil authenticated")
	}

	// 3. Grant.ValidateIdentity() error
	s3 := New("test", nil, WithGrantIdentity(GrantIdentityConfig{
		Bootstrap: validRef,
		Resolve:   dummyResolve,
	}))
	badGrantPrincipal := &auth.Principal{
		Owner: false,
		Grant: &auth.Grant{
			Subject: &access.PrincipalRef{Realm: "", Kind: access.PrincipalKindUser, ID: "u1"},
		},
	}
	if _, err := s3.principalResolver(ctx, badGrantPrincipal); err == nil {
		t.Fatal("expected error for bad grant identity")
	}

	// 4. Legacy grant lacks registered client identity (Subject == nil && PrincipalID == "")
	legacyEmptyPrincipal := &auth.Principal{
		Owner: false,
		Grant: &auth.Grant{
			PrincipalID: "",
		},
	}
	if _, err := s3.principalResolver(ctx, legacyEmptyPrincipal); err == nil || !strings.Contains(err.Error(), "legacy grant lacks registered client identity") {
		t.Fatalf("expected error for legacy empty grant, got %v", err)
	}

	// 5. subject.Validate() error (legacy grant with invalid PrincipalID having leading whitespace)
	legacyBadPrincipal := &auth.Principal{
		Owner: false,
		Grant: &auth.Grant{
			PrincipalID: " leading-space",
		},
	}
	if _, err := s3.principalResolver(ctx, legacyBadPrincipal); err == nil {
		t.Fatal("expected error for invalid subject from legacy grant")
	}
}

func TestRetentionBranches(t *testing.T) {
	t.Parallel()
	// 1. continuationDocument with multi-document YAML
	multiDocYAML := []byte("a: 1\n---\nb: 2\n")
	if _, err := continuationDocument(multiDocYAML, true); err == nil || !strings.Contains(err.Error(), "query must contain one YAML document") {
		t.Fatalf("expected multi-doc error, got %v", err)
	}

	// 2. continuationYAML non-mapping node
	scalarNode := &yaml.Node{Kind: yaml.ScalarNode, Value: "hello"}
	if continuationYAML(scalarNode) {
		t.Fatal("expected false for scalar node in continuationYAML")
	}

	// 3. withoutContinuationFields with invalid JSON (jsonEnvelope == true)
	badJSON := []byte("{not json")
	res := withoutContinuationFields(badJSON, true)
	if !bytes.Equal(res, badJSON) {
		t.Fatalf("expected unchanged data on JSON unmarshal error, got %s", res)
	}

	// 4. guardRetentionRead with q param exceeding maxQueryRequestBytes
	mNoRet := &manifest.Manifest{
		Database: manifest.Database{ID: "noret", SchemaMode: schema.ModeStrict, Retention: manifest.RetentionNone},
		Storage:  manifest.Storage{Engine: "sqlite"},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{}},
	}
	dbNoRet, err := core.Open(mNoRet, guardOperationDB{}, []schema.Mode{schema.ModeStrict}, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New("test", map[string]*core.Database{"noret": dbNoRet})

	bigQ := strings.Repeat("x", maxQueryRequestBytes+10)
	reqQ := httptest.NewRequest(http.MethodGet, "/v1/databases/noret/query?q="+bigQ, nil)
	recQ := httptest.NewRecorder()
	if ok := srv.guardRetentionRead(recQ, reqQ, dbNoRet); ok {
		t.Fatal("expected guardRetentionRead to return false for huge q param")
	}
	if recQ.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recQ.Code)
	}

	// 5. guardRetentionRead with POST body exceeding maxRequestBodyBytes
	bigBody := strings.Repeat("x", maxRequestBodyBytes+10)
	reqPost := httptest.NewRequest(http.MethodPost, "/v1/databases/noret/query", strings.NewReader(bigBody))
	recPost := httptest.NewRecorder()
	if ok := srv.guardRetentionRead(recPost, reqPost, dbNoRet); ok {
		t.Fatal("expected guardRetentionRead to return false for oversized post body")
	}
	if recPost.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", recPost.Code)
	}
}
