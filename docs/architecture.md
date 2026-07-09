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
  `dal.StructuredQuery`) pass through via `POST /v1/databases/{db}/dtql` —
  ovdb's job there is only routing/auth, never query evaluation;
- parent-scoped subcollection queries (e.g. Sneat's happenings under a space
  module) travel as a dal-escaped `parent` key path and become
  `dal.NewCollectionRef(name, "", parentKey)`.

Where dalgo drivers had gaps, the gaps were fixed **upstream** rather than
worked around (see "Upstream contributions" below).

## Components

```text
github.com/openvaultdb/openvaultdb-go
  cmd/ovdb          — CLI (`serve`, `init`, `status`, `databases`, `version`)
  pkg/manifest      — database manifest (id, schema mode, engine config, schemas, push policy)
  pkg/schema        — schema modes + record validation
  pkg/inferred      — inferred schema catalogue (observed fields)
  pkg/core          — Database: dal.DB + mode enforcement + batch pre-flight + queries + DTQL
  pkg/mount         — manifest → DALgo driver construction + git-push hooks
  pkg/server        — HTTP API handlers
  conformance/      — official dalgo end2end suite over the full stack

github.com/dal-go/dalgo2openvaultdb   — DALgo driver speaking the HTTP API
```

## Engines (DALgo drivers)

| Engine  | Driver                          | Schema modes (MVP)             |
|---------|--------------------------------|--------------------------------|
| ingitdb | github.com/ingitdb/dalgo2ingitdb | strict, partial, schemaless   |
| sqlite  | github.com/dal-go/dalgo2sqlite   | strict (MVP choice, not a permanent limitation) |

### inGitDB (reference engine)

Works on a plain directory; if the directory is a git work tree, every write
batch commits (one commit per dal transaction). Records are one YAML file per
record — human-browsable, portable, user-owned. Collection `definition.yaml`
files are provisioned through the driver's own `ddl.SchemaModifier`;
schemaless mode auto-creates definitions on first write (fields inferred from
the record, all optional), including nested subcollections via path-form
names (`spaces/ext`).

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
back from SQLite's 0/1 integers on reads.

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
query), `/dtql` (DTQL YAML pass-through), `/inferred-schema`, `/status`,
`/databases`.

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
schema catalogue, DTQL pass-through, dalgo2openvaultdb, Sneat CLI validation.

Out (see docs/roadmap.md): Firestore engine mount, hosted service, billing,
auth, GraphSpec/ModelSpec, GraphQL, admin UI, replication/sync, migrations,
SQLite partial/schemaless, cursors/offset/projections/group-by over the wire,
update preconditions, cross-request transaction isolation.
