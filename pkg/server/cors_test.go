package server_test

// CORS middleware integration tests.
//
// Each sub-test spins a real httptest.Server via startTestServer variants so
// the full middleware stack is exercised exactly as it runs in production.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/mount"
	"github.com/openvaultdb/openvaultdb-go/pkg/server"
)

// startCORSServer starts a test server with the given CORS origins.
// When withAuth is true a bearer auth layer is added (owner token: "test-owner").
func startCORSServer(t *testing.T, origins []string, withAuth bool) string {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "db.yaml")
	if err := os.WriteFile(manifestPath, []byte(schemalessManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := mount.File(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var opts []server.Option
	if withAuth {
		store, err := auth.OpenStore(filepath.Join(dir, "auth.json"))
		if err != nil {
			t.Fatalf("auth.OpenStore: %v", err)
		}
		opts = append(opts, server.WithAuth(&auth.Config{OwnerToken: "test-owner", Store: store}))
	}
	if len(origins) > 0 {
		corsCfg := server.ParseCORSOrigins(origins)
		if corsCfg != nil {
			opts = append(opts, server.WithCORS(corsCfg))
		}
	}
	ts := httptest.NewServer(server.New("test", map[string]*core.Database{db.ID(): db}, opts...).Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// preflight issues an OPTIONS preflight request to url with the given origin.
func preflight(t *testing.T, url, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, url, nil)
	if err != nil {
		t.Fatalf("http.NewRequest OPTIONS: %v", err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "Authorization")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS %s: %v", url, err)
	}
	return resp
}

// getWithOrigin issues a GET with an Origin header (simulating a browser cross-origin request).
func getWithOrigin(t *testing.T, url, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("http.NewRequest GET: %v", err)
	}
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// ── ParseCORSOrigins unit tests ─────────────────────────────────────────────

func TestParseCORSOrigins(t *testing.T) {
	t.Run("nil when empty", func(t *testing.T) {
		if cfg := server.ParseCORSOrigins(nil); cfg != nil {
			t.Errorf("expected nil, got %v", cfg)
		}
		if cfg := server.ParseCORSOrigins([]string{}); cfg != nil {
			t.Errorf("expected nil for empty slice, got %v", cfg)
		}
	})

	t.Run("nil when only blanks", func(t *testing.T) {
		if cfg := server.ParseCORSOrigins([]string{"", "  "}); cfg != nil {
			t.Errorf("expected nil for blank origins, got %v", cfg)
		}
	})

	t.Run("exact origin", func(t *testing.T) {
		cfg := server.ParseCORSOrigins([]string{"https://sneat.app"})
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if got := cfg.AllowedOrigin("https://sneat.app"); got != "https://sneat.app" {
			t.Errorf("AllowedOrigin: got %q, want %q", got, "https://sneat.app")
		}
		if got := cfg.AllowedOrigin("https://evil.com"); got != "" {
			t.Errorf("AllowedOrigin evil: got %q, want empty", got)
		}
	})

	t.Run("comma-separated in one flag value", func(t *testing.T) {
		cfg := server.ParseCORSOrigins([]string{"https://sneat.app,http://localhost:4200"})
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if got := cfg.AllowedOrigin("http://localhost:4200"); got != "http://localhost:4200" {
			t.Errorf("AllowedOrigin localhost: got %q, want %q", got, "http://localhost:4200")
		}
	})

	t.Run("repeat flag values", func(t *testing.T) {
		cfg := server.ParseCORSOrigins([]string{"https://sneat.app", "http://localhost:4200"})
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if got := cfg.AllowedOrigin("https://sneat.app"); got != "https://sneat.app" {
			t.Errorf("AllowedOrigin sneat: got %q", got)
		}
		if got := cfg.AllowedOrigin("http://localhost:4200"); got != "http://localhost:4200" {
			t.Errorf("AllowedOrigin localhost: got %q", got)
		}
	})

	t.Run("wildcard *", func(t *testing.T) {
		cfg := server.ParseCORSOrigins([]string{"*"})
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if got := cfg.AllowedOrigin("https://anything.example.com"); got != "*" {
			t.Errorf("AllowedOrigin *: got %q, want %q", got, "*")
		}
	})
}

// ── Preflight (OPTIONS) integration tests ───────────────────────────────────

func TestCORS_Preflight_AllowedOrigin(t *testing.T) {
	base := startCORSServer(t, []string{"http://localhost:4200"}, false)
	resp := preflight(t, base+"/v1/status", "http://localhost:4200")
	defer drainClose(resp)

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:4200" {
		t.Errorf("ACAO: got %q, want %q", got, "http://localhost:4200")
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("Access-Control-Allow-Methods header missing")
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got == "" {
		t.Error("Access-Control-Allow-Headers header missing")
	}
	if got := resp.Header.Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Access-Control-Max-Age: got %q, want %q", got, "600")
	}
}

func TestCORS_Preflight_DisallowedOrigin(t *testing.T) {
	base := startCORSServer(t, []string{"http://localhost:4200"}, false)
	resp := preflight(t, base+"/v1/status", "https://evil.com")
	defer drainClose(resp)

	// Must be 204 (not 403) but must NOT carry ACAO.
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for disallowed preflight, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO present for disallowed origin: %q", got)
	}
}

func TestCORS_Preflight_NoCORSConfig(t *testing.T) {
	// No --cors configured: preflight is handled by the mux normally (method not
	// explicitly registered → the default handler returns 200/404, no CORS headers).
	base := startTestServer(t, schemalessManifest)
	resp := preflight(t, base+"/v1/status", "http://localhost:4200")
	defer drainClose(resp)

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO unexpectedly present when --cors not set: %q", got)
	}
}

