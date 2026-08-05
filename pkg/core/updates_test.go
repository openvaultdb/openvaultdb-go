package core

import (
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record/update"
)

// ---------- applyUpdates tests ----------

func TestApplyUpdates_SetNestedPathCreatesIntermediates(t *testing.T) {
	data := map[string]any{
		"a": map[string]any{"b": 1},
	}
	ops := []UpdateOp{
		{FieldPath: []string{"a", "c", "d"}, Value: "hello"},
	}
	if err := applyUpdates(data, ops, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	aMap, ok := data["a"].(map[string]any)
	if !ok {
		t.Fatal("data[\"a\"] is not a map")
	}
	cMap, ok := aMap["c"].(map[string]any)
	if !ok {
		t.Fatal("data[\"a\"][\"c\"] is not a map")
	}
	if got := cMap["d"]; got != "hello" {
		t.Errorf("expected %q, got %v", "hello", got)
	}
}

func TestApplyUpdates_DeleteTopLevelField(t *testing.T) {
	data := map[string]any{"x": 1, "y": 2}
	ops := []UpdateOp{
		{FieldName: "x", Delete: true},
	}
	if err := applyUpdates(data, ops, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, exists := data["x"]; exists {
		t.Error("expected \"x\" to be deleted")
	}
	if data["y"] != 2 {
		t.Errorf("expected data[\"y\"] == 2, got %v", data["y"])
	}
}

func TestApplyUpdates_DeleteNestedField(t *testing.T) {
	data := map[string]any{
		"a": map[string]any{"b": 1},
	}
	ops := []UpdateOp{
		{FieldPath: []string{"a", "b"}, Delete: true},
	}
	if err := applyUpdates(data, ops, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	aMap, ok := data["a"].(map[string]any)
	if !ok {
		t.Fatal("data[\"a\"] is not a map")
	}
	if _, exists := aMap["b"]; exists {
		t.Error("expected \"b\" to be deleted")
	}
}

func TestApplyUpdates_DeleteMissingFieldIsNoOp(t *testing.T) {
	data := map[string]any{"x": 1}
	ops := []UpdateOp{
		{FieldName: "missing", Delete: true},
	}
	if err := applyUpdates(data, ops, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["x"] != 1 {
		t.Errorf("expected data[\"x\"] == 1, got %v", data["x"])
	}
}

func TestApplyUpdates_IncrementOnMissingField(t *testing.T) {
	data := map[string]any{}
	ops := []UpdateOp{
		{FieldName: "count", Transform: "increment", Value: float64(5)},
	}
	if err := applyUpdates(data, ops, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["count"] != 5.0 {
		t.Errorf("expected 5.0, got %v", data["count"])
	}
}

func TestApplyUpdates_IncrementOnExistingNumericField(t *testing.T) {
	data := map[string]any{"score": 10.0}
	ops := []UpdateOp{
		{FieldName: "score", Transform: "increment", Value: float64(3)},
	}
	if err := applyUpdates(data, ops, time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if data["score"] != 13.0 {
		t.Errorf("expected 13.0, got %v", data["score"])
	}
}

func TestApplyUpdates_IncrementOnNonNumericFieldReturnsError(t *testing.T) {
	data := map[string]any{"name": "alice"}
	ops := []UpdateOp{
		{FieldName: "name", Transform: "increment", Value: float64(1)},
	}
	if err := applyUpdates(data, ops, time.Now()); err == nil {
		t.Error("expected an error when incrementing a non-numeric field")
	}
}

func TestApplyUpdates_ServerTimestampSetsRFC3339NanoString(t *testing.T) {
	now := time.Date(2024, 6, 15, 12, 30, 45, 123456789, time.UTC)
	data := map[string]any{}
	ops := []UpdateOp{
		{FieldName: "updatedAt", ServerTimestamp: true},
	}
	if err := applyUpdates(data, ops, now); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw, ok := data["updatedAt"].(string)
	if !ok {
		t.Fatalf("expected string, got %T", data["updatedAt"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("value is not a valid RFC3339Nano string: %v", err)
	}
	if !parsed.Equal(now) {
		t.Errorf("expected %v, got %v", now, parsed)
	}
	if raw != now.Format(time.RFC3339Nano) {
		t.Errorf("expected %q, got %q", now.Format(time.RFC3339Nano), raw)
	}
}

func TestApplyUpdates_FieldNameAndFieldPathBothSetReturnsError(t *testing.T) {
	data := map[string]any{}
	ops := []UpdateOp{
		{FieldName: "x", FieldPath: []string{"x"}, Value: 1},
	}
	if err := applyUpdates(data, ops, time.Now()); err == nil {
		t.Error("expected an error when both fieldName and fieldPath are set")
	}
}

func TestApplyUpdates_UnknownTransformReturnsError(t *testing.T) {
	data := map[string]any{}
	ops := []UpdateOp{
		{FieldName: "x", Transform: "multiply", Value: 2},
	}
	if err := applyUpdates(data, ops, time.Now()); err == nil {
		t.Error("expected an error for unknown transform")
	}
}

// ---------- toDalUpdates tests ----------

func TestToDalUpdates_DeleteFieldSentinel(t *testing.T) {
	ops := []UpdateOp{
		{FieldName: "x", Delete: true},
	}
	results, err := toDalUpdates(ops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 update, got %d", len(results))
	}
	if results[0].Value() != update.DeleteField {
		t.Errorf("expected update.DeleteField, got %v", results[0].Value())
	}
}

func TestToDalUpdates_ServerTimestampSentinel(t *testing.T) {
	ops := []UpdateOp{
		{FieldName: "ts", ServerTimestamp: true},
	}
	results, err := toDalUpdates(ops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 update, got %d", len(results))
	}
	if results[0].Value() != update.ServerTimestamp {
		t.Errorf("expected update.ServerTimestamp, got %v", results[0].Value())
	}
}

func TestToDalUpdates_IncrementTransformViaDalIsTransform(t *testing.T) {
	ops := []UpdateOp{
		{FieldName: "c", Transform: "increment", Value: float64(5)},
	}
	results, err := toDalUpdates(ops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 update, got %d", len(results))
	}
	tr, ok := dal.IsTransform(results[0].Value())
	if !ok {
		t.Fatalf("expected a dal.Transform, got %T", results[0].Value())
	}
	if tr.Name() != "increment" {
		t.Errorf("expected transform Name() == \"increment\", got %q", tr.Name())
	}
}

func TestToDalUpdates_PlainValueByFieldName(t *testing.T) {
	ops := []UpdateOp{
		{FieldName: "x", Value: "hello"},
	}
	results, err := toDalUpdates(ops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 update, got %d", len(results))
	}
	if results[0].FieldName() != "x" {
		t.Errorf("expected FieldName() == \"x\", got %q", results[0].FieldName())
	}
	if results[0].Value() != "hello" {
		t.Errorf("expected Value() == \"hello\", got %v", results[0].Value())
	}
}

func TestToDalUpdates_PlainValueByFieldPath(t *testing.T) {
	ops := []UpdateOp{
		{FieldPath: []string{"a", "b"}, Value: 42},
	}
	results, err := toDalUpdates(ops)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 update, got %d", len(results))
	}
	fp := results[0].FieldPath()
	if len(fp) != 2 || fp[0] != "a" || fp[1] != "b" {
		t.Errorf("expected FieldPath() == [\"a\",\"b\"], got %v", fp)
	}
	if results[0].Value() != 42 {
		t.Errorf("expected Value() == 42, got %v", results[0].Value())
	}
}

func TestToDalUpdates_FieldNameAndFieldPathReturnsError(t *testing.T) {
	ops := []UpdateOp{
		{FieldName: "x", FieldPath: []string{"x"}, Value: 1},
	}
	if _, err := toDalUpdates(ops); err == nil {
		t.Error("expected an error when both fieldName and fieldPath are set")
	}
}

func TestToDalUpdates_NeitherFieldNameNorFieldPathReturnsError(t *testing.T) {
	ops := []UpdateOp{
		{Value: 1},
	}
	if _, err := toDalUpdates(ops); err == nil {
		t.Error("expected an error when neither fieldName nor fieldPath is set")
	}
}

func TestToDalUpdates_IncrementWithNonNumericValueReturnsError(t *testing.T) {
	ops := []UpdateOp{
		{FieldName: "c", Transform: "increment", Value: "five"},
	}
	if _, err := toDalUpdates(ops); err == nil {
		t.Error("expected an error for increment with non-numeric value")
	}
}

func TestToDalUpdates_UnknownTransformReturnsError(t *testing.T) {
	ops := []UpdateOp{
		{FieldName: "x", Transform: "multiply"},
	}
	if _, err := toDalUpdates(ops); err == nil {
		t.Error("expected an error for unknown transform")
	}
}
