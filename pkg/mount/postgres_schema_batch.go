package mount

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

// nativePostgresSchemaDescriber describes every relation of one schema in a
// fixed number of round trips, instead of the five per relation that
// DescribeCollection of the adapter costs. A native mount of a schema with 82
// relations over a 100 ms round trip spent most of its 30 s discovery deadline on
// those per-relation queries; the batched reader needs two queries per schema.
//
// The definitions carry exactly what native discovery reads from
// DescribeCollection: each field's name, type, decimal precision and nullability,
// the primary key in key order, and each column's declared type.
// TestPostgresIntegration_NativeBatchedDiscoveryMatchesPerRelationDescribe proves
// against a live server that both paths discover the same schema.
type nativePostgresSchemaDescriber interface {
	DescribeSchemaRelations(ctx context.Context, schemaName string) (map[string]*dbschema.CollectionDef, error)
}

// nativePostgresRelationVisibleSQL is the privilege test dalgo2postgres applies
// to a relation before it describes it (information_schema.tables applies the
// same), spelled over pg_class aliased c.
const nativePostgresRelationVisibleSQL = `(pg_catalog.pg_has_role(c.relowner, 'USAGE')
	      OR pg_catalog.has_table_privilege(c.oid, 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
	      OR pg_catalog.has_any_column_privilege(c.oid, 'SELECT, INSERT, UPDATE, REFERENCES'))`

// nativePostgresSchemaColumnsSQL reads the columns of every relation of schema $1
// in the shape dalgo2postgres reads one relation's: information_schema.columns
// for tables, views, partitioned and foreign tables, and pg_attribute (spelled as
// information_schema would) for materialized views, which
// information_schema.columns does not list.
const nativePostgresSchemaColumnsSQL = `SELECT ic.table_name::text, ic.column_name::text, ic.data_type::text, ic.udt_name::text,
	        ic.numeric_precision::bigint, ic.numeric_scale::bigint, ic.is_nullable::text, ic.ordinal_position::bigint
	 FROM information_schema.columns AS ic
	 WHERE ic.table_schema = $1::text
	 UNION ALL
	 SELECT c.relname::text, a.attname::text,
	        CASE WHEN t.typelem <> 0 AND t.typlen = -1 THEN 'ARRAY'
	             WHEN tn.nspname = 'pg_catalog' THEN pg_catalog.format_type(t.oid, NULL)
	             ELSE 'USER-DEFINED' END,
	        t.typname::text,
	        CASE WHEN t.typname = 'numeric' AND a.atttypmod > 4 THEN ((a.atttypmod - 4) >> 16) & 65535 END::bigint,
	        CASE WHEN t.typname = 'numeric' AND a.atttypmod > 4 THEN (a.atttypmod - 4) & 65535 END::bigint,
	        CASE WHEN a.attnotnull THEN 'NO' ELSE 'YES' END,
	        a.attnum::bigint
	 FROM pg_catalog.pg_attribute AS a
	 JOIN pg_catalog.pg_class AS c ON c.oid = a.attrelid
	 JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
	 JOIN pg_catalog.pg_type AS t ON t.oid = a.atttypid
	 JOIN pg_catalog.pg_namespace AS tn ON tn.oid = t.typnamespace
	 WHERE n.nspname = $1::text AND c.relkind = 'm' AND a.attnum > 0 AND NOT a.attisdropped
	   AND NOT pg_catalog.pg_is_other_temp_schema(n.oid)
	   AND ` + nativePostgresRelationVisibleSQL + `
	 ORDER BY 1, 8`

// nativePostgresSchemaPrimaryKeysSQL reads the primary-key columns of every
// relation of schema $1, in key order.
const nativePostgresSchemaPrimaryKeysSQL = `SELECT c.relname::text, a.attname::text
	 FROM pg_catalog.pg_constraint AS con
	 JOIN pg_catalog.pg_class AS c ON c.oid = con.conrelid
	 JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
	 JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS key_column(attnum, position) ON true
	 JOIN pg_catalog.pg_attribute AS a ON a.attrelid = c.oid AND a.attnum = key_column.attnum
	 WHERE con.contype = 'p' AND n.nspname = $1::text
	   AND ` + nativePostgresRelationVisibleSQL + `
	 ORDER BY c.relname, key_column.position`

