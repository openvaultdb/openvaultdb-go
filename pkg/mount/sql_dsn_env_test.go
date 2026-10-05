package mount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2mysql"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"
)

// markerDSN is a connection string used as a dsn_env value.
const markerDSN = "postgres://app:" + markerPassword + "@db.example.test:5432/orders"

// TestSQLMountChecksTheDSNVariableNameBeforeNamingIt: a postgres or mysql mount
// names the environment variable that holds the connection string in its errors
// only when the name passed the manifest's check. A manifest that was not
// validated and holds anything else there is refused before the environment is
// read or the opener is called, with a message that does not repeat the value.
func TestSQLMountChecksTheDSNVariableNameBeforeNamingIt(t *testing.T) {
	// The variable the manifest names is set, so a mount that read it would open.
	t.Setenv(markerDSN, "postgres://app@db.example.test/orders")
	for _, engine := range []string{"postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			m := sqlMountManifest(engine)
			if engine == "postgres" {
				m.Storage.Postgres.DSNEnv = markerDSN
			} else {
				m.Storage.MySQL.DSNEnv = markerDSN
			}
			calls := 0
			var err error
			if engine == "postgres" {
				_, _, err = openPostgresWith(m, func(string, dal.Schema, dalgo2sql.DbOptions, ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
					calls++
					return nil, nil
				})
			} else {
				_, _, err = openMySQLWith(m, func(string, dal.Schema, dalgo2sql.DbOptions) (*dalgo2mysql.Database, error) {
					calls++
					return nil, nil
				})
			}
			want := "storage." + engine + ".dsn_env is not the name of an environment variable"
			if calls != 0 {
				t.Errorf("the opener was called %d times", calls)
			}
			if err == nil || err.Error() != want {
				t.Fatalf("got %v, want %q", err, want)
			}
			assertNoMarker(t, err, markerPassword, markerDSN, "app:", "db.example.test")
		})
	}
}

// TestSQLMountOfAManifestWithAConnectionStringAsDSNVariableIsRefusedBeforeMounting:
// a manifest file whose dsn_env holds a connection string is refused when it is
// loaded, with a message that names the field and does not repeat the value, so
// no mount is attempted.
func TestSQLMountOfAManifestWithAConnectionStringAsDSNVariableIsRefusedBeforeMounting(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db.yaml")
			text := "database: {id: sqlmount, schema_mode: strict}\nstorage:\n  engine: " + engine + "\n  " + engine + ":\n    dsn_env: '" + markerDSN + "'\n" +
				"schemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n"
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			db, err := File(path)
			if db != nil || err == nil || !strings.Contains(err.Error(), "storage."+engine+".dsn_env") {
				t.Fatalf("got %v, %v", db, err)
			}
			assertNoMarker(t, err, markerPassword, markerDSN, "app:", "db.example.test")
		})
	}
}
