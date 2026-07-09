package core

import (
	"encoding/json"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func deepCopy(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	var out map[string]any
	if err = json.Unmarshal(b, &out); err != nil {
		return m
	}
	return out
}

// inferFields derives all-optional field declarations from one record's
// top-level fields — used to auto-create collections on first write in
// partial/schemaless modes.
func inferFields(data map[string]any) map[string]schema.Field {
	fields := make(map[string]schema.Field, len(data))
	for name, v := range data {
		fields[name] = schema.Field{Type: jsonFieldType(v)}
	}
	return fields
}

func jsonFieldType(v any) schema.FieldType {
	switch v.(type) {
	case string:
		return schema.TypeString
	case bool:
		return schema.TypeBoolean
	case float64, float32, int, int64, json.Number:
		return schema.TypeNumber
	case map[string]any:
		return schema.TypeObject
	case []any:
		return schema.TypeArray
	default:
		return schema.TypeAny
	}
}
