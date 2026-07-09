// Package inferred implements the inferred schema catalogue for OpenVaultDB.
//
// When data is written in partial or schemaless mode, OpenVaultDB observes and
// records inferred schema metadata. "Schemaless means no required pre-declared
// schema. It does not mean no schema information."
//
// The catalogue is persisted to a JSON file after each [Catalogue.Observe] call.
// Persistence is synchronous and uses an atomic temp-file + os.Rename pattern to
// avoid partial writes. This is deliberately simple for MVP: a future iteration
// could batch writes or use a write-ahead log to reduce fsync overhead on
// high-write workloads.
package inferred

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// maxDepth guards against pathological deeply-nested documents during field
// path recursion.
const maxDepth = 8

// FieldStats holds inferred type statistics for a single field path within a
// collection.
type FieldStats struct {
	// ObservedTypes maps JSON type name ("string","number","boolean","object",
	// "array","null") to the count of times that type was observed for this field.
	ObservedTypes map[string]int `json:"observed_types"`

	// IsArray is true when the field itself (not an element) is always or
	// sometimes an array.
	IsArray bool `json:"is_array,omitempty"`

	// ElementTypes maps JSON type names to counts for element values observed
	// within array values of this field.
	ElementTypes map[string]int `json:"element_types,omitempty"`

	// FirstSeen is the UTC time of the first observation of this field.
	FirstSeen time.Time `json:"first_seen"`

	// LastSeen is the UTC time of the most recent observation of this field.
	LastSeen time.Time `json:"last_seen"`

	// SampleCount is the number of collection records in which this field was
	// present.
	SampleCount int `json:"sample_count"`

	// MissingCount is the number of collection records observed that did NOT
	// contain this field.
	MissingCount int `json:"missing_count"`

	// HasConflict is true when more than one non-null type was observed for this
	// field across all records.
	HasConflict bool `json:"has_conflict,omitempty"`
}

// CollectionStats holds aggregate statistics for a single collection.
type CollectionStats struct {
	// Fields maps a dotted field path (e.g. "address.city") to its statistics.
	Fields map[string]*FieldStats `json:"fields"`

	// RecordCount is the total number of records observed for this collection.
	RecordCount int `json:"record_count"`

	// FirstSeen is the UTC time of the first record observed for this collection.
	FirstSeen time.Time `json:"first_seen"`

	// LastSeen is the UTC time of the most recent record observed for this
	// collection.
	LastSeen time.Time `json:"last_seen"`
}

// Snapshot is a deep-copyable, JSON-serializable view of the entire catalogue,
// suitable for serving over the HTTP API.
type Snapshot struct {
	Collections map[string]*CollectionStats `json:"collections"`
}

// catalogueData is the JSON-serializable backing store for [Catalogue].
type catalogueData struct {
	Collections map[string]*CollectionStats `json:"collections"`
}

// Catalogue is a thread-safe inferred schema catalogue. It accumulates field
// type observations across writes and persists them to a JSON file.
type Catalogue struct {
	mu       sync.Mutex
	filePath string
	data     catalogueData
}

// Load reads the catalogue JSON from filePath. A missing file yields an empty
// catalogue (not an error). Corrupt or unreadable files are returned as errors.
func Load(filePath string) (*Catalogue, error) {
	c := &Catalogue{
		filePath: filePath,
		data: catalogueData{
			Collections: make(map[string]*CollectionStats),
		},
	}

	f, err := os.Open(filePath)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inferred: open catalogue %q: %w", filePath, err)
	}
	defer func() { _ = f.Close() }()

	if err := json.NewDecoder(f).Decode(&c.data); err != nil {
		return nil, fmt.Errorf("inferred: decode catalogue %q: %w", filePath, err)
	}
	if c.data.Collections == nil {
		c.data.Collections = make(map[string]*CollectionStats)
	}
	return c, nil
}

// Observe records one written record's shape into the catalogue and persists
// the updated catalogue to disk atomically.
//
// MVP trade-off: persistence is synchronous — every Observe call writes the
// full JSON file via temp-file + os.Rename. This is safe and simple but adds
// latency on each write. A future iteration could batch flushes or use a
// write-ahead log.
func (c *Catalogue) Observe(collection string, data map[string]any) error {
	now := time.Now().UTC()

	c.mu.Lock()
	defer c.mu.Unlock()

	col, ok := c.data.Collections[collection]
	if !ok {
		col = &CollectionStats{
			Fields:    make(map[string]*FieldStats),
			FirstSeen: now,
		}
		c.data.Collections[collection] = col
	}

	col.RecordCount++
	col.LastSeen = now
	if col.FirstSeen.IsZero() {
		col.FirstSeen = now
	}

	// Track which field paths appear in this record so we can increment
	// MissingCount for fields seen in prior records but absent here.
	presentPaths := make(map[string]bool)
	observeFields(col, data, "", 0, now, presentPaths)

	// Increment MissingCount for every known field not present in this record.
	for path, fs := range col.Fields {
		if !presentPaths[path] {
			fs.MissingCount++
		}
	}

	return c.persist()
}

