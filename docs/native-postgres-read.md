# Native PostgreSQL read-only mounts

An operator can expose an existing PostgreSQL database as a native, read-only
OVDB source. This is additive: existing PostgreSQL mounts keep their declared
collections and write behavior.

```yaml
database:
  id: samples
  schema_mode: strict
storage:
  engine: postgres
  postgres:
    dsn_env: OVDB_SAMPLES_DSN
    read_only: true
```

The environment variable contains the PostgreSQL connection string. It is not
part of the manifest or API. Use a database role that has only the catalog and
`SELECT` privileges the mount needs. OVDB starts native read-only sessions with
`default_transaction_read_only=on`, refuses the `options` connection parameter
that could override it, and only advertises relations for which the role has
`SELECT`. Every read runs in an explicit `BEGIN READ ONLY` transaction of its
own, so a view whose function changes the session default cannot make a later
read writable. Keyed record reads and every write API are refused, and opening
the mount does not create or alter tables. The session settings are defense in
depth; least-privilege database grants remain necessary, especially for views
or functions owned by another role.

## Connection safety

A native read-only mount is opened with `mount.FileWithOptions`; these
`mount.Options` fields apply to it:

| Option | Default | Effect |
|---|---|---|
| `AllowInsecureNativePostgresTransport` | `false` | A connection to a remote host must be TLS with a verified server certificate: `sslmode=verify-full` (recommended; Neon and other managed services work with it and the system roots) or `sslmode=verify-ca`, or `sslmode=require` with an `sslrootcert`. `disable`, `allow`, `prefer` (pgx's default when no `sslmode` is given) and plain `require` are refused when the mount opens, before any connection is made. A host on this machine needs none of this: a Unix-socket directory, a loopback IP address (`127.0.0.0/8`, `::1`) or the name `localhost` never crosses a network. Every host of a multi-host connection string must be local for the exemption; other names are not resolved. This option, the manifest field `storage.postgres.insecure_transport: true`, or the environment variable `OVDB_NATIVE_POSTGRES_INSECURE_TRANSPORT=true` (which reaches tools such as the `ovdb` CLI that mount manifests as they are) allows a remote host without a verified certificate; the mount logs a warning whenever one of them is set. |
| `NativePostgresStatementTimeout` | 15s | Sent as the `statement_timeout` startup parameter. It overrides a `statement_timeout` in the connection string, so the server ends a statement that runs too long even if the client has gone. |
| `NativePostgresDiscoveryTimeout` | 30s | Bounds the catalogue read when the mount opens. |
| `RequireReadOnlyNativePostgresRole` | `false` | Refuses to mount a role that could change data or the schema, listing each reason. Every role the connecting role is a **member** of counts, `NOINHERIT` memberships included (the role can `SET ROLE` to them), and so do grants to `PUBLIC`. Refused: the `SUPERUSER`, `CREATEROLE`, `CREATEDB`, `REPLICATION` or `BYPASSRLS` attribute; membership in `pg_write_all_data`, `pg_execute_server_program`, `pg_read_server_files` or `pg_write_server_files`; owning any schema, relation or function outside the system schemas; `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `TRIGGER` or `REFERENCES` on an exposed relation, or `INSERT`/`UPDATE`/`REFERENCES` on any of its columns; `CREATE` on an exposed schema or on `public`; `USAGE` or `UPDATE` on a sequence of those schemas; and `EXECUTE` on a `SECURITY DEFINER` function in them. A service that exposes a database publicly should turn it on. |

Each read runs in its own `BEGIN READ ONLY` … `COMMIT`, two round trips more
than a bare statement. `default_transaction_read_only` and `statement_timeout`
are sent as startup parameters, which PgBouncer refuses by default
(`ignore_startup_parameters`); connect a native mount to the server's direct
endpoint (on Neon, the host without `-pooler`) rather than through a pooler.

`mount.All` mounts several manifests concurrently (four at a time;
`mount.AllWithOptions` sets the number) and returns a `mount.Health` for each
(mounted or not, the error, the time it took). A `mount.Spec` marked `Optional`
that fails, for example a PostgreSQL database whose catalogue read fails or
times out, is reported in its health and does not stop the others. A mount that
panics is reported as failed. A required spec that fails fails the call: the
specs not yet started are skipped (`mount.ErrMountSkipped`) and what was mounted
is closed.

Database metadata advertises `capabilities.dtql: true` and
`capabilities.query: false`. Native mounts support the catalog-backed DTQL
read surface; the legacy `/query` endpoint requires OVDB record keys and is
answered with `501 query_unsupported`.

## Upgrading

- **BREAKING:** a native read-only mount to a remote host now requires a
  verified TLS connection (`sslmode=verify-full` or `verify-ca`). Loopback and
  Unix-socket hosts are exempt. Change the connection string, or opt out for
  local development as described under Connection safety.
- Every native read runs in an explicit read-only transaction, and sessions
  start with `statement_timeout` (15s by default).
- On integer columns `==`, `In` and `NotIn` refuse values the column cannot
  hold, and filters on `bytea` columns are refused (see Filter values).

## Discovery and collection IDs

OVDB discovers supported schemas, tables/views, and fields from PostgreSQL's
catalog. Database metadata lists logical collection IDs and a `schemas` map.
Each discovered collection has a `source` object containing its original
`schema` and relation `name`; fields expose their original names, portable type,
`nativeType`, nullability, and primary-key metadata. PostgreSQL primary keys are
descriptive metadata only: they do not become OVDB record IDs.

Native collection IDs use the `pg1_` prefix followed by unpadded URL-safe
base64 of a tuple made from two big-endian 32-bit byte lengths and the exact
UTF-8 bytes of the schema and relation names. Length-prefixing makes the tuple
unambiguous even when either name contains punctuation. Clients should treat
this as an opaque route ID and use `source.schema` and `source.name` for
display. The encoding is versioned so its representation can evolve without
relabeling existing PostgreSQL mounts.

## Consumer-scoped relation exclusions

A mounting consumer can omit exact schema/relation pairs without changing the
manifest or the default discovery behavior:

```go
db, err := mount.FileWithOptions(manifestPath, mount.Options{
	ExcludedNativePostgresRelations: []schema.NativeCollectionSource{
		{Schema: "demodb", Name: "_import_manifest"},
	},
})
```

The filter applies only to native read-only PostgreSQL mounts. The omitted
relation is not described or included in database metadata, and its encoded
collection ID and schema-qualified DTQL source are both rejected before the
provider query. Matching is exact and schema-qualified; a same-named relation
in another schema remains available. An empty exclusion list preserves the
existing expose-all behavior. Consumers should keep their exclusions narrowly
scoped to their own internal or provenance relations.

## Structured reads

Use the original physical names in schema-qualified DTQL. OVDB first resolves
the exact `(schema, name)` pair against the discovered catalog, then passes the
resolved relation to the PostgreSQL driver. Names are separately quoted by the
driver; request text is never concatenated into SQL.

```yaml
from:
  schema: chinook
  name: Track
where:
  op: '=='
  left: {field: Composer}
  right: {value: 'Miles Davis'}
columns:
  - {field: TrackId}
  - {field: Name}
  - {field: Composer}
orderBy:
  - {field: Name}
limit: 50
```

Native collection IDs are opaque route IDs and are not held to PostgreSQL's
63-byte identifier limit: a schema and relation of up to 63 bytes each give an
ID well over 63 bytes, and every route accepts it. The 63-byte limit applies to
the schema, relation and column names the ID encodes, which discovery checks,
and still applies to the collection names of writable PostgreSQL mounts.

Queries use the server's configured relational row, byte, and execution bounds.
Values are parameterized by the driver. Exact PostgreSQL `NUMERIC` values are
returned as decimal strings; `NUMERIC` without a declared precision advertises
unbounded decimal metadata rather than an invented precision. PostgreSQL
`int64`/`uint64` values outside JavaScript's safe-integer range are returned as
decimal strings; smaller integers keep their JSON number representation.
`real` and `double precision` values that JSON has no number for are returned
as the strings PostgreSQL spells them with: `"NaN"`, `"Infinity"` and
`"-Infinity"`; a filter with the same string finds them.
JSON/JSONB columns are emitted as JSON values with their numeric lexemes
preserved. A browser's ordinary `JSON.parse` may still round a nested number
above 2^53−1; clients that need exact nested JSON numbers must use a lossless
JSON parser. Binary values are returned as base64 JSON strings. DATE is encoded
as `YYYY-MM-DD`, timestamp without time zone as a local timestamp without an
offset, and timestamp with time zone as a UTC RFC3339 value. Filters,
projections, ordering, and other DTQL operations
are limited to the structured forms supported by the installed DALgo PostgreSQL
adapter; this mount does not expose arbitrary SQL.

### Filter values

- An integer literal outside JavaScript's safe range (above 2^53−1 or below
  −(2^53−1)), inline as `value:`/`values:` or bound as a JSON parameter, is
  compared exactly: OVDB passes its digits to PostgreSQL as text, which parses
  them as the column's type. A large ID can therefore be sent as a JSON number or
  as a string, with the same result.
- On a `smallint`, `integer` or `bigint` column compared with `==`, `In` or
  `NotIn`, a value must be a whole number the column can hold. A fraction
  (`1.5`), a number outside the column's range (`2^70`, or `40000` on a
  smallint), and a boolean are refused with `400 invalid_dtql`, as text that is
  not an integer (`'abc'`, `'1e3'`) already is by PostgreSQL. PostgreSQL would
  otherwise compare such a value as a numeric and silently match no row (or, for
  `NotIn`, every row).
- An order comparison (`<`, `<=`, `>`, `>=`) on an integer column accepts a
  fraction or a number beyond the column's range and answers with the rows
  PostgreSQL's numeric comparison selects (`small < 40000` is every row,
  `id > 1.5` starts at 2). A boolean is refused. An integer above 2^53 is sent as
  text (above), so compared with a `smallint` or `integer` column, which cannot
  hold it, it is a 400.
- A `bytea` column cannot be compared with a value: the column is returned as
  base64 text, and comparing it with that text would compare the bytes with the
  bytes of the text. Such a filter is refused with `400 invalid_dtql`. A bytea
  column can still be projected and ordered by.

### Paging

PostgreSQL returns rows in no defined order unless the query has an `ORDER BY`.
An `offset` without an `orderBy` is still answered, but pages of a view or of a
table without a key can then repeat or skip rows, so the footer of such an
answer carries `"warnings": ["offset without orderBy: ..."]`. Page with an
`orderBy` over columns that identify a row, the primary key where there is one.
OVDB does not add an order of its own: ordering by every column is not possible
for every type, and `ctid` does not exist on views.

### Proving every collection answers

`pkg/server/servertest` drives a running server through the HTTP API.
`servertest.EveryCollection` (or `RequireEveryCollection` in a test) reads a
database's metadata and runs, on every collection it lists, a bounded read, an
unbounded streamed read, an order by the primary key (else every field), an
order by each field, an equality filter on each scalar field of a sampled row,
an offset page, the refusal of values an integer column cannot hold, and the
exact match of an integer above 2^53. It returns a report with a matrix of
passes and failures. The repository runs it against the demo datasets when
`OVDB_TEST_DEMODB_POSTGRES_DSN` points at a server that holds them
(`TestNativePostgresDemoDatasets_EveryCollectionAnswers`).

The current source contract is for bounded relational reads. Keyed record
endpoints and mutation APIs are unsupported. A collection with no primary key
can still be queried as rows. Composite and keyless tables do not receive a
synthetic record ID, and this profile does not provide point reads. Names that
are safely representable as quoted PostgreSQL field names, including spaces,
can be used in relational DTQL; the exact discovered field catalog is still
checked before the query reaches the driver. Names that violate that quoted-name
rule remain visible as metadata but cannot be used as query fields.
