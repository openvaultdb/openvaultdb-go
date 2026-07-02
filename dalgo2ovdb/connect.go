package dalgo2ovdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dal-go/dalgo/dal"
)

// ConnectParams describes an OVDB connect-flow request: which app
// (ClientID/RedirectURI) is asking for Role-scoped access to which vault
// namespace (Vault/NamespaceID). Mirrors the query params
// internal/server/connect.go's validateConnect expects.
type ConnectParams struct {
	ClientID    string
	RedirectURI string
	Vault       string
	NamespaceID string
	Role        string
	State       string
}

// Connect runs the OVDB connect flow (GET/POST /authorize -> code -> POST
// /token) against a running ovdb-server at baseURL and returns a ready-to-use
// dal.DB scoped to the granted vault namespace.
//
// It drives the consent step by POSTing decision=approve straight to
// /authorize, skipping the GET-rendered HTML consent page a browser would
// show a human. That is the right choice for this PoC's automated round trip
// and for a trusted server-side/CLI caller; a real end-user-facing flow would
// render the consent page and let a human click Approve, then land on
// RedirectURI with ?code=...&iss=..., and a caller would resume from there
// (see ExchangeCode) instead of calling Connect.
//
// Track A gap this surfaces: the resulting Conn.Token is held only in this
// process's memory. ovdb-server itself also holds it only in memory
// (internal/server/auth.go) — restarting either side loses the grant and
// Connect must run again. Persisting tokens/grants across restarts is
// Track A's #1 blocker, not something this adapter can paper over.
func Connect(ctx context.Context, httpClient *http.Client, baseURL string, p ConnectParams) (dal.DB, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}

	code, iss, err := authorize(ctx, httpClient, baseURL, p)
	if err != nil {
		return nil, err
	}
	return ExchangeCode(ctx, httpClient, iss, p.ClientID, p.RedirectURI, code)
}

// authorize POSTs the connect params straight to /authorize with an approve
// decision and returns the one-time code plus the issuing server (the `iss`
// query param — RFC 9207 — which is where the code must be exchanged).
func authorize(ctx context.Context, httpClient *http.Client, baseURL string, p ConnectParams) (code, iss string, err error) {
	noRedirect := *httpClient
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	form := url.Values{}
	form.Set("client_id", p.ClientID)
	form.Set("redirect_uri", p.RedirectURI)
	form.Set("vault", p.Vault)
	form.Set("namespaceId", p.NamespaceID)
	form.Set("role", p.Role)
	form.Set("decision", "approve")
	if p.State != "" {
		form.Set("state", p.State)
	}

	authorizeURL := strings.TrimRight(baseURL, "/") + "/authorize"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authorizeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("dalgo2ovdb: connect: POST /authorize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", "", fmt.Errorf("dalgo2ovdb: connect: /authorize returned %s (want 302 Found)", resp.Status)
	}
	loc, err := resp.Location()
	if err != nil {
		return "", "", fmt.Errorf("dalgo2ovdb: connect: /authorize redirect has no Location header: %w", err)
	}
	q := loc.Query()
	if reason := q.Get("error"); reason != "" {
		return "", "", fmt.Errorf("dalgo2ovdb: connect: access denied: %s", reason)
	}
	code = q.Get("code")
	if code == "" {
		return "", "", fmt.Errorf("dalgo2ovdb: connect: /authorize redirect has no code")
	}
	iss = q.Get("iss")
	if iss == "" {
		iss = baseURL // defensive fallback; ovdb-server always sets iss (connect.go)
	}
	return code, iss, nil
}

// ExchangeCode swaps an authorization code for a scoped app token via POST
// {baseURL}/token (baseURL is the issuing server from the `iss` param) and
// returns a ready-to-use dal.DB. Use this directly when resuming a real
// browser-driven consent flow (the code came from RedirectURI's callback, not
// from Connect's own authorize() call).
func ExchangeCode(ctx context.Context, httpClient *http.Client, baseURL, clientID, redirectURI, code string) (dal.DB, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	body, err := json.Marshal(map[string]string{
		"grant_type":   "authorization_code",
		"code":         code,
		"client_id":    clientID,
		"redirect_uri": redirectURI,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: connect: POST /token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Vault       string `json:"vault"`
		NamespaceID string `json:"namespaceId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: connect: decode /token response: %w", err)
	}

	conn := Conn{
		BaseURL:     baseURL,
		VaultID:     tok.Vault,
		NamespaceID: tok.NamespaceID,
		Token:       tok.AccessToken,
	}
	return NewDB(httpClient, conn), nil
}
