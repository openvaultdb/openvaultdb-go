package server

import (
	"net/http"
	"strings"
)

// CORSConfig holds the allowed-origin set for the CORS middleware.
// A nil *CORSConfig means CORS is disabled — no headers are added.
type CORSConfig struct {
	allowAll bool     // true when the configured origin is "*"
	exact    []string // exact origins, e.g. "https://sneat.app"
}

// ParseCORSOrigins builds a CORSConfig from a list of origin strings.
// Each element may be:
//   - "*"                       — allow any origin (dev only)
//   - "https://example.com"     — exact origin match
//   - "http://localhost:4200"   — exact (scheme + host + port)
//
// Comma-separated values within a single element are split automatically so
// both --cors "a,b" and --cors a --cors b are equivalent.
func ParseCORSOrigins(values []string) *CORSConfig {
	if len(values) == 0 {
		return nil
	}
	cfg := &CORSConfig{}
	for _, v := range values {
		for _, raw := range strings.Split(v, ",") {
			o := strings.TrimSpace(raw)
			if o == "" {
				continue
			}
			if o == "*" {
				cfg.allowAll = true
				return cfg // * overrides everything
			}
			cfg.exact = append(cfg.exact, o)
		}
	}
	if len(cfg.exact) == 0 {
		return nil
	}
	return cfg
}

// AllowedOrigin returns the value to echo back in Access-Control-Allow-Origin,
// or "" when the origin is not allowed.  Exported for testing.
func (c *CORSConfig) AllowedOrigin(origin string) string {
	if c == nil || origin == "" {
		return ""
	}
	if c.allowAll {
		return "*"
	}
	for _, o := range c.exact {
		if o == origin {
			return origin
		}
	}
	return ""
}

// allowedOrigin is the internal alias for use inside the middleware.
func (c *CORSConfig) allowedOrigin(origin string) string { return c.AllowedOrigin(origin) }

const (
	corsAllowMethods = "GET,HEAD,POST,PUT,PATCH,DELETE"
	corsAllowHeaders = "Authorization,Content-Type"
	corsMaxAge       = "600"
)

// corsMiddleware wraps next with CORS header injection.
//
// Preflight (OPTIONS) requests for allowed origins are short-circuited with
// 204 before reaching any auth middleware — browsers never send Authorization
// on preflights. Disallowed origins receive a plain 200 (no CORS headers)
// rather than a 403 so that non-browser clients using OPTIONS are unaffected.
//
// For all other methods, the handler runs normally; CORS headers are injected
// into the response when the origin is allowed, Vary: Origin is always added.
func corsMiddleware(cfg *CORSConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Always set Vary: Origin so caches know the response differs by origin.
		// We add it even when origin is absent or not allowed — a harmless
		// hint to shared caches that they must not conflate responses.
		w.Header().Add("Vary", "Origin")

		allowed := cfg.allowedOrigin(origin)

		// Preflight: browser sends OPTIONS with Origin before the real request.
		if r.Method == http.MethodOptions && origin != "" {
			if allowed != "" {
				w.Header().Set("Access-Control-Allow-Origin", allowed)
				w.Header().Set("Access-Control-Allow-Methods", corsAllowMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
				w.Header().Set("Access-Control-Max-Age", corsMaxAge)
			}
			// Respond 204 regardless: allowed origins get CORS headers,
			// disallowed origins get a plain 204 with no ACAO (browser will
			// block the subsequent request — correct behavior).
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Non-preflight: inject ACAO when origin is allowed, then pass through.
		if allowed != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowed)
		}
		next.ServeHTTP(w, r)
	})
}