// ── Actual GET with CORS headers ─────────────────────────────────────────────

func TestCORS_ActualRequest_AllowedOriginGetsACAO(t *testing.T) {
	base := startCORSServer(t, []string{"https://sneat.app"}, false)
	resp := getWithOrigin(t, base+"/v1/status", "https://sneat.app")
	mustStatus(t, resp, http.StatusOK)
	defer drainClose(resp)

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://sneat.app" {
		t.Errorf("ACAO: got %q, want %q", got, "https://sneat.app")
	}
	if got := resp.Header.Get("Vary"); got == "" {
		t.Error("Vary header missing")
	}
}

func TestCORS_ActualRequest_DisallowedOriginNoACAO(t *testing.T) {
	base := startCORSServer(t, []string{"https://sneat.app"}, false)
	resp := getWithOrigin(t, base+"/v1/status", "https://evil.com")
	mustStatus(t, resp, http.StatusOK) // request still processed normally
	defer drainClose(resp)

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO present for disallowed origin: %q", got)
	}
}

func TestCORS_NoCORSConfig_NoHeaders(t *testing.T) {
	base := startTestServer(t, schemalessManifest)
	resp := getWithOrigin(t, base+"/v1/status", "https://sneat.app")
	mustStatus(t, resp, http.StatusOK)
	defer drainClose(resp)

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO unexpectedly present when --cors not set: %q", got)
	}
}

// ── Wildcard * mode ──────────────────────────────────────────────────────────

func TestCORS_Wildcard(t *testing.T) {
	base := startCORSServer(t, []string{"*"}, false)

	// Preflight: any origin should get ACAO: *
	resp := preflight(t, base+"/v1/status", "https://random.example.com")
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("preflight: expected 204, got %d; body: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO wildcard preflight: got %q, want %q", got, "*")
	}
	drainClose(resp)

	// Actual request: any origin should get ACAO: *
	resp = getWithOrigin(t, base+"/v1/status", "https://another.example.com")
	mustStatus(t, resp, http.StatusOK)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO wildcard actual: got %q, want %q", got, "*")
	}
	drainClose(resp)
}

// ── Auth still enforced on actual requests from allowed origins ──────────────

func TestCORS_AuthStillEnforced(t *testing.T) {
	// Set up a server with both --cors and --auth enabled.
	base := startCORSServer(t, []string{"http://localhost:4200"}, true)

	// Actual GET from allowed origin without bearer token → 401.
	resp := getWithOrigin(t, base+"/v1/status", "http://localhost:4200")
	defer drainClose(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer token, got %d", resp.StatusCode)
	}
	// ACAO should still be echoed so the browser can read the 401 body.
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:4200" {
		t.Errorf("ACAO on 401: got %q, want %q", got, "http://localhost:4200")
	}
}

func TestCORS_Preflight_BypassesAuth(t *testing.T) {
	// Preflight (OPTIONS) must return 204 without Authorization — auth must not
	// intercept it and return 401.
	base := startCORSServer(t, []string{"http://localhost:4200"}, true)

	resp := preflight(t, base+"/v1/status", "http://localhost:4200")
	defer drainClose(resp)

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight with auth enabled: expected 204, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:4200" {
		t.Errorf("ACAO on preflight: got %q, want %q", got, "http://localhost:4200")
	}
}
