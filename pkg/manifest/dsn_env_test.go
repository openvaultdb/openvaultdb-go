package manifest_test

import (
	"strings"
	"testing"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// dsnEnvSecret is a connection string with a password, used as a dsn_env value
// that fails the check.
const dsnEnvSecret = "postgres://app:pw-MARKER-31c8@db.example.test:5432/orders?sslmode=require"

// dsnEnvManifest is a manifest of the given engine whose dsn_env is the YAML
// scalar given (written as is, so that a value can be quoted).
func dsnEnvManifest(engine, scalar string) string {
	return "database: {id: sqlmount, schema_mode: strict}\n" +
		"storage:\n  engine: " + engine + "\n  " + engine + ":\n    dsn_env: " + scalar + "\n" +
		"schemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n"
}

// TestDSNEnvIsAcceptedOnlyAsAnEnvironmentVariableName: storage.postgres.dsn_env
// and storage.mysql.dsn_env name the environment variable that holds the
// connection string, so each is accepted only as such a name. A value that fails
// the check is refused when the manifest is validated, with a message that names
// the field and does not repeat the value.
func TestDSNEnvIsAcceptedOnlyAsAnEnvironmentVariableName(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		field := "storage." + engine + ".dsn_env"
		for _, scalar := range []string{"MY_DSN", "_dsn", "OVDB_DSN_2", "a", "Mixed_Case_1"} {
			t.Run(engine+" accepts "+scalar, func(t *testing.T) {
				m, err := manifest.Parse([]byte(dsnEnvManifest(engine, scalar)))
				if err != nil {
					t.Fatalf("a variable name is refused: %v", err)
				}
				var got string
				if engine == "postgres" {
					got = m.Storage.Postgres.DSNEnvVar()
				} else {
					got = m.Storage.MySQL.DSNEnvVar()
				}
				if got != scalar {
					t.Errorf("variable %q, want %q", got, scalar)
				}
			})
		}
		for _, c := range []struct{ name, scalar string }{
			{"a postgres URL", "'" + dsnEnvSecret + "'"},
			{"a key=value string", "'host=db.example.test user=app password=pw-MARKER-31c8'"},
			{"a go-sql-driver string", "'app:pw-MARKER-31c8@tcp(db.example.test:3306)/orders'"},
			{"a name with a dash", "MY-DSN"},
			{"a name that starts with a digit", "'1DSN'"},
			{"a name with a space", "'MY DSN'"},
			{"a name with a dollar sign", "'$MY_DSN'"},
			{"a name with an equals sign", "'MY_DSN=x'"},
			{"a name with a letter that is not ASCII", "'MY_DSN_é'"},
			{"a name with a newline", `"MY_DSN\nx"`},
			{"a name with a trailing newline", `"MY_DSN\n"`},
			{"a very long value", "'" + strings.Repeat("pw-MARKER-31c8", 2000) + "'"},
		} {
			t.Run(engine+" refuses "+c.name, func(t *testing.T) {
				_, err := manifest.Parse([]byte(dsnEnvManifest(engine, c.scalar)))
				if err == nil {
					t.Fatal("a value that is not a variable name is accepted")
				}
				if !strings.Contains(err.Error(), field) {
					t.Errorf("the message does not name the field %s: %v", field, err)
				}
				for _, secret := range []string{"pw-MARKER-31c8", "db.example.test", "app:", "MY-DSN", "MY DSN", "MY_DSN", "1DSN"} {
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
}

// TestDSNEnvIsCheckedOnValidate: a manifest built in code is checked as one read
// from a file is, and an empty dsn_env (the default variable) is accepted.
func TestDSNEnvIsCheckedOnValidate(t *testing.T) {
	build := func(engine string) *manifest.Manifest {
		return &manifest.Manifest{
			Database: manifest.Database{ID: "sqlmount", SchemaMode: schema.ModeStrict},
			Storage:  manifest.Storage{Engine: engine},
			Schemas:  &schema.Schemas{Collections: map[string]schema.Collection{"customers": {Fields: map[string]schema.Field{"name": {Type: schema.TypeString}}}}},
		}
	}
	for _, engine := range []string{"postgres", "mysql"} {
		set := func(m *manifest.Manifest, value string) {
			if engine == "postgres" {
				m.Storage.Postgres = &manifest.PostgresOptions{DSNEnv: value}
			} else {
				m.Storage.MySQL = &manifest.MySQLOptions{DSNEnv: value}
			}
		}
		t.Run(engine, func(t *testing.T) {
			m := build(engine)
			if err := m.Validate(); err != nil {
				t.Fatalf("no options: %v", err)
			}
			set(m, "")
			if err := m.Validate(); err != nil {
				t.Fatalf("empty variable name (the default): %v", err)
			}
			set(m, "MY_DSN")
			if err := m.Validate(); err != nil {
				t.Fatalf("a variable name: %v", err)
			}
			set(m, dsnEnvSecret)
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), "storage."+engine+".dsn_env") || strings.Contains(err.Error(), "pw-MARKER-31c8") {
				t.Fatalf("a connection string: %v", err)
			}
		})
	}
}

// TestValidEnvVarName: the name of an environment variable is a letter or an
// underscore followed by letters, digits and underscores, all ASCII.
func TestValidEnvVarName(t *testing.T) {
	for name, want := range map[string]bool{
		"A": true, "_": true, "a1": true, "OVDB_POSTGRES_DSN": true,
		"": false, "1a": false, "a-b": false, "a b": false, "a=b": false, "é": false, "a\n": false, "\na": false, "a$": false,
	} {
		if got := manifest.ValidEnvVarName(name); got != want {
			t.Errorf("%q: %t, want %t", name, got, want)
		}
	}
}
