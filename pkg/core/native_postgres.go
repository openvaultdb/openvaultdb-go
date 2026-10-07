package core

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

type nativePostgresTuple struct{ schema, name string }

func nativePostgresMappings(schemas *schema.Schemas) (map[string]schema.NativeCollectionSource, map[nativePostgresTuple]string, error) {
	if schemas == nil || len(schemas.Collections) == 0 {
		return nil, nil, fmt.Errorf("native PostgreSQL mount requires discovered collections")
	}
	byID := make(map[string]schema.NativeCollectionSource, len(schemas.Collections))
	bySource := make(map[nativePostgresTuple]string, len(schemas.Collections))
	for id, collection := range schemas.Collections {
		if collection.Source == nil {
			return nil, nil, fmt.Errorf("native PostgreSQL collection is missing its source mapping")
		}
		source := *collection.Source
		canonical, err := schema.NativePostgresCollectionID(source.Schema, source.Name)
		if err != nil || canonical != id {
			return nil, nil, fmt.Errorf("native PostgreSQL collection ID does not match its source mapping")
		}
		key := nativePostgresTuple{schema: source.Schema, name: source.Name}
		if _, exists := bySource[key]; exists {
			return nil, nil, fmt.Errorf("native PostgreSQL source mapping is duplicated")
		}
		for field := range collection.Fields {
			if field == "" || len(field) > 63 || !utf8.ValidString(field) || strings.IndexByte(field, 0) >= 0 {
				return nil, nil, fmt.Errorf("native PostgreSQL column name is outside the identifier contract")
			}
		}
		byID[id] = source
		bySource[key] = id
	}
	return byID, bySource, nil
}

// ResolveNativePostgresCollection resolves exact schema and relation names to
// the logical collection ID used by OVDB authorization and catalog routes.
func (d *Database) ResolveNativePostgresCollection(schemaName, relationName string) (string, bool) {
	if !d.nativePostgres {
		return "", false
	}
	id, ok := d.nativeIDs[nativePostgresTuple{schema: schemaName, name: relationName}]
	return id, ok
}

func (d *Database) nativePostgresSource(id string) (schema.NativeCollectionSource, bool) {
	if !d.nativePostgres {
		return schema.NativeCollectionSource{}, false
	}
	source, ok := d.nativeSources[id]
	return source, ok
}

// NativePostgresSource returns the original physical schema and relation name
// for a logical collection ID in a native PostgreSQL mount.
func (d *Database) NativePostgresSource(id string) (schema.NativeCollectionSource, bool) {
	return d.nativePostgresSource(id)
}

func (d *Database) NativePostgresReadOnly() bool { return d.nativePostgres }

func validateNativeIdentifier(value string) error {
	if value == "" || len(value) > 63 || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: PostgreSQL identifier is not valid UTF-8, is empty, contains NUL, or exceeds 63 bytes", ErrInvalidDTQL)
	}
	return nil
}
