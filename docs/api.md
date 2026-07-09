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

## Explicitly not in MVP

Auth (server binds 127.0.0.1 by default), cursors/offset, projections, group-by,
collection-group queries, update preconditions, server-side transactions with
read-your-writes across HTTP round-trips (driver buffers writes client-side instead),
optimistic concurrency.
