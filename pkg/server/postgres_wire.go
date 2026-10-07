package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

const maxJavaScriptSafeInteger int64 = 1<<53 - 1

// nativePostgresJSONSafeIntegers returns a shallow copy of a PostgreSQL
// relational row with unsafe integer scalars represented as decimal strings.
// It deliberately does not walk nested values: JSON/JSONB retains its own JSON
// number semantics and is not advertised as safe for JavaScript's Number parser.
func nativePostgresJSONSafeIntegers(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	out := make(map[string]any, len(fields))
	for name, field := range fields {
		switch n := field.(type) {
		case int64:
			if n < -maxJavaScriptSafeInteger || n > maxJavaScriptSafeInteger {
				field = strconv.FormatInt(n, 10)
			}
		case uint64:
			if n > uint64(maxJavaScriptSafeInteger) {
				field = strconv.FormatUint(n, 10)
			}
		}
		out[name] = field
	}
	return out
}

// nativePostgresJSONValues applies the native scalar wire mapping using the
// discovered field metadata. PostgreSQL's driver exposes temporal fields as
// time.Time, whose default JSON encoding adds an offset even to DATE and
// timestamp-without-time-zone values. Use the source type to retain that
// distinction in the response.
func nativePostgresJSONValues(fields map[string]any, nativeTypes map[string]string) map[string]any {
	out := nativePostgresJSONSafeIntegers(fields)
	for name, value := range out {
		nativeType := strings.ToLower(strings.TrimSpace(nativeTypes[name]))
		switch nativeType {
		case "json", "jsonb":
			switch raw := value.(type) {
			case string:
				if json.Valid([]byte(raw)) {
					out[name] = json.RawMessage(raw)
				}
			case []byte:
				if json.Valid(raw) {
					out[name] = json.RawMessage(append([]byte(nil), raw...))
				}
			}
			continue
		}
		t, ok := value.(time.Time)
		if !ok {
			continue
		}
		switch nativeType {
		case "date":
			out[name] = t.Format("2006-01-02")
		case "time without time zone":
			out[name] = t.Format("15:04:05.999999999")
		case "time with time zone":
			out[name] = t.Format("15:04:05.999999999Z07:00")
		case "timestamp without time zone":
			out[name] = t.Format("2006-01-02T15:04:05.999999999")
		case "timestamp with time zone":
			out[name] = t.UTC().Format(time.RFC3339Nano)
		}
	}
	return out
}

func nativePostgresFieldTypes(collection *schema.Collection) map[string]string {
	if collection == nil {
		return nil
	}
	types := make(map[string]string, len(collection.Fields))
	for name, field := range collection.Fields {
		types[name] = field.NativeType
	}
	return types
}
