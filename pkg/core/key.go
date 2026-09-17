package core

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/dal-go/record"
)

// ErrNotFound is the server-side not-found sentinel (mapped to HTTP 404).
var ErrNotFound = errors.New("record not found")

// ErrAlreadyExists is the insert-conflict sentinel (mapped to HTTP 409).
var ErrAlreadyExists = errors.New("record already exists")

// ErrInvalidKey identifies a malformed or unsafe key, collection name or
// parent path (mapped to HTTP 400 invalid_key).
var ErrInvalidKey = errors.New("invalid key")

// ValidateSegment checks one decoded key segment (collection name or record
// id) for path-traversal safety on every engine. A segment must be non-empty,
// must not be "." or "..", must not contain control characters
// (U+0000–U+001F, U+007F), and — because IDs may legitimately carry an
// escaped '/' — none of its '/'- or '\'-separated components may be "." or
// "..": file-backed engines join segments into filesystem paths, so
// "../../secrets" inside an id would escape its collection.
func ValidateSegment(s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty segment", ErrInvalidKey)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: segment %q contains a control character", ErrInvalidKey, s)
		}
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return fmt.Errorf("%w: segment %q is only path separators", ErrInvalidKey, s)
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return fmt.Errorf("%w: segment %q contains a relative path component", ErrInvalidKey, s)
		}
	}
	return nil
}

// ValidateCollectionName applies ValidateSegment to a collection name taken
// from a request body (query collection, DTQL source).
func ValidateCollectionName(name string) error {
	return ValidateSegment(name)
}

// ParseKey builds a *record.Key from already-unescaped path segments
// (alternating collection/id pairs, root first). Segments are validated for
// path-traversal safety (ValidateSegment) before the key is constructed.
func ParseKey(segments ...string) (*record.Key, error) {
	if len(segments) < 2 || len(segments)%2 != 0 {
		return nil, fmt.Errorf("%w: key must have an even number of segments (collection/id pairs), got %d", ErrInvalidKey, len(segments))
	}
	for _, s := range segments {
		if err := ValidateSegment(s); err != nil {
			return nil, err
		}
	}
	var key *record.Key
	for i := 0; i+1 < len(segments); i += 2 {
		if key == nil {
			key = record.NewKeyWithID(segments[i], segments[i+1])
		} else {
			key = record.NewKeyWithParentAndID(key, segments[i], segments[i+1])
		}
	}
	return key, nil
}

// RootCollection returns the root collection name of key: the collection a
// per-collection capability scopes, and the top-level directory/table the
// driver writes under.
func RootCollection(key *record.Key) string {
	root := key
	for cur := key; cur != nil; cur = cur.Parent() {
		root = cur
	}
	return root.Collection()
}

// ParseKeyPath parses a dal-escaped key path ("collection/id[/sub/id...]")
// into a dalgo key: segments are split on '/' and percent-unescaped
// individually, so dal.EscapeID-encoded characters inside IDs survive.
func ParseKeyPath(raw string) (*record.Key, error) {
	parts := strings.Split(raw, "/")
	segments := make([]string, len(parts))
	for i, part := range parts {
		seg, err := url.PathUnescape(part)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid key segment %q: %v", ErrInvalidKey, part, err)
		}
		segments[i] = seg
	}
	return ParseKey(segments...)
}

// collectionChains returns the "/"-joined collection-name chains of a key,
// root-first: spaces/s1/ext/c1 → ["spaces", "spaces/ext"]. Path-form chains
// are what ddl.SchemaModifier implementations (dalgo2ingitdb) accept for
// subcollection creation.
func collectionChains(k *record.Key) []string {
	var names []string
	for cur := k; cur != nil; cur = cur.Parent() {
		names = append([]string{cur.Collection()}, names...)
	}
	chains := make([]string, 0, len(names))
	path := ""
	for _, name := range names {
		if path == "" {
			path = name
		} else {
			path += "/" + name
		}
		chains = append(chains, path)
	}
	return chains
}
