# OpenVaultDB MVP Architecture

Status: MVP (2026-07-09). This document describes what is built, not the full future vision.

## What OpenVaultDB MVP is

OpenVaultDB is a **thin server over DALgo drivers** with per-database schema modes.
The MVP exists to prove one path end-to-end:

```text
sneat-cli → Sneat facades → DALgo → dalgo2openvaultdb → OpenVaultDB HTTP API → dalgo2ingitdb | dalgo2sqlite
```

Sneat business logic must not know (or care) which engine stores its data.

## Design principle: dalgo-native

Storage access inside ovdb **is dalgo**. An "engine" is simply a `dal.DB`
driver plus a schema-mode capability declaration; ovdb adds what a server
must add — schema-mode enforcement, collection provisioning, inferred-schema
observation, publication policy, and (future) authentication — and passes
reads, writes, updates and queries through to the driver natively:

- writes: one HTTP batch → one `dal.RunReadwriteTransaction` → ordered
  `tx.Set/Insert/Update/Delete` (for inGitDB: at most one git commit per
  batch, with an auto-generated message when the client sends none);
- updates: wire ops → `[]update.Update` (nested field paths, delete-field,
  increment, server timestamps) executed by the driver;
- queries: wire JSON → `dal.StructuredQuery` builder → the driver's own query
  evaluator; **DTQL** documents (dalgo's native lossless YAML serialization of
  `dal.StructuredQuery`) are read by `POST /v1/databases/{db}/dtql` and
  `POST /v1/dtql`: the server parses, classifies and validates the document,
  runs a single-collection one through the mount, and runs a relational one
  (joins, grouping, aggregates, subqueries, several mounted databases) through
  DALgo, in the database where one database can run it and in memory
  otherwise; see docs/api.md. Structured
  queries run on SQLite, inGitDB and Firestore, and on PostgreSQL while the
  preview switch is on; a MySQL mount, and a PostgreSQL mount without the
  switch, are refused them with `501 query_unsupported` (see "Names and queries
  on the SQL engines" below);
- parent-scoped subcollection queries (e.g. Sneat's happenings under a space
  module) travel as a dal-escaped `parent` key path and become
  `dal.NewCollectionRef(name, "", parentKey)`.

Where dalgo drivers had gaps, the gaps were fixed **upstream** rather than
worked around (see "Upstream contributions" below).

## Components

```text
github.com/openvaultdb/openvaultdb-go
  pkg/manifest      — database manifest (id, schema mode, engine config, schemas, push policy)
  pkg/schema        — schema modes + record validation
  pkg/inferred      — inferred schema catalogue (observed fields)
  pkg/core          — Database: dal.DB + mode enforcement + batch pre-flight + queries + DTQL
  pkg/mount         — manifest → DALgo driver construction + git-push hooks
  pkg/server        — HTTP API handlers
  conformance/      — official dalgo end2end suite over the full stack

github.com/openvaultdb/ovdb           — CLI (`serve`, `init`, `status`, `databases`, `token`, `version`)
github.com/dal-go/dalgo2openvaultdb   — DALgo driver speaking the HTTP API
```

## Engines (DALgo drivers)

| Engine    | Driver                             | Schema modes                  |
|-----------|------------------------------------|-------------------------------|
| ingitdb   | github.com/ingitdb/dalgo2ingitdb   | strict, partial, schemaless   |
| sqlite    | github.com/dal-go/dalgo2sqlite     | strict (MVP choice, not a permanent limitation) |
| firestore | github.com/dal-go/dalgo2firestore  | strict, partial, schemaless   |
| postgres  | github.com/dal-go/dalgo2postgres   | strict (MVP; JSONB doc-column mode → all three, roadmap) |
| mysql     | github.com/dal-go/dalgo2mysql      | strict (MVP; JSON doc-column mode → all three, roadmap) |

Structured queries (`/query`, `/dtql`) are available on `sqlite`, `ingitdb` and
`firestore`, and on `postgres` while the preview switch
(`OVDB_PREVIEW_POSTGRES_QUERIES=1`, read when the mount opens) is on. `mysql`
mounts, and `postgres` mounts without the switch, answer them with `501
query_unsupported`; their key reads and writes work, under the rules in "Names
and queries on the SQL engines".

### inGitDB (reference engine)

Works on a plain directory; if the directory is a git work tree, every write
batch commits (one commit per dal transaction). Records are one YAML file per
record — human-browsable, portable, user-owned. Collection `definition.yaml`
files are provisioned through the driver's own `ddl.SchemaModifier`;
schemaless mode auto-creates definitions on first write (fields inferred from
the record, all optional), including nested subcollections via path-form
names (`spaces/ext`).

**GitHub backend** (`storage.ingitdb.github`): instead of a local working
tree, records are written directly to a GitHub repository over the API — no
local checkout. Each write batch (one dal transaction) is one commit via the
GitHub Git tree API (`dalgo2ingitdb4github`, batching variant); the collection
layout comes from an in-memory Definition built from the manifest's declared
schemas, so it supports strict and partial modes. The token comes from the env
var named by `token_env` (default `OVDB_GITHUB_TOKEN`); manifests never carry
secrets. This is the "your data lives in your GitHub repo" story without ovdb
owning a working tree.

Publication policy (`storage.ingitdb.push`): `none` (default) | `sync`
(push before the write is acknowledged; push failure fails the request but
the local commit stands) | `async` (coalescing single-flight background
pusher; failures logged). The deliberate transaction/save/commit/push model
is an open design topic: see the idea doc in the inGitDB spec repo
(`spec/ideas/transactions-save-commit-push-model.md`).

### SQLite

`dalgo2sqlite` (pure-Go `modernc.org/sqlite`) over `dalgo2sql`: one table per
collection, record key ID in the `id` primary-key column, top-level fields as
columns. Declared schemas come from the manifest and are provisioned as
tables at open. Strict-only in MVP because partial/schemaless need
inferred-schema-driven column evolution (roadmap). The `id` column is treated
as implicitly declared by validation, and declared boolean fields are coerced
back from SQLite's 0/1 integers on reads. The adapter quotes every collection,
field and primary-key name it writes, so a collection named with a space or a
hyphen is an ordinary table (see "Names and queries on the SQL engines").

### Firestore

`dalgo2firestore` over Application Default Credentials (or
`FIRESTORE_EMULATOR_HOST` for local development) — manifests carry only the
project/database ids, never credentials. Firestore collections are implicit
(no DDL to provision); all three schema modes are enforced by ovdb core
above the driver. The inferred catalogue persists next to the manifest.

### PostgreSQL

`dalgo2postgres` (pgx, pure Go) over `dalgo2sql` — the second relational
engine. The DSN (which carries credentials) comes from the environment
variable named by `storage.postgres.dsn_env` (default `OVDB_POSTGRES_DSN`);
manifests never carry secrets. Strict mode only in MVP, like SQLite (records
map to relational columns; a JSONB document-column mode enabling
partial/schemaless is on the roadmap and would make Postgres the first SQL
engine with all three modes). PostgreSQL folds identifiers to lower case, so
collection and field names are stored lower-cased; ovdb restores declared
field-name case on read (see `coerceToSchema`) so strict validation and
clients see faithful names.

### MySQL

`dalgo2mysql` (go-sql-driver, pure Go) over `dalgo2sql` — the third relational
engine, sharing the same base as SQLite and Postgres. The DSN comes from the
environment variable named by `storage.mysql.dsn_env` (default
`OVDB_MYSQL_DSN`); manifests never carry secrets. Strict mode only in MVP.
Unlike Postgres, MySQL preserves identifier case (with the default
`lower_case_table_names=0`) and treats column names case-insensitively, so no
case folding or read-side case restoration is needed. MySQL DDL is
non-transactional (each statement auto-commits), so collection provisioning is
not rolled back on a later error in the same open.

### Names and queries on the SQL engines

`dalgo2sql` binds record values as statement parameters. It writes collection,
field and primary-key names into the statement text of key reads and writes, and
checks every one first: on `sqlite` (the one engine whose dialect has reviewed
quoting) a name is written quoted, so a name with a space, a quote or a hyphen is
an ordinary identifier and a declared `Orders Status` reads its own table, never
`Orders`; on `postgres` and `mysql`, whose key reads and writes write a name unquoted, a
collection, field or primary-key name must be a plain ASCII identifier (ASCII
letters, digits and underscores, not starting with a digit) and any other is
refused by the adapter without a statement being sent (a `500 internal`, or a
`404` for `HEAD`). A `postgres` or `mysql` manifest that declares a collection or
field name outside that rule does not open: provisioning the declared collections
refuses the name, so the mount fails when the database opens. Quoting the
names of key reads and writes on those two engines is left to the task that
gives them a reviewed dialect. The
structured-query path of a MySQL mount has no dialect yet. The PostgreSQL
adapter forces its typed dialect (every value bound, every name quoted), and a
PostgreSQL mount states how it matches names (folded to lower case). ovdb does
not depend on the adapter for either:

- **Queries.** `/query` and `/dtql` are refused on `mysql` with `501
  query_unsupported` before any query reaches the driver, and on `postgres`
  unless the preview switch was on when the mount opened (one function of
  `pkg/core` decides, and the guard, the discovery document and the engines
  that run a whole document in the database all ask it). The errors the adapter
  of a database server returns on this path are built in `pkg/core` and repeat
  none of its text: `dal.ErrNotSupported` and an unknown dialect are `422
  query_unsupported` with a fixed message; a value or a name the server refuses
  by a SQLSTATE read by type, a field or a qualifier the tables do not hold and
  an over-long source alias are `400 invalid_dtql`; what DALgo refuses above the
  adapter keeps its type (a bound of its own join is `422
  query_budget_exceeded`, a document it cannot run `400 invalid_dtql`); any
  other failure is a `500` whose log line says which step failed. A join of the
  mount's own database is handed to the adapter without the database its sources
  name, so that the adapter can run it as one statement.
- **Collections.** On `sqlite`, `postgres` and `mysql` (and any engine ovdb
  does not recognise) a key read or write whose collection is not one the
  mount declared in the manifest's `schemas` when it opened is `404 not_found`
  before the adapter is called; so is a key with a parent, because a SQL mount
  has no subcollections (the adapter would address another table than the root
  collection the capability was checked on). The allow-list is fixed when the
  database opens, so an embedder that renames the live manifest's keys
  afterwards does not change it. Names match exactly, including case. The
  key-segment rule (`core.ValidateSegment`) is a path-safety check only and
  accepts quotes, spaces and semicolons, so the declaration is the allow-list.
  `inGitDB` and Firestore keep the key-segment rule alone: their adapters
  address a nested key as a subcollection of the parent record, so the
  capability is checked on the root collection.
- **One name per collection.** A SQLite manifest may key a collection by its
  SQL-quoted identifier (`"Order Details"` with the quotes). The collection is
  then declared under two spellings, the quoted key and the public name, and the
  public name is its canonical name (`core.Database.CanonicalCollection`): the
  only form the adapter is given, since the adapter quotes a name itself. `Get`,
  `Exists` and `Apply` rename the key before the adapter call, so the two
  spellings of one row are one record in a batch, and the mount provisions the
  table under the canonical name. On the key routes a capability scoped to either
  spelling covers a key written with either (`core.Database.CollectionSpellings`,
  used by the server's key-route check). The routes that give the adapter, or the
  coordinator that reads it by key, the collection as written (`/query`, `/dtql`,
  the protected `PATCH` and the authorization endpoints) take the canonical name
  only (`core.Database.GuardCanonicalCollection`): the quoted spelling names the
  table whose name carries the quote characters, so on those routes it is a
  collection the database does not declare, and a capability is matched against
  the name sent. On a mount with access policies that answer is the one for a
  declared collection the policy hides. A
  manifest in which one name would be a spelling of two collections, or two keys
  that are one table declare different fields, is refused when the database opens
  (`core.ErrCollectionNamesConflict`). The access-policy layer sees the canonical
  name on a key read or write. The class of the engine (a document engine or not)
  is recorded when the database opens, beside the declared set, and is not read
  from the live manifest afterwards.
- **Field names**, on every engine. The top-level keys of a write body, an
  update's `fieldName` and the first segment of its `fieldPath` pass
  `core.ValidateFieldName` (the rule `/query` and `/dtql` use) or the request
  is `400 bad_request` before the adapter is called. The later segments of a
  `fieldPath` are map keys: the same rule on the SQL engines, and on `inGitDB`
  and Firestore only a non-blank segment without control characters
  (Sneat's linkage writes `id@spaceID` keys there). On the SQL engines an update
  that names the record's key column (`id`, in any spelling of its case) is
  `400 bad_request` too (`core.ErrKeyUpdate`), and so is a write body whose keys
  name it in a case other than `id` (`core.ErrKeyColumnCase`); a manifest that
  declares a field so named does not open (`core.ErrFieldNamesConflict`).
- **Empty writes**, on the SQL engines. An `update` with no operation, and a
  `set` that names no field but `id` for a record that exists, leave the adapter
  nothing to put in a statement and are `400 bad_request` before the write.

The guard sits in `core.Database` (`Get`, `Exists`, `Apply`), so every caller
is covered; the protected `PATCH` and the authorization endpoints that read by
key apply the same checks before their coordinator runs. See `docs/api.md`
("Names the server accepts") and `docs/threat-model.md`.

### Cloud-managed relational (no new engine)

Amazon RDS & Aurora, Azure Database for PostgreSQL/MySQL, and Google Cloud SQL
are the same wire protocol as self-hosted PostgreSQL/MySQL. The existing
`postgres`/`mysql` engines reach them by pointing the DSN (in the env var) at
the cloud endpoint with TLS — e.g.
`postgres://user:pass@mydb.abc123.us-east-1.rds.amazonaws.com:5432/app?sslmode=require`
or `user:pass@tcp(mydb.mysql.database.azure.com:3306)/app?tls=true`. No new
engine is needed; only the DSN changes.

## Schema modes

- **strict** — schemas required before writes; declared fields validated
  (types, required); unknown fields rejected.
- **partial** — declared subset validated; undeclared fields allowed and
  observed into the inferred catalogue.
- **schemaless** — no declared schema required. *Schemaless means no required
  pre-declared schema; it does not mean no schema information*: every write
  is observed into the inferred schema catalogue.

Mode/engine compatibility is validated when a database is mounted; an
unsupported mode fails loudly, e.g.:

```text
requested schema mode "schemaless" is not supported by sqlite engine in MVP; supported schema modes for sqlite: strict
```

Enforcement is a **pre-flight simulation** in `pkg/core`: the whole batch is
staged against current store state (insert conflicts, updates of missing
records, final-state schema validation) *before* any file/row is written —
necessary because inGitDB cannot roll back files already written. After
pre-flight, ops pass through to the driver untouched.

## Inferred schema catalogue

Per collection and field path (dotted for nesting): observed types with
counts, array element types, null/missing counts, first/last seen, type
conflicts. Persisted as JSON next to the data
(`<dir>/.ovdb/inferred-schema.json` for inGitDB), served read-only at
`GET /v1/databases/{db}/inferred-schema`, and used to derive auto-created
collection definitions in schemaless mode. Deliberately minimal — enough to
prove the concept for DataTug/DTQL/GraphQL/admin-UI/AI-agent futures without
building GraphSpec/ModelSpec now.

## Database manifest

```yaml
database:
  id: sneat-dev
  schema_mode: schemaless   # strict | partial | schemaless

storage:
  engine: ingitdb           # sqlite | ingitdb
  path: ./data/sneat-dev
  ingitdb:                  # optional, ingitdb only
    push: async             # none | sync | async
    remote: origin
    branch: ""              # default: HEAD

schemas:                    # required for strict; optional for partial
  collections:
    contacts:
      fields:
        title: {type: string, required: true}
```

## HTTP API

See docs/api.md. Summary: records CRUD at
`/v1/databases/{db}/records/{key...}` (key = `dal.Key.String()` path),
`/batch` (ordered ops, one dal transaction), `/query` (JSON structured
query), `/dtql` (DTQL YAML documents), `/inferred-schema`, `/status`,
`/databases`.

## Runtime database provisioning (--data-dir)

Manifests are normally operator-authored files, but `ovdb serve --data-dir
<dir>` adds a second, server-managed source of databases: `POST
/v1/databases` provisions an inGitDB schemaless database at runtime —
`<data-dir>/<id>/` (git-initialised data directory) plus `<data-dir>/<id>.yaml`
(a generated manifest in the exact same format as hand-written ones). The
new database is mounted live into the running server (the mounted-databases
map is mutex-guarded for this), and on restart the data-dir is rescanned with
the ordinary manifest-directory loader, so created databases persist without
any extra registry. Creation is allowed for the owner or any token carrying
the server-level `databases:create` capability; a non-owner creator receives
a freshly minted token scoped to the new database (per-app isolation — see
docs/api.md).

## Conformance & validation

- `conformance/` runs the official `dalgo/end2end.TestDalgoDB` suite through
  driver → HTTP → core → engine for SQLite strict, inGitDB schemaless and
  inGitDB partial — all green.
- Sneat CLI end-to-end: `SNEAT_STORAGE=openvaultdb` routes the convo
  sandbox's `facade.GetSneatDB` through dalgo2openvaultdb; real
  Contactus/Listus/Calendarius facade operations ran unchanged, with records
  persisted across CLI processes and one git commit per facade transaction.

## Upstream contributions made for this MVP

- **dalgo2ingitdb**: subcollection DDL via path-form `CreateCollection`
  names; exported `ErrRecordAlreadyExists`; idempotent `Delete` and
  empty-result queries for unknown collections; nested field-path updates
  (delete-field, increment, server-timestamp); git staging fixed to absolute
  pathspecs so commits work when the server runs outside the repo dir.
- **dalgo2sqlite**: migrated from cgo `mattn/go-sqlite3` to pure-Go
  `modernc.org/sqlite` (its own README's open question).
- **dalgo2sql**: `Get`/`GetMulti`/query support for `map[string]any` record
  data; not-found errors wrap the key; `Set` rows-leak deadlock fix; readers
  return `dal.ErrNoMoreRecords`.

## MVP boundaries

In: `ovdb` CLI + `serve`, minimal HTTP API, SQLite strict engine, inGitDB
strict/partial/schemaless engine, manifest (incl. push policy), inferred
schema catalogue, single-collection DTQL (the relational profile came later; see docs/api.md), dalgo2openvaultdb, Sneat CLI validation.

Out (see docs/roadmap.md): hosted service, billing,
auth, GraphSpec/ModelSpec, GraphQL, admin UI, replication/sync, migrations,
SQLite partial/schemaless, cursors/offset/projections/group-by on `/query`,
update preconditions, cross-request transaction isolation.
