package mount

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dal-go/dalgo/access"
	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2sql"
	"github.com/dal-go/dalgo2sqlite"
	"github.com/dal-go/record"
	sqlite "modernc.org/sqlite" // also registers the "sqlite" driver the second handle is opened with
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/openvaultdb/openvaultdb-go/pkg/core"
	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// busyTimeout is how long a handle of a SQLite mount waits for a lock that another
// connection holds before it gives up with SQLITE_BUSY. SQLite allows one writer, and
// readers keep a writer from committing, so a handle that does not wait fails a write that
// meets a read at once; one that waits lets the write through when the read ends. It is a
// variable so that a test can shorten it.
//
// The wait is the engine's, and the engine cannot end it early: the context of a request
// is seen between the steps of a statement and not inside the wait, so a request whose
// deadline is shorter than busyTimeout gets its answer when the wait ends, at the latest.
// What it gets is its own deadline and not a lock error (see overDeadline).
var busyTimeout = 5 * time.Second

// overDeadline is err, reported as the end of the request when the request has ended
// and err is the engine giving up on a lock: a wait that outlasts the deadline of the
// request is the deadline's error (errors.Is context.DeadlineExceeded, or Canceled), with
// the lock error kept in the chain. Any other error, and any error of a request that is
// still running, is returned as it is.
func overDeadline(ctx context.Context, err error) error {
	var failure *sqlite.Error
	if err == nil || ctx.Err() == nil || !errors.As(err, &failure) || failure.Code()&0xff != sqlite3.SQLITE_BUSY {
		return err
	}
	return fmt.Errorf("%w: %w", ctx.Err(), err)
}

// busyTimeoutDSN is the name the driver opens the file at path with, carrying the
// busy timeout by the driver's own option (_busy_timeout, a number of milliseconds, applied
// to every connection of the handle).
func busyTimeoutDSN(path string) string { return sqliteWaitDSN(path, busyTimeout) }
func sqliteWaitDSN(path string, wait time.Duration) string {
	return fmt.Sprintf("%s?_busy_timeout=%d", path, wait.Milliseconds())
}

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
	wait, err := m.Storage.SQLite.LockWait(busyTimeout)
	if err != nil {
		return nil, nil, err
	}
	keys, err := sqliteRecordKeys(m)
	if err != nil {
		return nil, nil, err
	}
	recordsets := map[string]*dalgo2sql.Recordset{}
	if m.Schemas != nil {
		names := make([]string, 0, len(m.Schemas.Collections))
		for name := range m.Schemas.Collections {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			table := name
			if logical, ok := core.SQLiteLogicalName(name); ok {
				table = logical
			}
			key := "id"
			if configured, ok := keys[table]; ok {
				key = configured
			}
			recordset := dalgo2sql.NewRecordset(table, dalgo2sql.Table, []dal.FieldRef{dal.Field(key)})
			recordsets[name] = recordset
			if logicalName, ok := core.SQLiteLogicalName(name); ok {
				// A quoted schema key is the SQL-quoted identifier of its table. The
				// public name inside the quotes is the collection's canonical name
				// (core.Database.CanonicalCollection): key reads and writes and
				// provisioning are given it, and queries name it, so register that
				// lookup too.
				if _, exists := recordsets[logicalName]; !exists {
					recordsets[logicalName] = recordset
				}
			}
		}
	}
	// Both handles wait for a lock: the one the driver reads and writes with, and the one
	// that reads the columns of a table.
	dsn := sqliteWaitDSN(path, wait)
	db, err := newSQLiteDatabase(dsn, dal.NewSchema(nil, nil),
		dalgo2sql.DbOptions{Recordsets: recordsets, StructuredQueryDialect: "sqlite"})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open SQLite at %s: %w", path, err)
	}
	columns, err := openColumnsDB(dsn)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("failed to open SQLite at %s: %w", path, err)
	}
	if err := verifySQLiteKeys(context.Background(), columns, keys); err != nil {
		_ = columns.Close()
		_ = db.Close()
		return nil, nil, err
	}
	return &sqliteMount{Database: db, columns: columns, recordKeys: keys, lockWait: wait}, []schema.Mode{schema.ModeStrict}, nil
}

// newSQLiteDatabase opens the handle the driver reads and writes with, at the name the
// mount gives it (see busyTimeoutDSN).
var newSQLiteDatabase = dalgo2sqlite.NewDatabaseWithOptions

// openColumnsDB opens the handle the mount reads the columns of its tables with, at the
// name the mount gives it (see busyTimeoutDSN).
// The driver the mount is built on keeps its own handle to itself and hides the
// schema it reads from it, so the mount reads the columns of a table through a
// second handle on the same file. The handle is opened without a query, so it
// touches nothing until a table is asked for, and it only ever reads.
var openColumnsDB = func(path string) (*sql.DB, error) { return sql.Open("sqlite", path) }

