package mount

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/dalgo2postgres"
	"github.com/dal-go/dalgo2sql"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/openvaultdb/openvaultdb-go/pkg/manifest"
	"github.com/openvaultdb/openvaultdb-go/pkg/schema"
)

// postgresOpener opens and pings a PostgreSQL connection, as
// dalgo2postgres.NewDatabaseWithOptions does. openPostgresWith takes it so that a
// test can make the open fail the way a driver does.
type postgresOpener func(dsn string, schema dal.Schema, opts dalgo2sql.DbOptions, options ...dalgo2postgres.Option) (*dalgo2postgres.Database, error)

// openPostgresWithExcludedRelations opens a PostgreSQL mount while omitting
// exact schema/relation pairs from native read-only discovery. Exclusions are
// deliberately supplied by the mounting consumer, not stored in a manifest,
// so the generic mount keeps its existing expose-all default.
func openPostgresWithExcludedRelations(m *manifest.Manifest, excluded []schema.NativeCollectionSource) (dal.DB, []schema.Mode, error) {
	return openPostgresWithExclusions(m, dalgo2postgres.NewDatabaseWithOptions, excluded)
}

// openPostgresWith is openPostgresWithExclusions with no excluded relations.
// An error from it is not returned or wrapped (see postgresOpenError).
func openPostgresWith(m *manifest.Manifest, open postgresOpener) (dal.DB, []schema.Mode, error) {
	return openPostgresWithExclusions(m, open, nil)
}

func openPostgresWithExclusions(m *manifest.Manifest, open postgresOpener, excluded []schema.NativeCollectionSource) (dal.DB, []schema.Mode, error) {
	// A name the database cannot keep whole is refused before the environment is read
	// and before any connection is made, so that nothing is created for the entries
	// before it (the collections are provisioned one by one when the database opens).
	// manifest.Validate refuses it already; this is for a manifest that was not validated.
	nativeReadOnly := m.Storage.Postgres != nil && m.Storage.Postgres.ReadOnly
	if len(excluded) > 0 && !nativeReadOnly {
		return nil, nil, errors.New("native PostgreSQL relation exclusions require a read-only native mount")
	}
	excludedRelations := make(map[schema.NativeCollectionSource]struct{}, len(excluded))
	for _, relation := range excluded {
		if _, err := schema.NativePostgresCollectionID(relation.Schema, relation.Name); err != nil {
			return nil, nil, errors.New("native PostgreSQL relation exclusion has an invalid schema or relation name")
		}
		if _, exists := excludedRelations[relation]; exists {
			return nil, nil, errors.New("native PostgreSQL relation exclusions contain a duplicate pair")
		}
		excludedRelations[relation] = struct{}{}
	}
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
	if nativeReadOnly {
		var err error
		dsn, err = postgresReadOnlyDSN(dsn)
		if err != nil {
			return nil, nil, err
		}
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
		dbOptions.PreserveBinaryValues = true
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
		visibilityDB, openErr := sql.Open("pgx", dsn)
		if openErr != nil {
			cancel()
			_ = db.Close()
			return nil, nil, errors.New("could not check PostgreSQL read privileges")
		}
		nativeSchemas, err := discoverNativePostgresSchemasExcluding(ctx, catalog, postgresSelectVisibility{db: visibilityDB}, excludedRelations)
		closeErr := visibilityDB.Close()
		cancel()
		if err != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("failed to discover native PostgreSQL schema: %w", err)
		}
		if closeErr != nil {
			_ = db.Close()
			return nil, nil, errors.New("could not finish checking PostgreSQL read privileges")
		}
		m.Schemas = nativeSchemas
	}
	return db, []schema.Mode{schema.ModeStrict}, nil
}

