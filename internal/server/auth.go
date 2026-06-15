package server

import (
	"sync"
	"time"

	"github.com/openvaultdb/openvaultdb-go/internal/store"
)

// tokenTTL is how long a minted app token stays valid.
const tokenTTL = time.Hour

// authCode is a one-time authorization code bound to a connect request.
type authCode struct {
	clientID    string
	redirectURI string
	vault       string
	namespaceID string
	role        string
	scope       store.Scope
	expires     time.Time
}

// appToken is an issued, scoped opaque bearer token.
type appToken struct {
	vault       string
	namespaceID string
	scope       store.Scope
	expires     time.Time
}

// authStore holds in-memory one-time codes and issued tokens. Tokens are not
// persisted across restarts — apps re-run the connect flow, which is fine for
// the local demo.
type authStore struct {
	mu     sync.Mutex
	codes  map[string]authCode
	tokens map[string]appToken
}

func newAuthStore() *authStore {
	return &authStore{
		codes:  map[string]authCode{},
		tokens: map[string]appToken{},
	}
}

func (a *authStore) putCode(code string, c authCode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.codes[code] = c
}

// takeCode consumes a code (one-time use) and returns it if valid + unexpired.
func (a *authStore) takeCode(code string) (authCode, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.codes[code]
	if !ok {
		return authCode{}, false
	}
	delete(a.codes, code)
	if time.Now().After(c.expires) {
		return authCode{}, false
	}
	return c, true
}

func (a *authStore) putToken(tok string, t appToken) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tokens[tok] = t
}

// token returns an issued token if valid + unexpired.
func (a *authStore) token(tok string) (appToken, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tokens[tok]
	if !ok || time.Now().After(t.expires) {
		return appToken{}, false
	}
	return t, true
}

// allows reports whether the scope grants op on collection.
func scopeAllows(scope store.Scope, collection string, op store.Op) bool {
	for _, granted := range scope[collection] {
		if granted == op {
			return true
		}
	}
	return false
}
