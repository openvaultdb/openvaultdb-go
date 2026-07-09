package inferred_test

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/inferred"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

func newCatalogue(t *testing.T) (*inferred.Catalogue, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".ovdb", "inferred-schema.json")
	c, err := inferred.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c, path
}

// TestObserveSimpleRecord verifies that a flat record populates field stats.
func TestObserveSimpleRecord(t *testing.T) {
	c, _ := newCatalogue(t)

	record := map[string]any{
		"name":   "Alice",
		"age":    float64(30),
		"active": true,
	}
	if err := c.Observe("users", record); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	snap := c.Snapshot()
	col, ok := snap.Collections["users"]
	if !ok {
		t.Fatal("collection 'users' not found in snapshot")
	}
	if col.RecordCount != 1 {
		t.Errorf("RecordCount = %d, want 1", col.RecordCount)
	}

	checkField := func(path, wantType string) {
		t.Helper()
		fs, ok := col.Fields[path]
		if !ok {
			t.Errorf("field %q not found", path)
			return
		}
		if fs.ObservedTypes[wantType] != 1 {
			t.Errorf("field %q: ObservedTypes[%q] = %d, want 1", path, wantType, fs.ObservedTypes[wantType])
		}
		if fs.SampleCount != 1 {
			t.Errorf("field %q: SampleCount = %d, want 1", path, fs.SampleCount)
		}
	}

	checkField("name", "string")
	checkField("age", "number")
	checkField("active", "boolean")
}

// TestNestedObjectsDottedPaths verifies that nested objects produce dotted paths.
func TestNestedObjectsDottedPaths(t *testing.T) {
	c, _ := newCatalogue(t)

	record := map[string]any{
		"address": map[string]any{
			"city": "Dublin",
			"zip":  "D01",
		},
	}
	if err := c.Observe("places", record); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	snap := c.Snapshot()
	col := snap.Collections["places"]

	if _, ok := col.Fields["address"]; !ok {
		t.Error("expected field 'address'")
	}
	if _, ok := col.Fields["address.city"]; !ok {
		t.Error("expected field 'address.city'")
	}
	if _, ok := col.Fields["address.zip"]; !ok {
		t.Error("expected field 'address.zip'")
	}
}

// TestArraysRecordElementTypes verifies that array fields record element types.
func TestArraysRecordElementTypes(t *testing.T) {
	c, _ := newCatalogue(t)

	record := map[string]any{
		"tags": []any{"go", "database", "schemaless"},
	}
	if err := c.Observe("items", record); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	snap := c.Snapshot()
	col := snap.Collections["items"]
	fs, ok := col.Fields["tags"]
	if !ok {
		t.Fatal("field 'tags' not found")
	}
	if !fs.IsArray {
		t.Error("IsArray should be true for 'tags'")
	}
	if fs.ElementTypes["string"] != 3 {
		t.Errorf("ElementTypes[string] = %d, want 3", fs.ElementTypes["string"])
	}
}

// TestTypeConflictDetection verifies that observing the same field with two
// different non-null types sets HasConflict and causes FieldsForCollection to
// return TypeAny.
func TestTypeConflictDetection(t *testing.T) {
	c, _ := newCatalogue(t)

	if err := c.Observe("users", map[string]any{"age": float64(25)}); err != nil {
		t.Fatalf("first Observe: %v", err)
	}
	if err := c.Observe("users", map[string]any{"age": "twenty-five"}); err != nil {
		t.Fatalf("second Observe: %v", err)
	}

	snap := c.Snapshot()
	col := snap.Collections["users"]
	fs, ok := col.Fields["age"]
	if !ok {
		t.Fatal("field 'age' not found")
	}
	if !fs.HasConflict {
		t.Error("HasConflict should be true after seeing 'age' as both number and string")
	}

	fields := c.FieldsForCollection("users")
	f, ok := fields["age"]
	if !ok {
		t.Fatal("FieldsForCollection: field 'age' not returned")
	}
	if f.Type != schema.TypeAny {
		t.Errorf("Type = %q, want %q", f.Type, schema.TypeAny)
	}
	if f.Required {
		t.Error("Required should be false for inferred fields")
	}
}

