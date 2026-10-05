package manifest_test

import (
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// tokenEnvSecret is a token with characters that a variable name cannot hold,
// used as a token_env value that fails the check.
const tokenEnvSecret = "tok-MARKER-52d1/with.dots"

// tokenEnvManifest is a manifest of the inGitDB GitHub backend whose token_env is
// the YAML scalar given (written as is, so that a value can be quoted).
func tokenEnvManifest(scalar string) string {
	return "database: {id: ghmount, schema_mode: strict}\n" +
		"storage:\n  engine: ingitdb\n  ingitdb:\n    github:\n      owner: me\n      repo: data\n      token_env: " + scalar + "\n" +
		"schemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n"
}

// TestTokenEnvIsAcceptedOnlyAsAnEnvironmentVariableName: the token_env of the
// GitHub backend of the inGitDB engine names the environment variable that holds
// the token, so it is accepted only as such a name. A value that fails the check
// is refused when the manifest is validated, with a message that names the field
// and does not repeat the value.
func TestTokenEnvIsAcceptedOnlyAsAnEnvironmentVariableName(t *testing.T) {
	const field = "storage.ingitdb.github.token_env"
	for _, scalar := range []string{"MY_TOKEN", "_t", "OVDB_TOKEN_2", "a", "Mixed_Case_1"} {
		t.Run("accepts "+scalar, func(t *testing.T) {
			m, err := manifest.Parse([]byte(tokenEnvManifest(scalar)))
			if err != nil {
				t.Fatalf("a variable name is refused: %v", err)
			}
			if got := m.Storage.InGitDB.GitHub.TokenEnvVar(); got != scalar {
				t.Errorf("variable %q, want %q", got, scalar)
			}
		})
	}
	for _, c := range []struct{ name, scalar string }{
		{"a token with a dash", "'" + tokenEnvSecret + "'"},
		{"a name with a dash", "MY-TOKEN"},
		{"a name that starts with a digit", "'1TOKEN'"},
		{"a name with a space", "'MY TOKEN'"},
		{"a name with a dollar sign", "'$MY_TOKEN'"},
		{"a name with an equals sign", "'MY_TOKEN=x'"},
		{"a name with a letter that is not ASCII", "'MY_TOKEN_é'"},
		{"a name with a newline", `"MY_TOKEN\nx"`},
		{"a very long value", "'" + strings.Repeat("tok-MARKER-52d1", 2000) + "'"},
	} {
		t.Run("refuses "+c.name, func(t *testing.T) {
			_, err := manifest.Parse([]byte(tokenEnvManifest(c.scalar)))
			if err == nil {
				t.Fatal("a value that is not a variable name is accepted")
			}
			if !strings.Contains(err.Error(), field) {
				t.Errorf("the message does not name the field %s: %v", field, err)
			}
			for _, secret := range []string{"tok-MARKER-52d1", "with.dots", "MY-TOKEN", "MY TOKEN", "MY_TOKEN", "1TOKEN"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("the message repeats %q: %v", secret, err)
				}
			}
			if len(err.Error()) > 512 {
				t.Errorf("the message is %d bytes long", len(err.Error()))
			}
		})
	}
}

// TestTokenEnvIsCheckedOnValidate: a manifest built in code is checked as one read
// from a file is, and an empty token_env (the default variable) is accepted.
func TestTokenEnvIsCheckedOnValidate(t *testing.T) {
	m := &manifest.Manifest{
		Database: manifest.Database{ID: "ghmount", SchemaMode: schema.ModeStrict},
		Storage:  manifest.Storage{Engine: "ingitdb", InGitDB: &manifest.InGitDBOptions{GitHub: &manifest.InGitDBGitHubOptions{Owner: "me", Repo: "data"}}},
		Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("empty variable name (the default): %v", err)
	}
	m.Storage.InGitDB.GitHub.TokenEnv = "MY_TOKEN"
	if err := m.Validate(); err != nil {
		t.Fatalf("a variable name: %v", err)
	}
	m.Storage.InGitDB.GitHub.TokenEnv = tokenEnvSecret
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "storage.ingitdb.github.token_env") || strings.Contains(err.Error(), "MARKER") {
		t.Fatalf("a value that is not a variable name: %v", err)
	}
}
