// Package authorizationapi validates the DTQL authorization HTTP ingress.
// These descriptions carry no authenticated identity or execution authority.
package authorizationapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dtql/authorization"
	"github.com/openvaultdb/openvaultdb-go/pkg/core"
)

const MaxRequestBytes = 1 << 20

type Query struct {
	Format     string         `json:"format"`
	Text       string         `json:"text"`
	Parameters map[string]any `json:"parameters,omitempty"`
}
type Change struct {
	Op    string          `json:"op"`
	Path  []string        `json:"path"`
	Value json.RawMessage `json:"value"`
}
type Mutation struct {
	Data           map[string]any `json:"data,omitempty"`
	Changes        []Change       `json:"changes,omitempty"`
	IfDataRevision string         `json:"ifDataRevision,omitempty"`
}
type Operation struct {
	ID             string                       `json:"id"`
	Action         string                       `json:"action"`
	Resource       authorization.Resource       `json:"resource"`
	Query          *Query                       `json:"query,omitempty"`
	Mutation       *Mutation                    `json:"mutation,omitempty"`
	ExecutionClass authorization.ExecutionClass `json:"executionClass"`
	Callable       *authorization.Callable      `json:"callable,omitempty"`
}
type Sample struct {
	Query Query `json:"query"`
	Limit int   `json:"limit"`
}
type Simulation struct {
	Roles      []string       `json:"roles"`
	Groups     []string       `json:"groups"`
	Attributes map[string]any `json:"attributes"`
}
type Request struct {
	APIVersion      string               `json:"apiVersion"`
	Mode            authorization.Mode   `json:"mode"`
	DiagnosticLevel string               `json:"diagnosticLevel"`
	Subject         *access.PrincipalRef `json:"subject,omitempty"`
	Simulation      *Simulation          `json:"simulation,omitempty"`
	Operations      []Operation          `json:"operations"`
	Sample          *Sample              `json:"sample,omitempty"`
}

// Parse rejects ambiguous JSON before decoding a closed request. Validation
// derives redundant resource names and verifies the actual query collection.
func Parse(data []byte, database string) (Request, error) {
	var request Request
	if err := DecodeStrict(data, &request); err != nil {
		return request, err
	}
	if err := request.Validate(database); err != nil {
		return Request{}, err
	}
	return request, nil
}

