package license

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeclarationParsingAndLegacySerialization(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`"MIT"`, `"CC0-1.0 AND CC-BY-4.0"`, `{"url":"https://example.org/terms#reuse"}`, `{"text":"First line.\nSecond line."}`, `{"name":"Reuse conditions","spdx":"MIT","url":"https://example.org/terms","text":"Keep attribution."}`} {
		d, err := ParseJSON([]byte(input), Directory)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		encoded, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var unmarshaled Declaration
		if err := json.Unmarshal(encoded, &unmarshaled); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(unmarshaled, d) {
			t.Fatalf("Declaration.UnmarshalJSON: %+v != %+v", unmarshaled, d)
		}
		var actual, want any
		_ = json.Unmarshal(encoded, &actual)
		_ = json.Unmarshal([]byte(input), &want)
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("roundtrip: %s != %s", encoded, input)
		}
		yamlBytes, err := yaml.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var round Declaration
		if err := yaml.Unmarshal(yamlBytes, &round); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(round, d) {
			t.Fatalf("YAML changed declaration: %+v %+v", round, d)
		}
	}
	legacy, _ := ParseJSON([]byte(`"MIT"`), Directory)
	normalized, _ := json.Marshal(legacy.Normalized())
	if string(normalized) != `{"spdx":"MIT"}` {
		t.Fatal(string(normalized))
	}
}

func TestDeclarationInvalidSuppliedFields(t *testing.T) {
	t.Parallel()
	invalid := []string{
		`{`,
		`"MIT" "ISC"`,
		`null`, `{}`, `[]`, `1`, `true`, `""`,
		`{"name":"Only a name"}`, `{"url":null}`, `{"text":1}`,
		`{"spdx":"MIT","text":""}`, `{"text":"   "}`,
		`{"unknown":"x","text":"Terms"}`,
		`{"url":"javascript:alert(1)"}`, `{"url":"http://example.org/terms"}`,
		`{"url":"https://owner:secret@example.org/terms"}`, `{"url":"https:///terms"}`,
		`{"text":"NUL\u0000"}`, `{"text":"DEL\u007f"}`,
		`{"url":"https://example.org/terms","url":"https://elsewhere.org/"}`,
		`{"spdx":"MIT OR ISC"}`, `{"spdx":"MIT AND MIT"}`,
	}
	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseJSON([]byte(input), Directory); err == nil {
				t.Fatal("accepted invalid declaration")
			}
			var d Declaration
			if err := json.Unmarshal([]byte(input), &d); err == nil {
				t.Fatal("UnmarshalJSON accepted invalid declaration")
			}
		})
	}
	for _, d := range []Declaration{
		{Text: strings.Repeat("x", 65537)},
		{Text: "terms", Name: strings.Repeat("x", 257)},
		{URL: "https://example.org/" + strings.Repeat("x", 2048)},
		{Text: "terms", Name: "\t"},
		{SPDX: "MIT", Name: "extra", legacy: true},
	} {
		if err := d.Validate(Directory); err == nil {
			t.Fatal("accepted oversized/blank/control/legacy-mixed declaration")
		}
	}
}

func TestParseYAMLEdgeCases(t *testing.T) {
	t.Parallel()
	if _, err := ParseYAML(nil, Directory); err == nil {
		t.Fatal("expected error for nil yaml.Node")
	}
	// Valid alias pointing to scalar.
	target := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "MIT"}
	alias := &yaml.Node{Kind: yaml.AliasNode, Alias: target}
	got, err := ParseYAML(alias, Directory)
	if err != nil || !got.Legacy() || got.SPDX != "MIT" {
		t.Fatalf("ParseYAML alias: %+v, %v", got, err)
	}
	// Nil alias target.
	if _, err := ParseYAML(&yaml.Node{Kind: yaml.AliasNode}, Directory); err == nil {
		t.Fatal("expected error for nil alias target")
	}
	// Excessive alias depth / cycle.
	cycle := &yaml.Node{Kind: yaml.AliasNode}
	cycle.Alias = cycle
	if _, err := ParseYAML(cycle, Directory); err == nil {
		t.Fatal("expected error for alias cycle")
	}
	// Non-string/non-mapping node (sequence).
	if _, err := ParseYAML(&yaml.Node{Kind: yaml.SequenceNode}, Directory); err == nil {
		t.Fatal("expected error for sequence node")
	}
	// Non-string value in mapping.
	var d Declaration
	if err := yaml.Unmarshal([]byte("spdx: 123\n"), &d); err == nil {
		t.Fatal("expected error for numeric value in YAML mapping")
	}
	// Duplicate key in mapping node.
	dupMap := &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "spdx"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "MIT"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "spdx"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "ISC"},
		},
	}
	if _, err := ParseYAML(dupMap, Directory); err == nil {
		t.Fatal("expected error for duplicate key in YAML mapping")
	}
}

