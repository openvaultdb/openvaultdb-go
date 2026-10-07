package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const jsonAckPrivateMarker = "invented-private-payload-marker"

type jsonAckWriter struct {
	header          http.Header
	fail, partial   bool
	writes, headers int
	sawPayload      bool
}

func (w *jsonAckWriter) Header() http.Header { return w.header }
func (w *jsonAckWriter) WriteHeader(int)     { w.headers++ }
func (w *jsonAckWriter) Write(data []byte) (int, error) {
	w.writes++
	w.sawPayload = w.sawPayload || bytes.Contains(data, []byte(jsonAckPrivateMarker))
	if w.fail {
		if w.partial {
			return len(data) / 2, errors.New(jsonAckPrivateMarker)
		}
		return 0, errors.New(jsonAckPrivateMarker)
	}
	return len(data), nil
}

func jsonAckAssert(t *testing.T, logs []byte, want int) {
	t.Helper()
	if len(logs) > 512 || bytes.Contains(logs, []byte(jsonAckPrivateMarker)) {
		t.Fatal("acknowledgement leaked private input or exceeded bound")
	}
	lines := bytes.Split(bytes.TrimSpace(logs), []byte("\n"))
	if len(logs) == 0 {
		lines = nil
	}
	if len(lines) != want {
		t.Fatalf("producer acknowledgement count=%d want=%d", len(lines), want)
	}
	for _, line := range lines {
		var fields map[string]any
		if json.Unmarshal(line, &fields) != nil || len(fields) != 3 || fields["level"] != "ERROR" || fields["msg"] != "JSON response write failed" || fields["time"] == nil {
			t.Fatal("acknowledgement is not the fixed source-free event")
		}
	}
}

func TestJSONResponseWriterFailureAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fail, partial bool
		want          int
	}{
		{"successful response", false, false, 0},
		{"zero-byte writer error", true, false, 1},
		{"partial writer error", true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := New(jsonAckPrivateMarker, nil, WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
			t.Cleanup(s.CloseSnapshots)
			w := &jsonAckWriter{header: http.Header{}, fail: tc.fail, partial: tc.partial}
			r := httptest.NewRequest("GET", "http://"+jsonAckPrivateMarker+".invalid/v1/status?secret="+jsonAckPrivateMarker, nil)
			r.Header.Set("Authorization", jsonAckPrivateMarker)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			r = r.WithContext(ctx)
			s.Handler().ServeHTTP(w, r)
			if w.writes != 1 || w.headers != 1 || !w.sawPayload {
				t.Fatal("writer positive control failed or response was retried")
			}
			jsonAckAssert(t, logs.Bytes(), tc.want)
			if r.Context().Err() != nil {
				t.Fatal("caller-owned context unexpectedly cancelled")
			}
		})
	}
}

func TestJSONConfigurationRefusalWriteAcknowledgement(t *testing.T) {
	for _, field := range []string{"provider", "retention", "rights", "read profile"} {
		t.Run(field, func(t *testing.T) {
			var logs bytes.Buffer
			s := New("synthetic", nil, WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
			t.Cleanup(s.CloseSnapshots)
			err := errors.New(jsonAckPrivateMarker)
			switch field {
			case "provider":
				s.providerProfileErr = err
			case "retention":
				s.retentionErr = err
			case "rights":
				s.rightsErr = err
			case "read profile":
				s.readProfileErr = err
			}
			w := &jsonAckWriter{header: http.Header{}, fail: true}
			s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://synthetic.invalid/v1/status", nil))
			if w.writes != 1 || w.headers != 1 {
				t.Fatal("configuration refusal did not attempt one response")
			}
			jsonAckAssert(t, logs.Bytes(), 1)
		})
	}
}

// A log receives only the fixed event even when JSON's own marshaler supplies
// arbitrary error text. The payload never leaves transient test RAM.
type jsonAckMarshalFailure struct{}

func (jsonAckMarshalFailure) MarshalJSON() ([]byte, error) {
	return nil, errors.New(jsonAckPrivateMarker)
}

func TestJSONMarshalFailureAcknowledgement(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	w := &jsonAckWriter{header: http.Header{}}
	writeJSON(w, http.StatusOK, jsonAckMarshalFailure{})
	if w.writes != 0 || w.headers != 1 {
		t.Fatal("encoder failure wrote or retried payload")
	}
	jsonAckAssert(t, logs.Bytes(), 1)
	if strings.Contains(logs.String(), "MarshalJSON") {
		t.Fatal("arbitrary marshaler error logged")
	}
}

type jsonAckControllerWriter struct {
	jsonAckWriter
	flushed, duplex, hijacked   bool
	readDeadline, writeDeadline time.Time
}

func (w *jsonAckControllerWriter) FlushError() error       { w.flushed = true; return nil }
func (w *jsonAckControllerWriter) EnableFullDuplex() error { w.duplex = true; return nil }
func (w *jsonAckControllerWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, http.ErrNotSupported
}
func (w *jsonAckControllerWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadline = deadline
	return nil
}
func (w *jsonAckControllerWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return nil
}

func TestJSONResponseControllerForwarding(t *testing.T) {
	w := &jsonAckControllerWriter{jsonAckWriter: jsonAckWriter{header: http.Header{}}}
	wrapped := &jsonResponseWriter{ResponseWriter: w, logger: slog.Default(), ctx: context.Background()}
	c := http.NewResponseController(wrapped)
	deadline := time.Unix(123456, 0)
	if c.Flush() != nil || c.EnableFullDuplex() != nil || c.SetReadDeadline(deadline) != nil || c.SetWriteDeadline(deadline) != nil {
		t.Fatal("ResponseController forwarding failed")
	}
	if _, _, err := c.Hijack(); !errors.Is(err, http.ErrNotSupported) {
		t.Fatal("ResponseController hijack error not preserved")
	}
	if !w.flushed || !w.duplex || !w.hijacked || !w.readDeadline.Equal(deadline) || !w.writeDeadline.Equal(deadline) {
		t.Fatal("underlying controller not reached")
	}
}

func TestJSONZeroValueServerDefaultAcknowledgement(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	w := &jsonAckWriter{header: http.Header{}, fail: true}
	(&Server{}).Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://synthetic.invalid/v1/status", nil))
	jsonAckAssert(t, logs.Bytes(), 1)
}

func TestJSONMarshalFailureWithConfiguredReporter(t *testing.T) {
	var logs bytes.Buffer
	w := &jsonAckWriter{header: http.Header{}}
	writeJSON(&jsonResponseWriter{ResponseWriter: w, logger: slog.New(slog.NewJSONHandler(&logs, nil)), ctx: context.Background()}, http.StatusOK, jsonAckMarshalFailure{})
	jsonAckAssert(t, logs.Bytes(), 1)
	if w.writes != 0 || w.headers != 1 {
		t.Fatal("encoder failure wrote or retried payload")
	}
}
