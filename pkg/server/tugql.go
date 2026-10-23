package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	az "github.com/dal-go/dalgo/tugql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/auth"
	api "github.com/openvaultdb/openvaultdb-go/pkg/authorizationapi"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"gopkg.in/yaml.v3"
)

// crossDatabaseTugQLPath is the endpoint that takes documents which read several
// databases. The per-database endpoint takes the others.
const crossDatabaseTugQLPath = "/v1/tugql"

var streamTugQLQuery = func(db *core.Database, ctx context.Context, query dal.StructuredQuery, emit func(core.Record) error) error {
	return db.StreamTugQLQuery(ctx, query, emit)
}

// handleTugQL authenticates a bounded TugQL query and executes it through the
// mounted database's secured DALgo handle. A document is classified once, after
// the two qualifiers of its root source that name what the endpoint supplies are
// dropped (the default schema of the engine and the endpoint's own database): a
// document of one source with no other relational feature takes the
// single-collection path, whose records carry keys, and every other document the
// classifier accepts is answered by serveRelationalTugQL, a document whose only
// relational feature is a subquery included, so one query has one answer. The
// classifier's own refusals apply to both.
func (s *Server) handleTugQL(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	db := s.db(w, r)
	if db == nil {
		return
	}
	doc, ok := s.readTugQLDocument(w, r)
	if !ok {
		return
	}
	if s.providerRequestRestricted(db) && !validProviderTugQLWire(doc) {
		refuseProviderRequest(w, s.providerProfilesByDB[db].RequestProfile)
		return
	}
	doc = withoutOwnDatabase(withoutDefaultSchema(doc, db.Engine()), db.ID())
	if db.NativePostgresReadOnly() {
		doc = exactIntegerValues(doc)
	} else if hasUnsafeIntegerValue(doc) {
		writeError(w, http.StatusBadRequest, "invalid_tugql", errUnsafeIntegerValue)
		return
	}
	if s.boundedImmutable(db) {
		doc = withoutOwnDatabase(doc, r.PathValue("db"))
	}
	query, profile, err := classifyTugQLDocument(doc)
	if err != nil {
		s.writeMappedError(w, r, clippedError{err})
		return
	}
	if s.providerRequestRestricted(db) && !validProviderTugQL(db, query, profile, s.providerProfilesByDB[db].RequestProfile) {
		refuseProviderRequest(w, s.providerProfilesByDB[db].RequestProfile)
		return
	}
	if profile.Kind == core.ProfileRelational {
		s.serveRelationalTugQL(w, r, db, query, profile)
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
		s.handlePagedTugQL(w, r, db, query, doc)
		return
	}
	capture, rightsErr := s.singleRights(db, collection)
	if rightsErr != nil && (db.ReadOnlyHTTP() || !db.HasAccessPolicies()) {
		s.rightsError(w, rightsErr)
		return
	}
	ctx, providerCapture, providerErr := s.beginProviderRead(r, db, collection, capture)
	if providerErr != nil {
		s.providerReadError(w, providerErr)
		return
	}
	stream, clearWriteDeadline, streamErr := newJSONRowStream(w, s.queryLimits.Timeout, wantsQueryErrorStream(r))
	if streamErr != nil {
		s.writeInternalError(w, r, "could not start streamed response", streamErr)
		return
	}
	defer clearWriteDeadline()
	if len(capture.rights) > 0 {
		stream.maxBytes = core.ResultBufferBytes
	}
	cacheSet := false
	writeRecord := func(rec core.Record) error {
		if !cacheSet {
			if stream.errorCompletion {
				w.Header().Set("Cache-Control", "no-store")
			} else {
				s.cacheReadResponse(w, r, db)
			}
			cacheSet = true
		}
		data := rec.Data
		if db.NativePostgresReadOnly() {
			collectionSchema := db.Manifest.Schemas.Collection(collection)
			data = nativePostgresJSONValues(data, nativePostgresFieldTypes(collectionSchema))
		}
		return stream.WriteRow(struct {
			Key  string         `json:"key"`
			Data map[string]any `json:"data,omitempty"`
		}{Key: rec.Key.String(), Data: data})
	}
	err = streamTugQLQuery(db, ctx, query, writeRecord)
	if err != nil {
		err = hiddenAsDenied(db, err)
		if errors.Is(err, access.ErrAccessDenied) {
			if stream.started {
				op := api.Operation{ID: "q1", Action: "query", Resource: az.Resource{DatabaseID: db.ID(), Path: "/" + collection, Table: collection}, ExecutionClass: executionTugQL, Query: &api.Query{Format: "tugql-yaml", Text: string(doc)}}
				s.finishStreamErrorOrAbort(w, r, stream, func(w http.ResponseWriter) { s.writeQueryAuthorizationError(w, r, db, op, err) })
				return
			}
			clearWriteDeadline()
			op := api.Operation{ID: "q1", Action: "query", Resource: az.Resource{DatabaseID: db.ID(), Path: "/" + collection, Table: collection}, ExecutionClass: executionTugQL, Query: &api.Query{Format: "tugql-yaml", Text: string(doc)}}
			s.writeQueryAuthorizationError(w, r, db, op, err)
			return
		}
		if stream.started {
			s.finishStreamErrorOrAbort(w, r, stream, func(w http.ResponseWriter) { s.writeStreamMappedError(w, r, err) })
			return
		}
		clearWriteDeadline()
		s.writeMappedError(w, r, err)
		return
	}
	if rightsErr != nil {
		if stream.started {
			s.finishStreamErrorOrAbort(w, r, stream, func(w http.ResponseWriter) { s.rightsError(w, rightsErr) })
			return
		}
		clearWriteDeadline()
		s.rightsError(w, rightsErr)
		return
	}
	if !cacheSet {
		if stream.errorCompletion {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			s.cacheReadResponse(w, r, db)
		}
	}
	footer := map[string]any{}
	evidence, providerErr := finishProviderCapture(providerCapture, capture.allUsed())
	if providerErr != nil {
		if stream.started {
			s.finishStreamErrorOrAbort(w, r, stream, func(w http.ResponseWriter) { s.rightsError(w, providerErr) })
			return
		}
		clearWriteDeadline()
		s.rightsError(w, providerErr)
		return
	}
	if evidence != nil {
		footer["providerReads"] = evidence
	}
	attachRights(footer, capture.rights, capture.allUsed())
	if err := finishTugQLStream(stream, footer); err != nil {
		if stream.started {
			s.finishStreamErrorOrAbort(w, r, stream, func(w http.ResponseWriter) { s.writeStreamMappedError(w, r, err) })
			return
		}
		clearWriteDeadline()
		s.writeMappedError(w, r, err)
	}
}

