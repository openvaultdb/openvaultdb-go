package mount

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2mysql"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"
)

var errStopOpening = errors.New("stop")

// TestThePostgresMountStatesItsIdentifierModeAndForcesNoDialect: the mount says,
// in the options it hands the adapter, that names are folded to lower case (the
// adapter's default, and the one its DDL follows), so a later change of the
// adapter's default cannot flip a mount. It sets no structured-query dialect: the
// adapter forces its own (the typed PostgreSQL dialect, which binds every value
// and quotes every name), and a dialect the mount set would only be replaced. The
// connection string, the placeholder style and the recordsets of the declared
// collections reach the adapter as they did.
func TestThePostgresMountStatesItsIdentifierModeAndForcesNoDialect(t *testing.T) {
	t.Setenv("TEST_SQL_MOUNT_DSN", "postgres://app:"+markerPassword+"@db.example.test:5432/orders")
	var (
		dsn     string
		opts    dalgo2sql.DbOptions
		options []dalgo2postgres.Option
		opened  int
	)
	_, _, err := openPostgresWith(sqlMountManifest("postgres"), func(d string, _ dal.Schema, o dalgo2sql.DbOptions, extra ...dalgo2postgres.Option) (*dalgo2postgres.Database, error) {
		opened++
		dsn, opts, options = d, o, extra
		return nil, errStopOpening
	})
	if err == nil || opened != 1 {
		t.Fatalf("opened %d times, error %v", opened, err)
	}
	if dsn != "postgres://app:"+markerPassword+"@db.example.test:5432/orders" {
		t.Errorf("the connection string was changed on its way to the adapter")
	}
	if opts.IdentifierCase != dalgo2sql.IdentifierCaseFoldLower {
		t.Errorf("IdentifierCase = %q, want %q", opts.IdentifierCase, dalgo2sql.IdentifierCaseFoldLower)
	}
	if opts.StructuredQueryDialect != "" {
		t.Errorf("StructuredQueryDialect = %q: the mount sets none, the adapter forces its own", opts.StructuredQueryDialect)
	}
	if opts.NativeStructuredQueryCompiler != nil || opts.NativeJoinEligibility != nil || opts.NativeJoinHintTranslator != nil {
		t.Errorf("the mount hands the adapter a compiler of its own, which the adapter refuses")
	}
	if len(options) != 0 {
		t.Errorf("the mount passes %d adapter options, want none: the mode is stated in the DbOptions", len(options))
	}
	if opts.Placeholder != dalgo2sql.PlaceholderDollar {
		t.Errorf("Placeholder = %v", opts.Placeholder)
	}
	if len(opts.Recordsets) != 2 || opts.Recordsets["customers"] == nil || opts.Recordsets["orders"] == nil {
		t.Errorf("Recordsets = %v, want the two declared collections", opts.Recordsets)
	}
}

// TestTheMySQLMountSetsNoDialect: the MySQL mount is refused structured queries
// (core's allow-list leaves it out), and it must not claim a dialect the adapter
// does not have. It hands the adapter the options it always did.
func TestTheMySQLMountSetsNoDialect(t *testing.T) {
	t.Setenv("TEST_SQL_MOUNT_DSN", "app:"+markerPassword+"@tcp(db.example.test:3306)/orders")
	var opts dalgo2sql.DbOptions
	_, _, err := openMySQLWith(sqlMountManifest("mysql"), func(_ string, _ dal.Schema, o dalgo2sql.DbOptions) (*dalgo2mysql.Database, error) {
		opts = o
		return nil, errStopOpening
	})
	if err == nil {
		t.Fatal("the mount opened")
	}
	if opts.StructuredQueryDialect != "" || opts.IdentifierCase != "" {
		t.Errorf("dialect %q, identifier case %q: the MySQL mount states neither", opts.StructuredQueryDialect, opts.IdentifierCase)
	}
	if len(opts.Recordsets) != 2 {
		t.Errorf("Recordsets = %v", opts.Recordsets)
	}
}

// TestAPostgresManifestWithAccessControlOnDoesNotMount: access policies are not
// offered for a PostgreSQL mount, with the preview of queries on or off. The
// manifest fails to mount with the message of the file ACL refusal, before any
// connection string is read or any connection is attempted (the environment
// variable of the mount holds none here, and the error is not about it).
func TestAPostgresManifestWithAccessControlOnDoesNotMount(t *testing.T) {
	for _, value := range []string{"", "1"} {
		for name, acl := range map[string]string{
			"policy files": "acl: {enabled: true, policies: [policy.yaml]}\n",
			"policy store": "acl: {enabled: true}\nacl_store: {path: owner}\n",
		} {
			t.Run("switch "+value+"/"+name, func(t *testing.T) {
				t.Setenv("OVDB_PREVIEW_POSTGRES_QUERIES", value)
				dir := t.TempDir()
				path := dir + "/db.yaml"
				manifest := "database: {id: crm, schema_mode: strict}\nstorage: {engine: postgres, postgres: {dsn_env: TEST_PG_ACL_DSN_NOT_SET}}\nschemas:\n  collections:\n    customers:\n      fields:\n        name: {type: string}\n" + acl
				if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
					t.Fatal(err)
				}
				db, err := File(path)
				if err == nil {
					_ = db.Close()
					t.Fatal("the mount opened")
				}
				if !strings.Contains(err.Error(), "file ACL currently supports SQLite and local InGitDB mounts") {
					t.Fatalf("error = %v", err)
				}
			})
		}
	}
}
