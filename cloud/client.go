// Package cloud provides a small, dependency-free client for OpenVaultDB Cloud
// registration metadata. It does not access database records or credentials.
package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultPageSize is the number of registrations requested when a list
	// request does not specify a page size.
	DefaultPageSize = 100
	// MaxPageSize is the largest page accepted by the cloud catalogue API.
	MaxPageSize = 100

	defaultResponseLimit = 1 << 20 // 1 MiB
	defaultTimeout       = 15 * time.Second
)

var (
	// ErrMissingAccessToken is returned before making an unauthenticated API
	// request.
	ErrMissingAccessToken = errors.New("cloud access token is required")
	// ErrResponseTooLarge prevents an API response from consuming unbounded
	// memory in a client process.
	ErrResponseTooLarge = errors.New("cloud API response exceeds size limit")
)

// Database is safe registration metadata for one database in an accessible
// Space. It deliberately excludes database records and provider credentials.
type Database struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	SpaceID          string     `json:"spaceId"`
	SpaceType        string     `json:"spaceType"`
	Provider         string     `json:"provider"`
	Status           string     `json:"status"`
	SpaceName        string     `json:"spaceName,omitempty"`
	ServerURL        string     `json:"serverUrl,omitempty"`
	ServerDatabaseID string     `json:"serverDatabaseId,omitempty"`
	Repository       string     `json:"repository,omitempty"`
	Branch           string     `json:"branch,omitempty"`
	Path             string     `json:"path,omitempty"`
	CreatedAt        *time.Time `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time `json:"updatedAt,omitempty"`
}

// ListDatabasesRequest limits a catalogue list to one Space when Space is
// "personal" or an opaque Space ID. PageToken is opaque and must be passed
// unchanged from a previous response.
type ListDatabasesRequest struct {
	Space     string
	PageSize  int
	PageToken string
}

// ListDatabasesResponse is one page of database registration metadata.
// Databases is always non-nil, including for an empty response.
type ListDatabasesResponse struct {
	Databases     []Database `json:"databases"`
	NextPageToken string     `json:"nextPageToken,omitempty"`
}

// GetDatabaseResponse is the envelope returned for a single registration.
type GetDatabaseResponse struct {
	Database Database `json:"database"`
}

// CreateDemoSessionRequest asks OpenVaultDB Cloud to bind a local, loopback
// Listus database to the caller's dedicated demo Space. OriginToken is a
// database-scoped secret: callers must never log or persist it.
type CreateDemoSessionRequest struct {
	RequestID   string `json:"requestId"`
	App         string `json:"app"`
	LocalPort   int    `json:"localPort"`
	OriginToken string `json:"originToken"`
}

// DemoSession is the response to creating a demo session. TunnelToken is
// response-body-only and must be delivered to cloudflared via a protected
// temporary file, never rendered in a URL or log.
type DemoSession struct {
	SessionID   string    `json:"sessionId"`
	OwnerUserID string    `json:"ownerUserId"`
	SpaceID     string    `json:"spaceId"`
	SpaceType   string    `json:"spaceType"`
	DatabaseID  string    `json:"databaseId"`
	ExpiresAt   time.Time `json:"expiresAt"`
	ProxyURL    string    `json:"proxyUrl"`
	AppURL      string    `json:"appUrl"`
	TunnelToken string    `json:"tunnelToken,omitempty"`
}

// DemoSessionMetadata is the safe response returned to a signed-in owner. It
// deliberately excludes tunnel and origin credentials.
type DemoSessionMetadata struct {
	SessionID   string    `json:"sessionId"`
	OwnerUserID string    `json:"ownerUserId"`
	SpaceID     string    `json:"spaceId"`
	SpaceType   string    `json:"spaceType"`
	DatabaseID  string    `json:"databaseId"`
	ExpiresAt   time.Time `json:"expiresAt"`
	ProxyURL    string    `json:"proxyUrl"`
	AppURL      string    `json:"appUrl"`
}

// APIError is a structured error returned by the cloud catalogue API.
// Description is API-provided text. Callers that display it in a terminal or
// another rendered surface must sanitize it. Arbitrary response bodies are
// never included in Error.
type APIError struct {
	StatusCode  int
	Code        string
	Description string
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Code != "" && e.Description != "" {
		return fmt.Sprintf("cloud API: %s: %s", e.Code, e.Description)
	}
	if e.Code != "" {
		return "cloud API: " + e.Code
	}
	return fmt.Sprintf("cloud API returned HTTP %d", e.StatusCode)
}

// Option configures a Client.
type Option func(*clientConfig) error

type clientConfig struct {
	httpClient     *http.Client
	maxResponseLen int64
}

// WithHTTPClient uses a copy of client for requests. Redirect following is
// always disabled so bearer credentials are never forwarded to another host.
func WithHTTPClient(client *http.Client) Option {
	return func(config *clientConfig) error {
		if client == nil {
			return errors.New("cloud HTTP client is nil")
		}
		copy := *client
		config.httpClient = &copy
		return nil
	}
}

// WithMaxResponseBytes sets an upper bound for both success and error bodies.
func WithMaxResponseBytes(limit int64) Option {
	return func(config *clientConfig) error {
		if limit <= 0 {
			return errors.New("cloud response size limit must be positive")
		}
		config.maxResponseLen = limit
		return nil
	}
}

// Client makes authenticated, read-only requests to the public OpenVaultDB
// Cloud catalogue API.
type Client struct {
	baseURL        *url.URL
	httpClient     *http.Client
	maxResponseLen int64
}

// NewClient creates a catalogue client for an absolute http(s) base URL. HTTP
// is accepted only for loopback development, so bearer tokens are never sent
// to a remote cleartext host.
func NewClient(baseURL string, options ...Option) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, errors.New("cloud base URL must be absolute")
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, errors.New("cloud base URL must contain only an HTTP scheme and host")
	}
	if base.Scheme == "http" && !isLoopbackHost(base.Hostname()) {
		return nil, errors.New("cloud base URL must use HTTPS (HTTP is allowed only for loopback development)")
	}
	base.Path = ""
	base.RawPath = ""
	config := clientConfig{
		httpClient:     &http.Client{Timeout: defaultTimeout},
		maxResponseLen: defaultResponseLimit,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	if config.httpClient.Timeout == 0 {
		config.httpClient.Timeout = defaultTimeout
	}
	// Never follow redirects for authenticated requests. This also overrides
	// a caller's CheckRedirect setting, which could otherwise leak the token.
	config.httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{baseURL: base, httpClient: config.httpClient, maxResponseLen: config.maxResponseLen}, nil
}

// ListDatabases returns one page of accessible registrations.
func (c *Client) ListDatabases(ctx context.Context, accessToken string, request ListDatabasesRequest) (ListDatabasesResponse, error) {
	if request.PageSize < 0 || request.PageSize > MaxPageSize {
		return ListDatabasesResponse{}, fmt.Errorf("cloud page size must be between 1 and %d", MaxPageSize)
	}
	pageSize := request.PageSize
	if pageSize == 0 {
		pageSize = DefaultPageSize
	}
	query := url.Values{}
	query.Set("pageSize", fmt.Sprintf("%d", pageSize))
	if request.Space != "" {
		query.Set("space", request.Space)
	}
	if request.PageToken != "" {
		query.Set("pageToken", request.PageToken)
	}
	endpoint := *c.baseURL
	endpoint.Path = "/api/databases"
	endpoint.RawQuery = query.Encode()

	body, err := c.getJSON(ctx, endpoint.String(), accessToken)
	if err != nil {
		return ListDatabasesResponse{}, err
	}
	var envelope struct {
		Databases     json.RawMessage `json:"databases"`
		NextPageToken string          `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ListDatabasesResponse{}, fmt.Errorf("decode cloud database list: %w", err)
	}
	if envelope.Databases == nil {
		return ListDatabasesResponse{}, errors.New("cloud database list response is missing databases")
	}
	response := ListDatabasesResponse{NextPageToken: envelope.NextPageToken}
	if string(envelope.Databases) == "null" {
		response.Databases = []Database{}
	} else if err := json.Unmarshal(envelope.Databases, &response.Databases); err != nil {
		return ListDatabasesResponse{}, fmt.Errorf("decode cloud databases: %w", err)
	}
	for _, database := range response.Databases {
		if err := validateDatabase(database); err != nil {
			return ListDatabasesResponse{}, err
		}
	}
	return response, nil
}

