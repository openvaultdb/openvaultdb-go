package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"gopkg.in/yaml.v3"
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

func continuationDocument(data []byte, dtql bool) (bool, error) {
	if dtql {
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		var document yaml.Node
		if err := decoder.Decode(&document); err != nil {
			return false, err
		}
		found := continuationYAML(&document)
		if err := decoder.Decode(new(yaml.Node)); err != io.EOF {
			return found, errors.New("query must contain one YAML document")
		}
		return found, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	found, count := false, 0
	for {
		var fields map[string]json.RawMessage
		err := decoder.Decode(&fields)
		if err == io.EOF && count == 1 {
			return found, nil
		}
		if err != nil {
			return found, errors.New("query must contain one complete JSON object")
		}
		count++
		for name := range fields {
			found = found || continuationName(name)
		}
		if count > 1 {
			return found, errors.New("query must contain one JSON object")
		}
	}
}

func continuationYAML(document *yaml.Node) bool {
	if document.Kind == yaml.DocumentNode && len(document.Content) == 1 {
		document = document.Content[0]
	}
	if document.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(document.Content); i += 2 {
		if continuationName(document.Content[i].Value) {
			return true
		}
		// The JSON DTQL envelope carries its YAML document in query. Do not
		// traverse parameter values or arbitrary user data as protocol fields.
		if document.Content[i].Value == "query" && document.Content[i+1].Kind == yaml.ScalarNode {
			var nested yaml.Node
			if yaml.Unmarshal([]byte(document.Content[i+1].Value), &nested) == nil && continuationYAMLRoot(&nested) {
				return true
			}
		}
	}
	return false
}

func continuationYAMLRoot(document *yaml.Node) bool {
	if document.Kind == yaml.DocumentNode && len(document.Content) == 1 {
		document = document.Content[0]
	}
	if document.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(document.Content); i += 2 {
			if continuationName(document.Content[i].Value) {
				return true
			}
		}
	}
	return false
}

// withoutContinuationFields is only for metadata classification on the
// cross-database route. The original request remains available to the guard;
// this document must never execute until every source has been checked.
func withoutContinuationFields(data []byte, jsonEnvelope bool) []byte {
	if jsonEnvelope {
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return data
		}
		for name := range fields {
			if continuationName(name) {
				delete(fields, name)
			}
		}
		out, err := json.Marshal(fields)
		if err == nil {
			return out
		}
		return data
	}
	var document yaml.Node
	if yaml.Unmarshal(data, &document) != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return data
	}
	root := document.Content[0]
	fields := make([]*yaml.Node, 0, len(root.Content))
	for i := 0; i+1 < len(root.Content); i += 2 {
		if !continuationName(root.Content[i].Value) {
			fields = append(fields, root.Content[i], root.Content[i+1])
		}
	}
	root.Content = fields
	out, err := yaml.Marshal(&document)
	if err != nil {
		return data
	}
	return out
}

// guardRetentionRead runs before any adapter read, query admission or snapshot
// lookup. URL/JSON/YAML continuation fields are refused even on routes that otherwise
// ignore them. Request bodies are bounded and restored for the ordinary decoder.
func (s *Server) guardRetentionRead(w http.ResponseWriter, r *http.Request, db *core.Database) bool {
	if !db.NoRetention() {
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	forbidden := hasPagingHeaders(r)
	values, queryErr := url.ParseQuery(r.URL.RawQuery)
	// ParseQuery reports bad escapes while returning valid fields. Inspect
	// names separately so an invalid continuation value cannot erase presence.
	for _, field := range strings.Split(r.URL.RawQuery, "&") {
		name, _, _ := strings.Cut(field, "=")
		name, err := url.QueryUnescape(name)
		forbidden = forbidden || (err == nil && continuationName(name))
	}
	dtql := strings.HasSuffix(r.URL.Path, "/dtql")
	for name, values := range values {
		forbidden = forbidden || continuationName(name)
		if name == "q" {
			for _, value := range values {
				if len(value) > maxQueryRequestBytes {
					writeError(w, http.StatusBadRequest, "bad_request", "query parameter exceeds the bounded read limit")
					return false
				}
				found, err := continuationDocument([]byte(value), dtql)
				forbidden = forbidden || found
				if queryErr == nil {
					queryErr = err
				}
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
		found, err := continuationDocument(data, dtql)
		forbidden = forbidden || found
		if queryErr == nil {
			queryErr = err
		}
	}
	if forbidden {
		return !refuseRetainedOperation(w, db)
	}
	if queryErr != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "query request must contain one valid document and valid URL escapes")
		return false
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