// TestMissingFieldCounting verifies that MissingCount increments for fields
// absent in later records.
func TestMissingFieldCounting(t *testing.T) {
	c, _ := newCatalogue(t)

	// Record 1: has both fields.
	if err := c.Observe("items", map[string]any{"name": "a", "desc": "first"}); err != nil {
		t.Fatalf("Observe 1: %v", err)
	}
	// Record 2: missing "desc".
	if err := c.Observe("items", map[string]any{"name": "b"}); err != nil {
		t.Fatalf("Observe 2: %v", err)
	}
	// Record 3: missing "name".
	if err := c.Observe("items", map[string]any{"desc": "third"}); err != nil {
		t.Fatalf("Observe 3: %v", err)
	}

	snap := c.Snapshot()
	col := snap.Collections["items"]

	name := col.Fields["name"]
	desc := col.Fields["desc"]

	// "name" present in records 1 and 2; missing in record 3.
	if name.SampleCount != 2 {
		t.Errorf("name.SampleCount = %d, want 2", name.SampleCount)
	}
	if name.MissingCount != 1 {
		t.Errorf("name.MissingCount = %d, want 1", name.MissingCount)
	}

	// "desc" present in records 1 and 3; missing in record 2.
	if desc.SampleCount != 2 {
		t.Errorf("desc.SampleCount = %d, want 2", desc.SampleCount)
	}
	if desc.MissingCount != 1 {
		t.Errorf("desc.MissingCount = %d, want 1", desc.MissingCount)
	}
}

// TestPersistenceRoundTrip verifies that a catalogue loaded after Observe
// contains the same data.
func TestPersistenceRoundTrip(t *testing.T) {
	c, path := newCatalogue(t)

	record := map[string]any{
		"title": "hello",
		"count": float64(42),
	}
	if err := c.Observe("docs", record); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	// Load from same path.
	c2, err := inferred.Load(path)
	if err != nil {
		t.Fatalf("Load after Observe: %v", err)
	}

	snap := c2.Snapshot()
	col, ok := snap.Collections["docs"]
	if !ok {
		t.Fatal("collection 'docs' not found after round-trip")
	}
	if col.RecordCount != 1 {
		t.Errorf("RecordCount = %d, want 1", col.RecordCount)
	}
	if col.Fields["title"].ObservedTypes["string"] != 1 {
		t.Error("title field not persisted correctly")
	}
	if col.Fields["count"].ObservedTypes["number"] != 1 {
		t.Error("count field not persisted correctly")
	}
}

// TestMissingFilePath verifies that Load on a non-existent file returns an
// empty catalogue without error.
func TestMissingFilePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "inferred-schema.json")
	c, err := inferred.Load(path)
	if err != nil {
		t.Fatalf("Load of missing file should not error: %v", err)
	}
	snap := c.Snapshot()
	if len(snap.Collections) != 0 {
		t.Errorf("expected empty catalogue, got %d collections", len(snap.Collections))
	}
}

// TestFieldsForCollectionTypeMapping verifies JSON-type → schema.FieldType
// mapping for all supported types.
func TestFieldsForCollectionTypeMapping(t *testing.T) {
	c, _ := newCatalogue(t)

	records := []map[string]any{
		{"s": "hello"},
		{"n": float64(1.5)},
		{"b": true},
		{"o": map[string]any{"x": 1}},
		{"a": []any{1, 2}},
	}
	for _, r := range records {
		if err := c.Observe("types", r); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}

	fields := c.FieldsForCollection("types")
	tests := []struct {
		field    string
		wantType schema.FieldType
	}{
		{"s", schema.TypeString},
		{"n", schema.TypeNumber},
		{"b", schema.TypeBoolean},
		{"o", schema.TypeObject},
		{"a", schema.TypeArray},
	}
	for _, tt := range tests {
		f, ok := fields[tt.field]
		if !ok {
			t.Errorf("field %q not in FieldsForCollection result", tt.field)
			continue
		}
		if f.Type != tt.wantType {
			t.Errorf("field %q: Type = %q, want %q", tt.field, f.Type, tt.wantType)
		}
		if f.Required {
			t.Errorf("field %q: Required should be false", tt.field)
		}
	}
}

