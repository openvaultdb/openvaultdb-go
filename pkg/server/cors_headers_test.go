package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestCORSAdditiveImmutablePinsArePerInstanceAndExactOrigin(t *testing.T) {
	pins := []string{"OVDB-Provider-Revision", "OVDB-Source-SHA256", "OVDB-Serving-SHA256", "OVDB-Manifest-SHA256"}
	original := ParseCORSOrigins([]string{"https://datatug.app"})
	configured, err := original.WithHeaders(pins, pins)
	if err != nil {
		t.Fatal(err)
	}
	pins[0] = "Mutated"
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(409) })
	for _, origin := range []string{"https://datatug.app", "https://datatug.app.evil", "http://datatug.app", "https://datatug.app:443"} {
		resp := profileRequest(corsMiddleware(configured, next), "OPTIONS", "/", "", map[string]string{"Origin": origin, "Access-Control-Request-Headers": "OVDB-Manifest-SHA256"})
		allowed := origin == "https://datatug.app"
		if (resp.Header().Get("Access-Control-Allow-Origin") != "") != allowed {
			t.Fatalf("origin match %s: %#v", origin, resp.Header())
		}
		if allowed && (!strings.Contains(resp.Header().Get("Access-Control-Allow-Headers"), "OVDB-Manifest-SHA256") || !strings.Contains(resp.Header().Get("Access-Control-Allow-Headers"), "OVDB-Provider-Revision") || strings.Contains(resp.Header().Get("Access-Control-Allow-Headers"), "Mutated")) {
			t.Fatal(resp.Header())
		}
		resp = profileRequest(corsMiddleware(configured, next), "GET", "/", "", map[string]string{"Origin": origin})
		if resp.Code != 409 || (strings.Contains(resp.Header().Get("Access-Control-Expose-Headers"), "OVDB-Manifest-SHA256")) != allowed {
			t.Fatalf("error exposure %s: %#v", origin, resp.Header())
		}
	}
	legacy := profileRequest(corsMiddleware(original, next), "OPTIONS", "/", "", map[string]string{"Origin": "https://datatug.app"})
	if legacy.Header().Get("Access-Control-Allow-Headers") != corsAllowHeaders || legacy.Header().Get("Access-Control-Expose-Headers") != "" {
		t.Fatalf("legacy default changed: %#v", legacy.Header())
	}
	if _, err := original.WithHeaders([]string{"Bad\r\nHeader"}, nil); err == nil {
		t.Fatal("header injection accepted")
	}
}
