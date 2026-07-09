package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Config is the server-side auth configuration. A nil *Config means auth is
// disabled (the local-dev default documented in the threat model).
type Config struct {
	// OwnerToken grants full access to everything, including admin endpoints.
	OwnerToken string
	// Store holds application grants and pending authorization codes.
	Store *Store
}

type contextKey struct{}

// FromRequest returns the authenticated principal attached by Middleware,
// or nil when the request is unauthenticated (public path or auth disabled).
func FromRequest(r *http.Request) *Principal {
	p, _ := r.Context().Value(contextKey{}).(*Principal)
	return p
}

// publicPath reports whether the path is reachable without a token: the
// discovery document and the connect flow itself.
func publicPath(path string) bool {
	switch path {
	case "/.well-known/openvaultdb", "/authorize", "/token":
		return true
	}
	return false
}

// Middleware implements the spec's Layer-1 token validation: it
// authenticates every request (401 on missing/invalid tokens except on
// public paths) and attaches the Principal for the handlers' Layer-2
// capability enforcement.
func (cfg *Config) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		token := BearerToken(r)
		if token == "" {
			unauthorized(w, "missing bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.OwnerToken)) == 1 {
			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), contextKey{}, &Principal{Owner: true})))
			return
		}
		if g := cfg.Store.Lookup(token); g != nil {
			next.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), contextKey{}, &Principal{Grant: g})))
			return
		}
		unauthorized(w, "invalid or expired token")
	})
}

// BearerToken extracts the bearer token from the Authorization header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func unauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", `Bearer realm="openvaultdb"`)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"` + message + `"}}`))
}