// TestFieldsForCollectionNilOnUnknown verifies that FieldsForCollection returns
// nil for a collection that was never observed.
func TestFieldsForCollectionNilOnUnknown(t *testing.T) {
	c, _ := newCatalogue(t)
	if fields := c.FieldsForCollection("ghost"); fields != nil {
		t.Errorf("expected nil for unknown collection, got %v", fields)
	}
}

// TestFieldsForCollectionTopLevelOnly verifies that dotted (nested) paths are
// excluded from FieldsForCollection output.
func TestFieldsForCollectionTopLevelOnly(t *testing.T) {
	c, _ := newCatalogue(t)

	record := map[string]any{
		"address": map[string]any{
			"city": "Cork",
		},
	}
	if err := c.Observe("contacts", record); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	fields := c.FieldsForCollection("contacts")
	if _, ok := fields["address"]; !ok {
		t.Error("expected top-level field 'address'")
	}
	if _, ok := fields["address.city"]; ok {
		t.Error("nested field 'address.city' should not appear in FieldsForCollection")
	}
}

// TestConcurrentObserve verifies that concurrent Observe calls do not race.
// Run with -race to detect data races.
func TestConcurrentObserve(t *testing.T) {
	c, _ := newCatalogue(t)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			_ = c.Observe("concurrent", map[string]any{
				"index": float64(i),
				"name":  "worker",
			})
		}(i)
	}
	wg.Wait()

	snap := c.Snapshot()
	col, ok := snap.Collections["concurrent"]
	if !ok {
		t.Fatal("collection 'concurrent' not found after concurrent writes")
	}
	if col.RecordCount != goroutines {
		t.Errorf("RecordCount = %d, want %d", col.RecordCount, goroutines)
	}
}

// TestMaxDepthGuard verifies that deeply nested objects do not cause infinite
// recursion (paths beyond maxDepth are silently dropped).
func TestMaxDepthGuard(t *testing.T) {
	c, _ := newCatalogue(t)

	// Build a 10-level deep nested map (exceeds maxDepth=8).
	inner := map[string]any{"leaf": "value"}
	for i := 0; i < 9; i++ {
		inner = map[string]any{"nested": inner}
	}
	if err := c.Observe("deep", inner); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// Just verify no panic or error; we don't mandate exact depth cutoff behaviour.
}

// TestNullHandling verifies that null values are recorded as "null" type.
func TestNullHandling(t *testing.T) {
	c, _ := newCatalogue(t)

	if err := c.Observe("nulls", map[string]any{"maybe": nil}); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	snap := c.Snapshot()
	col := snap.Collections["nulls"]
	fs, ok := col.Fields["maybe"]
	if !ok {
		t.Fatal("field 'maybe' not found")
	}
	if fs.ObservedTypes["null"] != 1 {
		t.Errorf("ObservedTypes[null] = %d, want 1", fs.ObservedTypes["null"])
	}
	// A single null observation should not cause a conflict.
	if fs.HasConflict {
		t.Error("HasConflict should be false with only null observed")
	}
}

// TestNullDoesNotConflict verifies that null + one non-null type is not a conflict.
func TestNullDoesNotConflict(t *testing.T) {
	c, _ := newCatalogue(t)

	_ = c.Observe("mix", map[string]any{"x": nil})
	_ = c.Observe("mix", map[string]any{"x": "hello"})

	snap := c.Snapshot()
	fs := snap.Collections["mix"].Fields["x"]
	if fs.HasConflict {
		t.Error("null + string should not count as a conflict")
	}
}

// TestTimestamps verifies that FirstSeen/LastSeen are populated.
func TestTimestamps(t *testing.T) {
	c, _ := newCatalogue(t)

	if err := c.Observe("ts", map[string]any{"f": "v"}); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	snap := c.Snapshot()
	col := snap.Collections["ts"]
	if col.FirstSeen.IsZero() {
		t.Error("CollectionStats.FirstSeen should not be zero")
	}
	if col.LastSeen.IsZero() {
		t.Error("CollectionStats.LastSeen should not be zero")
	}
	fs := col.Fields["f"]
	if fs.FirstSeen.IsZero() {
		t.Error("FieldStats.FirstSeen should not be zero")
	}
	if fs.LastSeen.IsZero() {
		t.Error("FieldStats.LastSeen should not be zero")
	}
}