// Snapshot returns a deep-copyable, JSON-serializable view of the catalogue.
// The caller may safely read the returned value without holding any lock.
func (c *Catalogue) Snapshot() *Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Deep-copy via JSON round-trip: simple, correct, and adequate for MVP.
	b, _ := json.Marshal(&c.data)
	var snap Snapshot
	_ = json.Unmarshal(b, &snap)
	if snap.Collections == nil {
		snap.Collections = make(map[string]*CollectionStats)
	}
	return &snap
}

// FieldsForCollection derives [schema.Field] declarations from observations for
// the named collection's top-level fields only.
//
// Rules:
//   - All returned fields have Required=false (inferred fields are never required).
//   - Type is the single observed non-null type mapped to a [schema.FieldType], or
//     [schema.TypeAny] when conflicting types were observed.
//   - Returns nil if the collection was never observed.
func (c *Catalogue) FieldsForCollection(collection string) map[string]schema.Field {
	c.mu.Lock()
	defer c.mu.Unlock()

	col, ok := c.data.Collections[collection]
	if !ok {
		return nil
	}

	result := make(map[string]schema.Field)
	for path, fs := range col.Fields {
		// Top-level fields only: no dots in the path.
		if containsDot(path) {
			continue
		}
		result[path] = schema.Field{
			Type:     resolveFieldType(fs),
			Required: false,
		}
	}
	return result
}

// observeFields recursively traverses data, recording field statistics into col.
// path is the dotted prefix for nested fields; depth guards against infinite
// recursion on pathological documents (limit: maxDepth).
// presentPaths accumulates the set of dotted field paths seen in this record.
func observeFields(
	col *CollectionStats,
	data map[string]any,
	prefix string,
	depth int,
	now time.Time,
	presentPaths map[string]bool,
) {
	if depth >= maxDepth {
		return
	}
	for key, val := range data {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}

		presentPaths[path] = true

		fs, exists := col.Fields[path]
		if !exists {
			fs = &FieldStats{
				ObservedTypes: make(map[string]int),
				FirstSeen:     now,
			}
			col.Fields[path] = fs
			// Existing records (RecordCount-1 before this one) did not have this
			// field: set MissingCount to account for them.
			fs.MissingCount = col.RecordCount - 1
		}
		fs.SampleCount++
		fs.LastSeen = now

		typeName := jsonTypeName(val)
		fs.ObservedTypes[typeName]++

		if typeName == "array" {
			fs.IsArray = true
			if fs.ElementTypes == nil {
				fs.ElementTypes = make(map[string]int)
			}
			if arr, ok := val.([]any); ok {
				for _, elem := range arr {
					fs.ElementTypes[jsonTypeName(elem)]++
				}
			}
		}

		// Recurse into objects.
		if typeName == "object" {
			if nested, ok := val.(map[string]any); ok {
				observeFields(col, nested, path, depth+1, now, presentPaths)
			}
		}

		// Update conflict flag: conflict if more than one non-null type seen.
		fs.HasConflict = nonNullTypeCount(fs.ObservedTypes) > 1
	}
}

// jsonTypeName returns the JSON schema type name for a Go value decoded from
// JSON (i.e. as produced by json.Unmarshal into map[string]any).
func jsonTypeName(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case bool:
		return "boolean"
	case float64, json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		// Handles int, int64, etc. passed directly (non-JSON paths).
		return "number"
	}
}

// nonNullTypeCount returns the number of distinct type names in types that are
// not "null".
func nonNullTypeCount(types map[string]int) int {
	n := 0
	for t, cnt := range types {
		if t != "null" && cnt > 0 {
			n++
		}
	}
	return n
}

// resolveFieldType maps the observed types in fs to a single schema.FieldType.
// Returns schema.TypeAny when conflicting non-null types were observed.
func resolveFieldType(fs *FieldStats) schema.FieldType {
	if fs.HasConflict {
		return schema.TypeAny
	}
	// Find the single non-null type (if any).
	for typeName, cnt := range fs.ObservedTypes {
		if typeName == "null" || cnt == 0 {
			continue
		}
		switch typeName {
		case "string":
			return schema.TypeString
		case "number":
			return schema.TypeNumber
		case "boolean":
			return schema.TypeBoolean
		case "object":
			return schema.TypeObject
		case "array":
			return schema.TypeArray
		}
	}
	// Only null observed, or no observations: fall back to TypeAny.
	return schema.TypeAny
}

// containsDot reports whether s contains a dot character.
func containsDot(s string) bool {
	for _, r := range s {
		if r == '.' {
			return true
		}
	}
	return false
}

// persist serialises the catalogue to c.filePath atomically via a temp file.
// The caller must hold c.mu.
func (c *Catalogue) persist() error {
	dir := filepath.Dir(c.filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("inferred: create catalogue dir %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".inferred-schema-*.tmp")
	if err != nil {
		return fmt.Errorf("inferred: create temp file: %w", err)
	}
	tmpName := tmp.Name()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(&c.data); encErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("inferred: encode catalogue: %w", encErr)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("inferred: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, c.filePath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("inferred: rename catalogue to %q: %w", c.filePath, err)
	}
	return nil
}