// The SQLite driver embeds dal.DB, so explicitly forward the trusted mount
// factory; core subsequently retains only its secured result.
type sqliteMount struct {
	*dalgo2sqlite.Database
	// columns is the second handle on the file, which JoinFields reads the columns
	// of a table with. Close closes it with the driver.
	columns    *sql.DB
	recordKeys map[string]string
	lockWait   time.Duration
}

// RunReadwriteTransaction is the driver's. A write that waits for a lock past the deadline
// of its request ends with the deadline's error (see overDeadline).
//
// That holds for a mount with no access policies. core.New puts the database that
// ConfigureProtectedAccess returns in the place of a mount that has them, so the writes of such
// a mount do not pass through this method: they wait for the lock all the same (it is the same
// handle, opened with the busy timeout), and what a write that waited past its deadline reports
// there is the driver's own, which may be the lock error. The database it returns is not
// wrapped here: the driver's protected database offers more than dal.DB (a write session),
// which a wrapper that embeds dal.DB would hide from the code that looks for it.
func (s *sqliteMount) RunReadwriteTransaction(ctx context.Context, f dal.RWTxWorker, options ...dal.TransactionOption) error {
	return overDeadline(ctx, s.Database.RunReadwriteTransaction(ctx, f, options...))
}

func (s *sqliteMount) ConfigureProtectedAccess(participants ...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error) {
	factory, ok := s.DB.(interface {
		ConfigureProtectedAccess(...access.MandatoryParticipant) (dal.DB, *access.EnforcementCoordinator, error)
	})
	if !ok {
		return nil, nil, fmt.Errorf("SQLite protected storage unavailable")
	}
	return factory.ConfigureProtectedAccess(participants...)
}

// Close closes the driver and the second handle. Both are closed whatever the
// first returns, and the errors are returned together.
func (s *sqliteMount) Close() error {
	return errors.Join(s.Database.Close(), s.columns.Close())
}

var _ dal.JoinFieldsProvider = (*sqliteMount)(nil)

// JoinFields supplies the fields of a collection to the join engine: the columns
// of its table in the order the table declares them, the key column included, as
// a read of the whole table returns them. The name is the one the database gives
// the adapter for the collection (core.Database.CanonicalCollection), which is
// the name of the table, whatever spelling the manifest keys it by.
//
// It supplies nothing, with no error, for a source that is not a plain collection
// of the database (a derived source, or one qualified by a schema or a parent
// record, which the database never gives it) and for a table that has no column
// (one that is not there), so the engine reads the source as it does a source
// that has no schema, and the read says what is wrong with it.
func (s *sqliteMount) JoinFields(ctx context.Context, source dal.RecordsetSource) ([]string, error) {
	ref, ok := source.(dal.CollectionRef)
	if !ok || ref.Schema() != "" || ref.Parent() != nil {
		return nil, nil
	}
	fields, err := tableColumns(ctx, s.queryColumns, ref.Name())
	if err != nil {
		return nil, fmt.Errorf("cannot read the columns of table %q: %w", ref.Name(), overDeadline(ctx, err))
	}
	return fields, nil
}

