package mount

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2mysql"
	"github.com/dal-go/dalgo2sql"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// mysqlOpener opens and pings a MySQL connection, as
// dalgo2mysql.NewDatabaseWithOptions does. openMySQLWith takes it so that a test
// can make the open fail the way a driver does.
type mysqlOpener func(dsn string, schema dal.Schema, opts dalgo2sql.DbOptions) (*dalgo2mysql.Database, error)

// openMySQL opens a MySQL database through the dal-go dalgo2mysql driver
// (go-sql-driver, pure Go). The DSN — which carries credentials — is read
// from the environment variable named by storage.mysql.dsn_env (default
// OVDB_MYSQL_DSN); manifests never carry secrets (see docs/threat-model.md).
//
// Strict mode only in MVP, like SQLite and Postgres: records map to
// relational tables (id primary-key column + one column per declared field).
// Unlike Postgres, MySQL preserves identifier case, so no case folding is
// needed.
func openMySQL(m *manifest.Manifest) (dal.DB, []schema.Mode, error) {
	return openMySQLWith(m, dalgo2mysql.NewDatabaseWithOptions)
}

// openMySQLWith is openMySQL with the function that opens the connection. An
// error from it is not returned or wrapped (see mysqlOpenError).
func openMySQLWith(m *manifest.Manifest, open mysqlOpener) (dal.DB, []schema.Mode, error) {
	envVar := m.Storage.MySQL.DSNEnvVar()
	dsn := os.Getenv(envVar)
	if dsn == "" {
		return nil, nil, fmt.Errorf("mysql DSN not set: expected connection string in $%s", envVar)
	}
	recordsets := map[string]*dalgo2sql.Recordset{}
	if m.Schemas != nil {
		names := make([]string, 0, len(m.Schemas.Collections))
		for name := range m.Schemas.Collections {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			recordsets[name] = dalgo2sql.NewRecordset(name, dalgo2sql.Table, []dal.FieldRef{dal.Field("id")})
		}
	}
	db, err := open(dsn, dal.NewSchema(nil, nil), dalgo2sql.DbOptions{Recordsets: recordsets})
	if err != nil {
		return nil, nil, mysqlOpenError(envVar)
	}
	return db, []schema.Mode{schema.ModeStrict}, nil
}

// mysqlOpenError builds the error of a mysql mount that did not open: a fixed
// sentence that names the environment variable. No text of the opener's error is
// used, because the connection string carries credentials and mount errors are
// printed, and the result wraps nothing.
func mysqlOpenError(envVar string) error {
	return errors.New("failed to open MySQL via $" + envVar + ": the connection could not be opened or verified")
}
