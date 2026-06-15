package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/openvaultdb/openvaultdb-go/internal/store"
)

// consentTmpl renders the dev consent page. The form POSTs back to /authorize
// with all connect params plus the user's decision.
var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>OpenVaultDB — Authorize</title>
<style>
 body{font-family:system-ui,sans-serif;max-width:34rem;margin:3rem auto;padding:0 1rem;color:#111}
 .card{border:1px solid #ddd;border-radius:12px;padding:1.5rem}
 dt{font-weight:600;margin-top:.5rem} dd{margin:0 0 .25rem} code{background:#f4f4f4;padding:.1rem .3rem;border-radius:4px}
 button{font-size:1rem;padding:.6rem 1.2rem;border-radius:8px;border:0;cursor:pointer;margin-right:.5rem}
 .approve{background:#2563eb;color:#fff} .deny{background:#eee}
</style></head>
<body><div class="card">
<h1>Authorize app access</h1>
<p><strong>{{.ClientID}}</strong> is requesting access to a vault namespace.</p>
<dl>
 <dt>Vault</dt><dd><code>{{.Vault}}</code></dd>
 <dt>Namespace</dt><dd><code>{{.NamespaceID}}</code></dd>
 <dt>Role</dt><dd><code>{{.Role}}</code></dd>
 <dt>Grants</dt><dd>{{range $col, $ops := .Scope}}<code>{{$col}}</code>: {{range $ops}}{{.}} {{end}}<br>{{end}}</dd>
</dl>
<form method="POST" action="/authorize">
 <input type="hidden" name="client_id" value="{{.ClientID}}">
 <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
 <input type="hidden" name="vault" value="{{.Vault}}">
 <input type="hidden" name="namespaceId" value="{{.NamespaceID}}">
 <input type="hidden" name="role" value="{{.Role}}">
 <input type="hidden" name="state" value="{{.State}}">
 <button class="approve" name="decision" value="approve">Approve</button>
 <button class="deny" name="decision" value="deny">Deny</button>
</form>
</div></body></html>`))

type consentView struct {
	ClientID    string
	RedirectURI string
	Vault       string
	NamespaceID string
	Role        string
	State       string
	Scope       store.Scope
}

// validateConnect resolves and validates the connect params, returning the
// resolved scope or an error message.
func (s *Server) validateConnect(q url.Values) (consentView, string) {
	v := consentView{
		ClientID:    q.Get("client_id"),
		RedirectURI: q.Get("redirect_uri"),
		Vault:       q.Get("vault"),
		NamespaceID: q.Get("namespaceId"),
		Role:        q.Get("role"),
		State:       q.Get("state"),
	}
	if v.ClientID == "" || v.RedirectURI == "" || v.Vault == "" || v.NamespaceID == "" || v.Role == "" {
		return v, "missing required query parameter"
	}
	if _, ok := s.store.Vault(v.Vault); !ok {
		return v, "unknown vault"
	}
	ns, ok := s.store.Namespace(v.NamespaceID)
	if !ok {
		return v, "unknown namespace"
	}
	scope, ok := ns.ScopeForRole(v.Role)
	if !ok {
		return v, "unknown role"
	}
	v.Scope = scope
	return v, ""
}

// handleAuthorizeForm implements GET /authorize: validate params, show consent.
func (s *Server) handleAuthorizeForm(w http.ResponseWriter, r *http.Request) {
	view, errMsg := s.validateConnect(r.URL.Query())
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", errMsg)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentTmpl.Execute(w, view)
}

// handleAuthorizeDecision implements POST /authorize: on approve, mint a
// one-time code and 302 to redirect_uri; on deny, 302 with error=access_denied.
func (s *Server) handleAuthorizeDecision(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	view, errMsg := s.validateConnect(r.PostForm)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", errMsg)
		return
	}

	redirect, err := url.Parse(view.RedirectURI)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "bad redirect_uri")
		return
	}
	rq := redirect.Query()

	if r.PostForm.Get("decision") != "approve" {
		rq.Set("error", "access_denied")
		if view.State != "" {
			rq.Set("state", view.State)
		}
		redirect.RawQuery = rq.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
		return
	}

	code := randToken()
	s.auth.putCode(code, authCode{
		clientID:    view.ClientID,
		redirectURI: view.RedirectURI,
		vault:       view.Vault,
		namespaceID: view.NamespaceID,
		role:        view.Role,
		scope:       view.Scope,
		expires:     time.Now().Add(5 * time.Minute),
	})

	rq.Set("code", code)
	if view.State != "" {
		rq.Set("state", view.State)
	}
	redirect.RawQuery = rq.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// tokenRequest mirrors the `TokenRequest` model in main.tsp.
type tokenRequest struct {
	GrantType   string `json:"grant_type"`
	Code        string `json:"code"`
	ClientID    string `json:"client_id"`
	RedirectURI string `json:"redirect_uri"`
}

// handleToken implements POST /token: exchange a code for a scoped app token.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "bad JSON body")
		return
	}
	if req.GrantType != "authorization_code" {
		writeError(w, http.StatusBadRequest, "unsupported_grant_type", "expected authorization_code")
		return
	}
	c, ok := s.auth.takeCode(req.Code)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired code")
		return
	}
	// The code is bound to (client_id, redirect_uri); enforce that binding.
	if c.clientID != req.ClientID || c.redirectURI != req.RedirectURI {
		writeError(w, http.StatusBadRequest, "invalid_grant", "client_id/redirect_uri mismatch")
		return
	}

	tok := randToken()
	expires := time.Now().Add(tokenTTL)
	s.auth.putToken(tok, appToken{
		vault:       c.vault,
		namespaceID: c.namespaceID,
		scope:       c.scope,
		expires:     expires,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok,
		"token_type":   "Bearer",
		"expires_in":   int32(tokenTTL.Seconds()),
		"namespaceId":  c.namespaceID,
		"scope":        c.scope,
	})
}

func randToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// rand.Read never fails on supported platforms; fall back deterministically.
		return fmt.Sprintf("tok-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
