# OpenVaultDB Roadmap

Status: post-MVP planning (2026-07-09). Items below are not committed; they reflect known
follow-ups from the MVP implementation. Items labelled *MVP choice* are limitations we
intentionally deferred; items labelled *design constraint* require more significant rework.

---

## Near term

### Publish the openvaultdb module

Publish `github.com/openvaultdb/openvaultdb-go` and `github.com/dal-go/dalgo2openvaultdb` to
GitHub so consumers no longer need local `replace` directives. Tag `v0.1.0`.

### SQLite partial and schemaless modes

SQLite strict-only is an MVP choice. Adding partial/schemaless requires:

- **Inferred-schema-driven column evolution**: when a new field is first observed in a
  partial/schemaless database, `ALTER TABLE ... ADD COLUMN` with a nullable column. Track
  observed field names per collection in the inferred catalogue and reconcile on open.
- Column names must be sanitised before use in DDL (SQL injection prevention).
- Type conflicts (field seen as both string and integer) need a policy (widen to TEXT or reject).

### dalgo2sql id-column shim removal

OpenVaultDB currently adds a synthetic `"id"` column to every collection definition passed to
`ddl.SchemaModifier.CreateCollection`. This is a shim to satisfy `dalgo2sqlite`, which needs a
primary-key column named `"id"` to write the record key. Once `dalgo2sql` is updated to set
record keys from PK metadata in the `dbschema.CollectionDef.PrimaryKey` field, the shim can be
removed and OpenVaultDB can stop injecting the column.

### Observe post-update states into the inferred catalogue

Currently `Apply` only observes `set`/`insert` ops into the inferred catalogue because `update`
ops are applied inside the driver and the final record state is not returned. Fix: after the
transaction commits, read back updated records and observe them. This ensures the inferred
catalogue stays accurate for partial/schemaless databases that use field-level updates heavily.

### Filter pushdown notes

The MVP evaluates all query filters in OpenVaultDB core over a full collection scan (engine
provides `Scan(collection)` only). This is fine at small scale but does not benefit from engine
indexes. Filter pushdown per engine:

- **SQLite**: translate `Where` filters to SQL `WHERE` clauses; use column indexes for equality
  and range predicates on declared fields.
- **inGitDB**: the driver does not have a query surface; pushdown is not applicable. Core scan
  stays. Consider pagination via cursor tokens if collection sizes grow.

### ovdb CLI growth

Planned subcommands:

| Command                      | Purpose                                                                 |
|------------------------------|-------------------------------------------------------------------------|
| `ovdb doctor`                | Validate manifests, check engine health, report schema/catalogue state  |
| `ovdb collections <db>`      | List collections in a mounted database                                  |
| `ovdb schemas <db>`          | Show declared and inferred schemas for a database                       |
| `ovdb query <db> <col>`      | Interactive/piped query from the CLI                                    |
| `ovdb import <db> <file>`    | Bulk import from JSON/NDJSON                                            |
| `ovdb export <db> <col>`     | Export a collection to NDJSON                                           |

### Auth MVP — done (2026-07-09)

Shipped as `ovdb serve --auth`: owner bearer token (env/flag/generated) plus an
OAuth-style connect flow (consent page → one-time code → scoped app token).
Capability grants follow the spec taxonomy (`records:read` etc.) with optional
collection scoping and persist with SHA-256 token hashes. Follow-ups: OIDC and
passkeys per the auth spec, refresh/rotation, a revocation API, CSRF protection
on the consent form.

See [docs/threat-model.md](threat-model.md) for the current posture.

---

## Medium term

### DTQL as primary query surface

DTQL (DALgo Typed Query Language) YAML passes through `dalgo2openvaultdb` to the ovdb server
today as an opaque blob; the server does not parse or validate it. Proper DTQL support:

- Parse DTQL YAML on the server and translate to the `core.Query` structure.
- Return typed, potentially transformed record sets.
- Evaluate whether DTQL should cover the write path (batch mutations described in DTQL).

### GraphSpec / ModelSpec integration

Allow manifests to reference a GraphSpec or ModelSpec definition so OpenVaultDB can enforce
richer structural and relational constraints (cross-collection references, cardinality rules)
beyond what the current field-type schema supports.

### Hosted api.openvaultdb.com

A managed multi-tenant OpenVaultDB service. Requires:

- Auth (bearer tokens, per-database ACLs)
- TLS termination
- Multi-tenant namespace isolation (database IDs scoped to tenant)
- Billing and quota enforcement
- Operational tooling (metrics, alerting, backups)

This is a separate product track, not a change to the open-source server.

### Multi-tenancy in the open-source server

Support multiple users sharing one `ovdb serve` instance with isolated databases:

- Tenant-scoped database ID namespacing in the URL (`/v1/tenants/{tenant}/databases/{db}/...`).
- Per-tenant auth tokens.
- Filesystem isolation: each tenant's databases under a separate directory subtree.

### Firestore engine via dalgo2firestore — done (2026-07-09)