var (
	finishTugQLStream    = func(s *jsonRowStream, footer map[string]any) error { return s.Finish(footer) }
	marshalTugQLURLBody  = json.Marshal
	encodeTugQLParamNode = func(node *yaml.Node, value any) error { return node.Encode(value) }
)

// readTugQLDocument returns the TugQL document of a request: the q parameter of a
// GET, or the body of a POST (with its parameters bound when the body is JSON).
// It answers the 4xx of a request that carries none and reports false.
func (s *Server) readTugQLDocument(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	return s.readTugQLDocumentForClassification(w, r, false)
}

func (s *Server) readTugQLDocumentForClassification(w http.ResponseWriter, r *http.Request, classifySources bool) ([]byte, bool) {
	if r.Method == http.MethodGet {
		doc, err := tugqlFromURL(r.URL)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errTugQLURLTooLong) {
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
		writeError(w, http.StatusBadRequest, "bad_request", "body must contain a TugQL YAML document")
		return nil, false
	}
	// Relational source classification reads metadata first, then applies each
	// source's retention guard to the complete original request.
	r.Body = io.NopCloser(bytes.NewReader(doc))
	if strings.EqualFold(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]), "application/json") {
		if classifySources {
			doc = withoutContinuationFields(doc, true)
		}
		doc, err = bindTugQLParameters(doc)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_tugql", clipText(err.Error(), maxRefusalText))
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

// classifyTugQLDocument deserialises a document and classifies it. The error of
// a document that is not TugQL, or that the classifier refuses, wraps
// core.ErrInvalidTugQL.
func classifyTugQLDocument(doc []byte) (dal.StructuredQuery, core.Profile, error) {
	query, err := core.DeserializeTugQL(doc)
	if err != nil {
		return nil, core.Profile{}, err
	}
	profile, err := core.ClassifyTugQL(query)
	if err != nil {
		return nil, core.Profile{}, err
	}
	return query, profile, nil
}

var errTugQLURLTooLong = errors.New("TugQL URL exceeds 8 KiB limit")

// tugqlFromURL accepts a single YAML q value and an optional JSON parameters
// object. The same binder and validator handle POST and GET.
func tugqlFromURL(u *url.URL) ([]byte, error) {
	if len(u.RequestURI()) > 8<<10 {
		return nil, errTugQLURLTooLong
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid TugQL URL query: %w", err)
	}
	for name := range values {
		if name != "q" && name != "parameters" {
			return nil, fmt.Errorf("unsupported TugQL URL parameter %q", name)
		}
	}
	query := values["q"]
	if len(query) != 1 || strings.TrimSpace(query[0]) == "" {
		return nil, errors.New("TugQL URL requires exactly one nonempty q parameter")
	}
	parameters := json.RawMessage(`{}`)
	if raw, ok := values["parameters"]; ok {
		if len(raw) != 1 || raw[0] == "" {
			return nil, errors.New("TugQL URL requires one JSON parameters object")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw[0]), &object); err != nil || object == nil {
			return nil, errors.New("TugQL URL parameters must be a JSON object")
		}
		parameters = json.RawMessage(raw[0])
	}
	body, err := marshalTugQLURLBody(struct {
		Query      string          `json:"query"`
		Parameters json.RawMessage `json:"parameters"`
	}{Query: query[0], Parameters: parameters})
	if err != nil {
		return nil, fmt.Errorf("invalid TugQL URL parameters: %w", err)
	}
	return bindTugQLParameters(body)
}

