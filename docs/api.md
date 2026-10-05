# OpenVaultDB HTTP API (MVP)

This is the wire contract between `ovdb serve` and `dalgo2openvaultdb`. It is intentionally
small: just enough for DALgo-backed Sneat CRUD validation. Versioned under `/v1`; JSON only.

## Conventions

- **Record key path** `{key...}`: DALgo key path `collection/id[/subcollection/subid...]`,
  exactly as produced by `dal.Key.String()` (IDs percent-encode `. $ # [ ] /`). The server
  splits the *escaped* path on `/` and unescapes each segment. Each decoded segment must be
  non-empty, must not be `.` or `..`, must not contain control characters (U+0000–U+001F,
  U+007F), and must not contain a `.` or `..` component between `/` or `\` separators
  (e.g. an id `%2E%2E%2F%2E%2E%2Fp2`, decoded `../../p2`, is rejected). The same rule applies
  to batch `key`s, query `collection`/`parent` and DTQL collection names, on every engine.
  Capabilities scope the key's (or query parent's) root collection.
- **Record body**: the record's data as a JSON object.
- **Errors**: non-2xx responses carry `{"error": {"code": "<machine-code>", "message": "..."}}`.
  - `404 not_found` — record or database missing; also a collection the database does not
    declare, on an engine whose adapter builds SQL (see [Names the server accepts](#names-the-server-accepts))
  - `409 already_exists` — insert conflict
  - `422 schema_validation` — strict/partial mode validation failure
  - `400 invalid_key` — malformed or unsafe key, collection name or parent path
  - `400 bad_request` — malformed body/query; also a field name in a write body (or a query) that
    is not a plain field name, and on an engine whose adapter builds SQL a write that names
    nothing to change and an update that names the record's key column (see
    [Names the server accepts](#names-the-server-accepts))
  - `403 read_only` — server-wide read-only mode rejected a mutation
  - `500 internal` — unexpected server/engine error (details are logged server-side, not returned)
  - `501 not_supported` — operation not in MVP
  - `501 query_unsupported` — a structured query (`/query`, `/dtql`) on a storage engine not yet
    cleared for queries: a `postgres` or `mysql` mount. Key reads and writes keep working there

## Authentication (optional, `ovdb serve --auth`)

Off by default (local-dev). When enabled, every machine endpoint except
`/.well-known/openvaultdb`, `/authorize`, and `/token` requires
`Authorization: Bearer <token>`. The generic `/ovdb/` pages remain reachable
without a token, but hide the database catalog and profiles:

- **Owner token** (`--owner-token` / `$OVDB_OWNER_TOKEN`, generated and
  printed if unset): full administrative capability, including `/v1/databases` and the
  database list in `/v1/status`. Database ACL still applies to data operations.
- **App tokens** come from the connect flow and are scoped to ONE database
  with capabilities per the spec taxonomy, optionally collection-scoped:
  `records:read`, `records:write:contacts`, `records:delete`,
  `collections:read`, `schema:read`. Reads/queries need `records:read`;
  `/dtql` accepts a collection-scoped grant after validating its target. Missing/invalid token → `401 unauthorized`;
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

Without auth, this document also lists mounted databases with their stable
browser-openable `url` (`/ovdb/dbs/<id>`), versioned `apiUrl`, and capability
flags. The server uses the HTTP request origin by default; `ovdb serve
--public-url` sets the canonical external origin behind a reverse proxy.
Authenticated servers do not publish database names in public discovery.

The generic human pages are `GET /ovdb/`, `GET /ovdb/dbs/`,
`GET /ovdb/dbs/{db}`, and `GET /ovdb/dbs/{db}/collections/{collection}`.
The database URL is an identity and profile, not a
machine query endpoint. Collection pages show declared fields, database foreign
keys discovered through DALgo, and optional additional OVDB `references`.
Each relationship shows its source and enforcement state. OVDB declarations
are informational and do not enforce referential integrity. Database foreign
keys show the provider-reported enabled, disabled, or unknown state.
The pages use a neutral built-in stylesheet; a hosting
website may render its own pages while keeping these API/discovery semantics.

For example, a collection declaration can describe an outgoing reference:

```yaml
schemas:
  collections:
    Artist:
      fields:
        ArtistId: {type: integer}
    Album:
      fields:
        ArtistId: {type: integer}
      references:
        - {field: ArtistId, collection: Artist, target_field: ArtistId}
