package server

import (
	"bytes"
	"strconv"

	"gopkg.in/yaml.v3"
)

// exactIntegerValues rewrites, in a DTQL document for a native PostgreSQL
// database, each integer literal of a value (value: or an item of values:) that
// JavaScript cannot hold exactly, above 2^53-1 or below -(2^53-1), as the same
// digits in a string. DALgo accepts only portable numbers as values, so it would
// refuse the literal; PostgreSQL parses the text as the type of the column it is
// compared with, so a bigint ID that a JSON client sends as a number is matched
// exactly, as the same ID sent as a string is. A literal too large for a 64-bit
// integer is not an integer to YAML either (it reads it as a float) and is left
// for the column check to refuse. A document that is not YAML, or that holds no
// such literal, is returned unchanged.
var yamlMarshalExact = yaml.Marshal

func exactIntegerValues(doc []byte) []byte {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return doc
	}
	if !rewriteExactIntegers(&root) {
		return doc
	}
	rewritten, err := yamlMarshalExact(&root)
	if err != nil {
		return doc
	}
	return rewritten
}

// errUnsafeIntegerValue is the refusal, on an engine that compares values as
// JSON numbers, of an integer value outside JavaScript's exact range.
const errUnsafeIntegerValue = "invalid or unsupported DTQL query: an integer value exceeds the exact range (2^53-1) of this engine; send it as a string"

// hasUnsafeIntegerValue reports whether the document has an integer value that
// exactIntegerValues would rewrite. Every engine but native PostgreSQL refuses
// one, with errUnsafeIntegerValue.
func hasUnsafeIntegerValue(doc []byte) bool {
	return !bytes.Equal(exactIntegerValues(doc), doc)
}

func rewriteExactIntegers(node *yaml.Node) bool {
	changed := false
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			switch key.Value {
			case "value":
				changed = unsafeIntegerToString(value) || changed
			case "values":
				if value.Kind == yaml.SequenceNode {
					for _, item := range value.Content {
						changed = unsafeIntegerToString(item) || changed
					}
				}
			}
		}
	}
	for _, child := range node.Content {
		changed = rewriteExactIntegers(child) || changed
	}
	return changed
}

// unsafeIntegerToString turns a scalar YAML integer outside the safe range into
// a string of its decimal digits, and reports whether it did.
func unsafeIntegerToString(node *yaml.Node) bool {
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!int" {
		return false
	}
	var digits string
	if signed, err := strconv.ParseInt(node.Value, 0, 64); err == nil {
		if signed >= -maxJavaScriptSafeInteger && signed <= maxJavaScriptSafeInteger {
			return false
		}
		digits = strconv.FormatInt(signed, 10)
	} else if unsigned, err := strconv.ParseUint(node.Value, 0, 64); err == nil {
		digits = strconv.FormatUint(unsigned, 10)
	} else {
		return false
	}
	node.Tag, node.Value, node.Style = "!!str", digits, yaml.DoubleQuotedStyle
	return true
}