func DecodeStrict(data []byte, target any) error {
	if len(data) == 0 || len(data) > MaxRequestBytes || !utf8.Valid(data) {
		return fmt.Errorf("invalid request size or encoding")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	nodes := 0
	if err := scan(d, 0, &nodes); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	var shape any
	if err := json.Unmarshal(data, &shape); err != nil {
		return err
	}
	if err := validateShape(shape, reflect.TypeOf(target)); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	return nil
}

// encoding/json deliberately matches names case-insensitively and accepts
// null for many Go values. The wire contract permits neither ambiguity.
func validateShape(value any, typ reflect.Type) error {
	if typ == reflect.TypeOf(json.RawMessage{}) || typ.Kind() == reflect.Interface {
		return nil
	}
	if value == nil {
		return fmt.Errorf("null is not permitted for this request field")
	}
	if typ.Kind() == reflect.Pointer {
		return validateShape(value, typ.Elem())
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object")
		}
		known := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			name := tag[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			known[name] = true
			item, present := object[name]
			if !present {
				optional := false
				for _, option := range tag[1:] {
					optional = optional || option == "omitempty"
				}
				if !optional {
					return fmt.Errorf("missing required field %s", name)
				}
				continue
			}
			if err := validateShape(item, field.Type); err != nil {
				return fmt.Errorf("field %s: %w", name, err)
			}
		}
		for name := range object {
			if !known[name] {
				return fmt.Errorf("unknown field %s", name)
			}
		}
	case reflect.Slice:
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("expected array")
		}
		for _, item := range items {
			if err := validateShape(item, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("expected object")
		}
		for _, item := range object {
			if err := validateShape(item, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func scan(d *json.Decoder, depth int, nodes *int) error {
	*nodes++
	if depth > 32 || *nodes > 16384 {
		return fmt.Errorf("request structure limit exceeded")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := t.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			t, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := t.(string)
			if !ok || keys[key] {
				return fmt.Errorf("duplicate or invalid object key")
			}
			keys[key] = true
			if err := scan(d, depth+1, nodes); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := scan(d, depth+1, nodes); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

func (r *Request) Validate(database string) error {
	if r.APIVersion != authorization.APIVersion {
		return fmt.Errorf("unsupported authorization version")
	}
	if r.Mode != authorization.ModePlan && r.Mode != authorization.ModeInspect && r.Mode != authorization.ModeSample {
		return fmt.Errorf("invalid dry-run mode")
	}
	if r.DiagnosticLevel != "ordinary" && r.DiagnosticLevel != "references" && r.DiagnosticLevel != "policy" {
		return fmt.Errorf("invalid diagnostic level")
	}
	if r.Subject != nil {
		if err := r.Subject.Validate(); err != nil {
			return err
		}
	}
	if r.Simulation != nil {
		if !validNames(r.Simulation.Roles) || !validNames(r.Simulation.Groups) || r.Simulation.Attributes == nil || len(r.Simulation.Attributes) > 1000 {
			return fmt.Errorf("invalid simulation")
		}
		for name, value := range r.Simulation.Attributes {
			if !validString(name, 256) || !scalar(value) {
				return fmt.Errorf("invalid simulation attribute")
			}
		}
	}
	if len(r.Operations) < 1 || len(r.Operations) > 100 {
		return fmt.Errorf("expected 1..100 operations")
	}
	ids := map[string]bool{}
	for i := range r.Operations {
		op := &r.Operations[i]
		if !validString(op.ID, 128) || ids[op.ID] {
			return fmt.Errorf("invalid or duplicate operation id")
		}
		ids[op.ID] = true
		if err := op.Normalize(database); err != nil {
			return err
		}
		if r.Mode == authorization.ModeInspect && op.Resource.RowID == "" {
			return fmt.Errorf("inspection requires a concrete record")
		}
	}
	if r.Mode == authorization.ModeSample {
		if r.Sample == nil || len(r.Operations) != 1 || r.Sample.Limit < 1 || r.Sample.Limit > 100 {
			return fmt.Errorf("invalid sample request")
		}
		op := r.Operations[0]
		if op.Resource.RowID != "" || (op.Action != "get" && op.Action != "exists" && op.Action != "update" && op.Action != "delete") {
			return fmt.Errorf("sample requires a supported table operation template")
		}
		if _, err := r.Sample.Query.Parse(op.Resource.Table); err != nil {
			return err
		}
	} else if r.Sample != nil {
		return fmt.Errorf("sample is only permitted in sample mode")
	}
	return nil
}

// Normalize is shared by diagnostic and real execution ingress. It does not
// execute queries, resolve principals, read rows, or load policy snapshots.
func (o *Operation) Normalize(database string) error {
	r := &o.Resource
	if !validString(database, 256) || r.DatabaseID != database || !validString(r.Path, 4096) || !strings.HasPrefix(r.Path, "/") {
		return fmt.Errorf("resource does not match database")
	}
	parts := strings.Split(strings.TrimPrefix(r.Path, "/"), "/")
	if len(parts) < 1 || len(parts) > 2 {
		return fmt.Errorf("only root collection and record paths are supported")
	}
	for _, part := range parts {
		if !validSegment(part) {
			return fmt.Errorf("invalid resource path")
		}
	}
	table, row := parts[0], ""
	if len(parts) == 2 {
		row = parts[1]
	}
	if r.Table != "" && r.Table != table || r.RowID != "" && r.RowID != row {
		return fmt.Errorf("conflicting resource identifiers")
	}
	r.Table, r.RowID = table, row
	if len(r.Columns) > 32 {
		return fmt.Errorf("too many columns")
	}
	columns := map[string]bool{}
	for _, field := range r.Columns {
		if !validField(field) || columns[fieldKey(field)] {
			return fmt.Errorf("invalid or duplicate column")
		}
		columns[fieldKey(field)] = true
	}
	switch o.ExecutionClass {
	case authorization.ExecutionDTQL, authorization.ExecutionNativeSQL, authorization.ExecutionNativeGraphQL:
		if o.Callable != nil {
			return fmt.Errorf("callable requires stored_procedure")
		}
	case authorization.ExecutionStoredProcedure:
		if o.Callable == nil || !validString(o.Callable.Namespace, 256) || !validString(o.Callable.Name, 256) {
			return fmt.Errorf("invalid callable")
		}
	default:
		return fmt.Errorf("unknown execution class")
	}
	if o.Action != "query" && o.Query != nil {
		return fmt.Errorf("query supplied for non-query action")
	}
	switch o.Action {
	case "query":
		if o.Query == nil || row != "" || o.Mutation != nil {
			return fmt.Errorf("invalid query operation")
		}
		if _, err := o.Query.Parse(table); err != nil {
			return err
		}
	case "get", "exists", "truncate":
		if o.Mutation != nil {
			return fmt.Errorf("unexpected mutation")
		}
		if o.Action == "truncate" && row != "" {
			return fmt.Errorf("truncate requires table")
		}
	case "insert", "set", "update", "delete":
		if o.Mutation == nil {
			return fmt.Errorf("mutation is required")
		}
		m := o.Mutation
		if m.IfDataRevision != "" && !validString(m.IfDataRevision, 256) {
			return fmt.Errorf("invalid data revision")
		}
		switch o.Action {
		case "insert", "set":
			if m.Data == nil || m.Changes != nil {
				return fmt.Errorf("insert/set require data only")
			}
		case "delete":
			if m.Data != nil || m.Changes != nil {
				return fmt.Errorf("delete requires empty mutation")
			}
		case "update":
			if m.Data != nil || len(m.Changes) < 1 || len(m.Changes) > 32 {
				return fmt.Errorf("update requires 1..32 changes")
			}
			fields := map[string]bool{}
			for _, change := range m.Changes {
				if change.Op != "set" || !validField(change.Path) || len(change.Value) == 0 || fields[fieldKey(change.Path)] {
					return fmt.Errorf("invalid or duplicate change")
				}
				// Overlapping parent/child changes make the candidate order-dependent.
				for prior := range fields {
					current := fieldKey(change.Path)
					if strings.HasPrefix(prior, current+"\x00") || strings.HasPrefix(current, prior+"\x00") {
						return fmt.Errorf("overlapping changes")
					}
				}
				fields[fieldKey(change.Path)] = true
			}
			if len(columns) > 0 {
				if len(columns) != len(fields) {
					return fmt.Errorf("columns differ from mutation")
				}
				for key := range fields {
					if !columns[key] {
						return fmt.Errorf("columns differ from mutation")
					}
				}
			}
			r.Columns = make([][]string, 0, len(m.Changes))
			for _, c := range m.Changes {
				r.Columns = append(r.Columns, append([]string(nil), c.Path...))
			}
		}
	default:
		return fmt.Errorf("unknown operation action")
	}
	return nil
}

func (q Query) Parse(table string) (dal.StructuredQuery, error) {
	if q.Format != "dtql-yaml" || len(q.Text) == 0 || len(q.Text) > MaxRequestBytes {
		return nil, fmt.Errorf("invalid query format or size")
	}
	// Parameter binding must never be confused with trusted policy variables.
	if len(q.Parameters) != 0 {
		return nil, fmt.Errorf("query parameters are not supported in this profile")
	}
	query, collection, err := core.ParseDTQL([]byte(q.Text))
	if err != nil {
		return nil, err
	}
	if collection != table {
		return nil, fmt.Errorf("query collection does not match resource")
	}
	return query, nil
}

func validString(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && utf8.ValidString(s) && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func validSegment(s string) bool {
	return validString(s, 256) && s != "." && s != ".." && !strings.ContainsAny(s, "/\\%")
}
func validField(field []string) bool {
	if len(field) < 1 || len(field) > 16 {
		return false
	}
	for _, p := range field {
		if !validSegment(p) {
			return false
		}
	}
	return true
}
func fieldKey(field []string) string { return strings.Join(field, "\x00") }
func validNames(names []string) bool {
	if names == nil || len(names) > 1000 {
		return false
	}
	seen := map[string]bool{}
	for _, name := range names {
		if !validString(name, 256) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}
func scalar(value any) bool {
	switch value.(type) {
	case nil, bool, string, float64:
		return true
	}
	return false
}