// postgresReadOnlyDSN forces the PostgreSQL session default to read-only for a
// native read-only mount. This protects structured SELECTs that invoke volatile
// functions or read views, even when the configured role has broader grants.
// The caller's other connection settings remain intact. PostgreSQL's `options`
// parameter can override startup settings, so it is refused for this profile.
func postgresReadOnlyDSN(dsn string) (string, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The adapter below reports connection-string errors without exposing the
		// DSN. Keep this early check equally non-disclosing.
		return "", errors.New("native read-only PostgreSQL mounts require a valid PostgreSQL connection string")
	}
	if strings.TrimSpace(config.RuntimeParams["options"]) != "" {
		return "", errors.New("native read-only PostgreSQL mounts do not support the PostgreSQL options connection parameter")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("native read-only PostgreSQL mounts require a valid PostgreSQL connection string")
		}
		query := parsed.Query()
		query.Set("default_transaction_read_only", "on")
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	}
	// pgx's keyword/value parser keeps the last occurrence of a setting. Appending
	// the enforced value therefore overrides an earlier `off` without rewriting
	// or re-encoding credentials and connection settings.
	return dsn + " default_transaction_read_only='on'", nil
}

type nativePostgresCatalog interface {
	ListSchemas(context.Context) ([]string, error)
	ListSchemaCollections(context.Context, string) ([]dal.CollectionRef, error)
	DescribeCollection(context.Context, *dal.CollectionRef) (*dbschema.CollectionDef, error)
}

type nativePostgresSelectVisibility interface {
	SelectableRelations(context.Context, string) (map[string]bool, error)
}

// Native catalog descriptions run concurrently because each relation requires
// several metadata round trips. Keep the pool small and fixed so broad schemas
// fit the mount's single discovery deadline without creating one connection per
// relation.
const nativePostgresDescribeConcurrency = 4

type nativePostgresRelation struct {
	schema string
	ref    dal.CollectionRef
	id     string
}

type nativePostgresDescribeResult struct {
	definition *dbschema.CollectionDef
	err        error
	skipped    bool
}

type nativePostgresDescribeError struct {
	schema   string
	relation string
	cause    error
}

func (e *nativePostgresDescribeError) Error() string {
	return fmt.Sprintf("could not describe PostgreSQL relation %q.%q", e.schema, e.relation)
}

func (e *nativePostgresDescribeError) Unwrap() error { return e.cause }

type postgresSelectVisibility struct{ db *sql.DB }

func (v postgresSelectVisibility) SelectableRelations(ctx context.Context, schemaName string) (map[string]bool, error) {
	rows, err := v.db.QueryContext(ctx, `
		SELECT c.relname, has_table_privilege(current_user, c.oid, 'SELECT')
		FROM pg_catalog.pg_class AS c
		JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')`, schemaName)
	if err != nil {
		return nil, errors.New("could not check PostgreSQL relation read privileges")
	}
	defer func() { _ = rows.Close() }()
	selectable := make(map[string]bool)
	for rows.Next() {
		var relation string
		var canSelect bool
		if err := rows.Scan(&relation, &canSelect); err != nil {
			return nil, errors.New("could not read PostgreSQL relation privileges")
		}
		selectable[relation] = canSelect
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("could not read PostgreSQL relation privileges")
	}
	return selectable, nil
}

// discoverNativePostgresSchemas maps the provider catalog into the public
// logical schema while retaining exact physical names separately. It never
// uses those names to build SQL; structured queries pass the tuple to DALgo.
func discoverNativePostgresSchemas(ctx context.Context, catalog nativePostgresCatalog, visibility nativePostgresSelectVisibility) (*schema.Schemas, error) {
	return discoverNativePostgresSchemasExcluding(ctx, catalog, visibility, nil)
}

