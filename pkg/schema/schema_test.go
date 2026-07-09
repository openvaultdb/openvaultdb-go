package schema_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// ---------------------------------------------------------------------------
// Mode.Validate
// ---------------------------------------------------------------------------

func TestModeValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		mode        schema.Mode
		wantErr     bool
		errContains string
	}{
		{name: "strict", mode: schema.ModeStrict, wantErr: false},
		{name: "partial", mode: schema.ModePartial, wantErr: false},
		{name: "schemaless", mode: schema.ModeSchemaless, wantErr: false},
		{name: "empty string", mode: "", wantErr: true, errContains: "required"},
		{name: "unknown mode", mode: "unknown", wantErr: true, errContains: "unknown"},
		{name: "typo strict", mode: "Strict", wantErr: true, errContains: "unknown"},
		{name: "random garbage", mode: "garbage_mode", wantErr: true, errContains: "unknown"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.mode.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error but got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Fatalf("expected error containing %q, got %q", tc.errContains, err.Error())
				}
			} else {
				if err != nil {
					t.Fatalf("expected nil error but got: %v", err)
				}
			}
		})
	}
}

func TestModes(t *testing.T) {
	t.Parallel()
	modes := schema.Modes()
	if len(modes) != 3 {
		t.Fatalf("expected 3 modes, got %d", len(modes))
	}
	want := map[schema.Mode]bool{
		schema.ModeStrict:     true,
		schema.ModePartial:    true,
		schema.ModeSchemaless: true,
	}
	for _, m := range modes {
		if !want[m] {
			t.Errorf("unexpected mode in Modes(): %q", m)
		}
	}
}

// ---------------------------------------------------------------------------
// FieldType.Validate
// ---------------------------------------------------------------------------

func TestFieldTypeValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ft      schema.FieldType
		wantErr bool
	}{
		{name: "string", ft: schema.TypeString, wantErr: false},
		{name: "number", ft: schema.TypeNumber, wantErr: false},
		{name: "integer", ft: schema.TypeInteger, wantErr: false},
		{name: "boolean", ft: schema.TypeBoolean, wantErr: false},
		{name: "object", ft: schema.TypeObject, wantErr: false},
		{name: "array", ft: schema.TypeArray, wantErr: false},
		{name: "any", ft: schema.TypeAny, wantErr: false},
		{name: "empty string", ft: "", wantErr: true},
		{name: "bad_type", ft: "bad_type", wantErr: true},
		{name: "String (capital)", ft: "String", wantErr: true},
		{name: "text", ft: "text", wantErr: true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.ft.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error but got nil for type %q", tc.ft)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil error but got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateRecord — schemaless
// ---------------------------------------------------------------------------

func TestValidateRecord_Schemaless(t *testing.T) {
	t.Parallel()

	t.Run("nil col nil data", func(t *testing.T) {
		t.Parallel()
		if err := schema.ValidateRecord(schema.ModeSchemaless, "events", nil, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("nil col with data", func(t *testing.T) {
		t.Parallel()
		data := map[string]any{"foo": "bar", "num": 42}
		if err := schema.ValidateRecord(schema.ModeSchemaless, "events", nil, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("non-nil col any data passes", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"name": {Type: schema.TypeString, Required: true},
		}}
		data := map[string]any{"completely_random_key": true, "another": 999}
		if err := schema.ValidateRecord(schema.ModeSchemaless, "events", col, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("arbitrary keys pass", func(t *testing.T) {
		t.Parallel()
		data := map[string]any{
			"key1": []any{1, 2, 3},
			"key2": map[string]any{"nested": true},
			"key3": nil,
		}
		if err := schema.ValidateRecord(schema.ModeSchemaless, "stuff", nil, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// ValidateRecord — partial
// ---------------------------------------------------------------------------

func TestValidateRecord_Partial(t *testing.T) {
	t.Parallel()

	t.Run("nil col passes", func(t *testing.T) {
		t.Parallel()
		data := map[string]any{"anything": "goes"}
		if err := schema.ValidateRecord(schema.ModePartial, "logs", nil, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("declared fields correct types pass", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"name": {Type: schema.TypeString},
			"age":  {Type: schema.TypeInteger},
		}}
		data := map[string]any{"name": "Alice", "age": float64(30)}
		if err := schema.ValidateRecord(schema.ModePartial, "users", col, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("declared field wrong type returns error", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"name": {Type: schema.TypeString},
		}}
		data := map[string]any{"name": 42} // number, not string
		if err := schema.ValidateRecord(schema.ModePartial, "users", col, data); err == nil {
			t.Fatal("expected error for wrong type but got nil")
		}
	})

	t.Run("unknown field in data passes in partial", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"name": {Type: schema.TypeString},
		}}
		data := map[string]any{"name": "Bob", "extra_field": "whatever"}
		if err := schema.ValidateRecord(schema.ModePartial, "users", col, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("required declared field missing returns error", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"name": {Type: schema.TypeString, Required: true},
		}}
		data := map[string]any{"age": float64(25)}
		if err := schema.ValidateRecord(schema.ModePartial, "users", col, data); err == nil {
			t.Fatal("expected error for missing required field but got nil")
		}
	})

	t.Run("nil value for non-required declared field passes", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"name": {Type: schema.TypeString, Required: false},
		}}
		data := map[string]any{"name": nil}
		if err := schema.ValidateRecord(schema.ModePartial, "users", col, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("id field never causes unknown field error", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"name": {Type: schema.TypeString},
		}}
		data := map[string]any{"name": "Carol", "id": "abc-123"}
		if err := schema.ValidateRecord(schema.ModePartial, "users", col, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// ValidateRecord — strict
// ---------------------------------------------------------------------------

func TestValidateRecord_Strict(t *testing.T) {
	t.Parallel()

	t.Run("nil col returns error mentioning no schema declared", func(t *testing.T) {
		t.Parallel()
		err := schema.ValidateRecord(schema.ModeStrict, "orders", nil, map[string]any{"foo": "bar"})
		if err == nil {
			t.Fatal("expected error but got nil")
		}
		if !strings.Contains(err.Error(), "no schema declared") {
			t.Fatalf("error should mention 'no schema declared', got: %q", err.Error())
		}
	})

	t.Run("declared field present and correct passes", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"title": {Type: schema.TypeString},
		}}
		data := map[string]any{"title": "Hello"}
		if err := schema.ValidateRecord(schema.ModeStrict, "posts", col, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("unknown field returns ValidationError with unknown field rejected", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"title": {Type: schema.TypeString},
		}}
		data := map[string]any{"title": "Hello", "unknown_key": "value"}
		err := schema.ValidateRecord(schema.ModeStrict, "posts", col, data)
		if err == nil {
			t.Fatal("expected error for unknown field but got nil")
		}
		if !strings.Contains(err.Error(), "unknown field rejected") {
			t.Fatalf("expected 'unknown field rejected' in error, got: %q", err.Error())
		}
	})

	t.Run("id field is not rejected in strict mode", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"title": {Type: schema.TypeString},
		}}
		data := map[string]any{"title": "Hello", "id": "doc-001"}
		if err := schema.ValidateRecord(schema.ModeStrict, "posts", col, data); err != nil {
			t.Fatalf("unexpected error: 'id' should be implicitly allowed, got: %v", err)
		}
	})

	t.Run("required field missing returns error", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"email": {Type: schema.TypeString, Required: true},
		}}
		data := map[string]any{}
		if err := schema.ValidateRecord(schema.ModeStrict, "accounts", col, data); err == nil {
			t.Fatal("expected error for missing required field but got nil")
		}
	})

	t.Run("nil value for required field is treated as missing", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"email": {Type: schema.TypeString, Required: true},
		}}
		data := map[string]any{"email": nil}
		if err := schema.ValidateRecord(schema.ModeStrict, "accounts", col, data); err == nil {
			t.Fatal("expected error for nil required field but got nil")
		}
	})

	t.Run("wrong type returns error", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"count": {Type: schema.TypeInteger},
		}}
		data := map[string]any{"count": "not-a-number"}
		if err := schema.ValidateRecord(schema.ModeStrict, "stats", col, data); err == nil {
			t.Fatal("expected error for wrong type but got nil")
		}
	})

	t.Run("multiple fields all valid passes", func(t *testing.T) {
		t.Parallel()
		col := &schema.Collection{Fields: map[string]schema.Field{
			"name":   {Type: schema.TypeString, Required: true},
			"active": {Type: schema.TypeBoolean},
			"score":  {Type: schema.TypeNumber},
		}}
		data := map[string]any{
			"name":   "test",
			"active": true,
			"score":  float64(9.5),
		}
		if err := schema.ValidateRecord(schema.ModeStrict, "records", col, data); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Type matching — table-driven per type
// ---------------------------------------------------------------------------

func TestTypeMatching_String(t *testing.T) {
	t.Parallel()
	col := &schema.Collection{Fields: map[string]schema.Field{
		"f": {Type: schema.TypeString},
	}}
	accepts := []any{"hello", "", "with spaces"}
	// nil is not in rejects: validateFields skips type-checking when v == nil (non-required field)
	rejects := []any{1, true, []any{}, map[string]any{}, float64(3.14)}

	for _, v := range accepts {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err != nil {
			t.Errorf("TypeString should accept %T(%v) but got: %v", v, v, err)
		}
	}
	for _, v := range rejects {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err == nil {
			t.Errorf("TypeString should reject %T(%v) but got nil error", v, v)
		}
	}
}

func TestTypeMatching_Number(t *testing.T) {
	t.Parallel()
	col := &schema.Collection{Fields: map[string]schema.Field{
		"f": {Type: schema.TypeNumber},
	}}
	accepts := []any{float64(1.5), float64(0), int(1), int32(5), int64(100), float32(2.5), json.Number("3.14"), uint(7), uint32(8), uint64(9)}
	// nil skips type-checking for non-required fields; not in rejects list
	rejects := []any{"1", true, "3.14", []any{}, map[string]any{}}

	for _, v := range accepts {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err != nil {
			t.Errorf("TypeNumber should accept %T(%v) but got: %v", v, v, err)
		}
	}
	for _, v := range rejects {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err == nil {
			t.Errorf("TypeNumber should reject %T(%v) but got nil error", v, v)
		}
	}
}

func TestTypeMatching_Integer(t *testing.T) {
	t.Parallel()
	col := &schema.Collection{Fields: map[string]schema.Field{
		"f": {Type: schema.TypeInteger},
	}}
	accepts := []any{float64(2.0), int(3), json.Number("5"), int32(10), int64(20), uint(4), uint32(6), uint64(7)}
	// nil skips type-checking for non-required fields; not in rejects list
	rejects := []any{float64(1.5), "1", true, []any{}, json.Number("3.14")}

	for _, v := range accepts {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err != nil {
			t.Errorf("TypeInteger should accept %T(%v) but got: %v", v, v, err)
		}
	}
	for _, v := range rejects {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err == nil {
			t.Errorf("TypeInteger should reject %T(%v) but got nil error", v, v)
		}
	}
}

func TestTypeMatching_Boolean(t *testing.T) {
	t.Parallel()
	col := &schema.Collection{Fields: map[string]schema.Field{
		"f": {Type: schema.TypeBoolean},
	}}
	accepts := []any{true, false}
	// nil skips type-checking for non-required fields; not in rejects list
	rejects := []any{"true", "false", 1, 0, []any{}, map[string]any{}}

	for _, v := range accepts {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err != nil {
			t.Errorf("TypeBoolean should accept %T(%v) but got: %v", v, v, err)
		}
	}
	for _, v := range rejects {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err == nil {
			t.Errorf("TypeBoolean should reject %T(%v) but got nil error", v, v)
		}
	}
}

func TestTypeMatching_Object(t *testing.T) {
	t.Parallel()
	col := &schema.Collection{Fields: map[string]schema.Field{
		"f": {Type: schema.TypeObject},
	}}
	accepts := []any{map[string]any{}, map[string]any{"nested": true}}
	// nil skips type-checking for non-required fields; not in rejects list
	rejects := []any{"x", 1, true, []any{}, float64(1.0)}

	for _, v := range accepts {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err != nil {
			t.Errorf("TypeObject should accept %T(%v) but got: %v", v, v, err)
		}
	}
	for _, v := range rejects {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err == nil {
			t.Errorf("TypeObject should reject %T(%v) but got nil error", v, v)
		}
	}
}

func TestTypeMatching_Array(t *testing.T) {
	t.Parallel()
	col := &schema.Collection{Fields: map[string]schema.Field{
		"f": {Type: schema.TypeArray},
	}}
	accepts := []any{[]any{}, []any{1, 2, 3}, []any{"a", "b"}}
	// nil skips type-checking for non-required fields; not in rejects list
	rejects := []any{map[string]any{}, "x", 1, true}

	for _, v := range accepts {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err != nil {
			t.Errorf("TypeArray should accept %T(%v) but got: %v", v, v, err)
		}
	}
	for _, v := range rejects {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err == nil {
			t.Errorf("TypeArray should reject %T(%v) but got nil error", v, v)
		}
	}
}

func TestTypeMatching_Any(t *testing.T) {
	t.Parallel()
	col := &schema.Collection{Fields: map[string]schema.Field{
		"f": {Type: schema.TypeAny},
	}}
	accepts := []any{"hello", 42, true, nil, map[string]any{"k": "v"}, []any{1, 2}}

	for _, v := range accepts {
		v := v
		if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": v}); err != nil {
			t.Errorf("TypeAny should accept %T(%v) but got: %v", v, v, err)
		}
	}
}

// TestTypeMatching_NilSkipsTypeCheck documents that validateFields skips type
// checking when the value is nil and the field is not required. This applies
// across all typed fields (string, number, integer, boolean, object, array).
func TestTypeMatching_NilSkipsTypeCheck(t *testing.T) {
	t.Parallel()
	types := []schema.FieldType{
		schema.TypeString, schema.TypeNumber, schema.TypeInteger,
		schema.TypeBoolean, schema.TypeObject, schema.TypeArray,
	}
	for _, ft := range types {
		ft := ft
		t.Run(string(ft), func(t *testing.T) {
			t.Parallel()
			col := &schema.Collection{Fields: map[string]schema.Field{
				"f": {Type: ft, Required: false},
			}}
			// nil value for a non-required field must not trigger a type error
			if err := schema.ValidateRecord(schema.ModeStrict, "c", col, map[string]any{"f": nil}); err != nil {
				t.Errorf("nil value for non-required %s field should pass, got: %v", ft, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidationError
// ---------------------------------------------------------------------------

func TestValidationError_Error(t *testing.T) {
	t.Parallel()

	t.Run("with field", func(t *testing.T) {
		t.Parallel()
		e := &schema.ValidationError{Collection: "users", Field: "email", Message: "required field is missing"}
		got := e.Error()
		if !strings.Contains(got, "users") || !strings.Contains(got, "email") || !strings.Contains(got, "required field is missing") {
			t.Errorf("unexpected error string: %q", got)
		}
	})

	t.Run("without field", func(t *testing.T) {
		t.Parallel()
		e := &schema.ValidationError{Collection: "orders", Message: "no schema declared"}
		got := e.Error()
		if !strings.Contains(got, "orders") || !strings.Contains(got, "no schema declared") {
			t.Errorf("unexpected error string: %q", got)
		}
		// Field should not appear since it's empty
		if strings.Contains(got, `field ""`) {
			t.Errorf("error should not mention empty field name: %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Schemas.Validate
// ---------------------------------------------------------------------------

func TestSchemasValidate(t *testing.T) {
	t.Parallel()

	t.Run("nil schemas returns nil", func(t *testing.T) {
		t.Parallel()
		var s *schema.Schemas
		if err := s.Validate(); err != nil {
			t.Fatalf("expected nil error for nil *Schemas, got: %v", err)
		}
	})

	t.Run("collection with no fields returns error", func(t *testing.T) {
		t.Parallel()
		s := &schema.Schemas{
			Collections: map[string]schema.Collection{
				"empty": {Fields: map[string]schema.Field{}},
			},
		}
		if err := s.Validate(); err == nil {
			t.Fatal("expected error for collection with no fields but got nil")
		}
	})

	t.Run("collection with nil fields map returns error", func(t *testing.T) {
		t.Parallel()
		s := &schema.Schemas{
			Collections: map[string]schema.Collection{
				"nofields": {},
			},
		}
		if err := s.Validate(); err == nil {
			t.Fatal("expected error for collection with nil fields map but got nil")
		}
	})

	t.Run("collection with empty field name returns error", func(t *testing.T) {
		t.Parallel()
		s := &schema.Schemas{
			Collections: map[string]schema.Collection{
				"col": {Fields: map[string]schema.Field{
					"": {Type: schema.TypeString},
				}},
			},
		}
		if err := s.Validate(); err == nil {
			t.Fatal("expected error for empty field name but got nil")
		}
	})

	t.Run("collection with unknown field type returns error", func(t *testing.T) {
		t.Parallel()
		s := &schema.Schemas{
			Collections: map[string]schema.Collection{
				"col": {Fields: map[string]schema.Field{
					"name": {Type: "badtype"},
				}},
			},
		}
		if err := s.Validate(); err == nil {
			t.Fatal("expected error for unknown field type but got nil")
		}
	})

	t.Run("valid collection returns nil error", func(t *testing.T) {
		t.Parallel()
		s := &schema.Schemas{
			Collections: map[string]schema.Collection{
				"users": {Fields: map[string]schema.Field{
					"name":   {Type: schema.TypeString, Required: true},
					"age":    {Type: schema.TypeInteger},
					"active": {Type: schema.TypeBoolean},
					"meta":   {Type: schema.TypeObject},
					"tags":   {Type: schema.TypeArray},
					"score":  {Type: schema.TypeNumber},
					"extra":  {Type: schema.TypeAny},
				}},
				"posts": {Fields: map[string]schema.Field{
					"title": {Type: schema.TypeString, Required: true},
					"body":  {Type: schema.TypeString},
				}},
			},
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error for valid schemas: %v", err)
		}
	})

	t.Run("empty collections map is valid", func(t *testing.T) {
		t.Parallel()
		s := &schema.Schemas{
			Collections: map[string]schema.Collection{},
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error for empty collections: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Schemas.Collection lookup
// ---------------------------------------------------------------------------

func TestSchemasCollection(t *testing.T) {
	t.Parallel()

	t.Run("nil schemas returns nil", func(t *testing.T) {
		t.Parallel()
		var s *schema.Schemas
		if col := s.Collection("anything"); col != nil {
			t.Fatalf("expected nil for nil *Schemas, got %v", col)
		}
	})

	t.Run("existing collection returns pointer", func(t *testing.T) {
		t.Parallel()
		s := &schema.Schemas{
			Collections: map[string]schema.Collection{
				"users": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}},
			},
		}
		col := s.Collection("users")
		if col == nil {
			t.Fatal("expected non-nil collection for 'users'")
		}
	})

	t.Run("missing collection returns nil", func(t *testing.T) {
		t.Parallel()
		s := &schema.Schemas{
			Collections: map[string]schema.Collection{},
		}
		if col := s.Collection("nonexistent"); col != nil {
			t.Fatalf("expected nil for missing collection, got %v", col)
		}
	})
}

// ---------------------------------------------------------------------------
// ValidateRecord — unknown mode
// ---------------------------------------------------------------------------

func TestValidateRecord_UnknownMode(t *testing.T) {
	t.Parallel()
	col := &schema.Collection{Fields: map[string]schema.Field{
		"name": {Type: schema.TypeString},
	}}
	err := schema.ValidateRecord("unknown_mode", "col", col, map[string]any{"name": "test"})
	if err == nil {
		t.Fatal("expected error for unknown mode but got nil")
	}
}
