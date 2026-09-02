package demo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
