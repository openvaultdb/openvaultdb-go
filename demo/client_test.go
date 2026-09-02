package demo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientRejectsInvalidInputBeforeNetworkAndCancelledCalls(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.CreateSession(context.Background(), "token", CreateSessionRequest{RequestID: "bad/id", App: "listus", LocalPort: 6832, OriginToken: "secret"}); err == nil || calls != 0 {
		t.Fatalf("invalid request err=%v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = client.CreateSession(ctx, "token", CreateSessionRequest{RequestID: "request_1", App: "listus", LocalPort: 6832, OriginToken: "secret"}); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
	if calls != 0 {
		t.Fatalf("cancelled request reached server: %d", calls)
	}
}

func TestClientRejectsMalformedDemoMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sessionId":"session_1","ownerUserId":"user_1","spaceId":"space_1","spaceType":"group","databaseId":"demo-sneat-space","expiresAt":"2026-09-02T13:00:00Z","proxyUrl":"//not-an-origin","appUrl":"https://listus.app/space/group/space_1/lists"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.GetSession(context.Background(), "token", "space_1"); err == nil || !strings.Contains(err.Error(), "missing required") {
		t.Fatalf("metadata err=%v", err)
	}
}

func TestClientSessionLifecycleUsesExactPublicContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer device-token" {
			t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.Method + " " + request.URL.RequestURI() {
		case "POST /api/demo/sessions":
			if contentType := request.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("Content-Type = %q", contentType)
			}
			_, _ = w.Write([]byte(`{"sessionId":"session_1","ownerUserId":"user_1","spaceId":"space_1","spaceType":"group","databaseId":"demo-sneat-space","expiresAt":"2026-09-02T13:00:00Z","proxyUrl":"https://data.example","appUrl":"https://listus.example/space_1","tunnelToken":"secret"}`))
		case "GET /api/demo/session?spaceId=space_1":
			_, _ = w.Write([]byte(`{"sessionId":"session_1","ownerUserId":"user_1","spaceId":"space_1","spaceType":"group","databaseId":"demo-sneat-space","expiresAt":"2026-09-02T13:00:00Z","proxyUrl":"https://data.example","appUrl":"https://listus.example/space_1"}`))
		case "DELETE /api/demo/sessions/session_1":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	created, err := client.CreateSession(context.Background(), "device-token", CreateSessionRequest{RequestID: "request_1", App: "listus", LocalPort: 6832, OriginToken: "origin-secret"})
	if err != nil || created.SessionID != "session_1" || created.TunnelToken != "secret" {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	metadata, err := client.GetSession(context.Background(), "device-token", "space_1")
	if err != nil || metadata.SessionID != created.SessionID {
		t.Fatalf("metadata=%+v err=%v", metadata, err)
	}
	if err = client.EndSession(context.Background(), "device-token", created.SessionID); err != nil {
		t.Fatal(err)
	}
}
