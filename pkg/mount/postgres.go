package mount

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
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

var openPostgresDatabase = dalgo2postgres.NewDatabaseWithOptions

// nativePostgresSettings are the mount Options that apply to a native read-only
// PostgreSQL mount.
type nativePostgresSettings struct {
	excluded            []schema.NativeCollectionSource
	statementTimeout    time.Duration
	discoveryTimeout    time.Duration
	requireReadOnlyRole bool
	allowInsecure       bool
}

// Defaults of the native read-only PostgreSQL mount Options.
const (
	// DefaultNativePostgresStatementTimeout is the statement_timeout a native
	// read-only PostgreSQL session starts with when Options sets none.
	DefaultNativePostgresStatementTimeout = 15 * time.Second
	// DefaultNativePostgresDiscoveryTimeout bounds the catalogue read of a native
	// read-only PostgreSQL mount when Options sets none.
	DefaultNativePostgresDiscoveryTimeout = 30 * time.Second
)

func nativeSettingsOf(opts Options) nativePostgresSettings {
	settings := nativePostgresSettings{
		excluded:            opts.ExcludedNativePostgresRelations,
		statementTimeout:    opts.NativePostgresStatementTimeout,
		discoveryTimeout:    opts.NativePostgresDiscoveryTimeout,
		requireReadOnlyRole: opts.RequireReadOnlyNativePostgresRole,
		allowInsecure:       opts.AllowInsecureNativePostgresTransport,
	}
	if settings.statementTimeout <= 0 {
		settings.statementTimeout = DefaultNativePostgresStatementTimeout
	}
	if settings.discoveryTimeout <= 0 {
		settings.discoveryTimeout = DefaultNativePostgresDiscoveryTimeout
	}
	return settings
}

// openPostgresWithOptions opens a PostgreSQL mount with the native read-only
// settings of opts. Exclusions omit exact schema/relation pairs from native
// read-only discovery. They are deliberately supplied by the mounting consumer,
// not stored in a manifest, so the generic mount keeps its existing expose-all
// default.
func openPostgresWithOptions(m *manifest.Manifest, opts Options) (dal.DB, []schema.Mode, error) {
	return openPostgresNative(m, openPostgresDatabase, nativeSettingsOf(opts), sqlOpenPostgresAccess)
}

// openPostgresWithExcludedRelations opens a PostgreSQL mount while omitting
// exact schema/relation pairs from native read-only discovery. Exclusions are
// deliberately supplied by the mounting consumer, not stored in a manifest,
// so the generic mount keeps its existing expose-all default.
func openPostgresWithExcludedRelations(m *manifest.Manifest, excluded []schema.NativeCollectionSource) (dal.DB, []schema.Mode, error) {
	return openPostgresWithExclusions(m, openPostgresDatabase, excluded)
}

// openPostgresWith is openPostgresWithExclusions with no excluded relations.
// An error from it is not returned or wrapped (see postgresOpenError).
func openPostgresWith(m *manifest.Manifest, open postgresOpener) (dal.DB, []schema.Mode, error) {
	return openPostgresWithExclusions(m, open, nil)
}

var openVisibilityDB = func(driverName, dataSourceName string) (*sql.DB, error) {
	return sql.Open(driverName, dataSourceName)
}

var closeVisibilityDB = func(db *sql.DB) error {
	return db.Close()
}

var closePostgresDatabase = func(db *dalgo2postgres.Database) error {
	if db == nil {
		return nil
	}
	return db.Close()
}

var discoverNativeSchemas = discoverNativePostgresSchemasExcluding

// openPostgresWithExclusions opens with the default native settings and the
// given exclusions. It does not require a verified TLS connection, so that a test
// can hand it any connection string; the mount entry point requires one unless
// Options allow otherwise.
func openPostgresWithExclusions(m *manifest.Manifest, open postgresOpener, excluded []schema.NativeCollectionSource) (dal.DB, []schema.Mode, error) {
	settings := nativeSettingsOf(Options{ExcludedNativePostgresRelations: excluded, AllowInsecureNativePostgresTransport: true})
	return openPostgresNative(m, open, settings, sqlOpenPostgresAccess)
}

// postgresAccessOpener opens the second connection a native mount checks the
// role's privileges with.
type postgresAccessOpener func(dsn string) (nativePostgresAccess, error)

func sqlOpenPostgresAccess(dsn string) (nativePostgresAccess, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	return postgresSelectVisibility{db: db}, nil
}

