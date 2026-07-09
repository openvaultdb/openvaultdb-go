package mount

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	"github.com/dal-go/dalgo2sqlite"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// openSQLite opens a file-backed SQLite database through the dal-go
// dalgo2sqlite driver (pure-Go modernc build). Strict mode only in MVP — an
// implementation choice, not a permanent limitation: partial/schemaless need
// inferred-schema-driven column evolution, which is on the roadmap.
//
// dalgo2sql maps records to relational tables: the record key's ID goes into
// the "id" primary-key column and top-level map fields into columns, so each
// declared collection gets a Recordset naming that PK.
func openSQLite(path string, m *manifest.Manifest) (dal.DB, []schema.Mode, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, fmt.Errorf("failed to create SQLite directory for %s: %w", path, err)
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
	db, err := dalgo2sqlite.NewDatabaseWithOptions(path, dal.NewSchema(nil, nil),
		dalgo2sql.DbOptions{Recordsets: recordsets})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open SQLite at %s: %w", path, err)
	}
	return db, []schema.Mode{schema.ModeStrict}, nil
}
