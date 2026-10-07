package server

import (
	"reflect"
	"testing"
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
