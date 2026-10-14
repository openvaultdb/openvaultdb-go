package core

import (
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/dal-go/dalgo/dal"
)

// nativeIntegerRanges are the values each PostgreSQL integer type holds.
var nativeIntegerRanges = map[string]struct{ low, high int64 }{
	"smallint": {math.MinInt16, math.MaxInt16},
	"int2":     {math.MinInt16, math.MaxInt16},
	"integer":  {math.MinInt32, math.MaxInt32},
	"int4":     {math.MinInt32, math.MaxInt32},
	"bigint":   {math.MinInt64, math.MaxInt64},
	"int8":     {math.MinInt64, math.MaxInt64},
}

// nativeValue refuses, on a native PostgreSQL mount, a comparison of a column with
// a value that PostgreSQL would answer with no rows instead of an error, so that
// every value a column cannot hold is the same 400 invalid_dtql:
//
//   - on a smallint, integer or bigint column, a number that is not a whole number,
//     or is outside the range of the column, and a boolean. PostgreSQL compares a
//     bigint with 1.5 as numerics and finds nothing, while it refuses 'abc'; a text
//     value is left to PostgreSQL, which refuses one that is not an integer.
//   - on a bytea column, any value: the column is returned as base64 text, and a
//     comparison with that text would compare bytes with the bytes of the text.
//
// A comparison of two fields, a field with a parameter left unbound, and a field
// the walk cannot attribute to one source are not looked at.
func (w fieldWalk) nativeValue(c dal.Comparison) error {
	if !w.db.nativePostgres {
		return nil
	}
	field, value, ok := fieldAndValue(c.Left, c.Right)
	if !ok {
		if field, value, ok = fieldAndValue(c.Right, c.Left); !ok {
			return nil
		}
	}
	nativeType := w.nativeType(field)
	if nativeType == "bytea" {
		return fmt.Errorf("%w: field %q is a bytea column, which a filter cannot compare with a value", ErrInvalidDTQL, clipName(field.Name()))
	}
	limits, integer := nativeIntegerRanges[nativeType]
	if !integer {
		return nil
	}
	for _, item := range value {
		if !nativeIntegerValue(item, limits.low, limits.high) {
			return fmt.Errorf("%w: field %q is a %s column, and a value compared with it must be a whole number it can hold", ErrInvalidDTQL, clipName(field.Name()), nativeType)
		}
	}
	return nil
}

// fieldAndValue returns the field of a comparison whose other side is a value,
// and the values: one for a constant, each item for an array.
func fieldAndValue(left, right dal.Expression) (dal.FieldRef, []any, bool) {
	field, ok := left.(dal.FieldRef)
	if !ok {
		return dal.FieldRef{}, nil, false
	}
	switch value := right.(type) {
	case dal.Constant:
		return field, []any{value.Value}, true
	case dal.Array:
		items := reflect.ValueOf(value.Value)
		if items.Kind() != reflect.Slice && items.Kind() != reflect.Array {
			return field, nil, false
		}
		values := make([]any, items.Len())
		for i := range values {
			values[i] = items.Index(i).Interface()
		}
		return field, values, true
	}
	return field, nil, false
}

// nativeType is the declared PostgreSQL type of the column a field names, in lower
// case, or empty when the walk cannot attribute the field to one source.
func (w fieldWalk) nativeType(field dal.FieldRef) string {
	source := w.single
	if qualifier := field.Source(); qualifier != "" {
		found, ok := w.scope.find(qualifier)
		if !ok {
			return ""
		}
		source = &found
	}
	if source == nil {
		return ""
	}
	collection := w.db.schemaCollection(source.collection)
	if collection == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(collection.Fields[field.Name()].NativeType))
}

// nativeIntegerValue reports whether value is one an integer column of the range
// [low, high] can be compared with without PostgreSQL turning the comparison into
// one of numerics: a whole number in the range, or text (which PostgreSQL parses,
// and refuses when it is not an integer).
func nativeIntegerValue(value any, low, high int64) bool {
	switch v := value.(type) {
	case nil, string:
		return true
	case bool:
		return false
	case float32:
		return wholeInRange(float64(v), low, high)
	case float64:
		return wholeInRange(v, low, high)
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := reflected.Int()
		return n >= low && n <= high
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return reflected.Uint() <= uint64(high)
	}
	return true
}

// wholeInRange reports whether v is a whole number in [low, high]. The bounds are
// compared as float64: float64(high)+1 is the first float above the range (2^63
// for bigint, whose own maximum has no float64 of its own).
func wholeInRange(v float64, low, high int64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Trunc(v) == v && v >= float64(low) && v < float64(high)+1
}
