package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("read error")
}

func (errReader) Close() error {
	return nil
}

type errRoundTripper struct{}

func (errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       errReader{},
	}, nil
}

func TestClientListDatabasesSendsBoundedAuthenticatedRequest(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/databases" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got, want := request.URL.Query(), (url.Values{"space": {"personal"}, "pageSize": {"100"}, "pageToken": {"opaque + token"}}); !valuesEqual(got, want) {
			t.Fatalf("query = %v, want %v", got, want)
		}
		_, _ = w.Write([]byte(`{"databases":[{"id":"db_1","name":"Personal DB","spaceId":"space_1","spaceType":"personal","provider":"server","status":"active","createdAt":"2026-09-02T12:00:00Z"}],"nextPageToken":"next"}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ListDatabases(context.Background(), "test-token", ListDatabasesRequest{Space: "personal", PageToken: "opaque + token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Databases) != 1 || response.Databases[0].CreatedAt == nil || !response.Databases[0].CreatedAt.Equal(createdAt) || response.NextPageToken != "next" {
		t.Fatalf("response = %#v", response)
	}
}

func TestClientListDatabasesEmptyArrayAndPageSizeValidation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"databases":null}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ListDatabases(context.Background(), "token", ListDatabasesRequest{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if response.Databases == nil || len(response.Databases) != 0 {
		t.Fatalf("Databases = %#v, want non-nil empty slice", response.Databases)
	}
	if _, err := client.ListDatabases(context.Background(), "token", ListDatabasesRequest{PageSize: -1}); err == nil {
		t.Fatal("expected page size error for negative")
	}
	if _, err := client.ListDatabases(context.Background(), "token", ListDatabasesRequest{PageSize: 101}); err == nil {
		t.Fatal("expected page size error")
	}
}

func TestClientGetDatabaseUsesSafeOpaqueIdentifier(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got, want := request.RequestURI, "/api/databases/catalogue_db-01"; got != want {
			t.Fatalf("RequestURI = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(`{"database":{"id":"catalogue_db-01","name":"Database","spaceId":"space","spaceType":"team","provider":"github","status":"active"}}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.GetDatabase(context.Background(), "token", "catalogue_db-01")
	if err != nil {
		t.Fatal(err)
	}
	if response.Database.ID != "catalogue_db-01" {
		t.Fatalf("database = %#v", response.Database)
	}
	if _, err := client.GetDatabase(context.Background(), "token", "../not-valid"); err == nil {
		t.Fatal("expected unsafe identifier error")
	}
}

func TestClientRejectsMalformedSuccessEnvelopesAndRemoteHTTP(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/databases":
			_, _ = w.Write([]byte(`{}`))
		case "/api/databases/catalogue_db":
			_, _ = w.Write([]byte(`{"database":{"id":"different","name":"Database","spaceId":"space","spaceType":"team","provider":"server","status":"active"}}`))
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListDatabases(context.Background(), "token", ListDatabasesRequest{}); err == nil || !strings.Contains(err.Error(), "missing databases") {
		t.Fatalf("list error = %v", err)
	}
	if _, err := client.GetDatabase(context.Background(), "token", "catalogue_db"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("get error = %v", err)
	}
	if _, err := NewClient("http://catalogue.example"); err == nil {
		t.Fatal("expected remote HTTP rejection")
	}
}

func TestClientReturnsSafeStructuredAPIError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"insufficient_scope","error_description":"database metadata access requires databases:read"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListDatabases(context.Background(), "token", ListDatabasesRequest{})
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("error = %v, want APIError", err)
	}
	if apiError.StatusCode != http.StatusForbidden || apiError.Code != "insufficient_scope" || !strings.Contains(apiError.Error(), "databases:read") {
		t.Fatalf("APIError = %#v", apiError)
	}
}