func openPostgresNative(m *manifest.Manifest, open postgresOpener, settings nativePostgresSettings, openAccess postgresAccessOpener) (dal.DB, []schema.Mode, error) {
	excluded := settings.excluded
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
		if settings, err = withInsecureTransportOptOut(m, settings); err != nil {
			return nil, nil, err
		}
		dsn, err = postgresReadOnlyDSN(dsn, settings)
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
			_ = closePostgresDatabase(db)
			return nil, nil, errors.New("PostgreSQL driver does not provide native schema discovery")
		}
		ctx, cancel := context.WithTimeout(context.Background(), settings.discoveryTimeout)
		access, openErr := openAccess(dsn)
		if openErr != nil {
			cancel()
			_ = closePostgresDatabase(db)
			return nil, nil, errors.New("could not check PostgreSQL read privileges")
		}
		nativeSchemas, err := discoverNativeSchemas(ctx, catalog, access, excludedRelations)
		if err != nil {
			err = fmt.Errorf("failed to discover native PostgreSQL schema: %w", err)
		} else if settings.requireReadOnlyRole {
			err = refuseWritableRole(ctx, access, nativeSchemas)
		}
		closeErr := access.Close()
		cancel()
		if err != nil {
			_ = closePostgresDatabase(db)
			return nil, nil, err
		}
		if closeErr != nil {
			_ = closePostgresDatabase(db)
			return nil, nil, errors.New("could not finish checking PostgreSQL read privileges")
		}
		m.Schemas = nativeSchemas
	}
	return db, []schema.Mode{schema.ModeStrict}, nil
}

// postgresReadOnlyDSN forces the PostgreSQL session defaults of a native
// read-only mount: default_transaction_read_only=on, which protects structured
// SELECTs that invoke volatile functions or read views even when the configured
// role has broader grants, and a statement_timeout, so that the server ends a
// statement the mount no longer waits for. The caller's other connection settings
// remain intact. PostgreSQL's `options` parameter can override startup settings,
// so it is refused for this profile. Unless settings allow an insecure transport
// or every host is on this machine (localPostgresHosts), the connection must be
// TLS with a verified server certificate (sslmode verify-full or verify-ca, or
// require with a root certificate) and no plaintext fallback.
func postgresReadOnlyDSN(dsn string, settings nativePostgresSettings) (string, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The adapter below reports connection-string errors without exposing the
		// DSN. Keep this early check equally non-disclosing.
		return "", errors.New("native read-only PostgreSQL mounts require a valid PostgreSQL connection string")
	}
	if strings.TrimSpace(config.RuntimeParams["options"]) != "" {
		return "", errors.New("native read-only PostgreSQL mounts do not support the PostgreSQL options connection parameter")
	}
	if !settings.allowInsecure && !localPostgresHosts(config) {
		// Every way pgx may connect must verify: the first attempt and each fallback
		// (sslmode=prefer and allow fall back to a plaintext connection).
		attempts := []*tls.Config{config.TLSConfig}
		for _, fallback := range config.Fallbacks {
			attempts = append(attempts, fallback.TLSConfig)
		}
		for _, attempt := range attempts {
			if !verifiedTLS(attempt) {
				return "", errInsecureTransport
			}
		}
	}
	timeout := strconv.FormatInt(settings.statementTimeout.Milliseconds(), 10)
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, _ := url.Parse(dsn)
		query := parsed.Query()
		query.Set("default_transaction_read_only", "on")
		query.Set("statement_timeout", timeout)
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	}
	// pgx's keyword/value parser keeps the last occurrence of a setting. Appending
	// the enforced values therefore overrides an earlier one without rewriting or
	// re-encoding credentials and connection settings.
	return dsn + " default_transaction_read_only='on' statement_timeout='" + timeout + "'", nil
}

// InsecureNativePostgresTransportEnv, set to true, lets every native read-only
// PostgreSQL mount of the process reach a remote server without a verified TLS
// connection, as storage.postgres.insecure_transport does for one manifest. It is
// how a command-line tool that mounts manifests as they are reaches a local proxy
// or a development server on another host.
const InsecureNativePostgresTransportEnv = "OVDB_NATIVE_POSTGRES_INSECURE_TRANSPORT"

var errInsecureTransport = errors.New("native read-only PostgreSQL mounts to a remote host require a verified TLS connection: sslmode=verify-full (or verify-ca); " +
	"for local development set storage.postgres.insecure_transport: true in the manifest, " + InsecureNativePostgresTransportEnv + "=true in the environment, " +
	"or mount.Options.AllowInsecureNativePostgresTransport (loopback and Unix-socket hosts need none of these)")

