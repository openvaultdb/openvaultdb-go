package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJSONRowStreamFinishesEmptyResult(t *testing.T) {
	recorder := httptest.NewRecorder()
	stream := &jsonRowStream{w: recorder}
	if err := stream.Finish(map[string]any{}); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["complete"] != true || len(body["records"].([]any)) != 0 {
		t.Fatalf("response = %v", body)
	}
}

type brokenStreamWriter struct {
	header http.Header
	writes int
	body   bytes.Buffer
}

func (w *brokenStreamWriter) Header() http.Header { return w.header }
func (w *brokenStreamWriter) WriteHeader(int)     {}
func (w *brokenStreamWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 2 {
		_, _ = w.body.Write(p[:1])
		return 1, io.ErrClosedPipe
	}
	return w.body.Write(p)
}

func TestJSONRowStreamDoesNotAppendErrorFooterAfterTransportFailure(t *testing.T) {
	writer := &brokenStreamWriter{header: make(http.Header)}
	stream := &jsonRowStream{w: writer, contentType: queryErrorStreamMediaType, errorCompletion: true}
	if err := stream.WriteRow(map[string]any{"data": map[string]any{"id": 1}}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("WriteRow error = %v", err)
	}
	service := New("test", nil)
	defer service.CloseSnapshots()
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", recovered)
		}
		if bytes.Contains(writer.body.Bytes(), []byte(`"complete"`)) || writer.writes != 2 {
			t.Fatalf("stream tried to repair a failed write: writes=%d body=%q", writer.writes, writer.body.String())
		}
	}()
	service.finishStreamErrorOrAbort(writer, httptest.NewRequest(http.MethodPost, "/v1/dtql", nil), stream, func(w http.ResponseWriter) {
		writeError(w, http.StatusInternalServerError, "internal", "internal server error")
	})
}
