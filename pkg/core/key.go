package core

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/dal-go/dalgo/dal"
)

// ErrNotFound is the server-side not-found sentinel (mapped to HTTP 404).
var ErrNotFound = errors.New("record not found")

// ErrAlreadyExists is the insert-conflict sentinel (mapped to HTTP 409).
var ErrAlreadyExists = errors.New("record already exists")

// ParseKey builds a *dal.Key from already-unescaped path segments
// (alternating collection/id pairs, root first). Segments are validated for
// path-traversal safety before the key is constructed.
func ParseKey(segments ...string) (*dal.Key, error) {
	if len(segments) < 2 || len(segments)%2 != 0 {
		return nil, fmt.Errorf("key must have an even number of segments (collection/id pairs), got %d", len(segments))
	}
	for _, s := range segments {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("invalid key segment %q", s)
		}
	}
	var key *dal.Key
	for i := 0; i+1 < len(segments); i += 2 {
		if key == nil {
			key = dal.NewKeyWithID(segments[i], segments[i+1])
		} else {
			key = dal.NewKeyWithParentAndID(key, segments[i], segments[i+1])
		}
	}
	return key, nil
}

// ParseKeyPath parses a dal-escaped key path ("collection/id[/sub/id...]")
// into a dalgo key: segments are split on '/' and percent-unescaped
// individually, so dal.EscapeID-encoded characters inside IDs survive.
func ParseKeyPath(raw string) (*dal.Key, error) {
	parts := strings.Split(raw, "/")
	segments := make([]string, len(parts))
	for i, part := range parts {
		seg, err := url.PathUnescape(part)
		if err != nil {
			return nil, fmt.Errorf("invalid key segment %q: %w", part, err)
		}
		segments[i] = seg
	}
	return ParseKey(segments...)
}

// collectionChains returns the "/"-joined collection-name chains of a key,
// root-first: spaces/s1/ext/c1 → ["spaces", "spaces/ext"]. Path-form chains
// are what ddl.SchemaModifier implementations (dalgo2ingitdb) accept for
// subcollection creation.
func collectionChains(k *dal.Key) []string {
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
