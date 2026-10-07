package server

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestNativePostgresJSONSafeIntegers(t *testing.T) {
	input := map[string]any{
		"minSafe":      int64(-maxJavaScriptSafeInteger),
		"maxSafe":      uint64(maxJavaScriptSafeInteger),
		"wideSigned":   int64(maxJavaScriptSafeInteger + 1),
		"wideNegative": int64(-maxJavaScriptSafeInteger - 1),
		"wideUnsigned": uint64(maxJavaScriptSafeInteger) + 1,
		"smallInt":     int32(7),
		"json":         map[string]any{"n": int64(maxJavaScriptSafeInteger + 1)},
	}
	got := nativePostgresJSONSafeIntegers(input)
	want := map[string]any{
		"minSafe":      int64(-maxJavaScriptSafeInteger),
		"maxSafe":      uint64(maxJavaScriptSafeInteger),
		"wideSigned":   "9007199254740992",
		"wideNegative": "-9007199254740992",
		"wideUnsigned": "9007199254740992",
		"smallInt":     int32(7),
		"json":         map[string]any{"n": int64(maxJavaScriptSafeInteger + 1)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nativePostgresJSONSafeIntegers() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(input["wideSigned"], int64(maxJavaScriptSafeInteger+1)) {
		t.Fatal("conversion modified the source row")
	}
}

func TestNativePostgresJSONValuesPreserveTemporalTypes(t *testing.T) {
	instant := time.Date(2025, time.March, 4, 13, 14, 15, 123000000, time.FixedZone("+02", 2*60*60))
	input := map[string]any{
		"date":           instant,
		"time":           instant,
		"localTimestamp": instant,
		"timestamp":      instant,
		"ordinary":       "unchanged",
	}
	types := map[string]string{
		"date":           "date",
		"time":           "time without time zone",
		"localTimestamp": "timestamp without time zone",
		"timestamp":      "timestamp with time zone",
		"ordinary":       "text",
	}
	want := map[string]any{
		"date":           "2025-03-04",
		"time":           "13:14:15.123",
		"localTimestamp": "2025-03-04T13:14:15.123",
		"timestamp":      "2025-03-04T11:14:15.123Z",
		"ordinary":       "unchanged",
	}
	got := nativePostgresJSONValues(input, types)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nativePostgresJSONValues() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(input["date"], instant) {
		t.Fatal("conversion modified the source row")
	}
}

func TestNativePostgresJSONValuesEmitValidatedJSONDocuments(t *testing.T) {
	input := map[string]any{
		"json":       `{"n":9007199254740993}`,
		"jsonb":      []byte(`{"n":9007199254740993}`),
		"bad_json":   `not json`,
		"plain_text": `{"n":9007199254740993}`,
	}
	types := map[string]string{"json": "json", "jsonb": "jsonb", "bad_json": "jsonb", "plain_text": "text"}
	got := nativePostgresJSONValues(input, types)
	for _, name := range []string{"json", "jsonb"} {
		raw, ok := got[name].(json.RawMessage)
		if !ok || string(raw) != `{"n":9007199254740993}` {
			t.Errorf("%s = %#v, want validated raw JSON with original number lexeme", name, got[name])
		}
	}
	if got["bad_json"] != "not json" || got["plain_text"] != input["plain_text"] {
		t.Fatalf("non-JSON values were changed: %#v", got)
	}
}
