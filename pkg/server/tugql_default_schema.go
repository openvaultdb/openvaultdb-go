package server

import (
	"bytes"
	"io"

	"gopkg.in/yaml.v3"
)

// This file drops the two qualifiers of a root source that name what the
// per-database endpoint already supplies: the default schema of the engine and the
// database of the endpoint. A document that spells either out on its root source
// reads exactly the collection it would read without it, so the server drops the
// spelling before the document is classified and the document is then the one it
// would have been.

// defaultSchemas maps a storage engine to the schema its unqualified names live
// in. Every other schema, and the default schema of an engine not listed here, is
// left in place for the classifier to refuse.
var defaultSchemas = map[string]string{"sqlite": "main"}

// withoutDefaultSchema returns doc without the `schema` of its root source when
// that schema is the default one of engine, and doc itself in every other case:
// an engine with no default schema, a document that is not a mapping with a
// `from` mapping, one that holds more than one YAML document, a root source with
// no schema, with another schema, with the key more than once, or with a value
// that is not a plain scalar. Only the root
// source is read; the sources of joins and subqueries keep whatever they carry.
// A document the function cannot read is returned unchanged, so that the
// deserialiser gives the error.
func withoutDefaultSchema(doc []byte, engine string) []byte {
	return stripDefaultSchema(doc, engine, yaml.Marshal)
}

// stripDefaultSchema is withoutDefaultSchema with the encoder of the stripped
// document as a parameter. A document that cannot be encoded again is returned
// as it came.
func stripDefaultSchema(doc []byte, engine string, encode func(any) ([]byte, error)) []byte {
	schema := defaultSchemas[engine]
	if schema == "" {
		return doc
	}
	return stripRootSourceKey(doc, "schema", schema, encode)
}

// withoutOwnDatabase returns doc without the `database` of its root source when
// that database is id, the database of the per-database endpoint, and doc itself
// in every other case (the same cases as withoutDefaultSchema, and a root source
// that names another database). A document of one source then reads as it would
// have read had it named no database, and is classified as such.
func withoutOwnDatabase(doc []byte, id string) []byte {
	return stripOwnDatabase(doc, id, yaml.Marshal)
}

// stripOwnDatabase is withoutOwnDatabase with the encoder of the stripped document
// as a parameter.
func stripOwnDatabase(doc []byte, id string, encode func(any) ([]byte, error)) []byte {
	return stripRootSourceKey(doc, "database", id, encode)
}

// stripRootSourceKey returns doc without the entry key of its root source when
// the entry is a plain scalar equal to value, and doc itself in every other case.
func stripRootSourceKey(doc []byte, key, value string, encode func(any) ([]byte, error)) []byte {
	// The document is read as a stream so that a second document is seen: encoding
	// the first alone would drop the rest, and the deserialiser would take a
	// request it refuses as it came.
	decoder := yaml.NewDecoder(bytes.NewReader(doc))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil || root.Kind != yaml.DocumentNode || len(root.Content) != 1 {
		return doc
	}
	if err := decoder.Decode(new(yaml.Node)); err != io.EOF {
		return doc
	}
	from := mappingValue(root.Content[0], "from")
	if from == nil {
		return doc
	}
	at := -1
	for i := 0; i+1 < len(from.Content); i += 2 {
		if name := from.Content[i]; name.Kind == yaml.ScalarNode && name.Value == key {
			if at >= 0 {
				return doc
			}
			at = i
		}
	}
	if at < 0 {
		return doc
	}
	if entry := from.Content[at+1]; entry.Kind != yaml.ScalarNode || entry.Value != value {
		return doc
	}
	from.Content = append(from.Content[:at:at], from.Content[at+2:]...)
	stripped, err := encode(&root)
	if err != nil {
		return doc
	}
	return stripped
}

// mappingValue returns the value of key in node when node is a mapping, and nil
// otherwise or when the key is absent or its value is not itself a mapping.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Kind == yaml.ScalarNode && node.Content[i].Value == key {
			if value := node.Content[i+1]; value.Kind == yaml.MappingNode {
				return value
			}
			return nil
		}
	}
	return nil
}
