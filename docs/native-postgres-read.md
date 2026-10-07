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
`SELECT`. Keyed record reads and every write API are refused, and opening the
mount does not create or alter tables. The session setting is defense in depth;
least-privilege database grants remain necessary, especially for views or
functions owned by another role.

Database metadata advertises `capabilities.dtql: true` and
`capabilities.query: false`. Native mounts support the catalog-backed DTQL
read surface; the legacy `/query` endpoint requires OVDB record keys and is
answered with `501 query_unsupported`.

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

Queries use the server's configured relational row, byte, and execution bounds.
Values are parameterized by the driver. Exact PostgreSQL `NUMERIC` values are
returned as decimal strings; `NUMERIC` without a declared precision advertises
unbounded decimal metadata rather than an invented precision. PostgreSQL
`int64`/`uint64` values outside JavaScript's safe-integer range are returned as
decimal strings; smaller integers keep their JSON number representation.
JSON/JSONB columns are emitted as JSON values with their numeric lexemes
preserved. A browser's ordinary `JSON.parse` may still round a nested number
above 2^53−1; clients that need exact nested JSON numbers must use a lossless
JSON parser. Binary values are returned as base64 JSON strings. DATE is encoded
as `YYYY-MM-DD`, timestamp without time zone as a local timestamp without an
offset, and timestamp with time zone as a UTC RFC3339 value. Filters,
projections, ordering, and other DTQL operations
are limited to the structured forms supported by the installed DALgo PostgreSQL
adapter; this mount does not expose arbitrary SQL.

The current source contract is for bounded relational reads. Keyed record
endpoints and mutation APIs are unsupported. A collection with no primary key
can still be queried as rows. Composite and keyless tables do not receive a
synthetic record ID, and this profile does not provide point reads. Names that
are safely representable as quoted PostgreSQL field names, including spaces,
can be used in relational DTQL; the exact discovered field catalog is still
checked before the query reaches the driver. Names that violate that quoted-name
rule remain visible as metadata but cannot be used as query fields.
