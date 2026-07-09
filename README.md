# OpenVaultDB

OpenVaultDB is a small local database server with pluggable storage engines and per-database
schema modes. It stores data in backends you own — a SQLite file or an inGitDB directory tree
— and exposes a uniform JSON HTTP API. The primary goal of the current MVP is to prove that
Sneat's business logic can switch storage backends without modification:

```
sneat-cli → Sneat facades → DALgo → dalgo2openvaultdb → OpenVaultDB HTTP → SQLite | inGitDB
```

**Tagline:** user-owned, portable, versioned data.

- Records stored as human-readable YAML files (inGitDB) or SQLite rows you can inspect directly.
- Each write batch = one git commit (inGitDB), giving you a full write history for free.
- Schema modes let you start schemaless and tighten to strict later without changing client code.
- The DALgo driver (`dalgo2openvaultdb`) means any existing DALgo application can use OpenVaultDB
  with no business-logic changes.

---

## Quickstart

### 1. Install

```sh
go install github.com/openvaultdb/openvaultdb-go/cmd/ovdb@latest
```

Or build from source:

```sh
git clone https://github.com/openvaultdb/openvaultdb-go
cd openvaultdb
go build ./cmd/ovdb
```

### 2. Create a manifest

```sh
# inGitDB schemaless (recommended for exploration)
ovdb init --id mydb --engine ingitdb --schema-mode schemaless
# → writes mydb.yaml

# SQLite strict (requires declaring schemas upfront)
ovdb init --id mydb-sqlite --engine sqlite --schema-mode strict
# → writes mydb-sqlite.yaml (edit to add schemas.collections)
```

### 3. Start the server

```sh
ovdb serve --manifest mydb.yaml
# or point at a directory of *.yaml manifests:
ovdb serve --dir ./manifests
```

The server binds to `127.0.0.1:6832` by default. Override with `--addr`.

Authentication is off by default (local dev). Enable it with `ovdb serve --auth`:
the owner token is taken from `--owner-token` / `$OVDB_OWNER_TOKEN` (or generated
and printed), and apps obtain scoped tokens through the consent flow at
`/authorize` + `/token` — see the Authentication section in [docs/api.md](docs/api.md).

### 4. Try the API

```sh
BASE="http://127.0.0.1:6832/v1"

# Server status
curl -s "$BASE/status" | jq .

# Put a record
curl -s -X PUT "$BASE/databases/mydb/records/contacts/c1" \
  -H 'Content-Type: application/json' \
  -d '{"data":{"name":"Alice","email":"alice@example.com"}}' 

# Get the record
curl -s "$BASE/databases/mydb/records/contacts/c1" | jq .

# Query
curl -s -X POST "$BASE/databases/mydb/query" \
  -H 'Content-Type: application/json' \
  -d '{"collection":"contacts","where":[{"field":"name","op":"==","value":"Alice"}],"limit":10}' | jq .

# Batch (atomic multi-op, single git commit on inGitDB)
curl -s -X POST "$BASE/databases/mydb/batch" \
  -H 'Content-Type: application/json' \
  -d '{
    "message": "add contacts",
    "ops": [
      {"op":"set","key":"contacts/c2","data":{"name":"Bob"}},
      {"op":"delete","key":"contacts/old"}
    ]
  }' | jq .
```

---

## Manifest examples

### inGitDB — schemaless (recommended default)

```yaml
database:
  id: mydb
  schema_mode: schemaless   # no schema declaration required

storage:
  engine: ingitdb
  path: ./data/mydb          # directory; init'd automatically

  ingitdb:
    push: none               # none (default) | sync | async
    # remote: origin         # git remote to push to (default "origin")
    # branch: HEAD           # branch to push (default: current HEAD)
```

`push: sync` waits for the git push to complete before acknowledging the write.
`push: async` triggers a coalesced background push; failures are logged but do not fail the
write. `push: none` (default) commits locally and never pushes — suitable for local dev and
offline use.

### inGitDB — strict

```yaml
database:
  id: contacts-db
  schema_mode: strict

storage:
  engine: ingitdb
  path: ./data/contacts-db

  ingitdb:
    push: async

schemas:
  collections:
    contacts:
      fields:
        name:  {type: string, required: true}
        email: {type: string}
        tags:  {type: array}
```

### SQLite — strict (MVP: strict only)

```yaml
database:
  id: contacts-sqlite
  schema_mode: strict       # only mode supported by SQLite in MVP

storage:
  engine: sqlite
  path: ./data/contacts.sqlite

schemas:
  collections:
    contacts:
      fields:
        name:  {type: string, required: true}
        email: {type: string}
```

---

## Schema modes

| Mode         | SQLite (MVP) | inGitDB | Behaviour                                                                                          |
|--------------|:---:|:---:|----------------------------------------------------------------------------------------------------|
| `strict`     | yes | yes | Schema required before writes. Declared fields validated (type, required). Unknown fields rejected. |
| `partial`    | no  | yes | Declared fields validated; unknown fields pass through and are recorded in the inferred catalogue.  |
| `schemaless` | no  | yes | No declared schema needed. All writes observed into the inferred catalogue.                         |

SQLite supports strict mode only in the MVP — this is an implementation choice (per-field column
evolution for nested DTOs is a follow-up), not a permanent design constraint.

Schemaless does not mean no schema information: OpenVaultDB still observes and catalogues field
types from every write. The inferred catalogue is readable at
`GET /v1/databases/{db}/inferred-schema`.

### Field types