func discoverNativePostgresSchemasExcluding(ctx context.Context, catalog nativePostgresCatalog, visibility nativePostgresSelectVisibility, excluded map[schema.NativeCollectionSource]struct{}) (*schema.Schemas, error) {
	schemas, err := catalog.ListSchemas(ctx)
	if err != nil {
		return nil, errors.New("could not list PostgreSQL schemas")
	}
	var relations []nativePostgresRelation
	for _, schemaName := range schemas {
		selectable, err := visibility.SelectableRelations(ctx, schemaName)
		if err != nil {
			return nil, errors.New("could not check PostgreSQL relation read privileges")
		}
		refs, err := catalog.ListSchemaCollections(ctx, schemaName)
		if err != nil {
			return nil, errors.New("could not list PostgreSQL relations")
		}
		for _, ref := range refs {
			if ref.Schema() != schemaName || ref.Parent() != nil || ref.Name() == "" {
				continue
			}
			if _, omit := excluded[schema.NativeCollectionSource{Schema: schemaName, Name: ref.Name()}]; omit {
				continue
			}
			if !selectable[ref.Name()] {
				continue
			}
			id, err := schema.NativePostgresCollectionID(schemaName, ref.Name())
			if err != nil {
				// PostgreSQL's catalog can contain identifiers outside the public
				// route contract. Skip those relations rather than normalizing them.
				continue
			}
			relations = append(relations, nativePostgresRelation{schema: schemaName, ref: ref, id: id})
		}
	}
	definitions, err := describeNativePostgresRelations(ctx, catalog, relations)
	if err != nil {
		return nil, err
	}
	result := &schema.Schemas{Collections: make(map[string]schema.Collection, len(relations))}
	for i, relation := range relations {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("native PostgreSQL discovery deadline ended: %w", err)
		}
		def := definitions[i]
		collection := schema.Collection{Source: &schema.NativeCollectionSource{Schema: relation.schema, Name: relation.ref.Name()}, Fields: map[string]schema.Field{}}
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
				decimal := schema.Decimal{Storage: "text", Unbounded: true}
				if field.Precision != nil && field.Precision.Total > 0 && field.Precision.Total <= 1000 {
					precision, scale := field.Precision.Total, field.Precision.Scale
					if scale >= 0 && scale <= precision {
						decimal.Precision, decimal.Scale, decimal.Unbounded = precision, scale, false
					}
				}
				mapped.Decimal = &decimal
			}
			collection.Fields[name] = mapped
		}
		if len(collection.Fields) == 0 {
			continue
		}
		result.Collections[relation.id] = collection
	}
	if len(result.Collections) == 0 {
		return nil, errors.New("no supported PostgreSQL relations were discovered")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("native PostgreSQL discovery deadline ended: %w", err)
	}
	return result, nil
}

func describeNativePostgresRelations(ctx context.Context, catalog nativePostgresCatalog, relations []nativePostgresRelation) ([]*dbschema.CollectionDef, error) {
	if len(relations) == 0 {
		return nil, nil
	}
	workerCount := min(len(relations), nativePostgresDescribeConcurrency)
	discoveryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make([]nativePostgresDescribeResult, len(relations))
	jobs := make(chan int, len(relations))
	for i := range relations {
		jobs <- i
	}
	close(jobs)

	var workers sync.WaitGroup
	var failureMu sync.Mutex
	firstFailureIndex := -1
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if err := discoveryCtx.Err(); err != nil {
					results[index] = nativePostgresDescribeResult{err: &nativePostgresDescribeError{schema: relations[index].schema, relation: relations[index].ref.Name(), cause: err}, skipped: true}
					continue
				}
				definition, err := catalog.DescribeCollection(discoveryCtx, &relations[index].ref)
				if err == nil && definition == nil {
					err = errors.New("provider returned no collection definition")
				}
				if err != nil {
					failureMu.Lock()
					internalCancellation := ctx.Err() == nil && errors.Is(err, context.Canceled) && firstFailureIndex >= 0
					failureMu.Unlock()
					if internalCancellation {
						results[index] = nativePostgresDescribeResult{err: &nativePostgresDescribeError{schema: relations[index].schema, relation: relations[index].ref.Name(), cause: err}, skipped: true}
						continue
					}
					results[index] = nativePostgresDescribeResult{err: &nativePostgresDescribeError{schema: relations[index].schema, relation: relations[index].ref.Name(), cause: err}}
					failureMu.Lock()
					if firstFailureIndex < 0 {
						firstFailureIndex = index
					}
					failureMu.Unlock()
					cancel()
					continue
				}
				results[index].definition = definition
			}
		}()
	}
	workers.Wait()
	failureMu.Lock()
	firstFailure := firstFailureIndex
	failureMu.Unlock()
	if firstFailure >= 0 {
		return nil, results[firstFailure].err
	}

	definitions := make([]*dbschema.CollectionDef, len(relations))
	for i, result := range results {
		if result.err != nil {
			if result.skipped && errors.Is(result.err, context.Canceled) && ctx.Err() == nil {
				continue
			}
			return nil, result.err
		}
		definitions[i] = result.definition
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("native PostgreSQL discovery deadline ended: %w", err)
	}
	return definitions, nil
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
