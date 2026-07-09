package mount

import (
	"fmt"
	"os"
	"sort"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// openPostgres opens a PostgreSQL database through the dal-go dalgo2postgres
// driver (pgx, pure Go). The DSN — which carries credentials — is read from
// the environment variable named by storage.postgres.dsn_env (default
// OVDB_POSTGRES_DSN); manifests never carry secrets (see docs/threat-model.md).
//
// Strict mode only in MVP, like SQLite: records map to relational tables (id
// primary-key column + one column per declared field). Partial/schemaless via
// a JSONB document column is on the roadmap.
func openPostgres(m *manifest.Manifest) (dal.DB, []schema.Mode, error) {
	envVar := m.Storage.Postgres.DSNEnvVar()
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
	db, err := dalgo2postgres.NewDatabaseWithOptions(dsn, dal.NewSchema(nil, nil),
		dalgo2sql.DbOptions{
			Recordsets:  recordsets,
			Placeholder: dalgo2sql.PlaceholderDollar,
		})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open Postgres via $%s: %w", envVar, err)
	}
	return db, []schema.Mode{schema.ModeStrict}, nil
}