func TestClientDoesNotForwardTokenAcrossRedirect(t *testing.T) {
	t.Parallel()
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		redirected = true
		if token := request.Header.Get("Authorization"); token != "" {
			t.Errorf("redirected Authorization = %q", token)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()
	client, err := NewClient(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListDatabases(context.Background(), "sensitive", ListDatabasesRequest{})
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusFound {
		t.Fatalf("error = %v, want 302 APIError", err)
	}
	if redirected {
		t.Fatal("client followed authenticated redirect")
	}
}

func TestClientBoundsResponseAndRequiresAccessToken(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"databases":[{"id":"too-large"}]}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithMaxResponseBytes(10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.ListDatabases(context.Background(), "token", ListDatabasesRequest{}); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error = %v, want ErrResponseTooLarge", err)
	}
	if _, err = client.ListDatabases(context.Background(), "", ListDatabasesRequest{}); !errors.Is(err, ErrMissingAccessToken) {
		t.Fatalf("error = %v, want ErrMissingAccessToken", err)
	}
}

func TestAPIErrorVariants(t *testing.T) {
	t.Parallel()
	var nilErr *APIError
	if got := nilErr.Error(); got != "<nil>" {
		t.Errorf("nilErr.Error() = %q, want <nil>", got)
	}

	errOnlyCode := &APIError{StatusCode: 400, Code: "bad_request"}
	if got := errOnlyCode.Error(); got != "cloud API: bad_request" {
		t.Errorf("errOnlyCode.Error() = %q", got)
	}

	errOnlyStatus := &APIError{StatusCode: 502}
	if got := errOnlyStatus.Error(); got != "cloud API returned HTTP 502" {
		t.Errorf("errOnlyStatus.Error() = %q", got)
	}
}

func TestClientOptionsAndValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewClient("https://api.example.com", WithHTTPClient(nil)); err == nil {
		t.Fatal("expected error for nil http client")
	}
	if _, err := NewClient("https://api.example.com", WithMaxResponseBytes(0)); err == nil {
		t.Fatal("expected error for zero limit")
	}
	if _, err := NewClient("https://api.example.com", WithMaxResponseBytes(-5)); err == nil {
		t.Fatal("expected error for negative limit")
	}

	customClient := &http.Client{Timeout: 5 * time.Second}
	c, err := NewClient("https://api.example.com", nil, WithHTTPClient(customClient), WithMaxResponseBytes(1024))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.httpClient.Timeout != 5*time.Second {
		t.Errorf("expected 5s timeout, got %v", c.httpClient.Timeout)
	}

	// zero timeout should be set to defaultTimeout
	cZero, err := NewClient("https://api.example.com", WithHTTPClient(&http.Client{Timeout: 0}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cZero.httpClient.Timeout != defaultTimeout {
		t.Errorf("expected defaultTimeout, got %v", cZero.httpClient.Timeout)
	}

	// URL edge cases
	badURLs := []string{
		"",
		"://invalid",
		"ftp://example.com",
		"https://user:pass@example.com",
		"https://example.com?query=1",
		"https://example.com#hash",
		"https://example.com/some/path",
		"http://192.168.1.1",
	}
	for _, u := range badURLs {
		if _, err := NewClient(u); err == nil {
			t.Errorf("expected error for URL %q, got nil", u)
		}
	}

	// Valid loopback hosts
	for _, u := range []string{"http://127.0.0.1", "http://localhost", "https://api.example.com/"} {
		if _, err := NewClient(u); err != nil {
			t.Errorf("unexpected error for %q: %v", u, err)
		}
	}
}

func TestListDatabasesDecodeAndValidationErrors(t *testing.T) {
	t.Parallel()
	// Invalid JSON envelope
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`invalid json`))
	}))
	defer s1.Close()
	c1, _ := NewClient(s1.URL)
	if _, err := c1.ListDatabases(context.Background(), "tok", ListDatabasesRequest{}); err == nil {
		t.Fatal("expected JSON decode error")
	}

	// Invalid databases element (array of non-database)
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"databases": "not an array"}`))
	}))
	defer s2.Close()
	c2, _ := NewClient(s2.URL)
	if _, err := c2.ListDatabases(context.Background(), "tok", ListDatabasesRequest{}); err == nil {
		t.Fatal("expected array decode error")
	}

	// Database item missing required fields
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"databases": [{"id":"db1"}]}`))
	}))
	defer s3.Close()
	c3, _ := NewClient(s3.URL)
	if _, err := c3.ListDatabases(context.Background(), "tok", ListDatabasesRequest{}); err == nil {
		t.Fatal("expected database validation error")
	}
}