```

The server derives Artist's incoming reference from Album's declaration.
For a composite relationship, use ordered `fields` and `target_fields` arrays
instead of `field` and `target_field`. A matching database key and OVDB
declaration appear once with both sources shown. Provider foreign keys are
stored on the referencing collection; incoming links are derived from them.

## Endpoints

### Server / databases

```
GET /v1/status
→ 200 {"name":"OpenVaultDB","version":"<semver>","databases":["sneat-dev", ...]}

GET /v1/databases
→ 200 {"databases":[{"id":"sneat-dev","engine":"ingitdb","schemaMode":"schemaless"}, ...]}

GET /v1/databases/{db}
→ 200 {"id":"...","engine":"...","schemaMode":"...","collections":["..."]}   // declared collections, by canonical name

GET /v1/databases/{db}/inferred-schema
→ 200 inferred schema catalogue JSON (see pkg/inferred); 404 for strict databases
```

`collections` is the sorted list of collections the database holds. On `sqlite`, `postgres` and
`mysql` (and an engine the server does not recognise) it holds the declared collections only,
each by its canonical name: a name the storage driver reports is kept only when the database
declares it under exactly that name, so a table of the file that the manifest does not declare,
and a table named with the quote characters of the quoted spelling of a declared SQLite key, are
not listed, and every name listed is one the query routes accept. On `ingitdb` and `firestore`
the list is what the driver reports. On `postgres` the driver reports views as well as tables, so
a declared collection that is a view is listed, and the foreign keys shown for a table include
those of a table with uuid, json or array columns.

### Records

```
GET    /v1/databases/{db}/records/{key...}
→ 200 {"key":"contacts/c1","data":{...}}
→ 404 not_found

GET    /v1/databases/{db}/read?key=<percent-encoded full-key-path>
→ 200 {"key":"contacts/c1","data":{...}}
→ 400 invalid_key | 404 not_found

HEAD   /v1/databases/{db}/records/{key...}
→ 200 (exists) | 404

PUT    /v1/databases/{db}/records/{key...}          body: {"data":{...}}     // set (upsert)
→ 204 | 400 bad_request (field name) | 404 not_found (collection not declared, SQL engines)

POST   /v1/databases/{db}/records/{key...}          body: {"data":{...}}     // insert
→ 201 | 409 already_exists | 400 bad_request (field name) | 404 not_found (collection not declared, SQL engines)

PATCH  /v1/databases/{db}/records/{key...}          body: {"updates":[<update>, ...]}
→ 204 | 404 not_found (record must exist; or collection not declared, SQL engines) | 400 bad_request (field name)

