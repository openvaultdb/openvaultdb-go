package server

import "strconv"

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
