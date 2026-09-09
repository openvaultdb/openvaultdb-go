package server

import (
	"github.com/dal-go/dalgo/access"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMappedUnsupportedACLDoesNotExposeCause(t *testing.T) {
	for _, tc := range []struct {
		code   access.ReasonCode
		status int
	}{
		{access.CodeEnforcementUnsupported, 422}, {access.CodeAccessDenied, 403},
	} {
		response := httptest.NewRecorder()
		writeMappedError(response, &access.DeniedError{Decision: access.Decision{Operation: access.Query, Code: tc.code, Explanation: "private predicate or row"}})
		if response.Code != tc.status {
			t.Fatalf("code=%s: %d %s", tc.code, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "private predicate") {
			t.Fatal("private cause leaked")
		}
	}
}