DELETE /v1/databases/{db}/records/{key...}
→ 204 (idempotent — 204 even if absent) | 404 not_found (collection not declared, SQL engines)
```

### Names the server accepts

The server refuses these requests **before the storage adapter is called** (the adapter sees
nothing of them). They apply to every route that reads a record by key or writes: the six
`/records/{key...}` methods, `/read?key=`, `/batch`, the protected `PATCH` with
`Content-Type: application/vnd.dtql.operation+json`, and the authorization endpoints that read
by key (`/access/evaluate` inspection and sampling, `/access/evidence`).

- **Collections on engines whose adapter builds SQL** (`sqlite`, `postgres`, `mysql`; an engine
  the server does not recognise is held to the same rule). The key's collection must be one the
  database declared in its `schemas` when it opened. A SQLite manifest that keys a collection by
  its SQL-quoted identifier, `'"Order Details"'`, declares that one collection under two
  spellings, the quoted form and the public name `Order Details`, which is its canonical name.
  The routes differ in which spelling they take:
  - The **key routes** (`/records/{key...}` except the protected `PATCH`, `/read?key=`, `/batch`)
    take both spellings. They are the same collection: a grant scoped to either spelling covers a
    key written in either, and the adapter is given the public name.
  - The **query routes** (`/query`, `/dtql`) and the **by-key authorization routes** (the
    protected `PATCH`, `/access/evaluate` and `/access/evidence`) take the canonical name only.
    The quoted spelling of a SQLite key names the table whose name carries the quote characters,
    not the declared collection, so on these routes it is answered as a collection the database
    does not declare. A grant is matched against the name sent: a grant on the public name covers
    the public name, a grant on the quoted spelling covers a request that sends the quoted
    spelling (which these routes then refuse), and neither covers the other. On `postgres` and
    `mysql` a key is the name as written, which is its own canonical name, so every route takes
    the same name.

  A manifest in which one name would be a spelling of two collections, or in which two keys that
  are one table declare different fields, is refused when the database opens. Names match
  exactly, including case, on every SQL engine. Anything else is `404 not_found`, for `GET`,
  `HEAD`, `PUT`, `POST`, `PATCH` and `DELETE` alike (a `DELETE` of an undeclared collection is
  not the idempotent `204` of a declared one) and for a whole `/batch`: one undeclared key
  refuses every op. The key rule above (`invalid_key`) is a path-safety rule only and accepts
  quotes, spaces and semicolons, which is why the declaration is the allow-list here.
- **Names the adapter writes into a statement.** On `sqlite` the adapter quotes every collection,
  field and primary-key name it writes, so a declared name with a space or a hyphen (`Orders
  Status`, `order-items`) reads and writes its own table and no other (not `Orders`). On
  `postgres` and `mysql` the adapter writes a name as given and accepts only ASCII letters,
  digits and underscores, not starting with a digit (`^[A-Za-z_][A-Za-z0-9_]*$`), for a
  collection name, a field name and a primary-key name alike. It refuses any other before it
  sends a statement. The field-name rule below is wider than this, so on those two engines a
  field name that a request carries can pass it and still be refused by the adapter: `500
  internal`, and a `HEAD` answers `404`. A `postgres` or `mysql` manifest that
  declares a collection or field name outside that rule does not open: provisioning the declared
  collections refuses the name, so the mount fails when the database opens and no request is
  served for it. Quoting on those two engines follows the task that gives the two mounts a
  reviewed dialect (OV-01).
- **Access policies and spellings.** The access-policy layer sees the collection under the name
  the adapter is given. On `sqlite`, for a key read or write, that is the public name whichever
  spelling the caller sent, so a policy path names the public name; a policy path written with
  the quoted spelling does not match a key read or write. The query routes and the by-key
  authorization routes take the public name only (see above), so a policy path always names the
  public name there too.
- **No subcollections on these engines.** A key with a parent (`customers/c1/orders/o1`) is
  `404 not_found` whatever its segments, because the adapter maps it to a recordset named after
  the whole path (`<leaf>_<parent>`), which no mount registers and which is not the collection
  the capability was checked on.
- **Nested keys on the document engines.** `ingitdb` (the local and the GitHub-backed
  adapter) and `firestore` address a nested key as a subcollection of the parent record, in
  `GET`, `HEAD`, `PUT`, `POST`, `PATCH` and `DELETE` alike: `a/x/b/5` is document `5` of
  subcollection `b` under record `x` of collection `a`, never a top-level collection `b`. The
  key rule alone applies to them, and the capability is checked on the root collection (`a`).
- **Field names, on every engine.** Every field name a write carries that can become a column
  must pass the same rule as the names in `/query` and `/dtql`: dot-separated segments of
  letters, digits, underscore and hyphen (Unicode letters allowed), each optionally starting with
  `$` before a letter (`$id`), at most 256 bytes, no `--`. (The adapter of `postgres` and `mysql`
  accepts less; see above.) That covers the top-level keys of
  `data` in `PUT`, `POST` and batch `set`/`insert` ops, the `fieldName` of an update
  (`delete: true` included), the first segment of an update's `fieldPath`, and the same in a
  protected operation's columns and changes. Anything else is `400 bad_request`; an update that
  names no field is too. On the records routes and `/batch`, within one operation the collection
  is checked before the field names, so a request that breaks both rules is `404`. On the
  authorization routes the field names are checked first, so a request that breaks both rules
  is `400`, as it is for a table the database declares. In a batch the operations are checked in
  order and the first one that fails decides the answer: an earlier operation with a bad field
  name makes the whole batch `400`, even when a later one names an undeclared collection.
- **The key column on the SQL engines.** An update that names the record's key column, `id`, in
  any spelling of its case (as the `fieldName`, as the first segment of a `fieldPath`, or as a
  protected operation's change path; `delete`, a transform and `serverTimestamp` included) is
  `400 bad_request`, whatever else the request holds, and nothing is written: `PATCH` of
  `/records/{key...}`, an `update` op of `/batch`, the protected `PATCH` and the inspection of
  an update alike. A field whose name merely contains `id` is not the key column. `ingitdb` and
  `firestore` keep the key outside the record's fields and are unchanged.
- **Later segments of a `fieldPath`** are map keys. On the SQL engines they must pass the same
  rule. On `ingitdb` and `firestore` they are data and never reach SQL (Sneat's linkage writes
  `["related", ext, collection, "id@spaceID"]`), so only a blank segment (empty, or only white
  space) or one that holds a control character is `400 bad_request`. A path with no segment at
  all is `400 bad_request` on every engine.
- **Empty writes on the SQL engines.** An update with no operation (an `update` op of `/batch`
  whose `updates` is empty or absent; `PATCH` with an empty `updates` list is `400` on every
  engine), and a `set` (`PUT`, batch `set`) that names no field but `id` for a record that
  exists, give the adapter nothing to put in a statement and are `400 bad_request`; the batch
  is refused whole and nothing is written. (The `set` rule depends on the record, so the server
  reads it by key first, as it does before any write.) A `set` of no field for a record that
  does not exist inserts a record that holds only its id (`204`), as `POST` with
  `{"data":{}}` does (`201`).
- **A mount with access policies** does not say which collections it declares: it refuses
  schema discovery, and a collection it does not declare is answered as a declared collection
  the policy hides, with no message that names it. The routes that take a table give that
  answer in their own form. The protected `PATCH` and `/access/evidence` answer `404
  resource_unavailable`, redacted, as a hidden or missing record. `/access/evaluate` in
  inspection and in sampling answers `200` with the redacted deny (`result: deny`, disclosure
  redacted, no layer detail) for an undeclared table, for the quoted spelling of a declared
  SQLite key and for a declared table the policy hides alike: the same status and the same body
  but for the name the caller sent, whatever the request orders by, and for a multi-operation
  inspection the undeclared operation is redacted as a hidden one is. `/query` and `/dtql`
  answer `403 ACCESS_DENIED` with the generic message (see [DTQL](#dtql)). These routes give the
  coordinator the collection as written, which is why they take the canonical name only.

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

Schema-mode validation applies to the final materialized data of each written key that exists
after the batch. A `set` or `insert` op without `data` writes a record with no fields and is
validated as one, so on a mount that requires a field it is `422 schema_validation` and the
batch is refused whole; only a key that the batch leaves absent (deleted by a later op) is not
validated.

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

GET /v1/databases/{db}/query?q=<percent-encoded-JSON-query>
→ 200 {"records":[{"key":"contacts/c1","data":{...}}, ...]}
```

