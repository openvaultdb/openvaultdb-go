package dalgo2ovdb

import (
	"encoding/json"
	"fmt"

	"github.com/dal-go/dalgo/dal"
)

// notFoundErr wraps a collection+id pair as a dal "not found" error, the same
// sentinel dal.IsNotFound(err) checks for.
func notFoundErr(collection, id string) error {
	return dal.NewErrNotFoundByKey(dal.NewKeyWithID(collection, id), nil)
}

// idAsString converts a dal.Key.ID value (any comparable) to its string form,
// reporting ok=false for a nil or empty id (an incomplete key).
func idAsString(id any) (string, bool) {
	if id == nil {
		return "", false
	}
	s, ok := id.(string)
	if !ok {
		s = fmt.Sprintf("%v", id)
	}
	return s, s != ""
}

// recordToBody converts a dal.Record's Data() into the map[string]any body
// OVDB's JSON REST API expects. It accepts either a map[string]any directly
// (the common case for opaque records, e.g. store.Record in ovdb-server
// itself) or any other value that marshals to a JSON object (e.g. a tagged
// struct), mirroring the flexibility dalgo2firestore gets for free from
// firestore.DataTo/CollectionRef.Doc.Set.
func recordToBody(data any) (map[string]any, error) {
	if m, ok := data.(map[string]any); ok {
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out, nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: marshal record data (%T): %w", data, err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("dalgo2ovdb: record data (%T) must marshal to a JSON object: %w", data, err)
	}
	return out, nil
}

// unmarshalInto copies an OVDB record row (as returned by restClient.list,
// with its "id" field still attached) into a dal.Record's Data() target,
// dropping "id" (the record's key, not a stored field — same convention
// ovdb-server's own store.go uses internally).
//
// It special-cases data being a map[string]any (mutating it in place, so a
// record reused across calls — e.g. in GetMulti — does not leak stale keys
// from a previous row) and falls back to a JSON marshal/unmarshal round trip
// for any other target (e.g. a pointer to a tagged struct), the same
// flexibility DataTo-style APIs offer.
func unmarshalInto(row map[string]any, data any) error {
	body := make(map[string]any, len(row))
	for k, v := range row {
		if k == "id" {
			continue
		}
		body[k] = v
	}
	if target, ok := data.(map[string]any); ok {
		for k := range target {
			delete(target, k)
		}
		for k, v := range body {
			target[k] = v
		}
		return nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("dalgo2ovdb: marshal record row: %w", err)
	}
	if err := json.Unmarshal(b, data); err != nil {
		return fmt.Errorf("dalgo2ovdb: unmarshal record row into %T: %w", data, err)
	}
	return nil
}
