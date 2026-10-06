package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	az "github.com/dal-go/dalgo/dtql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"gopkg.in/yaml.v3"
)

// crossDatabaseDTQLPath is the endpoint that takes documents which read several
// databases. The per-database endpoint takes the others.
const crossDatabaseDTQLPath = "/v1/dtql"

// handleDTQL authenticates a bounded DTQL query and executes it through the
// mounted database's secured DALgo handle. A document is classified once, after
// the two qualifiers of its root source that name what the endpoint supplies are
// dropped (the default schema of the engine and the endpoint's own database): a
// document of one source with no other relational feature takes the
// single-collection path, whose records carry keys, and every other document the
// classifier accepts is answered by serveRelationalDTQL, a document whose only
// relational feature is a subquery included, so one query has one answer. The
// classifier's own refusals apply to both.
func (s *Server) handleDTQL(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	db := s.db(w, r)
	if db == nil {
		return
	}
	doc, ok := s.readDTQLDocument(w, r)
	if !ok {
		return
	}
	doc = withoutOwnDatabase(withoutDefaultSchema(doc, db.Engine()), db.ID())
	if s.boundedImmutable(db) {
		doc = withoutOwnDatabase(doc, r.PathValue("db"))
	}
	query, profile, err := classifyDTQLDocument(doc)
	if err != nil {
		s.writeMappedError(w, r, clippedError{err})
		return
	}
	if profile.Kind == core.ProfileRelational {
		s.serveRelationalDTQL(w, r, db, query, profile)
		return
	}
	// A document of this kind reads one plain collection and no other (no join, no
	// subquery), so the capability on that collection is the whole check: the
	// listing of core.QueryCollections is that collection and nothing else.
	collection := profile.Sources[0].Collection
	if !s.authorize(w, r, db.ID(), auth.CapRecordsRead, collection) {
		return
	}
	query, ok = s.prepareImmutableQuery(w, r, db, query, collection)
	if !ok {
		return
	}
	if hasPagingHeaders(r) {
		s.handlePagedDTQL(w, r, db, query, doc)
		return
	}
	capture, rightsErr := s.singleRights(db, collection)
	records, err := db.ExecuteDTQLQuery(r.Context(), query)
	if err != nil {
		err = hiddenAsDenied(db, err)
		if errors.Is(err, access.ErrAccessDenied) {
			op := api.Operation{ID: "q1", Action: "query", Resource: az.Resource{DatabaseID: db.ID(), Path: "/" + collection, Table: collection}, ExecutionClass: az.ExecutionDTQL, Query: &api.Query{Format: "dtql-yaml", Text: string(doc)}}
			s.writeQueryAuthorizationError(w, r, db, op, err)
			return
		}
		s.writeMappedError(w, r, err)
		return
	}
	type recordOut struct {
		Key  string         `json:"key"`
		Data map[string]any `json:"data,omitempty"`
	}
	out := make([]recordOut, 0, len(records))
	for _, rec := range records {
		out = append(out, recordOut{Key: rec.Key.String(), Data: rec.Data})
	}
	if rightsErr != nil {
		s.rightsError(w, rightsErr)
		return
	}
	s.cacheReadResponse(w, r, db)
	s.writeRightsResult(w, r, map[string]any{"records": out}, capture, capture.allUsed(), core.ResultBufferBytes)
}

// readDTQLDocument returns the DTQL document of a request: the q parameter of a
// GET, or the body of a POST (with its parameters bound when the body is JSON).
// It answers the 4xx of a request that carries none and reports false.
func (s *Server) readDTQLDocument(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	return s.readDTQLDocumentForClassification(w, r, false)
}

func (s *Server) readDTQLDocumentForClassification(w http.ResponseWriter, r *http.Request, classifySources bool) ([]byte, bool) {
	if r.Method == http.MethodGet {
		doc, err := dtqlFromURL(r.URL)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errDTQLURLTooLong) {
				status = http.StatusRequestURITooLong
			}
			writeError(w, status, "bad_request", clipText(err.Error(), maxRefusalText))
			return nil, false
		}
		return doc, true
	}
	doc, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read body: "+err.Error())
		return nil, false
	}
	if len(doc) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "body must contain a DTQL YAML document")
		return nil, false
	}
	// Relational source classification reads metadata first, then applies each
	// source's retention guard to the complete original request.
	r.Body = io.NopCloser(bytes.NewReader(doc))
	if strings.EqualFold(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]), "application/json") {
		if classifySources {
			doc = withoutContinuationFields(doc, true)
		}
		doc, err = bindDTQLParameters(doc)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_dtql", clipText(err.Error(), maxRefusalText))
			return nil, false
		}
	}
	return doc, true
}

// maxRefusalText is the most bytes of the text of a refused document an error body
// repeats. It is long enough to keep the whole message of any realistic name, and
// short enough that a name as long as the request body does not come back whole.
const maxRefusalText = 1024

