package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

// jsonRowStream commits only when the first row arrives, so failures before a
// result can still use the endpoint's ordinary mapped HTTP error response.
type jsonRowStream struct {
	w               http.ResponseWriter
	started         bool
	rows            int
	bytes           int
	maxBytes        int
	contentType     string
	errorCompletion bool
	transportErr    error
}

const (
	queryErrorStreamMediaType = "application/vnd.openvaultdb.query-stream+json"
	maxStreamFooterBytes      = 512 << 10
	maxStreamErrorBytes       = 16 << 10
)

func newJSONRowStream(w http.ResponseWriter, timeout time.Duration, errorCompletion bool) (*jsonRowStream, func(), error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, nil, err
	}
	contentType := "application/json"
	if errorCompletion {
		contentType = queryErrorStreamMediaType
	}
	return &jsonRowStream{w: w, contentType: contentType, errorCompletion: errorCompletion}, func() { _ = controller.SetWriteDeadline(time.Time{}) }, nil
}

func (s *jsonRowStream) write(p []byte) error {
	n, err := s.w.Write(p)
	s.bytes += n
	if err != nil {
		s.transportErr = err
		return err
	}
	if n != len(p) {
		s.transportErr = io.ErrShortWrite
		return s.transportErr
	}
	return nil
}

func (s *jsonRowStream) WriteRow(row any) error {
	encoded, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if !s.started {
		s.w.Header().Set("Content-Type", s.contentType)
		s.w.Header().Add("Vary", "Accept")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
		if err := s.write([]byte(`{"records":[`)); err != nil {
			return err
		}
	} else if err := s.write([]byte(",")); err != nil {
		return err
	}
	if err := s.write(encoded); err != nil {
		return err
	}
	s.rows++
	return s.flush()
}

// Finish appends the bounded metadata footer and marks a successful stream.
// It starts an empty result here when the source returned no rows.
func (s *jsonRowStream) Finish(footer map[string]any) error {
	return s.finish(footer, true)
}

// FinishFailure appends an error terminal to an opted-in stream. Success-only
// metadata is deliberately omitted by the caller; an error terminal is never
// accepted on the default application/json protocol.
func (s *jsonRowStream) FinishFailure(detail map[string]any) error {
	if !s.errorCompletion || !s.started || detail == nil {
		return errors.New("late error completion is not enabled")
	}
	encodedDetail, err := json.Marshal(detail)
	if err != nil || len(encodedDetail) > maxStreamErrorBytes {
		return errors.New("late error detail cannot be safely encoded")
	}
	return s.finish(map[string]any{"error": detail}, false)
}

func (s *jsonRowStream) finish(footer map[string]any, complete bool) error {
	if footer == nil {
		footer = map[string]any{}
	}
	footer["complete"] = complete
	encoded, err := json.Marshal(footer)
	if err != nil {
		return err
	}
	if len(encoded) > maxStreamFooterBytes {
		return errors.New("query response footer exceeds its byte limit")
	}
	encoded = bytes.TrimSpace(encoded)
	if len(encoded) < 2 {
		return errors.New("invalid JSON stream footer")
	}
	prefixBytes := 0
	if !s.started {
		prefixBytes = len([]byte(`{"records":[`))
	}
	if s.maxBytes > 0 && s.bytes+prefixBytes+1+1+(len(encoded)-2)+2 > s.maxBytes {
		return core.ErrResultTooLarge
	}
	if !s.started {
		s.w.Header().Set("Content-Type", s.contentType)
		s.w.Header().Add("Vary", "Accept")
		s.w.WriteHeader(http.StatusOK)
		s.started = true
		if err := s.write([]byte(`{"records":[`)); err != nil {
			return err
		}
	}
	if err = s.write([]byte("]")); err != nil {
		return err
	}
	if err = s.write([]byte(",")); err != nil {
		return err
	}
	if err = s.write(encoded[1 : len(encoded)-1]); err != nil {
		return err
	}
	if err = s.write([]byte("}\n")); err != nil {
		return err
	}
	return s.flush()
}

func (s *jsonRowStream) flush() error {
	err := flushJSONStream(s.w)
	if err != nil {
		s.transportErr = err
	}
	return err
}

func wantsQueryErrorStream(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept") {
		for _, item := range strings.Split(value, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil || !strings.EqualFold(mediaType, queryErrorStreamMediaType) {
				continue
			}
			if q, ok := params["q"]; ok {
				quality, parseErr := strconv.ParseFloat(q, 64)
				if parseErr != nil || math.IsNaN(quality) || math.IsInf(quality, 0) || quality <= 0 || quality > 1 {
					continue
				}
			}
			return true
		}
	}
	return false
}

func (s *Server) finishStreamErrorOrAbort(w http.ResponseWriter, r *http.Request, stream *jsonRowStream, writeMapped func(http.ResponseWriter)) {
	if stream.transportErr == nil && stream.errorCompletion {
		recorder := &boundedErrorResponseWriter{header: make(http.Header)}
		writeMapped(recorder)
		var body map[string]any
		if recorder.status >= 400 && json.Unmarshal(recorder.body, &body) == nil {
			if detail, ok := body["error"].(map[string]any); ok {
				encoded, err := json.Marshal(detail)
				if err == nil && len(encoded) <= maxStreamErrorBytes {
					if err = stream.FinishFailure(detail); err == nil {
						return
					}
				}
			}
		}
	}
	s.abortJSONStream(w, r)
}

type boundedErrorResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *boundedErrorResponseWriter) Header() http.Header { return w.header }
func (w *boundedErrorResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *boundedErrorResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if len(p) > maxStreamErrorBytes-len(w.body) {
		return 0, errors.New("mapped stream error exceeds byte limit")
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

func flushJSONStream(w http.ResponseWriter) error {
	err := http.NewResponseController(w).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// abortJSONStream terminates a committed response without a parseable success
// object. net/http treats ErrAbortHandler as a connection abort and avoids its
// default panic logging, so clients cannot mistake partial rows for success.
func (s *Server) abortJSONStream(w http.ResponseWriter, r *http.Request) {
	s.logger.WarnContext(r.Context(), "query response stream aborted",
		slog.String("method", r.Method), slog.String("path", r.URL.Path))
	panic(http.ErrAbortHandler)
}
