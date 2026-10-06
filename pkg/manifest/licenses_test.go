package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

const licenseManifest = "database: {id: test, schema_mode: schemaless}\nstorage: {engine: ingitdb, path: data}\n"

func TestManifestLicensesCompatibleAndInvalidOverrides(t *testing.T) {
	good := strings.Replace(licenseManifest, "schema_mode: schemaless}", "schema_mode: schemaless, license: MIT}", 1) + "recordset_licenses: {Rates: {url: 'https://example.org/terms'}}\n"
	m, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(m)
	if err != nil || !strings.Contains(string(encoded), `"license":"MIT"`) {
		t.Fatal(string(encoded), err)
	}
	old, err := Parse([]byte(licenseManifest))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(old)
	if strings.Contains(string(encoded), "license") {
		t.Fatal("changed old manifest serialization", string(encoded))
	}
	for _, decl := range []string{"null", "{}", "{name: title}", "{spdx: MIT, text: ''}", "{url: 'javascript:alert(1)'}", "{text: terms, extra: x}", "{text: null}"} {
		for _, input := range []string{strings.Replace(licenseManifest, "schema_mode: schemaless}", "schema_mode: schemaless, license: "+decl+"}", 1), licenseManifest + "recordset_licenses: {Rates: " + decl + "}\n"} {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatalf("invalid override accepted: %s", input)
			}
		}
	}
	if _, err := Parse([]byte(licenseManifest + "recordset_licenses: null\n")); err == nil {
		t.Fatal("null overrides accepted")
	}
}

func TestManifestLicenseNullAliasDoesNotInherit(t *testing.T) {
	for _, input := range []string{
		"database: &db {id: test, schema_mode: schemaless, license: null}\nstorage: {engine: ingitdb, path: data}\n",
		"database: {<<: &defaults {id: test, schema_mode: schemaless, license: null}}\nstorage: {engine: ingitdb, path: data}\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatal("null declaration became omission", input)
		}
	}
}