Shipped exactly as predicted by the design: a ~35-line mount over
`dalgo2firestore` v0.9.0 (which gained bare-map record support upstream),
manifest `storage.firestore: {project, database}` with credentials from ADC /
`FIRESTORE_EMULATOR_HOST`, all three schema modes enforced by core, DDL
provisioning skipped (collections are implicit). Conformance runs against the
emulator when `FIRESTORE_EMULATOR_HOST` is set.

### More engines (assessed 2026-07-09)

- **PostgreSQL** — done (2026-07-09): `dalgo2postgres` v0.1.1 (pgx, Postgres
  DDL dialect, information_schema introspection) mounted in ovdb; strict mode,
  DSN from env. Full dalgo conformance green against PostgreSQL 17. Two dalgo
  upstream additions made it work: `dalgo2sql` v0.9.0 `PlaceholderDialect`
  (Postgres `$N` markers) and bracket-ident stripping. The SQL engines stay
  strict-only: the document/partial/schemaless story is inGitDB's job (it is
  the JSON store), so a JSONB/JSON document-column mode is deliberately NOT
  pursued. Remaining follow-up: case-preserving identifiers (needs dalgo core
  to quote column identifiers in structured-query rendering). MySQL is done
  (2026-07-09): dalgo2mysql over the same dalgo2sql base, needing no dalgo2sql
  change (MySQL uses `?` placeholders and preserves identifier case).
- **Cloud databases** — managed PostgreSQL/MySQL (Amazon RDS & Aurora, Azure
  Database for PostgreSQL/MySQL, Google Cloud SQL) already work through the
  existing `dalgo2postgres`/`dalgo2mysql` engines: point the DSN at the cloud
  endpoint with TLS. Genuinely new engines to build: Azure SQL / SQL Server
  (relational — `dalgo2mssql` over dalgo2sql; notably dalgo's native query
  String() already emits T-SQL `[brackets]` + `SELECT TOP`), and cloud NoSQL
  (Amazon DynamoDB, Azure Cosmos DB — full `dal.DB` drivers like
  dalgo2firestore, testable via DynamoDB Local / the Cosmos emulator).
- **Embedded KV (Badger/BuntDB)** — dalgo drivers exist but target dalgo
  v0.24; modernize upstream first. Value: embedded schemaless without git.
- **Redis** — not planned as a storage engine (ephemeral-by-default fits
  poorly with user-owned/portable/versioned data); relevant later as a
  cache/materialized-read tier or change-notification pub/sub in front of
  engines.

---

## Longer term

### Replication and sync

Replicate a database across multiple nodes or devices:

- For inGitDB: git push/pull is the sync mechanism; branch-per-writer and merge strategies
  need design.
- For SQLite: SQLite replication (Litestream, MVSR) or a custom WAL-based approach.

### Admin UI

A local web UI for browsing databases, collections, records, and the inferred schema catalogue.
Possibly a companion to DataTug.

### DataTug / GraphQL adapters

Expose OpenVaultDB databases through DataTug's data exploration UI and a GraphQL endpoint
generated from the inferred or declared schema.

### inGitDB transactions: save-commit-push model

The inGitDB design doc at
`ingitdb/ingitdb/spec/ideas/transactions-save-commit-push-model.md` describes a three-phase
write model: save files to disk, then commit, then push — each phase independently reliable and
retryable. Adopting this model in `dalgo2ingitdb` would make the non-atomicity window described
in the threat model disappear: a partial save would be explicitly uncommitted and recoverable by
re-running the commit phase.

### Branch-per-writer multi-writer story

inGitDB's natural concurrency model is one writer per branch. A multi-writer design would assign
each client a short-lived branch, then merge (fast-forward if no conflicts, or auto-merge for
CRDT-friendly record types). This requires conflict resolution semantics and is a significant
design undertaking.

---

## Unsupported DALgo capabilities over the HTTP API

The following DALgo capabilities are supported by the inGitDB and SQLite drivers directly but
are not exposed by the `ovdb` HTTP wire protocol in MVP. The `dalgo2openvaultdb` driver returns
`dal.ErrNotSupported` for these.

| DALgo capability                  | Notes                                                                   |
|-----------------------------------|-------------------------------------------------------------------------|
| Update preconditions              | Conditional writes (check-and-set) not in batch wire format             |
| `ExecuteQueryToRecordsetReader`   | Returns `ErrNotSupported`; same as `dalgo2firestore`                    |
| Query cursors / `StartFrom`       | Pagination via cursor token not in MVP                                  |
| Query `Offset`                    | Skip-N not in query wire format                                         |
| Query column projections          | `Columns` field in `dal.Query` not translated                           |
| Query `GroupBy` / `Having`        | Aggregation not in MVP                                                  |
| Collection-group queries          | Cross-collection queries not supported                                  |
| Cross-transaction isolation       | No optimistic concurrency; no MVCC                                      |

These are wire-protocol gaps, not engine limitations. Adding any of them requires extending the
`/v1/databases/{db}/query` and `/v1/databases/{db}/batch` request schemas and implementing the
corresponding server-side translation.