// clippedError is err with the text of its message cut at maxRefusalText. It keeps
// the chain, so the status and code of the error are the ones err would have.
type clippedError struct{ err error }

func (e clippedError) Error() string { return clipText(e.err.Error(), maxRefusalText) }
func (e clippedError) Unwrap() error { return e.err }

// classifyDTQLDocument deserialises a document and classifies it. The error of
// a document that is not DTQL, or that the classifier refuses, wraps
// core.ErrInvalidDTQL.
func classifyDTQLDocument(doc []byte) (dal.StructuredQuery, core.Profile, error) {
	query, err := core.DeserializeDTQL(doc)
	if err != nil {
		return nil, core.Profile{}, err
	}
	profile, err := core.ClassifyDTQL(query)
	if err != nil {
		return nil, core.Profile{}, err
	}
	return query, profile, nil
}

var errDTQLURLTooLong = errors.New("DTQL URL exceeds 8 KiB limit")

// dtqlFromURL accepts a single YAML q value and an optional JSON parameters
// object. The same binder and validator handle POST and GET.
func dtqlFromURL(u *url.URL) ([]byte, error) {
	if len(u.RequestURI()) > 8<<10 {
		return nil, errDTQLURLTooLong
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid DTQL URL query: %w", err)
	}
	for name := range values {
		if name != "q" && name != "parameters" {
			return nil, fmt.Errorf("unsupported DTQL URL parameter %q", name)
		}
	}
	query := values["q"]
	if len(query) != 1 || strings.TrimSpace(query[0]) == "" {
		return nil, errors.New("DTQL URL requires exactly one nonempty q parameter")
	}
	parameters := json.RawMessage(`{}`)
	if raw, ok := values["parameters"]; ok {
		if len(raw) != 1 || raw[0] == "" {
			return nil, errors.New("DTQL URL requires one JSON parameters object")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw[0]), &object); err != nil || object == nil {
			return nil, errors.New("DTQL URL parameters must be a JSON object")
		}
		parameters = json.RawMessage(raw[0])
	}
	body, err := json.Marshal(struct {
		Query      string          `json:"query"`
		Parameters json.RawMessage `json:"parameters"`
	}{Query: query[0], Parameters: parameters})
	if err != nil {
		return nil, fmt.Errorf("invalid DTQL URL parameters: %w", err)
	}
	return bindDTQLParameters(body)
}

// bindDTQLParameters replaces parsed DTQL parameter nodes with JSON values.
// Values never enter YAML or SQL as text, so a string cannot change query shape.
func bindDTQLParameters(body []byte) ([]byte, error) {
	var payload struct {
		Query      string                     `json:"query"`
		Parameters map[string]json.RawMessage `json:"parameters"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("invalid DTQL JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("DTQL JSON must contain one object")
	}
	if strings.TrimSpace(payload.Query) == "" {
		return nil, errors.New("query must contain DTQL YAML")
	}
	if len(payload.Parameters) > 64 {
		return nil, errors.New("too many DTQL parameters")
	}
	var query yaml.Node
	if err := yaml.Unmarshal([]byte(payload.Query), &query); err != nil {
		return nil, fmt.Errorf("invalid DTQL YAML: %w", err)
	}
	used := make(map[string]bool, len(payload.Parameters))
	var visit func(*yaml.Node) error
	visit = func(node *yaml.Node) error {
		if node.Kind == yaml.MappingNode && len(node.Content) == 2 && node.Content[0].Value == "param" {
			name := node.Content[1].Value
			raw, ok := payload.Parameters[name]
			if !ok {
				return fmt.Errorf("parameter %q is not bound", name)
			}
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return fmt.Errorf("invalid parameter %q: %w", name, err)
			}
			if value == nil {
				return fmt.Errorf("parameter %q cannot be null", name)
			}
			key := "value"
			if values, ok := value.([]any); ok {
				if len(values) > 1000 {
					return fmt.Errorf("parameter %q has too many values", name)
				}
				for _, item := range values {
					if item == nil || !isDTQLScalar(item) {
						return fmt.Errorf("parameter %q must contain only non-null scalars", name)
					}
				}
				key = "values"
			} else if !isDTQLScalar(value) {
				return fmt.Errorf("parameter %q must be a scalar or array", name)
			}
			var bound yaml.Node
			if err := bound.Encode(value); err != nil {
				return err
			}
			node.Content[0].Value = key
			node.Content[1] = &bound
			used[name] = true
			return nil
		}
		for _, child := range node.Content {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(&query); err != nil {
		return nil, err
	}
	for name := range payload.Parameters {
		if !used[name] {
			return nil, fmt.Errorf("parameter %q is not used", name)
		}
	}
	return yaml.Marshal(&query)
}

func isDTQLScalar(value any) bool {
	switch value.(type) {
	case string, bool, float64:
		return true
	default:
		return false
	}
}