var queryPostgresSchemaColumns = func(ctx context.Context, db *sql.DB, schemaName string) (*sql.Rows, error) {
	return db.QueryContext(ctx, nativePostgresSchemaColumnsSQL, schemaName)
}

var queryPostgresSchemaPrimaryKeys = func(ctx context.Context, db *sql.DB, schemaName string) (*sql.Rows, error) {
	return db.QueryContext(ctx, nativePostgresSchemaPrimaryKeysSQL, schemaName)
}

// nativePostgresSchemaDescribeError reports a schema whose relations could not be
// described. Its text is fixed and names only the schema; it wraps the cause, so
// a caller can tell an ended deadline (context.DeadlineExceeded) from the rest.
type nativePostgresSchemaDescribeError struct {
	schema string
	cause  error
}

func (e *nativePostgresSchemaDescribeError) Error() string {
	return fmt.Sprintf("could not describe the PostgreSQL relations of schema %q", e.schema)
}

func (e *nativePostgresSchemaDescribeError) Unwrap() error { return e.cause }

// DescribeSchemaRelations describes every relation of schemaName that the role
// can see, keyed by relation name, in two queries.
func (v postgresSelectVisibility) DescribeSchemaRelations(ctx context.Context, schemaName string) (map[string]*dbschema.CollectionDef, error) {
	primaryKeys, err := readNativePostgresPrimaryKeys(ctx, v.db, schemaName)
	if err != nil {
		return nil, &nativePostgresSchemaDescribeError{schema: schemaName, cause: err}
	}
	rows, err := queryPostgresSchemaColumns(ctx, v.db, schemaName)
	if err != nil {
		return nil, &nativePostgresSchemaDescribeError{schema: schemaName, cause: err}
	}
	defer func() { _ = rows.Close() }()
	definitions := map[string]*dbschema.CollectionDef{}
	for rows.Next() {
		var (
			relation, column, dataType, udtName, nullable string
			precision, scale                              sql.NullInt64
			ordinal                                       int64
		)
		if err := rows.Scan(&relation, &column, &dataType, &udtName, &precision, &scale, &nullable, &ordinal); err != nil {
			return nil, &nativePostgresSchemaDescribeError{schema: schemaName, cause: err}
		}
		def := definitions[relation]
		if def == nil {
			def = &dbschema.CollectionDef{Name: relation, SourceDefinition: &dbschema.SourceDefinition{Dialect: "postgres"}}
			for _, key := range primaryKeys[relation] {
				def.PrimaryKey = append(def.PrimaryKey, dal.FieldName(key))
			}
			definitions[relation] = def
		}
		fieldType := nativePostgresColumnType(dataType, udtName)
		field := dbschema.FieldDef{Name: dal.FieldName(column), Type: fieldType, Nullable: nullable == "YES" && !primaryKeyHas(primaryKeys[relation], column)}
		if fieldType == dbschema.Decimal && precision.Valid && scale.Valid {
			field.Precision = &dbschema.Precision{Total: int(precision.Int64), Scale: int(scale.Int64)}
		}
		def.Fields = append(def.Fields, field)
		def.SourceDefinition.Columns = append(def.SourceDefinition.Columns, dbschema.SourceColumnDef{
			Name:               column,
			DeclaredType:       dataType,
			NotNull:            nullable == "NO",
			PrimaryKeyPosition: primaryKeyPosition(primaryKeys[relation], column),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, &nativePostgresSchemaDescribeError{schema: schemaName, cause: err}
	}
	return definitions, nil
}

func readNativePostgresPrimaryKeys(ctx context.Context, db *sql.DB, schemaName string) (map[string][]string, error) {
	rows, err := queryPostgresSchemaPrimaryKeys(ctx, db, schemaName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	keys := map[string][]string{}
	for rows.Next() {
		var relation, column string
		if err := rows.Scan(&relation, &column); err != nil {
			return nil, err
		}
		keys[relation] = append(keys[relation], column)
	}
	return keys, rows.Err()
}

func primaryKeyHas(key []string, column string) bool {
	return primaryKeyPosition(key, column) > 0
}

func primaryKeyPosition(key []string, column string) int {
	for i, name := range key {
		if name == column {
			return i + 1
		}
	}
	return 0
}

var nativePostgresNumericTypeRe = regexp.MustCompile(`^(?:NUMERIC|DECIMAL)\s*\(\s*(\d+)\s*,\s*(\d+)\s*\)$`)

// nativePostgresColumnType maps information_schema.columns data_type (or
// udt_name) to a dbschema.Type exactly as dalgo2postgres DescribeCollection does
// (dbschemaTypeFromPostgres): a type without a dbschema counterpart is a String.
func nativePostgresColumnType(dataType, udtName string) dbschema.Type {
	upper := strings.ToUpper(strings.TrimSpace(dataType))
	if m := nativePostgresNumericTypeRe.FindStringSubmatch(upper); m != nil {
		_, errTotal := strconv.Atoi(m[1])
		_, errScale := strconv.Atoi(m[2])
		if errTotal == nil && errScale == nil {
			return dbschema.Decimal
		}
	}
	switch upper {
	case "BIGINT", "INTEGER", "SMALLINT", "INT", "INT2", "INT4", "INT8":
		return dbschema.Int
	case "DOUBLE PRECISION", "REAL", "FLOAT4", "FLOAT8":
		return dbschema.Float
	case "BOOLEAN":
		return dbschema.Bool
	case "TIMESTAMP WITH TIME ZONE", "TIMESTAMPTZ", "TIMESTAMP WITHOUT TIME ZONE", "TIMESTAMP", "DATE",
		"TIME WITH TIME ZONE", "TIME WITHOUT TIME ZONE", "TIME":
		return dbschema.Time
	case "NUMERIC", "DECIMAL":
		return dbschema.Decimal
	case "BYTEA":
		return dbschema.Bytes
	}
	switch strings.ToLower(strings.TrimSpace(udtName)) {
	case "timestamptz":
		return dbschema.Time
	case "numeric":
		return dbschema.Decimal
	}
	return dbschema.String
}

// describeNativePostgresRelationsBatched describes relations with one batched
// read per schema. A listed relation the batch did not describe (it was dropped
// in between, or the role sees none of its columns) gets an empty definition,
// which discovery skips as it skips a relation without columns.
func describeNativePostgresRelationsBatched(ctx context.Context, describer nativePostgresSchemaDescriber, relations []nativePostgresRelation) ([]*dbschema.CollectionDef, error) {
	bySchema := map[string]map[string]*dbschema.CollectionDef{}
	definitions := make([]*dbschema.CollectionDef, len(relations))
	for i, relation := range relations {
		described, done := bySchema[relation.schema]
		if !done {
			var err error
			if described, err = describer.DescribeSchemaRelations(ctx, relation.schema); err != nil {
				return nil, err
			}
			bySchema[relation.schema] = described
		}
		definition := described[relation.ref.Name()]
		if definition == nil {
			definition = &dbschema.CollectionDef{Name: relation.ref.Name()}
		}
		definitions[i] = definition
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("native PostgreSQL discovery deadline ended: %w", err)
	}
	return definitions, nil
}

// ErrNativePostgresDiscoveryTimeout is wrapped by the error of a native read-only
// PostgreSQL mount whose catalogue read (discovery and the read-only role check)
// did not finish within Options.NativePostgresDiscoveryTimeout. The error also
// wraps context.DeadlineExceeded.
var ErrNativePostgresDiscoveryTimeout = errors.New("native PostgreSQL discovery did not finish in time")
