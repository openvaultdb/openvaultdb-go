package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/license"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
	"gopkg.in/yaml.v3"
)

const licenseManifest = "database: {id: test, schema_mode: schemaless}\nstorage: {engine: ingitdb, path: data}\n"

func TestManifestLicensesCompatibleAndInvalidOverrides(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	for _, input := range []string{
		"database: &db {id: test, schema_mode: schemaless, license: null}\nstorage: {engine: ingitdb, path: data}\n",
		"database: {<<: &defaults {id: test, schema_mode: schemaless, license: null}}\nstorage: {engine: ingitdb, path: data}\n",
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Fatal("null declaration became omission", input)
		}
	}
}

func TestValidateLicensesAndNodeEdgeCases(t *testing.T) {
	t.Parallel()
	badLic := license.Declaration{}
	m := &Manifest{Database: Database{License: &badLic}}
	if err := m.ValidateLicenses(); err == nil {
		t.Fatal("expected error for invalid Database.License")
	}
	m = &Manifest{RecordsetLicenses: map[string]license.Declaration{"   ": {SPDX: "MIT"}}}
	if err := m.ValidateLicenses(); err == nil {
		t.Fatal("expected error for whitespace recordset license name")
	}
	if err := validateDatabaseLicenseNode(nil, 0); err == nil {
		t.Fatal("expected error for nil node")
	}
	if err := validateDatabaseLicenseNode(&yaml.Node{Kind: yaml.MappingNode}, 33); err == nil {
		t.Fatal("expected error for depth > 32")
	}
	aliasTarget := &yaml.Node{Kind: yaml.ScalarNode, Value: "ok"}
	aliasNode := &yaml.Node{Kind: yaml.AliasNode, Alias: aliasTarget}
	if err := validateDatabaseLicenseNode(aliasNode, 0); err != nil {
		t.Fatalf("unexpected error for alias to scalar: %v", err)
	}
	seqValid := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{aliasTarget}}
	if err := validateDatabaseLicenseNode(seqValid, 0); err != nil {
		t.Fatalf("unexpected error for valid sequence: %v", err)
	}
	seqInvalid := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{nil}}
	if err := validateDatabaseLicenseNode(seqInvalid, 0); err == nil {
		t.Fatal("expected error for sequence with nil child")
	}
}

func TestValidateHTTP(t *testing.T) {
	t.Parallel()
	strField := schema.Field{Type: schema.TypeString}
	ecbFields := map[string]schema.Field{"time": strField, "currency": strField, "rate": strField}
	ianaFields := map[string]schema.Field{"Value": strField, "Description": strField, "Reference": strField}

	m := &Manifest{Storage: Storage{Engine: "sqlite", HTTP: &HTTPOptions{Profile: HTTPProfileECBDaily, Collection: "rates"}}}
	if err := m.ValidateHTTP(); err == nil {
		t.Fatal("expected error for HTTP options on non-http engine")
	}
	m = &Manifest{Storage: Storage{Engine: "http"}}
	if err := m.ValidateHTTP(); err == nil {
		t.Fatal("expected error for missing HTTPOptions")
	}
	m.Storage.HTTP = &HTTPOptions{Profile: "unknown", Collection: "rates"}
	if err := m.ValidateHTTP(); err == nil {
		t.Fatal("expected error for unsupported profile")
	}
	m.Storage.HTTP.Profile = HTTPProfileECBDaily
	m.Storage.Path = "local"
	if err := m.ValidateHTTP(); err == nil {
		t.Fatal("expected error for http with Path")
	}
	m.Storage.Path = ""
	if err := m.ValidateHTTP(); err == nil {
		t.Fatal("expected error for non-strict or missing schemas")
	}
	m.Database.SchemaMode = schema.ModeStrict
	m.Schemas = &schema.Schemas{Collections: map[string]schema.Collection{
		"other": {Fields: ecbFields},
	}}
	if err := m.ValidateHTTP(); err == nil {
		t.Fatal("expected error for mismatched collection name")
	}
	m.Schemas.Collections = map[string]schema.Collection{
		"rates": {Fields: map[string]schema.Field{"time": strField, "currency": strField, "rate": {Type: schema.TypeInteger}}},
	}
	if err := m.ValidateHTTP(); err == nil {
		t.Fatal("expected error for non-string native field")
	}
	m.Schemas.Collections["rates"] = schema.Collection{Fields: ecbFields}
	if err := m.ValidateHTTP(); err != nil {
		t.Fatalf("unexpected error for valid ECB HTTP manifest: %v", err)
	}

	// IANA profile checks
	m.Storage.HTTP = &HTTPOptions{Profile: HTTPProfileIANAHTTPStatus, Collection: "status"}
	m.Schemas.Collections = map[string]schema.Collection{
		"status": {Source: &schema.NativeCollectionSource{}, Fields: ianaFields},
	}
	if err := m.ValidateHTTP(); err == nil {
		t.Fatal("expected error for IANA with source override")
	}
	reqTrue := true
	m.Schemas.Collections["status"] = schema.Collection{
		Fields: map[string]schema.Field{
			"Value":       {Type: schema.TypeString, Required: reqTrue},
			"Description": strField,
			"Reference":   strField,
		},
	}
	if err := m.ValidateHTTP(); err == nil {
		t.Fatal("expected error for IANA field with extra attributes")
	}
	m.Schemas.Collections["status"] = schema.Collection{Fields: ianaFields}
	if err := m.ValidateHTTP(); err != nil {
		t.Fatalf("unexpected error for valid IANA HTTP manifest: %v", err)
	}
}
