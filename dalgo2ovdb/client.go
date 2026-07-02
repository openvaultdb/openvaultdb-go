package dalgo2ovdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Conn is a scoped connection to one namespace within one vault on one
// ovdb-server, established by the OVDB connect flow (see Connect). BaseURL is
// the issuing server (the `iss` callback param — RFC 9207), VaultID and
// NamespaceID identify the granted scope, and Token is the bearer app token
// minted by POST /token.
type Conn struct {
	BaseURL     string
	VaultID     string
	NamespaceID string
	Token       string
}

// restClient is the thin HTTP client for ovdb-server's record CRUD routes:
//
//	GET    /vaults/{vaultId}/ns/{ns}/collections/{collection}/records
//	POST   /vaults/{vaultId}/ns/{ns}/collections/{collection}/records
//	PATCH  /vaults/{vaultId}/ns/{ns}/collections/{collection}/records/{id}
//	DELETE /vaults/{vaultId}/ns/{ns}/collections/{collection}/records/{id}
//
// {ns} is URL-path-escaped (its "/" become "%2F"); the server decodes it back
// via url.PathUnescape (internal/server/records.go).
type restClient struct {
	http *http.Client
	conn Conn
}

func (c *restClient) recordsURL(collection, id string) string {
	u := fmt.Sprintf("%s/vaults/%s/ns/%s/collections/%s/records",
		strings.TrimRight(c.conn.BaseURL, "/"),
		url.PathEscape(c.conn.VaultID),
		url.PathEscape(c.conn.NamespaceID),
		url.PathEscape(collection),
	)
	if id != "" {
		u += "/" + url.PathEscape(id)
	}
	return u
}

func (c *restClient) do(ctx context.Context, method, urlStr string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("dalgo2ovdb: marshal request body: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.conn.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

// list returns every record in collection. OVDB's REST API has no
// single-record GET route, only this collection-wide list — Get/Exists/
// GetMulti/Query all funnel through it (see doc.go).
func (c *restClient) list(ctx context.Context, collection string) ([]map[string]any, error) {
	resp, err := c.do(ctx, http.MethodGet, c.recordsURL(collection, ""), nil)
	if err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: list %q: %w", collection, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var recs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&recs); err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: decode list response: %w", err)
	}
	return recs, nil
}

func (c *restClient) create(ctx context.Context, collection string, body map[string]any) (map[string]any, error) {
	resp, err := c.do(ctx, http.MethodPost, c.recordsURL(collection, ""), body)
	if err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: create in %q: %w", collection, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, apiError(resp)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: decode create response: %w", err)
	}
	return out, nil
}

func (c *restClient) patch(ctx context.Context, collection, id string, body map[string]any) (map[string]any, error) {
	resp, err := c.do(ctx, http.MethodPatch, c.recordsURL(collection, id), body)
	if err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: patch %s/%s: %w", collection, id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, notFoundErr(collection, id)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: decode patch response: %w", err)
	}
	return out, nil
}

func (c *restClient) delete(ctx context.Context, collection, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, c.recordsURL(collection, id), nil)
	if err != nil {
		return fmt.Errorf("dalgo2ovdb: delete %s/%s: %w", collection, id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return notFoundErr(collection, id)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return apiError(resp)
	}
	return nil
}

// apiError reads an OVDB ApiError body ({"code","message"}), or falls back to
// the raw status line.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return fmt.Errorf("dalgo2ovdb: ovdb %d: %s: %s", resp.StatusCode, e.Code, e.Message)
	}
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("dalgo2ovdb: ovdb %d: %s", resp.StatusCode, msg)
}