func (s *sqliteMount) queryColumns(ctx context.Context, statement string, args ...any) (columnRows, error) {
	rows, err := s.columns.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// columnRows is the part of *sql.Rows that reading the columns of a table uses.
type columnRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// columnsOfTable lists the columns a read of the whole table returns, in the order
// of the table: the ordinary ones (hidden is 0) and the generated ones (2 and 3).
// A hidden column of a virtual table (1) is not returned by a read, and neither is
// it listed. The name of the table is a parameter, so a name that holds a quote or
// a space is read as the name it is.
const columnsOfTable = `SELECT name FROM pragma_table_xinfo(?) WHERE hidden IN (0, 2, 3) ORDER BY cid`

// tableColumns reads the column names of table through query. It returns nil for
// a table that has none.
func tableColumns(ctx context.Context, query func(ctx context.Context, statement string, args ...any) (columnRows, error), table string) ([]string, error) {
	rows, err := query(ctx, columnsOfTable, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

// SQLiteRecordKeys reports only keys verified against the physical SQLite file.
func (s *sqliteMount) SQLiteRecordKeys() (map[string]string, time.Duration) {
	out := make(map[string]string, len(s.recordKeys))
	for name, key := range s.recordKeys {
		out[name] = key
	}
	return out, s.lockWait
}

func sqliteRecordKeys(m *manifest.Manifest) (map[string]string, error) {
	if m.Storage.SQLite == nil || m.Storage.SQLite.RecordKeys == nil {
		return nil, nil
	}
	configured := m.Storage.SQLite.RecordKeys
	if len(configured) == 0 || m.Schemas == nil {
		return nil, fmt.Errorf("SQLite record keys require declared collections")
	}
	keys := make(map[string]string)
	spellings := make(map[string]string)
	for name, collection := range m.Schemas.Collections {
		canonical := name
		if logical, ok := core.SQLiteLogicalName(name); ok {
			canonical = logical
		}
		// Opted-in maps use native logical names, never quoted aliases.
		key, ok := configured[canonical]
		if !ok || key == "" {
			return nil, fmt.Errorf("SQLite record key missing for collection %q", canonical)
		}
		if _, exists := keys[canonical]; exists {
			return nil, fmt.Errorf("ambiguous SQLite record key collection %q", canonical)
		}
		for _, spelling := range []string{name, canonical} {
			folded := strings.ToLower(spelling)
			if prior, exists := spellings[folded]; exists && prior != canonical {
				return nil, fmt.Errorf("ambiguous SQLite record key collection %q", canonical)
			}
			spellings[folded] = canonical
		}
		field, ok := collection.Fields[key]
		if !ok || field.Type != schema.TypeString {
			return nil, fmt.Errorf("SQLite record key for %q must name a declared string field", canonical)
		}
		keys[canonical] = key
	}
	if len(configured) != len(keys) {
		return nil, fmt.Errorf("SQLite record key map must cover exactly the declared canonical collections")
	}
	return keys, nil
}

// quoteSQLiteIdentifier is used only for mount-time physical integrity checks.
// Query/record SQL generation and quoting remain the DALgo driver's responsibility.
func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func verifySQLiteKeys(ctx context.Context, db *sql.DB, keys map[string]string) error {
	for table, key := range keys {
		var kind, actualType string
		if err := db.QueryRowContext(ctx, `SELECT type FROM sqlite_schema WHERE name = ? COLLATE BINARY`, table).Scan(&kind); err != nil || kind != "table" {
			return fmt.Errorf("SQLite serving collection %q must be an existing physical table", table)
		}
		if err := db.QueryRowContext(ctx, `SELECT type FROM pragma_table_xinfo(?) WHERE name = ? COLLATE BINARY AND hidden = 0`, table, key).Scan(&actualType); err != nil || !strings.EqualFold(actualType, "TEXT") {
			return fmt.Errorf("SQLite serving key of %q must be a physical TEXT column", table)
		}
		var indexed bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pragma_index_list(?) AS i WHERE i.[unique] = 1 AND i.partial = 0 AND (SELECT count(*) FROM pragma_index_xinfo(i.name) WHERE [key] = 1) = 1 AND EXISTS(SELECT 1 FROM pragma_index_xinfo(i.name) WHERE [key] = 1 AND name = ? COLLATE BINARY))`, table, key).Scan(&indexed)
		if err != nil {
			return fmt.Errorf("verify SQLite serving index: %w", err)
		}
		if !indexed {
			return fmt.Errorf("SQLite serving key of %q requires a complete single-column unique index", table)
		}
		column, relation := quoteSQLiteIdentifier(key), quoteSQLiteIdentifier(table)
		// A unique index can override the column's collation. The ordinary ORDER BY
		// uses the column's own collation, so distinct index keys must also be unique
		// under that comparison (e.g. NOCASE column + BINARY unique index).
		var duplicateOrder bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+relation+` GROUP BY `+column+` HAVING count(*) > 1 LIMIT 1)`).Scan(&duplicateOrder); err != nil {
			return fmt.Errorf("verify SQLite serving order: %w", err)
		}
		if duplicateOrder {
			return fmt.Errorf("SQLite serving key of %q is not unique under its ordering collation", table)
		}
		rows, err := db.QueryContext(ctx, `SELECT `+column+`, typeof(`+column+`) FROM `+relation)
		if err != nil {
			return fmt.Errorf("verify SQLite serving values: %w", err)
		}
		for rows.Next() {
			var value any
			var valueType string
			if err := rows.Scan(&value, &valueType); err != nil {
				_ = rows.Close()
				return fmt.Errorf("verify SQLite serving values: %w", err)
			}
			text, ok := value.(string)
			if !ok || valueType != "text" || !utf8.ValidString(text) || record.ValidateStringID(text) != nil || core.ValidateSegment(text) != nil {
				_ = rows.Close()
				return fmt.Errorf("SQLite serving key of %q contains an invalid transport ID", table)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("verify SQLite serving values: %w", err)
		}

	}
	return nil
}