func TestGetDatabaseErrors(t *testing.T) {
	t.Parallel()
	// Invalid JSON envelope
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`invalid`))
	}))
	defer s1.Close()
	c1, _ := NewClient(s1.URL)
	if _, err := c1.GetDatabase(context.Background(), "tok", "valid_id"); err == nil {
		t.Fatal("expected decode error")
	}

	// Missing database
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"database": null}`))
	}))
	defer s2.Close()
	c2, _ := NewClient(s2.URL)
	if _, err := c2.GetDatabase(context.Background(), "tok", "valid_id"); err == nil {
		t.Fatal("expected missing database error")
	}

	// Malformed database content
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"database": 1234}`))
	}))
	defer s3.Close()
	c3, _ := NewClient(s3.URL)
	if _, err := c3.GetDatabase(context.Background(), "tok", "valid_id"); err == nil {
		t.Fatal("expected decode metadata error")
	}

	// Validation error on database
	s4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"database": {"id": "valid_id"}}`))
	}))
	defer s4.Close()
	c4, _ := NewClient(s4.URL)
	if _, err := c4.GetDatabase(context.Background(), "tok", "valid_id"); err == nil {
		t.Fatal("expected validateDatabase error")
	}
}

func TestDemoSessionLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Now().Add(time.Hour)
	validSession := DemoSession{
		SessionID:   "sess_1",
		OwnerUserID: "user_1",
		SpaceID:     "space_1",
		SpaceType:   "personal",
		DatabaseID:  "db_1",
		ProxyURL:    "https://proxy.example.com",
		AppURL:      "https://app.example.com",
		ExpiresAt:   now,
		TunnelToken: "tun_tok",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/demo/sessions":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"sessionId": "sess_1",
				"ownerUserId": "user_1",
				"spaceId": "space_1",
				"spaceType": "personal",
				"databaseId": "db_1",
				"proxyUrl": "https://proxy.example.com",
				"appUrl": "https://app.example.com",
				"expiresAt": "` + now.Format(time.RFC3339Nano) + `",
				"tunnelToken": "tun_tok"
			}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/demo/sessions/sess_1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/demo/session":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"sessionId": "sess_1",
				"ownerUserId": "user_1",
				"spaceId": "space_1",
				"spaceType": "personal",
				"databaseId": "db_1",
				"proxyUrl": "https://proxy.example.com",
				"appUrl": "https://app.example.com",
				"expiresAt": "` + now.Format(time.RFC3339Nano) + `"
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	// CreateDemoSession validation failures
	badReqs := []CreateDemoSessionRequest{
		{RequestID: "invalid/id", App: "listus", LocalPort: 8080, OriginToken: "tok"},
		{RequestID: "req1", App: "badapp", LocalPort: 8080, OriginToken: "tok"},
		{RequestID: "req1", App: "listus", LocalPort: 0, OriginToken: "tok"},
		{RequestID: "req1", App: "listus", LocalPort: 70000, OriginToken: "tok"},
		{RequestID: "req1", App: "listus", LocalPort: 8080, OriginToken: ""},
	}
	for _, br := range badReqs {
		if _, err := client.CreateDemoSession(context.Background(), "tok", br); err == nil {
			t.Errorf("expected error for bad request: %+v", br)
		}
	}

	// CreateDemoSession success
	sess, err := client.CreateDemoSession(context.Background(), "tok", CreateDemoSessionRequest{
		RequestID:   "req_1",
		App:         "listus",
		LocalPort:   8080,
		OriginToken: "tok",
	})
	if err != nil {
		t.Fatalf("CreateDemoSession failed: %v", err)
	}
	if sess.SessionID != validSession.SessionID || sess.TunnelToken != "tun_tok" {
		t.Fatalf("unexpected session: %+v", sess)
	}

	// EndDemoSession
	if err := client.EndDemoSession(context.Background(), "tok", "sess/invalid"); err == nil {
		t.Fatal("expected error for unsafe identifier")
	}
	if err := client.EndDemoSession(context.Background(), "tok", "sess_1"); err != nil {
		t.Fatalf("EndDemoSession failed: %v", err)
	}

	// GetDemoSession
	if _, err := client.GetDemoSession(context.Background(), "tok", "space/invalid"); err == nil {
		t.Fatal("expected error for unsafe identifier")
	}
	meta, err := client.GetDemoSession(context.Background(), "tok", "space_1")
	if err != nil {
		t.Fatalf("GetDemoSession failed: %v", err)
	}
	if meta.SessionID != "sess_1" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
}

func TestDemoSessionErrors(t *testing.T) {
	t.Parallel()
	now := time.Now().Add(time.Hour)

	// CreateDemoSession: decode error
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`invalid`))
	}))
	defer s1.Close()
	c1, _ := NewClient(s1.URL)
	if _, err := c1.CreateDemoSession(context.Background(), "tok", CreateDemoSessionRequest{
		RequestID:   "req1",
		App:         "listus",
		LocalPort:   8080,
		OriginToken: "tok",
	}); err == nil {
		t.Fatal("expected decode error")
	}

	// CreateDemoSession: validation error (missing metadata)
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sessionId":"s1"}`))
	}))
	defer s2.Close()
	c2, _ := NewClient(s2.URL)
	if _, err := c2.CreateDemoSession(context.Background(), "tok", CreateDemoSessionRequest{
		RequestID:   "req1",
		App:         "listus",
		LocalPort:   8080,
		OriginToken: "tok",
	}); err == nil {
		t.Fatal("expected validateDemoSession error")
	}

	// CreateDemoSession: missing tunnel token
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"sessionId": "sess_1",
			"ownerUserId": "user_1",
			"spaceId": "space_1",
			"spaceType": "personal",
			"databaseId": "db_1",
			"proxyUrl": "https://proxy.example.com",
			"appUrl": "https://app.example.com",
			"expiresAt": "` + now.Format(time.RFC3339Nano) + `"
		}`))
	}))
	defer s3.Close()
	c3, _ := NewClient(s3.URL)
	if _, err := c3.CreateDemoSession(context.Background(), "tok", CreateDemoSessionRequest{
		RequestID:   "req1",
		App:         "listus",
		LocalPort:   8080,
		OriginToken: "tok",
	}); err == nil {
		t.Fatal("expected missing tunnel token error")
	}

	// GetDemoSession: decode error
	c1Get, _ := NewClient(s1.URL)
	if _, err := c1Get.GetDemoSession(context.Background(), "tok", "space1"); err == nil {
		t.Fatal("expected decode error")
	}

	// GetDemoSession: validateDemoSessionMetadata error
	c2Get, _ := NewClient(s2.URL)
	if _, err := c2Get.GetDemoSession(context.Background(), "tok", "space1"); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestPostJSONMarshalError(t *testing.T) {
	t.Parallel()
	client, err := NewClient("https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	unmarshalable := make(chan int)
	if _, err := client.postJSON(context.Background(), "https://example.com/api", "tok", unmarshalable); err == nil {
		t.Fatal("expected error for unmarshalable body")
	}
}

func TestReadBoundedErrorAndCustomTransport(t *testing.T) {
	t.Parallel()
	client, err := NewClient("https://example.com", WithHTTPClient(&http.Client{
		Transport: errRoundTripper{},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetDatabase(context.Background(), "tok", "db1"); err == nil {
		t.Fatal("expected read bounded error")
	}
}

func TestHelperCoverage(t *testing.T) {
	t.Parallel()
	if !isSafeIdentifier("abc-123_XYZ") {
		t.Error("expected valid safe identifier")
	}
	if isSafeIdentifier("") {
		t.Error("expected empty string to be unsafe")
	}
	if isSafeIdentifier("hello world") {
		t.Error("expected space to be unsafe")
	}

	escaped := escapePathSegment("a-b_c~1!")
	if !strings.Contains(escaped, "%21") {
		t.Errorf("expected ! to be percent-encoded, got %s", escaped)
	}

	if isAbsoluteHTTPSURL("http://example.com") {
		t.Error("expected false for http")
	}
	if isAbsoluteHTTPSURL("https://example.com?query=1") {
		t.Error("expected false for query")
	}
	if isAbsoluteHTTPSURL("https://user@example.com") {
		t.Error("expected false for userinfo")
	}
	if isAbsoluteHTTPSURL("https://example.com#frag") {
		t.Error("expected false for frag")
	}
	if isAbsoluteHTTPSURL("://invalid") {
		t.Error("expected false for invalid url")
	}
	if !isAbsoluteHTTPSURL("https://example.com") {
		t.Error("expected true for valid https")
	}

	// decodeAPIError with non-json
	err := decodeAPIError(500, []byte("plain text error"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRequestJSONAndContextCancellationErrors(t *testing.T) {
	t.Parallel()
	client, err := NewClient("https://example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Invalid HTTP method triggers build cloud request error
	if _, err := client.requestJSON(context.Background(), "INVALID METHOD WITH SPACE", "https://example.com", "token", nil); err == nil {
		t.Fatal("expected error for invalid method")
	}

	// Cancelled context triggers client.Do error
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.requestJSON(ctx, http.MethodGet, "https://example.com", "token", nil); err == nil {
		t.Fatal("expected error for cancelled context in requestJSON")
	}

	// CreateDemoSession with cancelled context triggers postJSON error
	if _, err := client.CreateDemoSession(ctx, "token", CreateDemoSessionRequest{
		RequestID:   "req_1",
		App:         "listus",
		LocalPort:   8080,
		OriginToken: "tok",
	}); err == nil {
		t.Fatal("expected error for cancelled context in CreateDemoSession")
	}

	// GetDemoSession with cancelled context triggers getJSON error
	if _, err := client.GetDemoSession(ctx, "token", "space_1"); err == nil {
		t.Fatal("expected error for cancelled context in GetDemoSession")
	}
}

func valuesEqual(got, want url.Values) bool {
	return got.Encode() == want.Encode()
}