`string`, `number`, `integer`, `boolean`, `object`, `array`, `any`.

---

## DALgo driver

Use `dalgo2openvaultdb` to talk to `ovdb serve` from Go:

```go
import dalgo2openvaultdb "github.com/dal-go/dalgo2openvaultdb"

db, err := dalgo2openvaultdb.NewDB("http://127.0.0.1:6832", "mydb")

// Write inside a transaction (buffered → single POST /batch on commit)
err = db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
    rec := dal.NewRecordWithData(dal.NewKeyWithID("contacts", "c1"), &MyData{Name: "Alice"})
    return tx.Set(ctx, rec)
}, dal.TxWithMessage("add Alice"))
```

See `github.com/dal-go/dalgo2openvaultdb` for the full driver README and capability table.

---

## Documentation

- [Architecture](docs/architecture.md) — component overview, engine internals, schema modes, batch semantics
- [HTTP API](docs/api.md) — wire contract, all endpoints, error codes, update op format
- [Threat model](docs/threat-model.md) — security posture, known risks, non-goals
- [Roadmap](docs/roadmap.md) — near/medium/long-term follow-ups

---

## Sneat end-to-end validation

The MVP is validated end-to-end by running Sneat's conversational CLI commands against an
OpenVaultDB server backed by an inGitDB schemaless manifest in a real git repo.

### Prerequisites

- `ovdb` built and on PATH
- `sneat` CLI built from `sneat-go/sneat-cli`
- A git-initialised directory for the data store

### Setup

```sh
# Create the data repo
mkdir -p /tmp/sneat-ovdb-data && cd /tmp/sneat-ovdb-data
git init && git commit --allow-empty -m "init"

# Write an inGitDB schemaless manifest
cat > sneat.yaml <<'EOF'
database:
  id: sneat-dev
  schema_mode: schemaless

storage:
  engine: ingitdb
  path: ./data/sneat-dev

  ingitdb:
    push: none
EOF

# Start the server
ovdb serve --manifest sneat.yaml &
```

### Run Sneat commands through OpenVaultDB

```sh
export SNEAT_STORAGE=openvaultdb
export OPENVAULTDB_URL=http://127.0.0.1:6832

# Real facade operations (Contactus / Listus / Calendarius), persisted across processes:
sneat convo say --yes "add Jane Doe to contacts"
sneat convo say "list my contacts"
sneat convo say --yes "add milk and bread to shopping list"
sneat convo say --yes "add contact John"
sneat convo say --yes "meet John tomorrow at 5pm"
sneat convo say "list my calendar"

# Inspect the records that landed as YAML files in the git repo
find /tmp/sneat-ovdb-data/data/sneat-dev -name '*.yaml' -not -path '*/.git/*' | head -20
git -C /tmp/sneat-ovdb-data/data/sneat-dev log --oneline
```

Each `sneat convo` command results in one or more git commits in the data directory, with
human-readable YAML record files and auto-generated commit messages from OpenVaultDB.

---

### Firestore manifest example

```yaml
database:
  id: myapp
  schema_mode: schemaless   # firestore supports strict | partial | schemaless

storage:
  engine: firestore
  firestore:
    project: my-gcp-project
    database: ""            # default "(default)"
```

Credentials come from Application Default Credentials, or set
`FIRESTORE_EMULATOR_HOST` for the local emulator. Firestore conformance tests
run when that env var is set (`gcloud emulators firestore start`).

### PostgreSQL manifest example

```yaml
database:
  id: myapp
  schema_mode: strict     # postgres: strict only in MVP

storage:
  engine: postgres
  postgres:
    dsn_env: OVDB_POSTGRES_DSN   # env var holding the DSN (default shown)

schemas:
  collections:
    contacts:
      fields:
        title: {type: string, required: true}
```

Set the connection string in the environment, never the manifest:
`export OVDB_POSTGRES_DSN='postgres://user:pass@host:5432/db?sslmode=require'`.
Conformance tests run when `DALGO2POSTGRES_TEST_DSN` points at a reachable
PostgreSQL (e.g. `docker run -e POSTGRES_PASSWORD=… -p 5432:5432 postgres:17`).

## MVP boundaries

### In MVP

- `ovdb` CLI: `serve`, `init`, `status`, `databases`, `version`
- HTTP API v1: get/exists/put/post/patch/delete per record, batch, query, inferred-schema endpoint
- SQLite engine (strict mode only)
- inGitDB engine (strict / partial / schemaless)
- Database manifest (YAML): id, schema\_mode, engine, path, ingitdb push options, schemas
- Inferred schema catalogue for partial/schemaless databases
- `dalgo2openvaultdb` driver (full DALgo CRUD + queries + transactions)
- DALgo end2end conformance suite passes for all three engine/mode combinations
- Sneat CLI validated end-to-end via `SNEAT_STORAGE=openvaultdb`

### Not in MVP

- Auth (no bearer tokens, no ACLs; local-dev only)
- TLS, rate limiting, audit log
- Firestore or other remote engines inside OpenVaultDB
- Hosted `api.openvaultdb.com`
- Multi-tenancy, billing
- SQLite partial/schemaless schema modes
- GraphSpec / ModelSpec integration
- GraphQL, admin UI
- Replication, sync, conflict resolution, migrations
- Read-your-writes across HTTP round-trips (driver buffers writes client-side)
- Per-field SQLite columns / indexes
- Query cursors, offsets, projections, group-by over the HTTP API

See [docs/roadmap.md](docs/roadmap.md) for the prioritised follow-up list.
