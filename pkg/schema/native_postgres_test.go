package schema

import (
	"strings"
	"testing"
)

func TestNativePostgresCollectionIDRoundTripAndSeparatesTupleParts(t *testing.T) {
	tests := []struct{ schema, name string }{
		{"public", "Artist"},
		{"sales data", "Order Details"},
		{"a/b", "c"},
		{"a", "b/c"},
		{"café", "名"},
		{strings.Repeat("s", 63), strings.Repeat("t", 63)},
	}
	seen := map[string]bool{}
	for _, tc := range tests {
		id, err := NativePostgresCollectionID(tc.schema, tc.name)
		if err != nil {
			t.Fatalf("NativePostgresCollectionID(%q, %q): %v", tc.schema, tc.name, err)
		}
		if seen[id] {
			t.Fatalf("collision for (%q, %q): %q", tc.schema, tc.name, id)
		}
		seen[id] = true
		gotSchema, gotName, err := ParseNativePostgresCollectionID(id)
		if err != nil || gotSchema != tc.schema || gotName != tc.name {
			t.Errorf("ParseNativePostgresCollectionID(%q) = (%q, %q, %v), want (%q, %q)", id, gotSchema, gotName, err, tc.schema, tc.name)
		}
	}
	left, _ := NativePostgresCollectionID("a/b", "c")
	right, _ := NativePostgresCollectionID("a", "b/c")
	if left == right {
		t.Fatalf("tuple-boundary collision: %q", left)
	}
}

func TestNativePostgresCollectionIDRejectsIllegalPhysicalIdentifiers(t *testing.T) {
	for _, tc := range []struct{ schema, name string }{
		{"", "table"}, {"schema", ""}, {strings.Repeat("x", 64), "table"},
		{"schema", strings.Repeat("x", 64)}, {"bad\x00schema", "table"}, {"schema", "bad\x00table"},
		{"bad\xff", "table"},
	} {
		if _, err := NativePostgresCollectionID(tc.schema, tc.name); err == nil {
			t.Errorf("NativePostgresCollectionID(%q, %q) accepted invalid identifiers", tc.schema, tc.name)
		}
	}
}

func TestParseNativePostgresCollectionIDRejectsMalformedAndNonCanonicalIDs(t *testing.T) {
	for _, id := range []string{"", "pg1_", "pg1_%%", "pg1_YQ", "pg1_!!!!", "pg2_"} {
		if _, _, err := ParseNativePostgresCollectionID(id); err == nil {
			t.Errorf("ParseNativePostgresCollectionID(%q) unexpectedly succeeded", id)
		}
	}
}