The `q` value is the same JSON object accepted by `POST /query`, URL-encoded
once. It is limited to 1 MiB. Query results remain subject to the server's 8
MiB result buffer. For a public, unprotected mounted database, embedders can
set `database.cache_ttl: 24h` in that database's manifest and run the server
with `server.WithReadOnly(true)`. Successful GET `/read`, `/query`, and `/dtql`
responses then send `Cache-Control: public, max-age=N, s-maxage=N`, where N is
that database's duration in seconds. The duration must be whole seconds from
`0s` through `8760h`; absent or `0s` disables public caching. These URL-query forms
default to `Cache-Control: no-store`, including authentication failures,
missing databases, and HEAD requests. Authentication-enabled or
policy-protected responses are never marked cacheable.

`server.WithReadOnly(true)` rejects every record, database, and token mutation
with `403 {"error":{"code":"read_only"...}}`, including mutations authenticated
with the owner token. Read and query endpoints continue to enforce their
ordinary authentication and database policies.

Result keys are full key paths from the database root: a query with `"parent":"lists/to-buy"`
on `items` returns `lists/to-buy/items/x` (not `items/x`), usable as-is with `/records`.
With a `parent`, the read capability is checked on the parent's root collection (`lists`).

Structured queries run on `sqlite`, `ingitdb` and `firestore` mounts only. On a `postgres` or
`mysql` mount `/query` and `/dtql` answer `501 query_unsupported` (the message names the engine),
and the database's metadata advertises `query: false` and `dtql: false`, until the reviewed query
compiler for those engines lands. Key reads and writes are unaffected, subject to
[Names the server accepts](#names-the-server-accepts).

Supported `op`: `==`, `<`, `<=`, `>`, `>=`, `in`, `array-contains`, `array-contains-any`.
Queries translate 1:1 to `dal.StructuredQuery` and execute on the DALgo driver's own
query evaluator — ovdb never evaluates queries itself. Keys-only queries without explicit
ordering return IDs sorted (limit applied after sorting), matching document-store drivers.

### DTQL

```
POST /v1/databases/{db}/dtql        body: a DTQL-YAML document (max 1 MiB)
→ 200 {"records":[{"key":"...","data":{...}}, ...]}

GET /v1/databases/{db}/dtql?q=<percent-encoded-DTQL-YAML>&parameters=<percent-encoded-JSON-object>
→ 200 {"records":[{"key":"...","data":{...}}, ...]}
```

`parameters` is optional. GET accepts one `q` and at most one `parameters`
value; the full request URI is limited to 8 KiB. Use GET only for public,
non-sensitive queries: URLs can appear in browser history, proxy logs, and
analytics. Use POST when query values are sensitive. Snapshot pages always
send `no-store`, even if their initial request uses GET. Cacheable unpaged GET
responses vary on the three paging headers, so a proxy must keep paged and
unpaged requests separate.

For a complete result larger than the ordinary 1000-row/8 MiB response, the
client can opt into **result snapshot paging** on the same endpoint:

1. Send `OVDB-Page-Size: 1..1000` with a DTQL document whose `limit` and
   `offset` are zero or omitted. The first request captures the result through
   one DALgo reader into a private temporary file before returning its first
   page. A client can show “Preparing source snapshot” while this request is
   pending.
2. The response is `{"records":[...],"snapshotToken":"...","nextPageToken":"...","snapshotExpiresAt":"..."}`.
   The last page omits `nextPageToken`. Each page is at most 7 MiB, even if it
   contains fewer than `OVDB-Page-Size` rows.
3. Send the identical DTQL document and page size with
   `OVDB-Page-Token: <nextPageToken>` for each subsequent page. Tokens are
   opaque, retryable until expiry, bound to the database and bearer credential, and are
   never placed in URLs. Continue until `nextPageToken` is absent.
4. After receiving the last page, or when cancelling the scan, send the same
   document and page size with `OVDB-Page-Token: <snapshotToken>` and
   `OVDB-Page-Close: true`. The server returns `204` and immediately releases
   the snapshot's disk space and capacity slot. Repeating a valid close also
   returns `204`. Pages requested after close return `410 snapshot_expired`.
   The `snapshotToken` field is present even when the first page is final.

Pages come from the captured result, so an OVDB write between page requests
does not change later pages. This contract does not promise a transaction
across different mounted databases or protect against a source's own external
writes during its one reader traversal. A capture is capped at 1,000,000 rows,
512 MiB on disk and 60 seconds. At most two captures/snapshots are active per
server (1 GiB maximum disk usage). A snapshot expires five minutes after
capture, on explicit close, on database unmount, or on server shutdown; stale files from a crash
are swept on startup. Call `Server.CloseSnapshots` after stopping HTTP serving
and draining requests. `410 snapshot_expired` means the client must restart
the query. `413 snapshot_too_large`, `503 snapshot_capacity`, and
`422 snapshot_unsupported` are explicit terminal errors. Policy-protected
databases currently return `snapshot_unsupported` because cached results
cannot safely reflect policy changes between pages. Browser CORS preflight
allows all three paging headers when the origin is allowed.

DTQL is dalgo's native lossless YAML serialization of `dal.StructuredQuery`
(`github.com/dal-go/dalgo/dtql`). OpenVaultDB validates the target, checks token
capabilities, and executes through the mounted DALgo policy enforcement layers.
`POST /v1/databases/{db}/dtql` accepts the raw YAML document or
`Content-Type: application/json` with `{"query":"<DTQL YAML>","parameters":{"Name":1}}`.
The JSON form binds named scalar or scalar-array values to `{param: Name}`
expressions. Bindings are parsed as values, so their text cannot alter the
query structure. Missing, unused, null, object, and oversized array bindings
are rejected. Database metadata advertises `queryFormat: dtql-yaml+json`;
raw YAML remains accepted for existing clients.
The supported profile is a single unaliased root collection with field projection,
filtering, ordering, and pagination. Joins, aggregation, cursors, and native queries
are not supported by this endpoint. Limit defaults to 1000 (maximum 1000), offset
is at most 10000, execution context deadline is 10 seconds, and result buffering is capped
at 8 MiB. Rows are filtered before pagination; column restrictions also apply to
explicit projections, caller filters, and ordering. Denial returns HTTP 403 with
`error.code: ACCESS_DENIED` and a generic message. The first slice does not expose
policy diagnostics or implement the full DTQL blocker response contract.

A document of one collection can read others in a subquery, so the `records:read` capability is
checked for every collection a document reads (its root source, every joined or derived source
and every subquery, wherever it sits), not only for the one it names first: a collection-scoped
grant covers a document only when it covers each of them, each named as the document writes it.
The owner token is not scoped.

On `/query` and `/dtql` a mount with access policies does not say which collections it declares:
a query or document of a collection the database does not declare is answered as one of a
declared collection the policy denies (`403 ACCESS_DENIED`, the same body), and so is one that
names a spelling that is not the canonical name, such as the quoted spelling of a declared
SQLite key; a mount without access policies answers both `404 not_found`. Such a mount serves one
plain collection per document: a document that reads more than that (a join, a derived source, a
subquery of any kind, or a scan bound on its source) is `422 authorization_unsupported`, with a
message that names no collection, whichever collections it names and before any name is looked
at. A document of one readable collection is read as before.

See [local ACL setup and demonstration](layered-acl-implementation.md). Example:

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

## CORS (browser cross-origin access)

Browser clients make cross-origin requests; without CORS headers the browser
blocks the response. Use `ovdb serve --cors <origin>` to enable CORS support.

### Flag

```
ovdb serve --cors https://sneat.app --cors http://localhost:4200
```

- `--cors` is repeatable; comma-separated values in a single flag are accepted:
  `--cors "https://sneat.app,http://localhost:4200"`
- `--cors '*'` allows any origin (development only — see threat model).
- Omitting `--cors` entirely (the default) means no CORS headers are emitted:
  non-browser clients are completely unaffected.

### Behavior

| Situation | Response |
|---|---|
| No `--cors` configured | No CORS headers ever added |
| `Origin` absent | No CORS headers (non-browser) |
| `Origin` in allowed list | `Access-Control-Allow-Origin: <origin>`, `Vary: Origin` |
| `Origin` not in allowed list | No ACAO; request still processed normally |
| `OPTIONS` preflight from allowed origin | 204 + ACAO + Allow-Methods + Allow-Headers + Max-Age |
| `OPTIONS` preflight from disallowed origin | 204, no ACAO (browser blocks subsequent request) |

Allowed headers: `Authorization, Content-Type`. Allowed methods: `GET, HEAD,
POST, PUT, PATCH, DELETE`. Preflight Max-Age: 600 s.

`Access-Control-Allow-Credentials` is never set — bearer tokens travel in the
`Authorization` header, not cookies.

Preflight `OPTIONS` requests are handled **before** auth middleware, so browsers
receive a 204 (not 401) even when `--auth` is enabled. Auth is still enforced
on subsequent non-preflight requests.

### Example (local web app)

Start the server allowing a local Angular dev server:

```sh
ovdb serve --manifest mydb.yaml --cors http://localhost:4200
```

Fetch from the browser:

```js
const resp = await fetch('http://localhost:6832/v1/databases/mydb/records/notes/n1', {
  headers: { Authorization: 'Bearer ovdb_...' }
})
```

## Explicitly not in MVP

Auth (server binds 127.0.0.1 by default), cursors/offset, projections, group-by,
collection-group queries, update preconditions, server-side transactions with
read-your-writes across HTTP round-trips (driver buffers writes client-side instead),
optimistic concurrency.