// GetDatabase returns one accessible registration by its opaque ID.
func (c *Client) GetDatabase(ctx context.Context, accessToken, id string) (GetDatabaseResponse, error) {
	if !isSafeIdentifier(id) {
		return GetDatabaseResponse{}, errors.New("cloud database ID must be a URL-safe identifier")
	}
	endpoint := *c.baseURL
	endpoint.Path = "/api/databases/" + id
	endpoint.RawPath = "/api/databases/" + escapePathSegment(id)
	body, err := c.getJSON(ctx, endpoint.String(), accessToken)
	if err != nil {
		return GetDatabaseResponse{}, err
	}
	var envelope struct {
		Database json.RawMessage `json:"database"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return GetDatabaseResponse{}, fmt.Errorf("decode cloud database: %w", err)
	}
	if envelope.Database == nil || string(envelope.Database) == "null" {
		return GetDatabaseResponse{}, errors.New("cloud database response is missing database")
	}
	var response GetDatabaseResponse
	if err := json.Unmarshal(envelope.Database, &response.Database); err != nil {
		return GetDatabaseResponse{}, fmt.Errorf("decode cloud database metadata: %w", err)
	}
	if err := validateDatabase(response.Database); err != nil {
		return GetDatabaseResponse{}, err
	}
	if response.Database.ID != id {
		return GetDatabaseResponse{}, errors.New("cloud database response ID does not match request")
	}
	return response, nil
}

// CreateDemoSession creates one bounded Listus demo session. The access token
// must carry the demo:write device scope.
func (c *Client) CreateDemoSession(ctx context.Context, accessToken string, request CreateDemoSessionRequest) (DemoSession, error) {
	if err := validateCreateDemoSessionRequest(request); err != nil {
		return DemoSession{}, err
	}
	endpoint := *c.baseURL
	endpoint.Path = "/api/demo/sessions"
	body, err := c.postJSON(ctx, endpoint.String(), accessToken, request)
	if err != nil {
		return DemoSession{}, err
	}
	var response DemoSession
	if err = json.Unmarshal(body, &response); err != nil {
		return DemoSession{}, fmt.Errorf("decode cloud demo session: %w", err)
	}
	if err = validateDemoSession(response, true); err != nil {
		return DemoSession{}, err
	}
	return response, nil
}

// EndDemoSession asks Cloud to end the caller's active session. It is safe to
// call again after a successful end.
func (c *Client) EndDemoSession(ctx context.Context, accessToken, sessionID string) error {
	if !isSafeIdentifier(sessionID) {
		return errors.New("cloud demo session ID must be a URL-safe identifier")
	}
	endpoint := *c.baseURL
	endpoint.Path = "/api/demo/sessions/" + sessionID
	endpoint.RawPath = "/api/demo/sessions/" + escapePathSegment(sessionID)
	_, err := c.requestJSON(ctx, http.MethodDelete, endpoint.String(), accessToken, nil)
	return err
}

// GetDemoSession returns safe metadata for the caller's active session in a
// Space. No origin or connector credentials are returned.
func (c *Client) GetDemoSession(ctx context.Context, accessToken, spaceID string) (DemoSessionMetadata, error) {
	if !isSafeIdentifier(spaceID) {
		return DemoSessionMetadata{}, errors.New("cloud demo Space ID must be a URL-safe identifier")
	}
	endpoint := *c.baseURL
	endpoint.Path = "/api/demo/session"
	endpoint.RawQuery = url.Values{"spaceId": {spaceID}}.Encode()
	body, err := c.getJSON(ctx, endpoint.String(), accessToken)
	if err != nil {
		return DemoSessionMetadata{}, err
	}
	var response DemoSessionMetadata
	if err = json.Unmarshal(body, &response); err != nil {
		return DemoSessionMetadata{}, fmt.Errorf("decode cloud demo session metadata: %w", err)
	}
	if err = validateDemoSessionMetadata(response); err != nil {
		return DemoSessionMetadata{}, err
	}
	return response, nil
}

func (c *Client) getJSON(ctx context.Context, endpoint, accessToken string) ([]byte, error) {
	return c.requestJSON(ctx, http.MethodGet, endpoint, accessToken, nil)
}

func (c *Client) postJSON(ctx context.Context, endpoint, accessToken string, body any) ([]byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode cloud request: %w", err)
	}
	return c.requestJSON(ctx, http.MethodPost, endpoint, accessToken, strings.NewReader(string(encoded)))
}

func (c *Client) requestJSON(ctx context.Context, method, endpoint, accessToken string, requestBody io.Reader) ([]byte, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, ErrMissingAccessToken
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return nil, fmt.Errorf("build cloud request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request cloud API: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := readBounded(response.Body, c.maxResponseLen)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, decodeAPIError(response.StatusCode, body)
	}
	return body, nil
}

func validateCreateDemoSessionRequest(request CreateDemoSessionRequest) error {
	if !isSafeIdentifier(request.RequestID) {
		return errors.New("cloud demo request ID must be a URL-safe identifier")
	}
	if request.App != "listus" {
		return errors.New("cloud demo app must be listus")
	}
	if request.LocalPort < 1 || request.LocalPort > 65535 {
		return errors.New("cloud demo local port must be between 1 and 65535")
	}
	if strings.TrimSpace(request.OriginToken) == "" {
		return errors.New("cloud demo origin token is required")
	}
	return nil
}

func validateDemoSession(session DemoSession, requireTunnelToken bool) error {
	if !isSafeIdentifier(session.SessionID) || !isSafeIdentifier(session.OwnerUserID) || !isSafeIdentifier(session.SpaceID) || !isSafeIdentifier(session.DatabaseID) || session.SpaceType == "" || session.ExpiresAt.IsZero() || !isAbsoluteHTTPSURL(session.ProxyURL) || !isAbsoluteHTTPSURL(session.AppURL) {
		return errors.New("cloud demo session response is missing required metadata")
	}
	if requireTunnelToken && strings.TrimSpace(session.TunnelToken) == "" {
		return errors.New("cloud demo session response is missing tunnel token")
	}
	return nil
}

func validateDemoSessionMetadata(session DemoSessionMetadata) error {
	if !isSafeIdentifier(session.SessionID) || !isSafeIdentifier(session.OwnerUserID) || !isSafeIdentifier(session.SpaceID) || !isSafeIdentifier(session.DatabaseID) || session.SpaceType == "" || session.ExpiresAt.IsZero() || !isAbsoluteHTTPSURL(session.ProxyURL) || !isAbsoluteHTTPSURL(session.AppURL) {
		return errors.New("cloud demo session metadata is missing required fields")
	}
	return nil
}

func isAbsoluteHTTPSURL(raw string) bool {
	value, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && value.Scheme == "https" && value.Host != "" && value.User == nil && value.RawQuery == "" && value.Fragment == ""
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read cloud API response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

func decodeAPIError(statusCode int, body []byte) error {
	var response struct {
		Code        string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return &APIError{StatusCode: statusCode}
	}
	return &APIError{StatusCode: statusCode, Code: response.Code, Description: response.Description}
}

func escapePathSegment(value string) string {
	var escaped strings.Builder
	for _, byteValue := range []byte(value) {
		if (byteValue >= 'a' && byteValue <= 'z') || (byteValue >= 'A' && byteValue <= 'Z') ||
			(byteValue >= '0' && byteValue <= '9') || byteValue == '-' || byteValue == '_' || byteValue == '~' {
			escaped.WriteByte(byteValue)
			continue
		}
		fmt.Fprintf(&escaped, "%%%02X", byteValue)
	}
	return escaped.String()
}

func validateDatabase(database Database) error {
	for _, required := range []struct {
		name  string
		value string
	}{
		{"id", database.ID},
		{"name", database.Name},
		{"spaceId", database.SpaceID},
		{"spaceType", database.SpaceType},
		{"provider", database.Provider},
		{"status", database.Status},
	} {
		if required.value == "" {
			return fmt.Errorf("cloud database metadata is missing %s", required.name)
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func isSafeIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, byteValue := range []byte(value) {
		if (byteValue >= 'a' && byteValue <= 'z') || (byteValue >= 'A' && byteValue <= 'Z') ||
			(byteValue >= '0' && byteValue <= '9') || byteValue == '-' || byteValue == '_' {
			continue
		}
		return false
	}
	return true
}
