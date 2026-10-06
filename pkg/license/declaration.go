// Package license validates source-data terms. Declarations describe terms;
// they neither grant access nor certify that derived output may be published.
package license

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const Format = "ovdb-data-rights/1"
const MaxEvidenceBytes = 256 << 10

// Profile preserves the existing Directory and Publisher SPDX policies.
type Profile uint8

const (
	Directory Profile = iota
	Publisher
)

var atomShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+-]{0,63}$`)
var knownAtoms = []string{"0BSD", "AGPL-3.0-only", "Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "CC-BY-4.0", "CC-BY-SA-4.0", "CC0-1.0", "GPL-2.0-only", "GPL-3.0-only", "ISC", "LGPL-3.0-only", "MIT", "MPL-2.0", "ODC-By-1.0", "ODbL-1.0", "PDDL-1.0", "Unlicense"}

func ValidateSPDX(value string, profile Profile) error {
	if profile != Directory && profile != Publisher {
		return fmt.Errorf("unknown license validation profile")
	}
	if len(value) <= 64 {
		if atomShape.MatchString(value) && (profile == Directory || slices.Contains(knownAtoms, value)) {
			return nil
		}
		atoms := strings.Split(value, " AND ")
		if len(atoms) >= 2 && len(atoms) <= 4 {
			seen := map[string]bool{}
			valid := true
			for _, atom := range atoms {
				if !slices.Contains(knownAtoms, atom) || seen[atom] {
					valid = false
				}
				seen[atom] = true
			}
			if valid {
				return nil
			}
		}
	}
	return fmt.Errorf("invalid SPDX declaration")
}

// Declaration retains the author's legacy scalar representation on marshaling.
// A nil *Declaration means omitted/inherit. A nonnil zero value is invalid.
// Use Normalized for resolved evidence, which always has an object shape.
type Declaration struct {
	Name   string `json:"name,omitempty" yaml:"name,omitempty"`
	SPDX   string `json:"spdx,omitempty" yaml:"spdx,omitempty"`
	URL    string `json:"url,omitempty" yaml:"url,omitempty"`
	Text   string `json:"text,omitempty" yaml:"text,omitempty"`
	legacy bool
}

func (d Declaration) Legacy() bool            { return d.legacy }
func (d Declaration) Normalized() Declaration { d.legacy = false; return d }

func boundedText(value string, bound int, multiline bool) bool {
	if len(value) > bound || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, ch := range value {
		if unicode.IsControl(ch) && !(multiline && (ch == '\n' || ch == '\r' || ch == '\t')) {
			return false
		}
	}
	return true
}

// ValidateURL accepts HTTPS terms links, including fragments. It never fetches them.
func ValidateURL(value string) error {
	if !boundedText(value, 2048, false) || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\\ \t\r\n") {
		return fmt.Errorf("terms URL must be an absolute HTTPS URL without credentials")
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return fmt.Errorf("terms URL must be an absolute HTTPS URL without credentials")
	}
	return nil
}
func (d Declaration) Validate(profile Profile) error {
	if d.SPDX == "" && d.URL == "" && d.Text == "" {
		return fmt.Errorf("license requires spdx, url or text")
	}
	if d.Name != "" && !boundedText(d.Name, 256, false) {
		return fmt.Errorf("license name must be nonblank and at most 256 UTF-8 bytes")
	}
	if d.SPDX != "" {
		if err := ValidateSPDX(d.SPDX, profile); err != nil {
			return err
		}
	}
	if d.URL != "" {
		if err := ValidateURL(d.URL); err != nil {
			return err
		}
	}
	if d.Text != "" && !boundedText(d.Text, 65536, true) {
		return fmt.Errorf("license text must be nonblank and at most 65536 UTF-8 bytes without forbidden controls")
	}
	if d.legacy && (d.Name != "" || d.URL != "" || d.Text != "") {
		return fmt.Errorf("legacy license must contain only SPDX")
	}
	return nil
}

// ParseJSON validates a closed declaration, including supplied empty/null fields.
func ParseJSON(data []byte, profile Profile) (Declaration, error) {
	var raw any
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return Declaration{}, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Declaration{}, fmt.Errorf("license must contain exactly one JSON value")
	}
	// Decode an object field-by-field too, to reject duplicate keys.
	var d Declaration
	if value, ok := raw.(string); ok {
		d.SPDX, d.legacy = value, true
	} else {
		if _, ok := raw.(map[string]any); !ok {
			return d, fmt.Errorf("license must be a string or object")
		}
		obj := json.NewDecoder(bytes.NewReader(data))
		_, _ = obj.Token()
		seen := map[string]bool{}
		for obj.More() {
			token, err := obj.Token()
			if err != nil {
				return d, err
			}
			key := token.(string)
			if seen[key] {
				return d, fmt.Errorf("duplicate license field")
			}
			seen[key] = true
			var value any
			if err := obj.Decode(&value); err != nil {
				return d, err
			}
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return d, fmt.Errorf("license fields must be nonblank strings")
			}
			switch key {
			case "name":
				d.Name = text
			case "spdx":
				d.SPDX = text
			case "url":
				d.URL = text
			case "text":
				d.Text = text
			default:
				return d, fmt.Errorf("unknown license field")
			}
		}
	}
	return d, d.Validate(profile)
}
func (d *Declaration) UnmarshalJSON(data []byte) error {
	value, err := ParseJSON(data, Directory)
	if err == nil {
		*d = value
	}
	return err
}
func (d Declaration) MarshalJSON() ([]byte, error) {
	if d.legacy {
		return json.Marshal(d.SPDX)
	}
	type plain Declaration
	return json.Marshal(plain(d))
}

func ParseYAML(node *yaml.Node, profile Profile) (Declaration, error) {
	if node == nil {
		return Declaration{}, fmt.Errorf("missing license")
	}
	for depth := 0; node.Kind == yaml.AliasNode; depth++ {
		if depth >= 16 || node.Alias == nil {
			return Declaration{}, fmt.Errorf("license alias nesting is invalid")
		}
		node = node.Alias
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		d := Declaration{SPDX: node.Value, legacy: true}
		return d, d.Validate(profile)
	}
	if node.Kind != yaml.MappingNode {
		return Declaration{}, fmt.Errorf("license must be a string or object")
	}
	raw := map[string]string{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag != "!!str" || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return Declaration{}, fmt.Errorf("license fields must be nonblank strings")
		}
		if _, exists := raw[key.Value]; exists {
			return Declaration{}, fmt.Errorf("duplicate license field")
		}
		raw[key.Value] = value.Value
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return Declaration{}, err
	}
	return ParseJSON(data, profile)
}
func (d *Declaration) UnmarshalYAML(node *yaml.Node) error {
	value, err := ParseYAML(node, Directory)
	if err == nil {
		*d = value
	}
	return err
}
func (d Declaration) MarshalYAML() (any, error) {
	if d.legacy {
		return d.SPDX, nil
	}
	type plain Declaration
	return plain(d), nil
}
