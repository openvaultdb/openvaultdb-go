package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
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
	nativeReadOnly := m.Storage.Postgres != nil && m.Storage.Postgres.ReadOnly
	if !nativeReadOnly {
		if err := m.CheckPostgresNames(); err != nil {
			return nil, nil, err
		}
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
	if !nativeReadOnly && m.Schemas != nil {
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
	dbOptions := dalgo2sql.DbOptions{
		Recordsets:     recordsets,
		Placeholder:    dalgo2sql.PlaceholderDollar,
		IdentifierCase: dalgo2sql.IdentifierCaseFoldLower,
	}
	var options []dalgo2postgres.Option
	if nativeReadOnly {
		dbOptions.IdentifierCase = dalgo2sql.IdentifierCaseExact
		dbOptions.ExactNumericValues = true
		options = append(options, dalgo2postgres.WithIdentifierMode(dalgo2postgres.IdentifierExact))
	}
	db, err := open(dsn, dal.NewSchema(nil, nil), dbOptions, options...)
	if err != nil {
		return nil, nil, postgresOpenError(envVar, err)
	}
	if nativeReadOnly {
		catalog, ok := any(db).(interface {
			ListSchemas(context.Context) ([]string, error)
			ListSchemaCollections(context.Context, string) ([]dal.CollectionRef, error)
			DescribeCollection(context.Context, *dal.CollectionRef) (*dbschema.CollectionDef, error)
		})
		if !ok {
			_ = db.Close()
			return nil, nil, errors.New("PostgreSQL driver does not provide native schema discovery")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		nativeSchemas, err := discoverNativePostgresSchemas(ctx, catalog)
		cancel()
		if err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("failed to discover native PostgreSQL schema: %w", err)
		}
		m.Schemas = nativeSchemas
	}
	return db, []schema.Mode{schema.ModeStrict}, nil
}

type nativePostgresCatalog interface {
	ListSchemas(context.Context) ([]string, error)
	ListSchemaCollections(context.Context, string) ([]dal.CollectionRef, error)
	DescribeCollection(context.Context, *dal.CollectionRef) (*dbschema.CollectionDef, error)
}

// discoverNativePostgresSchemas maps the provider catalog into the public
// logical schema while retaining exact physical names separately. It never
// uses those names to build SQL; structured queries pass the tuple to DALgo.
func discoverNativePostgresSchemas(ctx context.Context, catalog nativePostgresCatalog) (*schema.Schemas, error) {
	schemas, err := catalog.ListSchemas(ctx)
	if err != nil {
		return nil, errors.New("could not list PostgreSQL schemas")
	}
	result := &schema.Schemas{Collections: map[string]schema.Collection{}}
	for _, schemaName := range schemas {
		refs, err := catalog.ListSchemaCollections(ctx, schemaName)
		if err != nil {
			return nil, errors.New("could not list PostgreSQL relations")
		}
		for _, ref := range refs {
			if ref.Schema() != schemaName || ref.Parent() != nil || ref.Name() == "" {
				continue
			}
			id, err := schema.NativePostgresCollectionID(schemaName, ref.Name())
			if err != nil {
				// PostgreSQL's catalog can contain identifiers outside the public
				// route contract. Skip those relations rather than normalizing them.
				continue
			}
			def, err := catalog.DescribeCollection(ctx, &ref)
			if err != nil || def == nil {
				return nil, errors.New("could not describe a PostgreSQL relation")
			}
			collection := schema.Collection{Source: &schema.NativeCollectionSource{Schema: schemaName, Name: ref.Name()}, Fields: map[string]schema.Field{}}
			primary := make(map[string]bool, len(def.PrimaryKey))
			for _, field := range def.PrimaryKey {
				primary[string(field)] = true
			}
			for _, field := range def.Fields {
				name := string(field.Name)
				if name == "" || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 || len(name) > 63 {
					return nil, errors.New("PostgreSQL relation has a column outside the native name contract")
				}
				mapped := schema.Field{Type: nativePostgresFieldType(field.Type), NativeType: nativePostgresDeclaredType(def.SourceDefinition, name), PrimaryKey: primary[name], Nullable: field.Nullable, Required: !field.Nullable}
				if field.Type == dbschema.Decimal {
					precision, scale := 1000, 0 // unbounded PostgreSQL NUMERIC
					if field.Precision != nil && field.Precision.Total > 0 && field.Precision.Total <= 1000 {
						precision, scale = field.Precision.Total, field.Precision.Scale
					}
					if scale < 0 || scale > precision {
						scale = 0
					}
					mapped.Decimal = &schema.Decimal{Precision: precision, Scale: scale, Storage: "text"}
				}
				collection.Fields[name] = mapped
			}
			if len(collection.Fields) == 0 {
				continue
			}
			result.Collections[id] = collection
		}
	}
	if len(result.Collections) == 0 {
		return nil, errors.New("no supported PostgreSQL relations were discovered")
	}
	return result, nil
}

func nativePostgresFieldType(sourceType dbschema.Type) schema.FieldType {
	switch sourceType {
	case dbschema.Bool:
		return schema.TypeBoolean
	case dbschema.Int:
		return schema.TypeInteger
	case dbschema.Float:
		return schema.TypeNumber
	case dbschema.Decimal:
		return schema.TypeDecimal
	case dbschema.Bytes:
		return schema.TypeString // JSON transports []byte as base64 text
	case dbschema.Time:
		return schema.TypeString // the native type is retained separately
	default:
		return schema.TypeString
	}
}

func nativePostgresDeclaredType(def *dbschema.SourceDefinition, column string) string {
	if def == nil {
		return ""
	}
	for _, sourceColumn := range def.Columns {
		if sourceColumn.Name == column {
			return sourceColumn.DeclaredType
		}
	}
	return ""
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
