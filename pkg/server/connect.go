package server

import (
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"net/http"
	"net/url"

	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
)

// consentTmpl renders the dev consent page (reimplemented from the original
// scaffold). The form POSTs back to /authorize with all connect params plus
// the user's decision.
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
<p><strong>{{.ClientID}}</strong> is requesting access to a database.</p>
<dl>
 <dt>Database</dt><dd><code>{{.DatabaseID}}</code></dd>
 <dt>Capabilities</dt><dd>{{range .Capabilities}}<code>{{.}}</code> {{end}}</dd>
 <dt>Redirect</dt><dd><code>{{.RedirectURI}}</code></dd>
</dl>
<form method="POST" action="/authorize">
 <input type="hidden" name="client_id" value="{{.ClientID}}">
 <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
 <input type="hidden" name="db" value="{{.DatabaseID}}">
 <input type="hidden" name="capabilities" value="{{.CapabilitiesCSV}}">
 <input type="hidden" name="state" value="{{.State}}">
 <button class="approve" name="decision" value="approve">Approve</button>
 <button class="deny" name="decision" value="deny">Deny</button>
</form>
</div></body></html>`))

type consentView struct {
	ClientID        string
	RedirectURI     string
	DatabaseID      string
	Capabilities    []auth.Capability
	CapabilitiesCSV string
	State           string
}

// validateConnect resolves and validates connect params from query or form
// values, returning the consent view or a client error message.
func (s *Server) validateConnect(v url.Values) (consentView, string) {
	cv := consentView{
		ClientID:        v.Get("client_id"),
		RedirectURI:     v.Get("redirect_uri"),
		DatabaseID:      v.Get("db"),
		CapabilitiesCSV: v.Get("capabilities"),
		State:           v.Get("state"),
	}
	if cv.ClientID == "" {
		return cv, "client_id is required"
	}
	if cv.DatabaseID == "" {
		return cv, "db is required"
	}
	if s.getDB(cv.DatabaseID) == nil {
		return cv, "unknown database: " + cv.DatabaseID
	}
	u, err := url.Parse(cv.RedirectURI)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return cv, "redirect_uri must be an absolute URL"
	}
	caps, err := auth.ParseCapabilities(cv.CapabilitiesCSV)
	if err != nil {
		return cv, err.Error()
	}
	cv.Capabilities = caps
	return cv, ""
}

// handleWellKnown implements GET /.well-known/openvaultdb: discovery of the
// protocol version and, when auth is enabled, the connect endpoints.
func (s *Server) handleWellKnown(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"name":        "OpenVaultDB",
		"protocol":    "openvaultdb/0.1",
		"version":     s.version,
		"authEnabled": s.authCfg != nil,
	}
	if s.authCfg != nil {
		doc["authorizeEndpoint"] = "/authorize"
		doc["tokenEndpoint"] = "/token"
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleAuthorizeGet renders the consent page for a connect request.
func (s *Server) handleAuthorizeGet(w http.ResponseWriter, r *http.Request) {
	cv, errMsg := s.validateConnect(r.URL.Query())
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", errMsg)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentTmpl.Execute(w, cv)
}

// handleAuthorizePost consumes the consent decision: approve mints a
// one-time code and redirects back to the app; deny redirects with an error.
func (s *Server) handleAuthorizePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid form body")
		return
	}
	cv, errMsg := s.validateConnect(r.PostForm)
	if errMsg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", errMsg)
		return
	}
	redirect, _ := url.Parse(cv.RedirectURI)
	q := redirect.Query()
	if cv.State != "" {
		q.Set("state", cv.State)
	}
	if r.PostForm.Get("decision") != "approve" {
		q.Set("error", "access_denied")
		redirect.RawQuery = q.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
		return
	}
	code, err := newAuthCode()
	if err != nil {
		writeMappedError(w, err)
		return
	}
	s.authCfg.Store.PutCode(code, cv.ClientID, cv.RedirectURI, cv.DatabaseID, cv.Capabilities)
	q.Set("code", code)
	redirect.RawQuery = q.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// handleToken implements POST /token: exchanges a one-time code for a scoped
// bearer token (form-encoded, OAuth style: grant_type=authorization_code).
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid form body")
		return
	}
	if gt := r.PostForm.Get("grant_type"); gt != "" && gt != "authorization_code" {
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported grant_type: "+gt)
		return
	}
	token, grant, err := s.authCfg.Store.ExchangeCode(r.PostForm.Get("code"), r.PostForm.Get("client_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int(auth.TokenTTL.Seconds()),
		"database":     grant.DatabaseID,
	})
}

func newAuthCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
