// Package schema defines OpenVaultDB schema modes and record validation.
package schema

import (
	"encoding/json"
	"fmt"
	"math"
)

// Mode is the schema mode of a logical database.
//
// Schemaless means no required pre-declared schema.
// It does not mean no schema information: partial and schemaless writes are
// observed into the inferred schema catalogue (see pkg/inferred).
type Mode string

const (
	// ModeStrict requires a declared schema for every collection before writes;
	// writes are validated and unknown fields are rejected.
	ModeStrict Mode = "strict"
	// ModePartial validates declared fields; undeclared fields are allowed.
	ModePartial Mode = "partial"
	// ModeSchemaless requires no declared schema before writes.
	ModeSchemaless Mode = "schemaless"
)

// Modes lists all valid schema modes.
func Modes() []Mode { return []Mode{ModeStrict, ModePartial, ModeSchemaless} }

// Validate returns an error if m is not a known schema mode.
func (m Mode) Validate() error {
	switch m {
	case ModeStrict, ModePartial, ModeSchemaless:
		return nil
	case "":
		return fmt.Errorf("schema_mode is required, one of: strict, partial, schemaless")
	default:
		return fmt.Errorf("unknown schema_mode %q, expected one of: strict, partial, schemaless", string(m))
	}
}

// FieldType is the declared type of a field in a collection schema.
type FieldType string

const (
	TypeString  FieldType = "string"
	TypeNumber  FieldType = "number"
	TypeInteger FieldType = "integer"
	TypeBoolean FieldType = "boolean"
	TypeObject  FieldType = "object"
	TypeArray   FieldType = "array"
	TypeAny     FieldType = "any"
)

// Validate returns an error if t is not a known field type.
func (t FieldType) Validate() error {
	switch t {
	case TypeString, TypeNumber, TypeInteger, TypeBoolean, TypeObject, TypeArray, TypeAny:
		return nil
	default:
		return fmt.Errorf("unknown field type %q", string(t))
	}
}

// Field declares a single field of a collection schema.
type Field struct {
	Type     FieldType `yaml:"type" json:"type"`
	Required bool      `yaml:"required,omitempty" json:"required,omitempty"`
}

// Collection declares the schema of one collection.
type Collection struct {
	Fields map[string]Field `yaml:"fields" json:"fields"`
}

// Schemas holds declared collection schemas of a database.
type Schemas struct {
	Collections map[string]Collection `yaml:"collections" json:"collections"`
}

// Collection returns the declared schema for the named collection, or nil.
func (s *Schemas) Collection(name string) *Collection {
	if s == nil {
		return nil
	}
	if c, ok := s.Collections[name]; ok {
		return &c
	}
	return nil
}

// Validate checks declared schemas for structural correctness.
func (s *Schemas) Validate() error {
	if s == nil {
		return nil
	}
	for colName, col := range s.Collections {
		if len(col.Fields) == 0 {
			return fmt.Errorf("collection %q declares no fields", colName)
		}
		for fieldName, f := range col.Fields {
			if fieldName == "" {
				return fmt.Errorf("collection %q has a field with an empty name", colName)
			}
			if err := f.Type.Validate(); err != nil {
				return fmt.Errorf("collection %q field %q: %w", colName, fieldName, err)
			}
		}
	}
	return nil
}

// ValidationError describes a schema-mode validation failure for a write.
type ValidationError struct {
	Collection string
	Field      string
	Message    string
}

func (e *ValidationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("collection %q: %s", e.Collection, e.Message)
	}
	return fmt.Sprintf("collection %q field %q: %s", e.Collection, e.Field, e.Message)
}

// ValidateRecord validates record data against the collection schema per the
// database schema mode. col may be nil when no schema is declared for the
// collection: that is an error in strict mode and a pass otherwise.
func ValidateRecord(mode Mode, collection string, col *Collection, data map[string]any) error {
	switch mode {
	case ModeSchemaless:
		return nil
	case ModePartial:
		if col == nil {
			return nil
		}
		return validateFields(collection, col, data, false)
	case ModeStrict:
		if col == nil {
			return &ValidationError{Collection: collection,
				Message: "no schema declared; strict mode requires a schema for every collection before writing"}
		}
		return validateFields(collection, col, data, true)
	default:
		return fmt.Errorf("unknown schema mode %q", mode)
	}
}

func validateFields(collection string, col *Collection, data map[string]any, rejectUnknown bool) error {
	for name, f := range col.Fields {
		v, ok := data[name]
		if !ok || v == nil {
			if f.Required {
				return &ValidationError{Collection: collection, Field: name, Message: "required field is missing"}
			}
			continue
		}
		if !valueMatchesType(v, f.Type) {
			return &ValidationError{Collection: collection, Field: name,
				Message: fmt.Sprintf("expected type %s, got %T", f.Type, v)}
		}
	}
	if rejectUnknown {
		for name := range data {
			// "id" is implicitly declared for every collection: relational
			// drivers store the record key there and echo it on reads.
			if name == "id" {
				continue
			}
			if _, ok := col.Fields[name]; !ok {
				return &ValidationError{Collection: collection, Field: name,
					Message: "unknown field rejected in strict mode"}
			}
		}
	}
	return nil
}

func valueMatchesType(v any, t FieldType) bool {
	switch t {
	case TypeAny:
		return true
	case TypeString:
		_, ok := v.(string)
		return ok
	case TypeBoolean:
		_, ok := v.(bool)
		return ok
	case TypeObject:
		_, ok := v.(map[string]any)
		return ok
	case TypeArray:
		_, ok := v.([]any)
		return ok
	case TypeNumber:
		return isNumber(v)
	case TypeInteger:
		f, ok := asFloat(v)
		return ok && f == math.Trunc(f)
	default:
		return false
	}
}

func isNumber(v any) bool {
	_, ok := asFloat(v)
	return ok
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
