package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

const ownerToken = "ovdb_test_owner_token"

// startAuthServer mounts one schemaless ingitdb database "dev" behind an
// auth-enabled server.
func startAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(manifestPath, []byte(`
database:
  id: dev
  schema_mode: schemaless
storage:
  engine: ingitdb
  path: ./data
`), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New("test", map[string]*core.Database{"dev": db},
		server.WithAuth(&auth.Config{OwnerToken: ownerToken, Store: store}))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// request sends a request with an optional bearer token and returns status + body.
func request(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // capture redirects instead of following
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// connectFlow runs consent → code → token and returns the app bearer token.
func connectFlow(t *testing.T, ts *httptest.Server, capabilities string) string {
	t.Helper()
	form := url.Values{
		"client_id":    {"test-app"},
		"redirect_uri": {"http://localhost:1/cb"},
		"db":           {"dev"},
		"capabilities": {capabilities},
		"state":        {"xyz"},
		"decision":     {"approve"},
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Query().Get("state") != "xyz" {
		t.Fatalf("state not round-tripped: %s", loc)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %s", loc)
	}

	tokenForm := url.Values{
		"grant_type": {"authorization_code"},
		"code":       {code},
		"client_id":  {"test-app"},
	}
	tokResp, err := http.Post(ts.URL+"/token", "application/x-www-form-urlencoded",
		strings.NewReader(tokenForm.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tokResp.Body.Close() }()
	if tokResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(tokResp.Body)
		t.Fatalf("token: status %d: %s", tokResp.StatusCode, b)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err = json.NewDecoder(tokResp.Body).Decode(&tok); err != nil {
		t.Fatal(err)
	}
	if tok.TokenType != "Bearer" || tok.AccessToken == "" {
		t.Fatalf("token response: %+v", tok)
	}
	return tok.AccessToken
}

func TestAuth_WellKnownIsPublic(t *testing.T) {
	ts := startAuthServer(t)
	status, body := request(t, ts, http.MethodGet, "/.well-known/openvaultdb", "", "")
	if status != http.StatusOK || !strings.Contains(body, `"authEnabled":true`) {
		t.Fatalf("well-known: %d %s", status, body)
	}
}

func TestAuth_MissingAndInvalidTokens(t *testing.T) {
	ts := startAuthServer(t)
	if status, _ := request(t, ts, http.MethodGet, "/v1/databases/dev/records/notes/n1", "", ""); status != http.StatusUnauthorized {
		t.Fatalf("missing token: %d", status)
	}
	if status, _ := request(t, ts, http.MethodGet, "/v1/databases/dev/records/notes/n1", "ovdb_bogus", ""); status != http.StatusUnauthorized {
		t.Fatalf("invalid token: %d", status)
	}
}

func TestAuth_OwnerFullAccess(t *testing.T) {
	ts := startAuthServer(t)
	if status, _ := request(t, ts, http.MethodPut, "/v1/databases/dev/records/notes/n1", ownerToken,
		`{"data":{"text":"hi"}}`); status != http.StatusNoContent {
		t.Fatalf("owner put: %d", status)
	}
	status, body := request(t, ts, http.MethodGet, "/v1/status", ownerToken, "")
	if status != http.StatusOK || !strings.Contains(body, `"databases"`) {
		t.Fatalf("owner status must include databases: %d %s", status, body)
	}
	if status, _ = request(t, ts, http.MethodGet, "/v1/databases", ownerToken, ""); status != http.StatusOK {
		t.Fatalf("owner databases list: %d", status)
	}
}

func TestAuth_ConnectFlowAndScopedEnforcement(t *testing.T) {
	ts := startAuthServer(t)

	// Seed data as owner: one record in notes, one in secrets.
	for _, kv := range [][2]string{{"notes/n1", `{"data":{"text":"note"}}`}, {"secrets/s1", `{"data":{"hush":true}}`}} {
		if status, body := request(t, ts, http.MethodPut, "/v1/databases/dev/records/"+kv[0], ownerToken, kv[1]); status != http.StatusNoContent {
			t.Fatalf("seed %s: %d %s", kv[0], status, body)
		}
	}

	appToken := connectFlow(t, ts, "records:read:notes,records:write:notes")

	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"read granted collection", http.MethodGet, "/v1/databases/dev/records/notes/n1", "", http.StatusOK},
		{"read other collection denied", http.MethodGet, "/v1/databases/dev/records/secrets/s1", "", http.StatusForbidden},
		{"write granted collection", http.MethodPut, "/v1/databases/dev/records/notes/n2", `{"data":{"text":"two"}}`, http.StatusNoContent},
		{"delete not granted", http.MethodDelete, "/v1/databases/dev/records/notes/n2", "", http.StatusForbidden},
		{"scoped grant cannot use dtql", http.MethodPost, "/v1/databases/dev/dtql", "from:\n  name: notes\n", http.StatusForbidden},
		{"query granted collection", http.MethodPost, "/v1/databases/dev/query", `{"collection":"notes"}`, http.StatusOK},
		{"query other collection denied", http.MethodPost, "/v1/databases/dev/query", `{"collection":"secrets"}`, http.StatusForbidden},
		{"databases list denied", http.MethodGet, "/v1/databases", "", http.StatusForbidden},
		{"collections metadata denied without cap", http.MethodGet, "/v1/databases/dev", "", http.StatusForbidden},
		{"batch write granted", http.MethodPost, "/v1/databases/dev/batch",
			`{"ops":[{"op":"set","key":"notes/n3","data":{"text":"three"}}]}`, http.StatusOK},
		{"batch touching other collection denied", http.MethodPost, "/v1/databases/dev/batch",
			`{"ops":[{"op":"set","key":"notes/n4","data":{"x":1}},{"op":"set","key":"secrets/s2","data":{"x":1}}]}`, http.StatusForbidden},
	} {
		status, body := request(t, ts, tc.method, tc.path, appToken, tc.body)
		if status != tc.want {
			t.Errorf("%s: status %d (want %d): %s", tc.name, status, tc.want, body)
		}
	}

	// The denied batch must not have written its first (allowed) op either:
	// capability checks run before Apply.
	if status, _ := request(t, ts, http.MethodGet, "/v1/databases/dev/records/notes/n4", ownerToken, ""); status != http.StatusNotFound {
		t.Errorf("denied batch leaked a write: notes/n4 status %d", status)
	}

	// Status without databases for the app principal.
	status, body := request(t, ts, http.MethodGet, "/v1/status", appToken, "")
	if status != http.StatusOK || strings.Contains(body, `"databases"`) {
		t.Errorf("app status must redact databases: %d %s", status, body)
	}
}

func TestAuth_UnscopedGrantCanUseDTQL(t *testing.T) {
	ts := startAuthServer(t)
	if status, _ := request(t, ts, http.MethodPut, "/v1/databases/dev/records/notes/n1", ownerToken,
		`{"data":{"text":"x"}}`); status != http.StatusNoContent {
		t.Fatal("seed failed")
	}
	appToken := connectFlow(t, ts, "records:read")
	status, body := request(t, ts, http.MethodPost, "/v1/databases/dev/dtql", appToken, "from:\n  name: notes\n")
	if status != http.StatusOK {
		t.Fatalf("dtql with unscoped read: %d %s", status, body)
	}
}

func TestAuth_DenyDecisionRedirectsWithError(t *testing.T) {
	ts := startAuthServer(t)
	form := url.Values{
		"client_id": {"app"}, "redirect_uri": {"http://localhost:1/cb"},
		"db": {"dev"}, "capabilities": {"records:read"}, "decision": {"deny"},
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Query().Get("error") != "access_denied" {
		t.Fatalf("deny: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestAuth_ConsentPageRendersEscaped(t *testing.T) {
	ts := startAuthServer(t)
	q := url.Values{
		"client_id": {`<script>alert(1)</script>`}, "redirect_uri": {"http://localhost:1/cb"},
		"db": {"dev"}, "capabilities": {"records:read"},
	}
	status, body := request(t, ts, http.MethodGet, "/authorize?"+q.Encode(), "", "")
	if status != http.StatusOK {
		t.Fatalf("consent: %d", status)
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("client_id must be HTML-escaped on the consent page")
	}
	if !strings.Contains(body, "records:read") {
		t.Fatal("consent page must show requested capabilities")
	}
}

func TestAuth_DisabledKeepsOpenBehavior(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(manifestPath, []byte("\ndatabase:\n  id: dev\n  schema_mode: schemaless\nstorage:\n  engine: ingitdb\n  path: ./data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New("test", map[string]*core.Database{"dev": db}).Handler())
	t.Cleanup(ts.Close)

	if status, _ := request(t, ts, http.MethodPut, "/v1/databases/dev/records/notes/n1", "",
		`{"data":{"text":"open"}}`); status != http.StatusNoContent {
		t.Fatalf("authless put: %d", status)
	}
	status, body := request(t, ts, http.MethodGet, "/.well-known/openvaultdb", "", "")
	if status != http.StatusOK || !strings.Contains(body, `"authEnabled":false`) {
		t.Fatalf("well-known without auth: %d %s", status, body)
	}
	// Connect endpoints are not served when auth is off.
	if status, _ = request(t, ts, http.MethodPost, "/token", "", fmt.Sprintf("code=%s", "x")); status != http.StatusNotFound {
		t.Fatalf("token endpoint should 404 when auth disabled: %d", status)
	}
}
