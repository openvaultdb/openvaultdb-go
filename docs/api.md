# OpenVaultDB HTTP API (MVP)

This is the wire contract between `ovdb serve` and `dalgo2openvaultdb`. It is intentionally
small: just enough for DALgo-backed Sneat CRUD validation. Versioned under `/v1`; JSON only.

## Conventions

- **Record key path** `{key...}`: DALgo key path `collection/id[/subcollection/subid...]`,
  exactly as produced by `dal.Key.String()` (IDs percent-encode `. $ # [ ] /`). The server
  splits the *escaped* path on `/` and unescapes each segment. Segments must be non-empty
  and must not be `.` or `..`.
- **Record body**: the record's data as a JSON object.
- **Errors**: non-2xx responses carry `{"error": {"code": "<machine-code>", "message": "..."}}`.
  - `404 not_found` — record or database missing
  - `409 already_exists` — insert conflict
  - `422 schema_validation` — strict/partial mode validation failure
  - `400 bad_request` — malformed key/body/query
  - `501 not_supported` — operation not in MVP

## Authentication (optional, `ovdb serve --auth`)

Off by default (local-dev). When enabled, every endpoint except the three
public ones (`/.well-known/openvaultdb`, `/authorize`, `/token`) requires
`Authorization: Bearer <token>`:

- **Owner token** (`--owner-token` / `$OVDB_OWNER_TOKEN`, generated and
  printed if unset): full access, including `/v1/databases` and the
  database list in `/v1/status`.
- **App tokens** come from the connect flow and are scoped to ONE database
  with capabilities per the spec taxonomy, optionally collection-scoped:
  `records:read`, `records:write:contacts`, `records:delete`,
  `collections:read`, `schema:read`. Reads/queries need `records:read`;
  `/dtql` needs an UNSCOPED `records:read` (its target collection is known
  only after deserialization). Missing/invalid token → `401 unauthorized`;
  insufficient capability → `403 forbidden`.

Connect flow (OAuth-style, dev consent page):

```
GET  /authorize?client_id=app&redirect_uri=<abs-url>&db=<id>&capabilities=records:read,records:write:notes&state=s
     → consent HTML; POST /authorize with decision=approve
     → 302 redirect_uri?code=<one-time, 5 min>&state=s
POST /token   grant_type=authorization_code&code=...&client_id=app   (form-encoded)
     → 200 {"access_token":"ovdb_...","token_type":"Bearer","expires_in":3600,"database":"<id>"}
```

Grants are persisted in `--auth-store` (default `ovdb-auth.json`) with
SHA-256 token hashes — the raw token exists only in the client. Tokens
expire after 1 h (no refresh in MVP; re-run the connect flow).

```
GET /.well-known/openvaultdb
→ 200 {"name":"OpenVaultDB","protocol":"openvaultdb/0.1","version":"...","authEnabled":true,
       "authorizeEndpoint":"/authorize","tokenEndpoint":"/token"}
```

## Endpoints

### Server / databases

```
GET /v1/status
→ 200 {"name":"OpenVaultDB","version":"<semver>","databases":["sneat-dev", ...]}

GET /v1/databases
→ 200 {"databases":[{"id":"sneat-dev","engine":"ingitdb","schemaMode":"schemaless"}, ...]}

GET /v1/databases/{db}
→ 200 {"id":"...","engine":"...","schemaMode":"...","collections":["..."]}   // declared collections, if any

GET /v1/databases/{db}/inferred-schema
→ 200 inferred schema catalogue JSON (see pkg/inferred); 404 for strict databases
```

### Records