// withInsecureTransportOptOut applies the manifest's and the environment's opt-out
// of verified TLS to settings, and logs a warning whenever any opt-out is set.
func withInsecureTransportOptOut(m *manifest.Manifest, settings nativePostgresSettings) (nativePostgresSettings, error) {
	var sources []string
	if settings.allowInsecure {
		sources = append(sources, "mount.Options.AllowInsecureNativePostgresTransport")
	}
	if m.Storage.Postgres.InsecureTransport {
		sources = append(sources, "storage.postgres.insecure_transport")
	}
	if value := strings.TrimSpace(os.Getenv(InsecureNativePostgresTransportEnv)); value != "" {
		allowed, err := strconv.ParseBool(value)
		if err != nil {
			return settings, fmt.Errorf("%s must be true or false", InsecureNativePostgresTransportEnv)
		}
		if allowed {
			sources = append(sources, InsecureNativePostgresTransportEnv)
		}
	}
	if len(sources) > 0 {
		settings.allowInsecure = true
		slog.Warn("native PostgreSQL mount may connect without a verified TLS connection; use this for local development only",
			"database", m.Database.ID, "opt-out", strings.Join(sources, ", "))
	}
	return settings, nil
}

// localPostgresHosts reports whether every host a connection may use is on this
// machine: a Unix-socket directory, a loopback IP address, or the name
// localhost. Traffic to them never crosses a network, so TLS adds nothing. A
// name other than localhost is not resolved and does not qualify: what it
// resolves to can change.
func localPostgresHosts(config *pgx.ConnConfig) bool {
	hosts := []string{config.Host}
	for _, fallback := range config.Fallbacks {
		hosts = append(hosts, fallback.Host)
	}
	for _, host := range hosts {
		if strings.HasPrefix(host, "/") || strings.EqualFold(host, "localhost") {
			continue
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			continue
		}
		return false
	}
	return true
}

// verifiedTLS reports whether a connection made with config verifies the
// server's certificate: TLS with the standard verification (verify-full), or with
// pgx's own chain check (verify-ca, and require with a root certificate).
func verifiedTLS(config *tls.Config) bool {
	return config != nil && (!config.InsecureSkipVerify || config.VerifyPeerCertificate != nil)
}

type nativePostgresCatalog interface {
	ListSchemas(context.Context) ([]string, error)
	ListSchemaCollections(context.Context, string) ([]dal.CollectionRef, error)
	DescribeCollection(context.Context, *dal.CollectionRef) (*dbschema.CollectionDef, error)
}

type nativePostgresSelectVisibility interface {
	SelectableRelations(context.Context, string) (map[string]bool, error)
}

