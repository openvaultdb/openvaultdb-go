package mount

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// postgresOpener opens and pings a PostgreSQL connection, as
// dalgo2postgres.NewDatabaseWithOptions does. openPostgresWith takes it so that a
// test can make the open fail the way a driver does.
type postgresOpener func(dsn string, schema dal.Schema, opts dalgo2sql.DbOptions, options ...dalgo2postgres.Option) (*dalgo2postgres.Database, error)

// openPostgres opens a PostgreSQL database through the dal-go dalgo2postgres
// driver (pgx, pure Go). The DSN — which carries credentials — is read from
// the environment variable named by storage.postgres.dsn_env (default
// OVDB_POSTGRES_DSN); manifests never carry secrets (see docs/threat-model.md).
//
// Strict mode only in MVP, like SQLite: records map to relational tables (id
// primary-key column + one column per declared field). Partial/schemaless via
// a JSONB document column is on the roadmap.
func openPostgres(m *manifest.Manifest) (dal.DB, []schema.Mode, error) {
	return openPostgresWith(m, dalgo2postgres.NewDatabaseWithOptions)
}

// openPostgresWith is openPostgres with the function that opens the connection.
// An error from it is not returned or wrapped (see postgresOpenError).
func openPostgresWith(m *manifest.Manifest, open postgresOpener) (dal.DB, []schema.Mode, error) {
	// A name the database cannot keep whole is refused before the environment is read
	// and before any connection is made, so that nothing is created for the entries
	// before it (the collections are provisioned one by one when the database opens).
	// manifest.Validate refuses it already; this is for a manifest that was not validated.
	if err := m.CheckPostgresNames(); err != nil {
		return nil, nil, err
	}
	envVar := m.Storage.Postgres.DSNEnvVar()
	if !manifest.ValidEnvVarName(envVar) {
		// A manifest that was not validated (see manifest.Manifest.Validate): the
		// message does not repeat the value.
		return nil, nil, errors.New("storage.postgres.dsn_env is not the name of an environment variable")
	}
	dsn := os.Getenv(envVar)
	if dsn == "" {
		return nil, nil, fmt.Errorf("postgres DSN not set: expected connection string in $%s", envVar)
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
	// The mount states how names are matched and written, in the options the adapter
	// is given: lower-cased inside quotes, which is what the adapter's own DDL
	// stores and what it does when nothing is said. It is stated so that a later
	// change of the adapter's default cannot flip a mount. No structured-query
	// dialect is set: the adapter forces the typed PostgreSQL dialect, which binds
	// every value and quotes every name, and refuses a compiler of the mount's own.
	db, err := open(dsn, dal.NewSchema(nil, nil),
		dalgo2sql.DbOptions{
			Recordsets:     recordsets,
			Placeholder:    dalgo2sql.PlaceholderDollar,
			IdentifierCase: dalgo2sql.IdentifierCaseFoldLower,
		})
	if err != nil {
		return nil, nil, postgresOpenError(envVar, err)
	}
	return db, []schema.Mode{schema.ModeStrict}, nil
}

// postgresOpenError builds the error of a postgres mount that did not open. No
// text of the opener's error is used: the connection string carries credentials,
// and mount errors are printed. The error is a fixed sentence that names the
// environment variable, and, when the opener returned the adapter's
// *dalgo2postgres.ConnectionError, that error's own text: the adapter builds it
// from a fixed sentence, the SQLSTATE code, and the host, port and database name
// it has checked. No user name, no password, no other text of the connection
// string and none of the driver's message is in it. The result wraps nothing, so
// errors.Unwrap reaches neither.
func postgresOpenError(envVar string, err error) error {
	sentence := "failed to open Postgres via $" + envVar
	var connection *dalgo2postgres.ConnectionError
	if errors.As(err, &connection) {
		return errors.New(sentence + ": " + connection.Error())
	}
	return errors.New(sentence + ": the connection could not be opened or verified")
}
