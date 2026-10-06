package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

var errRetentionNotAuthorized = errors.New("retained result copies are not authorized for this source")

func headerPresent(r *http.Request, header string) bool {
	for name := range r.Header {
		if strings.EqualFold(name, header) {
			return true
		}
	}
	return false
}

func hasPagingHeaders(r *http.Request) bool {
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "ovdb-page-") {
			return true
		}
	}
	return false
}

// Never collapse duplicate or empty protocol fields with Header.Get.
func validPagingHeaders(w http.ResponseWriter, r *http.Request) bool {
	for name, values := range r.Header {
		if !strings.HasPrefix(strings.ToLower(name), "ovdb-page-") {
			continue
		}
		known := false
		for _, header := range pagingHeaders {
			known = known || strings.EqualFold(name, header)
		}
		count := 0
		for other, entries := range r.Header {
			if strings.EqualFold(name, other) {
				count += len(entries)
			}
		}
		if !known || count != 1 || len(values) != 1 || strings.TrimSpace(values[0]) == "" || strings.Contains(values[0], ",") {
			writeError(w, http.StatusBadRequest, "bad_request", "paging headers must be known, nonempty and supplied once")
			return false
		}
	}
	return true
}

func continuationName(name string) bool {
	switch strings.ToLower(name) {
	case "snapshottoken", "pagetoken", "nextpagetoken", "continuation", "continuationtoken":
		return true
	}
	return false
}

func continuationDocument(data []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	for name := range fields {
		if continuationName(name) {
			return true
		}
	}
	return false
}

// guardRetentionRead runs before any adapter read, query admission or snapshot
// lookup. URL/JSON continuation fields are refused even on routes that otherwise
// ignore them. Request bodies are bounded and restored for the ordinary decoder.
func (s *Server) guardRetentionRead(w http.ResponseWriter, r *http.Request, db *core.Database) bool {
	if !db.NoRetention() {
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	forbidden := hasPagingHeaders(r)
	for name, values := range r.URL.Query() {
		forbidden = forbidden || continuationName(name)
		if name == "q" {
			for _, value := range values {
				forbidden = forbidden || continuationDocument([]byte(value))
			}
		}
	}
	if !forbidden && r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/query") || strings.HasSuffix(r.URL.Path, "/dtql")) && r.Body != nil {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
		if err != nil {
			writeError(w, 400, "bad_request", "query request exceeds the bounded read limit")
			return false
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		forbidden = continuationDocument(data)
	}
	if forbidden {
		return !refuseRetainedOperation(w, db)
	}
	return true
}

func refuseRetainedOperation(w http.ResponseWriter, db *core.Database) bool {
	if !db.NoRetention() {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	writeError(w, http.StatusUnprocessableEntity, "retention_not_authorized", errRetentionNotAuthorized.Error())
	return true
}