// nativePostgresAccess is the privilege view of the role a native mount connects
// as: what it may read, and every way it could change data or the schema.
type nativePostgresAccess interface {
	nativePostgresSelectVisibility
	// WriteCapabilities lists, as short reasons, every way the role could write
	// (writeCapabilitiesQuery); it is empty for a read-only role. The exposed
	// relations are given as parallel lists of schema and relation names.
	WriteCapabilities(ctx context.Context, exposedSchemas, relationSchemas, relationNames []string) ([]string, error)
	Close() error
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

func (v postgresSelectVisibility) Close() error { return v.db.Close() }

// writeCapabilitiesQuery lists every way the current role could change data or the
// schema. Membership is MEMBER, not USAGE: a NOINHERIT member of a role does not
// use its privileges by default but can SET ROLE to it, so each privilege is
// checked for every role the current role is a member of, itself included, and
// privileges granted to PUBLIC count too (the has_*_privilege functions include
// them). $1 lists the exposed schemas; $2 and $3 the exposed relations.
const writeCapabilitiesQuery = `
WITH member_of AS (
	SELECT r.oid, r.rolname, r.rolsuper, r.rolcreaterole, r.rolcreatedb, r.rolreplication, r.rolbypassrls
	FROM pg_catalog.pg_roles AS r
	WHERE pg_catalog.pg_has_role(current_user, r.oid, 'MEMBER')
), exposed AS (
	SELECT c.oid, n.nspname, c.relname
	FROM unnest($2::text[], $3::text[]) AS e(nspname, relname)
	JOIN pg_catalog.pg_namespace AS n ON n.nspname = e.nspname
	JOIN pg_catalog.pg_class AS c ON c.relnamespace = n.oid AND c.relname = e.relname
), exposed_schemas AS (
	SELECT n.oid, n.nspname FROM pg_catalog.pg_namespace AS n
	WHERE n.nspname = ANY($1::text[]) OR n.nspname = 'public'
), user_namespaces AS (
	SELECT n.oid, n.nspname, n.nspowner FROM pg_catalog.pg_namespace AS n
	WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp\_%'
)
SELECT reason FROM (
	SELECT format('role %I has the %s attribute', m.rolname, a.attribute)
	FROM member_of AS m, LATERAL (VALUES ('SUPERUSER', m.rolsuper), ('CREATEROLE', m.rolcreaterole), ('CREATEDB', m.rolcreatedb), ('REPLICATION', m.rolreplication), ('BYPASSRLS', m.rolbypassrls)) AS a(attribute, held)
	WHERE a.held
	UNION ALL
	SELECT format('is a member of %I', m.rolname) FROM member_of AS m
	WHERE m.rolname IN ('pg_write_all_data', 'pg_execute_server_program', 'pg_read_server_files', 'pg_write_server_files')
	UNION ALL
	SELECT format('owns schema %I (as %I)', n.nspname, m.rolname) FROM user_namespaces AS n JOIN member_of AS m ON m.oid = n.nspowner
	UNION ALL
	SELECT format('owns relation %I.%I (as %I)', n.nspname, c.relname, m.rolname)
	FROM pg_catalog.pg_class AS c JOIN user_namespaces AS n ON n.oid = c.relnamespace JOIN member_of AS m ON m.oid = c.relowner
	UNION ALL
	SELECT format('owns function %I.%I (as %I)', n.nspname, p.proname, m.rolname)
	FROM pg_catalog.pg_proc AS p JOIN user_namespaces AS n ON n.oid = p.pronamespace JOIN member_of AS m ON m.oid = p.proowner
	UNION ALL
	SELECT format('may write %I.%I (as %I)', e.nspname, e.relname, m.rolname) FROM exposed AS e, member_of AS m
	WHERE pg_catalog.has_table_privilege(m.oid, e.oid, 'INSERT, UPDATE, DELETE, TRUNCATE, TRIGGER, REFERENCES')
		OR pg_catalog.has_any_column_privilege(m.oid, e.oid, 'INSERT, UPDATE, REFERENCES')
	UNION ALL
	SELECT format('may create in schema %I (as %I)', s.nspname, m.rolname) FROM exposed_schemas AS s, member_of AS m
	WHERE pg_catalog.has_schema_privilege(m.oid, s.oid, 'CREATE')
	UNION ALL
	SELECT format('may use or update sequence %I.%I (as %I)', s.nspname, c.relname, m.rolname)
	FROM pg_catalog.pg_class AS c JOIN exposed_schemas AS s ON s.oid = c.relnamespace, member_of AS m
	WHERE c.relkind = 'S' AND pg_catalog.has_sequence_privilege(m.oid, c.oid, 'USAGE, UPDATE')
	UNION ALL
	SELECT format('may execute SECURITY DEFINER function %I.%I', s.nspname, p.proname)
	FROM pg_catalog.pg_proc AS p JOIN exposed_schemas AS s ON s.oid = p.pronamespace
	WHERE p.prosecdef AND pg_catalog.has_function_privilege(current_user, p.oid, 'EXECUTE')
) AS reasons(reason)
LIMIT 20`

func (v postgresSelectVisibility) WriteCapabilities(ctx context.Context, exposedSchemas, relationSchemas, relationNames []string) ([]string, error) {
	rows, err := v.db.QueryContext(ctx, writeCapabilitiesQuery, exposedSchemas, relationSchemas, relationNames)
	if err != nil {
		return nil, errors.New("could not check the PostgreSQL role's write privileges")
	}
	defer func() { _ = rows.Close() }()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			return nil, errors.New("could not read the PostgreSQL role's write privileges")
		}
		reasons = append(reasons, reason)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("could not read the PostgreSQL role's write privileges")
	}
	return reasons, nil
}

// refuseWritableRole refuses a native read-only mount whose role could change
// data or the schema (Options.RequireReadOnlyNativePostgresRole): see
// writeCapabilitiesQuery for what counts. The session is read-only either way;
// this check makes a misconfigured role a mount failure instead of a latent risk.
func refuseWritableRole(ctx context.Context, access nativePostgresAccess, schemas *schema.Schemas) error {
	var relationSchemas, relationNames []string
	exposedSchemas := map[string]bool{}
	for _, collection := range schemas.Collections {
		relationSchemas = append(relationSchemas, collection.Source.Schema)
		relationNames = append(relationNames, collection.Source.Name)
		exposedSchemas[collection.Source.Schema] = true
	}
	reasons, err := access.WriteCapabilities(ctx, slices.Sorted(maps.Keys(exposedSchemas)), relationSchemas, relationNames)
	if err != nil {
		return err
	}
	if len(reasons) == 0 {
		return nil
	}
	sort.Strings(reasons)
	return fmt.Errorf("native read-only PostgreSQL mount refused: the role is not read-only: %s; connect as a role with only SELECT on the exposed relations (and USAGE on their schemas)", strings.Join(reasons, "; "))
}

var queryPostgresSelectableRelations = func(ctx context.Context, db *sql.DB, schemaName string) (*sql.Rows, error) {
	return db.QueryContext(ctx, `
		SELECT c.relname, has_table_privilege(current_user, c.oid, 'SELECT')
		FROM pg_catalog.pg_class AS c
		JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')`, schemaName)
}

func (v postgresSelectVisibility) SelectableRelations(ctx context.Context, schemaName string) (map[string]bool, error) {
	rows, err := queryPostgresSelectableRelations(ctx, v.db, schemaName)
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
