package schema

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"unicode/utf8"
)

const nativePostgresCollectionPrefix = "pg1_"

var errNativePostgresCollectionID = errors.New("invalid native PostgreSQL collection ID")

// NativePostgresCollectionID encodes the exact UTF-8 schema and relation name
// as a length-prefixed tuple. The ID is logical; SQL code must use the two
// original identifiers separately so the PostgreSQL driver can quote them.
func NativePostgresCollectionID(schemaName, relationName string) (string, error) {
	if !validNativePostgresIdentifier(schemaName) || !validNativePostgresIdentifier(relationName) {
		return "", errNativePostgresCollectionID
	}
	buf := make([]byte, 8+len(schemaName)+len(relationName))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(schemaName)))
	copy(buf[4:], schemaName)
	nameOffset := 4 + len(schemaName)
	binary.BigEndian.PutUint32(buf[nameOffset:nameOffset+4], uint32(len(relationName)))
	copy(buf[nameOffset+4:], relationName)
	return nativePostgresCollectionPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// ParseNativePostgresCollectionID decodes an ID produced by
// NativePostgresCollectionID. Non-canonical or malformed encodings are refused.
func ParseNativePostgresCollectionID(id string) (schemaName, relationName string, err error) {
	if len(id) <= len(nativePostgresCollectionPrefix) || id[:len(nativePostgresCollectionPrefix)] != nativePostgresCollectionPrefix {
		return "", "", errNativePostgresCollectionID
	}
	buf, decodeErr := base64.RawURLEncoding.DecodeString(id[len(nativePostgresCollectionPrefix):])
	if decodeErr != nil || len(buf) < 8 || nativePostgresCollectionPrefix+base64.RawURLEncoding.EncodeToString(buf) != id {
		return "", "", errNativePostgresCollectionID
	}
	schemaLength := uint64(binary.BigEndian.Uint32(buf[:4]))
	if schemaLength == 0 || schemaLength > uint64(len(buf)-8) {
		return "", "", errNativePostgresCollectionID
	}
	nameOffset := 4 + int(schemaLength)
	nameLength := uint64(binary.BigEndian.Uint32(buf[nameOffset : nameOffset+4]))
	if nameLength == 0 || uint64(len(buf)-nameOffset-4) != nameLength {
		return "", "", errNativePostgresCollectionID
	}
	schemaName = string(buf[4:nameOffset])
	relationName = string(buf[nameOffset+4:])
	if !validNativePostgresIdentifier(schemaName) || !validNativePostgresIdentifier(relationName) {
		return "", "", errNativePostgresCollectionID
	}
	return schemaName, relationName, nil
}

// validNativePostgresIdentifier validates the physical identifier constraints
// needed by PostgreSQL and DALgo without normalizing its exact spelling.
func validNativePostgresIdentifier(value string) bool {
	return value != "" && len(value) <= 63 && utf8.ValidString(value) && !containsNUL(value)
}

func containsNUL(value string) bool {
	for _, r := range value {
		if r == 0 {
			return true
		}
	}
	return false
}
