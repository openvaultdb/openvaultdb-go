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

func TestClientListDatabasesSendsBoundedAuthenticatedRequest(t *testing.T) {
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
	if _, err := client.ListDatabases(context.Background(), "token", ListDatabasesRequest{PageSize: 101}); err == nil {
		t.Fatal("expected page size error")
	}
}

func TestClientGetDatabaseUsesSafeOpaqueIdentifier(t *testing.T) {
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

func valuesEqual(got, want url.Values) bool {
	return got.Encode() == want.Encode()
}
