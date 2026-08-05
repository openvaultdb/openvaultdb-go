package core

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record/update"
)

// toDalUpdates converts wire update ops into dalgo update.Update values so
// they pass through natively to the driver.
func toDalUpdates(ops []UpdateOp) ([]update.Update, error) {
	updates := make([]update.Update, 0, len(ops))
	for i, u := range ops {
		byPath := len(u.FieldPath) > 0
		if u.FieldName != "" && byPath {
			return nil, fmt.Errorf("update %d: fieldName and fieldPath are mutually exclusive", i)
		}
		if u.FieldName == "" && !byPath {
			return nil, fmt.Errorf("update %d: one of fieldName or fieldPath is required", i)
		}
		var value any
		switch {
		case u.Delete:
			value = update.DeleteField
		case u.ServerTimestamp:
			value = update.ServerTimestamp
		case u.Transform == "increment":
			n, ok := toFloat(u.Value)
			if !ok {
				return nil, fmt.Errorf("update %d: increment value must be a number, got %T", i, u.Value)
			}
			value = dal.Increment(int(n))
		case u.Transform != "":
			return nil, fmt.Errorf("update %d: unknown transform %q", i, u.Transform)
		default:
			value = u.Value
		}
		if byPath {
			updates = append(updates, update.ByFieldPath(update.FieldPath(u.FieldPath), value))
		} else {
			updates = append(updates, update.ByFieldName(u.FieldName, value))
		}
	}
	return updates, nil
}

// applyUpdates mutates data in place — used only to SIMULATE updates for
// pre-flight validation (insert conflicts, strict/partial schema checks)
// before ops pass through to the driver. The driver applies the real updates.
func applyUpdates(data map[string]any, updates []UpdateOp, now time.Time) error {
	for i, u := range updates {
		path := u.FieldPath
		if u.FieldName != "" {
			if len(u.FieldPath) > 0 {
				return fmt.Errorf("update %d: fieldName and fieldPath are mutually exclusive", i)
			}
			path = []string{u.FieldName}
		}
		if len(path) == 0 {
			return fmt.Errorf("update %d: one of fieldName or fieldPath is required", i)
		}
		for _, seg := range path {
			if seg == "" {
				return fmt.Errorf("update %d: empty field path segment", i)
			}
		}
		switch {
		case u.Delete:
			deleteAtPath(data, path)
		case u.ServerTimestamp:
			if err := setAtPath(data, path, now.Format(time.RFC3339Nano)); err != nil {
				return fmt.Errorf("update %d: %w", i, err)
			}
		case u.Transform == "increment":
			if err := incrementAtPath(data, path, u.Value); err != nil {
				return fmt.Errorf("update %d: %w", i, err)
			}
		case u.Transform != "":
			return fmt.Errorf("update %d: unknown transform %q", i, u.Transform)
		default:
			if err := setAtPath(data, path, u.Value); err != nil {
				return fmt.Errorf("update %d: %w", i, err)
			}
		}
	}
	return nil
}

// parentAtPath walks to the map holding the last path segment, creating
// intermediate maps when create is true. Returns nil when the path traverses
// a non-map value and create is false.
func parentAtPath(data map[string]any, path []string, create bool) (map[string]any, error) {
	m := data
	for _, seg := range path[:len(path)-1] {
		v, ok := m[seg]
		if !ok || v == nil {
			if !create {
				return nil, nil
			}
			child := map[string]any{}
			m[seg] = child
			m = child
			continue
		}
		child, ok := v.(map[string]any)
		if !ok {
			if !create {
				return nil, nil
			}
			return nil, fmt.Errorf("field path segment %q is not an object (got %T)", seg, v)
		}
		m = child
	}
	return m, nil
}

func setAtPath(data map[string]any, path []string, value any) error {
	m, err := parentAtPath(data, path, true)
	if err != nil {
		return err
	}
	m[path[len(path)-1]] = value
	return nil
}

func deleteAtPath(data map[string]any, path []string) {
	m, _ := parentAtPath(data, path, false)
	if m != nil {
		delete(m, path[len(path)-1])
	}
}

func incrementAtPath(data map[string]any, path []string, delta any) error {
	d, ok := toFloat(delta)
	if !ok {
		return fmt.Errorf("increment value must be a number, got %T", delta)
	}
	m, err := parentAtPath(data, path, true)
	if err != nil {
		return err
	}
	leaf := path[len(path)-1]
	current := 0.0
	if v, exists := m[leaf]; exists && v != nil {
		if current, ok = toFloat(v); !ok {
			return fmt.Errorf("cannot increment non-numeric field %q (got %T)", leaf, v)
		}
	}
	m[leaf] = current + d
	return nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
