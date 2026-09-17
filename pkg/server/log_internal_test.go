package server

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

func TestInternalErrorsAreLoggedRedactionSafe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		logged bool
	}{
		{"unrecognised driver error", errors.New("disk exploded"), http.StatusInternalServerError, true},
		{"wrapped driver error", fmt.Errorf("failed to query collection %q: %w", "notes", errors.New("disk exploded")), http.StatusInternalServerError, true},
		{"invalid key is a client error", fmt.Errorf("%w: segment", core.ErrInvalidKey), http.StatusBadRequest, false},
		{"not found is a client error", core.ErrNotFound, http.StatusNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := New("test", nil, WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
			r := httptest.NewRequest(http.MethodPut, "/v1/databases/dev/records/notes/n1?access_token=ovdb_secret", strings.NewReader(`{"data":{"ssn":"123-45-6789"}}`))
			r.Header.Set("Authorization", "Bearer ovdb_secret")
			w := httptest.NewRecorder()
			s.writeMappedError(w, r, tc.err)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
			out := logs.String()
			if !tc.logged {
				if out != "" {
					t.Fatalf("client error was logged: %s", out)
				}
				return
			}
			for _, want := range []string{`"level":"ERROR"`, `"method":"PUT"`, `"path":"/v1/databases/dev/records/notes/n1"`, "disk exploded"} {
				if !strings.Contains(out, want) {
					t.Errorf("log %s missing %s", out, want)
				}
			}
			for _, secret := range []string{"ovdb_secret", "123-45-6789", "ssn"} {
				if strings.Contains(out, secret) {
					t.Errorf("log leaks %q: %s", secret, out)
				}
			}
			if strings.Contains(w.Body.String(), "disk exploded") {
				t.Errorf("response leaks internal error: %s", w.Body.String())
			}
		})
	}
}

func TestWithLoggerNilKeepsDefault(t *testing.T) {
	if s := New("test", nil, WithLogger(nil)); s.logger == nil {
		t.Fatal("nil WithLogger must keep the default logger")
	}
}