func TestSPDXProfileBoundary(t *testing.T) {
	t.Parallel()
	if err := ValidateSPDX("MIT", Profile(99)); err == nil {
		t.Fatal("expected error for unknown profile")
	}
	if err := ValidateSPDX("CC-BY-3.0", Directory); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSPDX("CC-BY-3.0", Publisher); err == nil {
		t.Fatal("publisher accepted non-whitelist SPDX")
	}
	for _, input := range []string{"MIT AND ISC", "CC0-1.0 AND CC-BY-4.0"} {
		for _, profile := range []Profile{Directory, Publisher} {
			if err := ValidateSPDX(input, profile); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, input := range []string{"MIT AND CC-BY-3.0", "MIT AND MIT", "MIT  AND ISC", "MIT AND ISC AND 0BSD AND MPL-2.0 AND CC0-1.0"} {
		if err := ValidateSPDX(input, Directory); err == nil {
			t.Fatal(input)
		}
	}
}

func TestResolveWholeDeclarationAndInvalidOverrides(t *testing.T) {
	t.Parallel()
	source := Identity{ServerID: "s", DatabaseID: "db", Recordset: "Rates"}
	server := &Declaration{Name: "Server terms", Text: "server", SPDX: "MIT"}
	database := &Declaration{Text: "database"}
	recordset := &Declaration{URL: "https://example.org/rates"}
	for _, test := range []struct {
		database, recordset *Declaration
		scope               Scope
		decl                Declaration
	}{{nil, nil, ServerScope, *server}, {database, nil, DatabaseScope, *database}, {database, recordset, RecordsetScope, *recordset}} {
		got, err := Resolve(source, server, test.database, test.recordset)
		if err != nil {
			t.Fatal(err)
		}
		if got.DeclarationScope != test.scope || got.Declaration != test.decl {
			t.Fatalf("wrong precedence/merged fields: %+v", got)
		}
		if got.Source != source || got.SourceID != "ovdb:s/db/Rates" || got.EvidenceOrigin != "server-declared" || len(got.Pins) != 0 {
			t.Fatal(got)
		}
	}
	legacyDecl, err := ParseJSON([]byte(`"MIT"`), Directory)
	if err != nil {
		t.Fatal(err)
	}
	legacyResolved, err := Resolve(source, &legacyDecl, nil, nil)
	if err != nil || legacyResolved.EvidenceOrigin != "legacy-metadata" {
		t.Fatalf("expected legacy-metadata origin, got %+v, %v", legacyResolved, err)
	}
	if _, err := Resolve(source, server, &Declaration{}, recordset); err == nil {
		t.Fatal("valid leaf masked invalid parent")
	}
	if _, err := Resolve(source, server, database, &Declaration{}); err == nil {
		t.Fatal("empty override inherited")
	}
	missing, err := Resolve(source, nil, nil, nil)
	if err != nil || missing != nil {
		t.Fatal(missing, err)
	}
	if _, err := Resolve(Identity{}, server, nil, nil); err == nil {
		t.Fatal("fabricated source identity")
	}
}

func TestInventoryIdentityAndBudget(t *testing.T) {
	t.Parallel()
	id := Identity{ServerID: "fixture-server", DatabaseID: "fx", Recordset: "Rates / 100%"}
	if id.SourceID() != "ovdb:fixture-server/fx/Rates%20%2F%20100%25" {
		t.Fatal(id.SourceID())
	}
	a, _ := Resolve(Identity{ServerID: "s", DatabaseID: "db", Recordset: "a"}, &Declaration{Text: "terms"}, nil, nil)
	b, _ := Resolve(Identity{ServerID: "s", DatabaseID: "db", Recordset: "b"}, &Declaration{Text: "terms"}, nil, nil)
	got, _, err := Inventory([]SourceRight{*b, *a, *a})
	if err != nil || len(got) != 2 || got[0].SourceID != a.SourceID {
		t.Fatal(got, err)
	}
	conflict := *a
	conflict.Declaration.Text = "changed"
	if _, _, err := Inventory([]SourceRight{*a, conflict}); err == nil {
		t.Fatal("accepted conflicting source")
	}
	var large []SourceRight
	for _, name := range []string{"a", "b", "c", "d"} {
		item, _ := Resolve(Identity{ServerID: "s", DatabaseID: "db", Recordset: name}, &Declaration{Text: strings.Repeat("<", 65536)}, nil, nil)
		large = append(large, *item)
	}
	if _, _, err := Inventory(large); err == nil {
		t.Fatal("accepted oversize encoded terms")
	}
}

// Not parallel: mutates package-level seam jsonMarshal.
func TestInventoryMarshalSeams(t *testing.T) {
	origMarshal := jsonMarshal
	t.Cleanup(func() {
		jsonMarshal = origMarshal
	})

	a, err := Resolve(Identity{ServerID: "s", DatabaseID: "db", Recordset: "a"}, &Declaration{Text: "terms"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Fail on first item marshal.
	jsonMarshal = func(v any) ([]byte, error) {
		return nil, errors.New("marshal failed")
	}
	if _, _, err := Inventory([]SourceRight{*a}); err == nil {
		t.Fatal("expected error when item marshal fails")
	}

	// 2. Fail on final slice marshal.
	calls := 0
	jsonMarshal = func(v any) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("final marshal failed")
		}
		return origMarshal(v)
	}
	if _, _, err := Inventory([]SourceRight{*a}); err == nil {
		t.Fatal("expected error when final slice marshal fails")
	}

	// 3. Final slice marshal exceeds MaxEvidenceBytes.
	calls = 0
	jsonMarshal = func(v any) ([]byte, error) {
		calls++
		if calls == 2 {
			return make([]byte, MaxEvidenceBytes+1), nil
		}
		return origMarshal(v)
	}
	if _, _, err := Inventory([]SourceRight{*a}); err == nil {
		t.Fatal("expected error when final slice exceeds MaxEvidenceBytes")
	}
}
