package license

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"reflect"
	"strings"
	"testing"
)

func TestDeclarationParsingAndLegacySerialization(t *testing.T) {
	for _, input := range []string{`"MIT"`, `"CC0-1.0 AND CC-BY-4.0"`, `{"url":"https://example.org/terms#reuse"}`, `{"text":"First line.\nSecond line."}`, `{"name":"Reuse conditions","spdx":"MIT","url":"https://example.org/terms","text":"Keep attribution."}`} {
		d, err := ParseJSON([]byte(input), Directory)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		encoded, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
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
	invalid := []string{`null`, `{}`, `[]`, `1`, `true`, `""`, `{"name":"Only a name"}`, `{"url":null}`, `{"text":1}`, `{"spdx":"MIT","text":""}`, `{"text":"   "}`, `{"unknown":"x","text":"Terms"}`, `{"url":"javascript:alert(1)"}`, `{"url":"http://example.org/terms"}`, `{"url":"https://owner:secret@example.org/terms"}`, `{"url":"https:///terms"}`, `{"text":"NUL\u0000"}`, `{"text":"DEL\u007f"}`, `{"url":"https://example.org/terms","url":"https://elsewhere.org/"}`, `{"spdx":"MIT OR ISC"}`, `{"spdx":"MIT AND MIT"}`}
	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseJSON([]byte(input), Directory); err == nil {
				t.Fatal("accepted invalid declaration")
			}
		})
	}
	for _, d := range []Declaration{{Text: strings.Repeat("x", 65537)}, {Text: "terms", Name: strings.Repeat("x", 257)}, {URL: "https://example.org/" + strings.Repeat("x", 2048)}, {Text: "terms", Name: "\t"}} {
		if err := d.Validate(Directory); err == nil {
			t.Fatal("accepted oversized/blank/control declaration")
		}
	}
}
func TestSPDXProfileBoundary(t *testing.T) {
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
