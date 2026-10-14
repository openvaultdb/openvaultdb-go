package joinexec

import (
	"encoding/json"
	"errors"
	"math"
)

// encodeRow is the JSON a row is measured by against the byte budgets. A value
// JSON has no number for, NaN and ±Inf of a PostgreSQL real or double precision
// column, is measured as the string the server answers it with ("NaN",
// "Infinity", "-Infinity"), so such a row is read and not refused as one that
// cannot be encoded. Any other failure is returned as it is.
func encodeRow(data any) ([]byte, error) {
	encoded, err := json.Marshal(data)
	var unsupported *json.UnsupportedValueError
	if err == nil || !errors.As(err, &unsupported) {
		return encoded, err
	}
	return json.Marshal(withSpelledFloats(data))
}

// withSpelledFloats is a copy of value with each NaN or infinite float spelled as
// a string, through maps and slices.
func withSpelledFloats(value any) any {
	switch v := value.(type) {
	case float64:
		return spelledFloat(v, value)
	case float32:
		return spelledFloat(float64(v), value)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = withSpelledFloats(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = withSpelledFloats(item)
		}
		return out
	case []float64:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = spelledFloat(item, item)
		}
		return out
	case []float32:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = spelledFloat(float64(item), item)
		}
		return out
	}
	return value
}

func spelledFloat(n float64, value any) any {
	switch {
	case math.IsNaN(n):
		return "NaN"
	case math.IsInf(n, 1):
		return "Infinity"
	case math.IsInf(n, -1):
		return "-Infinity"
	}
	return value
}