// bindTugQLParameters replaces parsed TugQL parameter nodes with JSON values.
// Values never enter YAML or SQL as text, so a string cannot change query shape.
func bindTugQLParameters(body []byte) ([]byte, error) {
	var payload struct {
		Query      string                     `json:"query"`
		Parameters map[string]json.RawMessage `json:"parameters"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("invalid TugQL JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("TugQL JSON must contain one object")
	}
	if strings.TrimSpace(payload.Query) == "" {
		return nil, errors.New("query must contain TugQL YAML")
	}
	if len(payload.Parameters) > 64 {
		return nil, errors.New("too many TugQL parameters")
	}
	var query yaml.Node
	if err := yaml.Unmarshal([]byte(payload.Query), &query); err != nil {
		return nil, fmt.Errorf("invalid TugQL YAML: %w", err)
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
			value, err := decodeTugQLParameter(raw)
			if err != nil {
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
					if item == nil || !isTugQLScalar(item) {
						return fmt.Errorf("parameter %q must contain only non-null scalars", name)
					}
				}
				key = "values"
			} else if !isTugQLScalar(value) {
				return fmt.Errorf("parameter %q must be a scalar or array", name)
			}
			var bound yaml.Node
			if err := encodeTugQLParamNode(&bound, value); err != nil {
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

// decodeTugQLParameter decodes the JSON value of a parameter. A number written as
// an integer that fits 64 bits is kept as that integer (int64), so a large ID is
// not rounded through float64: DALgo then refuses one outside JavaScript's safe
// range as a value on most engines, and a native PostgreSQL database compares it
// exactly (exactIntegerValues). Every other number is a float64, as before.
func decodeTugQLParameter(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return tugqlParameterNumbers(value)
}

func tugqlParameterNumbers(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if integer, err := strconv.ParseInt(v.String(), 10, 64); err == nil {
			return integer, nil
		}
		number, err := strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return nil, fmt.Errorf("number %s is out of range", clipText(v.String(), maxEchoLen))
		}
		return number, nil
	case []any:
		for i, item := range v {
			converted, err := tugqlParameterNumbers(item)
			if err != nil {
				return nil, err
			}
			v[i] = converted
		}
	}
	return value, nil
}

func isTugQLScalar(value any) bool {
	switch value.(type) {
	case string, bool, float64, int64:
		return true
	default:
		return false
	}
}