```
GET    /v1/databases/{db}/records/{key...}
→ 200 {"key":"contacts/c1","data":{...}}
→ 404 not_found

HEAD   /v1/databases/{db}/records/{key...}
→ 200 (exists) | 404

PUT    /v1/databases/{db}/records/{key...}          body: {"data":{...}}     // set (upsert)
→ 204

POST   /v1/databases/{db}/records/{key...}          body: {"data":{...}}     // insert
→ 201 | 409 already_exists

PATCH  /v1/databases/{db}/records/{key...}          body: {"updates":[<update>, ...]}
→ 204 | 404 not_found (record must exist)

DELETE /v1/databases/{db}/records/{key...}
→ 204 (idempotent — 204 even if absent)
```

### Update operation object

```json
{"fieldName": "title", "value": "New"}                 // set top-level field
{"fieldPath": ["emails", "e1"], "value": {...}}        // set nested field (map keys created as needed)
{"fieldName": "obsolete", "delete": true}              // delete field
{"fieldPath": ["counters", "n"], "transform": "increment", "value": 2}
{"fieldName": "updatedAt", "serverTimestamp": true}    // RFC3339 UTC server time
```

Exactly one of `fieldName` / `fieldPath` must be set. `delete`, `transform`,
`serverTimestamp` are mutually exclusive with plain `value` semantics as shown.

### Batch (transaction commit)

```
POST /v1/databases/{db}/batch
body:
{
  "message": "optional commit message",     // inGitDB: git commit message
  "ops": [
    {"op":"set",    "key":"contacts/c1", "data":{...}},
    {"op":"insert", "key":"contacts/c2", "data":{...}},
    {"op":"update", "key":"spaces/s1",   "updates":[...] },
    {"op":"delete", "key":"contacts/c3"}
  ]
}
→ 200 {"applied": 4}
```

Ops are applied **in order** with read-your-writes inside the batch (an `update` sees a
prior `set` of the same key). The whole batch is applied in one engine transaction:
one SQL transaction for SQLite, one `RunReadwriteTransaction` (⇒ at most one git commit)
for inGitDB. Any failure (insert conflict, update of missing record, validation error)
rejects the whole batch — nothing is written. `update` requires the record to exist
(in the store or earlier in the batch), else 404.

Schema-mode validation applies to the final materialized data of each written key.

### Query

```
POST /v1/databases/{db}/query
body:
{
  "collection": "contacts",
  "parent":  "spaces/s1/ext/calendarius",   // optional: dal-escaped parent key path for scoped subcollection queries
  "where":   [{"field":"status","op":"==","value":"active"},
              {"field":"accounts","op":"array-contains","value":"x"}],   // AND-ed; optional
  "orderBy": [{"field":"title","desc":false}],                            // optional
  "limit":   10,                                                          // optional, 0 = no limit
  "keysOnly": false
}
→ 200 {"records":[{"key":"contacts/c1","data":{...}}, ...]}               // data omitted when keysOnly
```

Supported `op`: `==`, `<`, `<=`, `>`, `>=`, `in`, `array-contains`, `array-contains-any`.
Queries translate 1:1 to `dal.StructuredQuery` and execute on the DALgo driver's own
query evaluator — ovdb never evaluates queries itself. Keys-only queries without explicit
ordering return IDs sorted (limit applied after sorting), matching document-store drivers.

### DTQL

```
POST /v1/databases/{db}/dtql        body: a DTQL-YAML document (max 1 MiB)
→ 200 {"records":[{"key":"...","data":{...}}, ...]}
```

DTQL is dalgo's native lossless YAML serialization of `dal.StructuredQuery`
(`github.com/dal-go/dalgo/dtql`). ovdb deserializes and passes the query straight to the
driver — authenticate-and-bypass by design. Example:

```yaml
from:
  name: spaces
```

## Token Admin API (owner only)

These endpoints are only available when auth is enabled (`ovdb serve --auth`).
All three require the owner token. App tokens get `403 forbidden`.
This is how you create revocable scoped tokens for applications without running
the consent flow — and how you revoke them without restarting the server.

```
POST /v1/tokens
Authorization: Bearer <owner-token>
Content-Type: application/json
{
  "label":        "optional display name",
  "databaseId":   "mydb",         // optional ONLY when capabilities include databases:create;
                                  // empty = SERVER-LEVEL grant (matches every database)
  "capabilities": ["records:read", "records:write"],  // required; validated server-side
  "expiresIn":    "720h"          // optional Go duration; omit = never expires
}
→ 201 {
    "id":           "598e4e3f0dea7be1",    // short unique identifier
    "token":        "ovdb_...",             // SECRET — returned ONCE, never stored
    "label":        "...",
    "databaseId":   "mydb",
    "capabilities": ["records:read", "records:write"],
    "issuedAt":     "2026-07-09T00:00:00Z",
    "expiresAt":    "2026-08-08T00:00:00Z"  // omitted when never expires
  }
→ 400 bad_request  — missing/invalid databaseId, unknown capability, bad expiresIn
→ 401/403          — missing/invalid owner token

Server-level grants: a grant whose databaseId is empty matches EVERY mounted
database — the typical use is a provisioning token carrying only
databases:create (no records capabilities), but the owner may also mint
server-wide data grants deliberately. A db-scoped grant never matches a
server-level check.

GET /v1/tokens
Authorization: Bearer <owner-token>
→ 200 {"tokens": [{
    "id":           "...",
    "label":        "...",
    "databaseId":   "...",
    "capabilities": [...],
    "issuedAt":     "...",
    "expiresAt":    "...",  // omitted when never expires
    "revokedAt":    "..."   // omitted when not revoked
  }, ...]}
  (token secrets are never included in list output)

DELETE /v1/tokens/{id}
Authorization: Bearer <owner-token>
→ 200 {id, label, databaseId, capabilities, issuedAt, [expiresAt], revokedAt}
→ 404 not_found    — unknown id
→ 401/403          — missing/invalid owner token
  (revoking an already-revoked token is idempotent — 200)
```

The revoked grant stays persisted for the audit trail. Revocation takes effect
immediately on the running server — no restart required.

## Runtime database creation

`POST /v1/databases` provisions a new database at runtime — the multi-app
story: each app holds a provisioning token (databases:create only, no data
capabilities) and creates its own database, receiving a fresh token scoped to
just that database.

Requires `ovdb serve --data-dir <dir>`: each created database gets an inGitDB
schemaless data directory (`<data-dir>/<id>/`, git-initialised) plus a
manifest YAML (`<data-dir>/<id>.yaml`); on restart the data-dir is rescanned
and all created databases are remounted. The database is mounted live — no
restart needed.

Allowed callers: the owner, or any principal whose grant allows the
server-level `databases:create` capability.

```
POST /v1/databases
Authorization: Bearer <owner-token | databases:create token>
Content-Type: application/json
{"id": "my-app-db", "label": "optional label"}   // id must match ^[a-zA-Z0-9][a-zA-Z0-9_-]*$
→ 201 {
    "database": {"id":"my-app-db","engine":"ingitdb","schemaMode":"schemaless"},
    "token": {                       // ONLY when the creator is NOT the owner:
      "id":           "...",         // a freshly minted grant scoped to the new
      "token":        "ovdb_...",    // database (records:read/write/delete,
      "databaseId":   "my-app-db",   // collections:read, schema:read) — the
      "capabilities": [...],         // secret is returned ONCE, never again.
      "issuedAt":     "..."          // Owner-created databases get no auto-token.
    }
  }
→ 400 bad_request     — invalid id
→ 401/403             — missing token / token without databases:create
→ 409 already_exists  — id collides with a mounted database
→ 501 not_supported   — server started without --data-dir
```

## Explicitly not in MVP

Auth (server binds 127.0.0.1 by default), cursors/offset, projections, group-by,
collection-group queries, update preconditions, server-side transactions with
read-your-writes across HTTP round-trips (driver buffers writes client-side instead),
optimistic concurrency.
