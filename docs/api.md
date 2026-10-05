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

With auth on or off, the document carries a `query` block that states the query profile and its
limits (see [Query profile and relational documents](#query-profile-and-relational-documents)).
Without auth, it also lists mounted databases with their stable
browser-openable `url` (`/ovdb/dbs/<id>`), versioned `apiUrl`, capability
flags, and the `joins` and `aggregation` booleans of the query profile.
The server uses the HTTP request origin by default; `ovdb serve
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
→ 200 {"id":"...","engine":"...","schemaMode":"...","collections":["..."],"capabilities":{"read":true,"query":true,"dtql":true,"write":true,"joins":true,"aggregation":true}}   // declared collections, by canonical name

GET /v1/databases/{db}/inferred-schema
→ 200 inferred schema catalogue JSON (see pkg/inferred); 404 for strict databases
```

`collections` is the sorted list of collections the database holds. On `sqlite`, `postgres` and
`mysql` (and an engine the server does not recognise) it holds the declared collections only,
each by its canonical name and only when the storage driver reports it, so a table of the file
that the manifest does not declare, and a table named with the quote characters of the quoted
spelling of a declared SQLite key, are not listed, and every name listed is one the query routes
accept. A declared name is matched with the reported one exactly, except on `postgres`, which
stores a name lower-cased and reports it so, and on `sqlite`, which finds a table whatever the
case of the ASCII letters of its name: there a declared name that has an upper-case letter
(`Customers`) is listed under its declared spelling when its ASCII lower-cased form is reported
(`customers`). On `mysql` the match stays exact. On
`ingitdb` and `firestore` the list is what the driver reports. On `postgres` the driver reports
views as well as tables, so a declared collection that is a view is listed, and the foreign keys
shown for a table include those of a table with uuid, json or array columns.

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
  an update alike. A field whose name merely contains `id` is not the key column. A write
  whose `data` names the key column in a case other than `id` (`ID`) is `400 bad_request` too
  (`PUT`, `POST`, batch `set`/`insert`, and the protected operations), as every spelling is the
  one column; a manifest that declares a field named so does not open. `ingitdb` and
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
  resource_unavailable`, redacted, as a hidden or missing record: for an undeclared table, for
  the quoted spelling of a declared SQLite key and for a declared table the policy hides alike,
  the same status and the same body but for the name the caller sent, whichever fields of the
  resource the request carries. `/access/evaluate` in inspection and in sampling answers `200`
  with the redacted deny (`result: deny`, disclosure redacted, no layer detail) for an
  undeclared table, for the quoted spelling of a declared SQLite key and for a declared table the
  policy hides alike, for a caller who may not inspect protected rows: the same status and the
  same body but for the name the caller sent, whatever the request orders by, and for a
  multi-operation inspection the undeclared operation is redacted as a hidden one is, and the
  facts given for the other operations of the request do not depend on which of the two it is.
  A caller who may inspect protected rows is given layer detail for a record it cannot read only
  when a policy admits the operation for the stored row to decide; for a declared table the
  policy hides, or cannot decide anything about (a principal outside the policy's realm, a policy
  source that is unavailable), it gets the answer an undeclared table gets. So does a declared
  table that the protected session of the adapter cannot prepare (a SQLite table with a column
  default, a foreign key, a trigger or a key that is not a text id, for instance): the protected
  `PATCH` and `/access/evidence` answer `404 resource_unavailable` and an inspection the redacted
  deny, and each of the three routes logs one warning for the request ("protected session refused
  an operation", with the method, the route path and the names of the collections; it does not
  state a cause). A refusal is answered that way only while the request is still alive: when the
  request was canceled or ran past its deadline, the answer is `503 authorization_unavailable`
  and nothing is logged. A protected `PATCH` whose request was canceled or ran past its
  deadline is answered `503 authorization_unavailable` at every later point as well (while the
  update is executed, for instance), never `422 validation_failed`. An inspection that holds more evidence than one protected
  session accepts is assessed one operation at a time and answered as it would be if the session
  accepted it.
  While a policy layer cannot be used, a sample is refused alike whatever collection it names.
  A sample asks the policies about its query before it checks the collections the query names,
  so what the policies answer (a denial, or a result they cannot decide) is the same whichever
  collection the query names. The routes check an operation in one order: its field names, then
  whether the protected session supports it (an execution class other than `dtql`, or an action
  or change it cannot carry: `422 authorization_unsupported`), and its table last, so that
  answer is the same for every table.
  `/query` and `/dtql` answer `403 ACCESS_DENIED` with the generic message (see
  [DTQL](#dtql)). These routes give the coordinator the collection as written, which is why they
  take the canonical name only.

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
server (1 GiB maximum disk usage). The row, byte and slot limits are the defaults of
`server.DefaultSnapshotLimits()`; an embedder sets them with `server.WithSnapshotLimits`, whose
three fields must all be positive (the option panics when the server is built otherwise). On a
memory-backed file system, where the spool counts against the instance memory, set slots times
bytes well below the memory. A snapshot expires five minutes after
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
The profile of a document of one plain collection is a single unaliased root collection with
field projection, filtering, ordering, and pagination. A document that joins, groups, aliases or
reads another collection in a subquery is relational and is described under
[Query profile and relational documents](#query-profile-and-relational-documents); cursors and
native queries are not supported. Limit defaults to 1000 (maximum 1000), offset
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
subquery of any kind) is `422 authorization_unsupported`, with a
message that names no collection, whichever collections it names and before any name is looked
at. A document of one readable collection is read as before.

See [local ACL setup and demonstration](layered-acl-implementation.md). Example:

```yaml
from:
  name: spaces
```

### Query profile and relational documents

DTQL documents are read by two endpoints. Both take the same body (a raw DTQL-YAML document, or
`Content-Type: application/json` with `{"query": "<DTQL YAML>", "parameters": {...}}`) and the same
GET form (`?q=` and `?parameters=`, URI at most 8 KiB, `414` beyond that).

```
POST|GET /v1/databases/{db}/dtql     the sources of the document belong to {db}
POST|GET /v1/dtql                    the document reads one or several databases
```

A document of one plain collection (no alias, join, grouping, aggregate or subquery, and a root
that names no database or names `{db}` itself) is answered as described under [DTQL](#dtql), with
a key on every record. Any other document the classifier accepts is **relational**: a join, a
`groupBy`, a `having`, an aggregate, a column alias, a null test (`isNull`, `isNotNull`), a column
qualified with its `source`, a computed column, a subquery of any kind (a derived source, a
scalar subquery, `exists`, a query-valued comparison), or a source that names its `database`. A
relational document is answered as described below, on both endpoints.

#### Discovery

`GET /.well-known/openvaultdb` carries a `query` block, with auth on or off. It states what the
server does, from the configuration it runs with:

<!-- doc-example method=GET path=/.well-known/openvaultdb status=200 -->
```json
{
  "authEnabled": false,
  "databases": [
    {
      "apiUrl": "http://localhost:8080/v1/databases/chinook",
      "capabilities": {
        "aggregation": true,
        "dtql": true,
        "joins": true,
        "query": true,
        "read": true,
        "write": true
      },
      "id": "chinook",
      "url": "http://localhost:8080/ovdb/dbs/chinook"
    },
    {
      "apiUrl": "http://localhost:8080/v1/databases/countries",
      "capabilities": {
        "aggregation": true,
        "dtql": true,
        "joins": true,
        "query": true,
        "read": true,
        "write": true
      },
      "id": "countries",
      "url": "http://localhost:8080/ovdb/dbs/countries"
    },
    {
      "apiUrl": "http://localhost:8080/v1/databases/crm",
      "capabilities": {
        "aggregation": false,
        "dtql": true,
        "joins": false,
        "query": true,
        "read": true,
        "write": true
      },
      "id": "crm",
      "url": "http://localhost:8080/ovdb/dbs/crm"
    },
    {
      "apiUrl": "http://localhost:8080/v1/databases/events",
      "capabilities": {
        "aggregation": false,
        "dtql": true,
        "joins": false,
        "query": true,
        "read": true,
        "write": true
      },
      "id": "events",
      "url": "http://localhost:8080/ovdb/dbs/events"
    }
  ],
  "name": "OpenVaultDB",
  "protocol": "openvaultdb/0.1",
  "query": {
    "endpoint": "/v1/dtql",
    "features": {
      "aggregates": [
        "count",
        "sum",
        "avg",
        "min",
        "max"
      ],
      "crossDatabase": true,
      "externalSources": false,
      "fieldNames": "plain",
      "groupBy": true,
      "having": true,
      "joins": [
        "inner",
        "left"
      ],
      "protectedDatabases": false,
      "subqueries": true,
      "windowFunctions": false
    },
    "format": "dtql-yaml+json",
    "joinEngines": [
      "sqlite",
      "ingitdb"
    ],
    "limits": {
      "maxGroups": 100000,
      "maxInMemoryJoinBytes": 16777216,
      "maxInMemoryJoinRows": 10000,
      "maxLimit": 1000,
      "maxOffset": 10000,
      "maxResultBytes": 8388608,
      "maxResultRows": 1000,
      "maxSourceBytes": 67108864,
      "maxSourceRows": 100000,
      "maxSources": 8,
      "maxSubqueryDepth": 4,
      "timeoutMs": 10000
    }
  },
  "version": "0.1.0"
}
```

- `endpoint` and `format`: the endpoint that reads several databases and the document format.
- `features`: `joins` lists the join types (`inner` and `left`); `aggregates` the aggregate
  functions of the profile (`count`, `sum`, `avg`, `min`, `max`; `first` and `last` are not in the
  profile, see [Launch limits](#launch-limits)); `crossDatabase` is true (one document may read
  several mounted databases); `externalSources`, `windowFunctions` and `protectedDatabases` are
  false (see [Launch limits](#launch-limits)); `fieldNames` is `plain`.
- `limits`: what one request may ask for, each value the one the server enforces. `timeoutMs`,
  `maxSourceRows` and `maxSourceBytes` are the server's configuration (`QueryLimits`);
  `maxResultRows` and `maxResultBytes` bound the answer. `maxSources` is the most collection reads
  one document makes, `maxSubqueryDepth` the most levels of subquery below the outermost query, and
  `maxLimit` and `maxOffset` the largest `limit` and `offset` of the outermost query. The
  remaining three bound the in-memory route: `maxInMemoryJoinRows` and `maxInMemoryJoinBytes` the most rows
  and bytes a join holds, and `maxGroups` the most groups an aggregation keeps. The block states
  none of the server's capacity (the slots of the concurrency gate and the queue wait).
- `joinEngines`: the storage engines whose databases may take part in a relational document. An
  engine is listed when the operator's list names it, the server clears it for structured queries
  and it is not the GitHub-backed inGitDB engine, which no list enables. A database on a listed
  engine advertises `joins: true` unless it has access policies.

Each database in the list (listed when auth is off) and the metadata of a database
(`GET /v1/databases/{db}`, the way to read it when auth is on) carry two booleans, `joins` and
`aggregation`, in the `capabilities` map beside `read`, `query`, `dtql` and `write`. They are true
when a relational document that names the database is not refused for the database itself: its
engine is in `joinEngines`, and it has no access policies. The value comes from the check the
relational handler applies to the request, so a client that reads `joins: true` is not refused by
the database it names.

<!-- doc-example method=GET path=/v1/databases/chinook status=200 -->
```json
{
  "capabilities": {
    "aggregation": true,
    "dtql": true,
    "joins": true,
    "query": true,
    "read": true,
    "write": true
  },
  "collections": [
    "Customer",
    "Invoice"
  ],
  "endpoints": {
    "dtql": "http://localhost:8080/v1/databases/chinook/dtql"
  },
  "engine": "sqlite",
  "id": "chinook",
  "queryFormat": "dtql-yaml+json",
  "schemaMode": "strict"
}
```

The metadata of a database with access policies is `422 authorization_unsupported`. With auth off
the discovery list says `joins: false` for such a database. With auth on there is no list, and the
`422` of the metadata route is itself the statement that the database takes no relational
document: a relational document that names it is refused with the same code.

#### Requests and answers

On `/v1/databases/{db}/dtql` a source that names no database belongs to `{db}`, and a source that
names another database is `400 invalid_dtql` whose message points to `/v1/dtql`. On `/v1/dtql`
every source names its database, and one that does not is `400 invalid_dtql`.

<!-- doc-example method=POST path=/v1/databases/chinook/dtql status=200 -->
```yaml
from:
  name: Invoice
  alias: i
  joins:
    - type: inner
      from: {name: Customer, alias: c}
      on:
        - {left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}
orderBy:
  - {field: id, source: i}
columns:
  - {field: id, source: i}
  - {field: name, source: c, as: customer_name}
  - {field: total, source: i, as: total}
```
```json
{
  "columns": [
    "id",
    "customer_name",
    "total"
  ],
  "execution": {
    "elapsedMs": 0,
    "route": "database",
    "rowsReturned": 5,
    "sources": [
      {
        "collection": "Invoice",
        "database": "chinook"
      },
      {
        "collection": "Customer",
        "database": "chinook"
      }
    ]
  },
  "records": [
    {
      "data": {
        "customer_name": "Ada",
        "id": "i1",
        "total": 10
      }
    },
    {
      "data": {
        "customer_name": "Ada",
        "id": "i2",
        "total": 20
      }
    },
    {
      "data": {
        "customer_name": "Grace",
        "id": "i3",
        "total": 5
      }
    },
    {
      "data": {
        "customer_name": "Edsger",
        "id": "i4",
        "total": 7
      }
    },
    {
      "data": {
        "customer_name": "Edsger",
        "id": "i5",
        "total": 8
      }
    }
  ]
}
```

The answer is `{"records": [{"data": {...}}], "columns": [...], "execution": {...}}`:

- `records` holds the rows. A relational row carries **no record key**, only `data`.
- `columns` names the columns in the order the document selects them.
- `execution.route` is `database` when one database ran the whole document and `in-memory` when
  the server read each source and joined them. `rowsReturned` is the row count. `sources` lists
  the collections read, with the rows each delivered and its read time on the `in-memory` route
  (the fields are absent where the database ran the document). `elapsedMs` includes the wait for
  a slot of the concurrency gate.

An answer holds at most 1000 rows and 8 MiB; a larger result is refused (`422
query_budget_exceeded`), never cut short. The paging headers of the snapshot protocol are refused
(`422 snapshot_unsupported`): a relational answer is returned whole.

A grouping and an aggregate:

<!-- doc-example method=POST path=/v1/databases/chinook/dtql status=200 -->
```yaml
from:
  name: Invoice
  alias: i
  joins:
    - type: inner
      from: {name: Customer, alias: c}
      on:
        - {left: {field: customer_id, source: i}, op: '==', right: {field: id, source: c}}
groupBy:
  - {field: country, source: c}
orderBy:
  - {field: country, source: c}
columns:
  - {field: country, source: c}
  - {aggregate: {function: sum, args: [{field: total, source: i}]}, as: revenue}
```
```json
{
  "columns": [
    "country",
    "revenue"
  ],
  "execution": {
    "elapsedMs": 0,
    "route": "database",
    "rowsReturned": 3,
    "sources": [
      {
        "collection": "Invoice",
        "database": "chinook"
      },
      {
        "collection": "Customer",
        "database": "chinook"
      }
    ]
  },
  "records": [
    {
      "data": {
        "country": "NL",
        "revenue": 15
      }
    },
    {
      "data": {
        "country": "UK",
        "revenue": 30
      }
    },
    {
      "data": {
        "country": "US",
        "revenue": 5
      }
    }
  ]
}
```

A left join keeps the rows of its left side that have no match:

<!-- doc-example method=POST path=/v1/databases/chinook/dtql status=200 -->
```yaml
from:
  name: Customer
  alias: c
  joins:
    - type: left
      from: {name: Invoice, alias: i}
      on:
        - {left: {field: id, source: c}, op: '==', right: {field: customer_id, source: i}}
groupBy:
  - {field: name, source: c}
orderBy:
  - {field: name, source: c}
columns:
  - {field: name, source: c}
  - {aggregate: {function: count, args: [{field: id, source: i}]}, as: invoices}
```
```json
{
  "columns": [
    "name",
    "invoices"
  ],
  "execution": {
    "elapsedMs": 0,
    "route": "database",
    "rowsReturned": 3,
    "sources": [
      {
        "collection": "Customer",
        "database": "chinook"
      },
      {
        "collection": "Invoice",
        "database": "chinook"
      }
    ]
  },
  "records": [
    {
      "data": {
        "invoices": 2,
        "name": "Ada"
      }
    },
    {
      "data": {
        "invoices": 2,
        "name": "Edsger"
      }
    },
    {
      "data": {
        "invoices": 1,
        "name": "Grace"
      }
    }
  ]
}
```

A subquery. The root of this document names the database of the endpoint it is posted to, which
is read as if it named none (see [Launch rulings](#launch-rulings)):

<!-- doc-example method=POST path=/v1/databases/chinook/dtql status=200 -->
```yaml
from:
  database: chinook
  name: Invoice
  alias: i
where:
  exists:
    query:
      from: {database: chinook, name: Customer, alias: c}
      where:
        op: '=='
        left: {field: id, source: c}
        right: {field: customer_id, source: i}
orderBy:
  - {field: id, source: i}
columns:
  - {field: id, source: i}
```
```json
{
  "columns": [
    "id"
  ],
  "execution": {
    "elapsedMs": 0,
    "route": "in-memory",
    "rowsReturned": 5,
    "sources": [
      {
        "collection": "Invoice",
        "database": "chinook",
        "elapsedMs": 0,
        "rows": 5
      },
      {
        "collection": "Customer",
        "database": "chinook",
        "elapsedMs": 0,
        "rows": 10
      }
    ]
  },
  "records": [
    {
      "data": {
        "id": "i1"
      }
    },
    {
      "data": {
        "id": "i2"
      }
    },
    {
      "data": {
        "id": "i3"
      }
    },
    {
      "data": {
        "id": "i4"
      }
    },
    {
      "data": {
        "id": "i5"
      }
    }
  ]
}
```

A derived source on the edge of a join is read again for every row on its left, and every read
counts against the source budget (`maxSourceRows`, `maxSourceBytes`). In this document five
invoices meet a derived source over three customers, so the customers are read fifteen times:

<!-- doc-example method=POST path=/v1/databases/chinook/dtql status=200 -->
```yaml
from:
  name: Invoice
  alias: i
  joins:
    - type: inner
      from:
        query:
          as: d
          from: {name: Customer}
          columns:
            - {field: id}
            - {field: name}
      on:
        - {left: {field: customer_id, source: i}, op: '==', right: {field: id, source: d}}
orderBy:
  - {field: id, source: i}
columns:
  - {field: id, source: i}
  - {field: name, source: d}
```
```json
{
  "columns": [
    "id",
    "name"
  ],
  "execution": {
    "elapsedMs": 0,
    "route": "in-memory",
    "rowsReturned": 5,
    "sources": [
      {
        "collection": "Invoice",
        "database": "chinook",
        "elapsedMs": 0,
        "rows": 5
      },
      {
        "collection": "Customer",
        "database": "chinook",
        "elapsedMs": 0,
        "rows": 15
      }
    ]
  },
  "records": [
    {
      "data": {
        "id": "i1",
        "name": "Ada"
      }
    },
    {
      "data": {
        "id": "i2",
        "name": "Ada"
      }
    },
    {
      "data": {
        "id": "i3",
        "name": "Grace"
      }
    },
    {
      "data": {
        "id": "i4",
        "name": "Edsger"
      }
    },
    {
      "data": {
        "id": "i5",
        "name": "Edsger"
      }
    }
  ]
}
```

A correlated subquery is likewise evaluated for every row of the outer query, and its reads count against the same budget.

A document that reads several databases goes to `/v1/dtql`, with every source naming its database:

<!-- doc-example method=POST path=/v1/dtql status=200 -->
```yaml
from:
  database: chinook
  name: Customer
  alias: c
  joins:
    - type: inner
      from: {database: countries, name: Country, alias: k}
      on:
        - {left: {field: country, source: c}, op: '==', right: {field: code, source: k}}
orderBy:
  - {field: name, source: c}
columns:
  - {field: name, source: c, as: customer}
  - {field: name, source: k, as: country}
  - {field: region, source: k}
```
```json
{
  "columns": [
    "customer",
    "country",
    "region"
  ],
  "execution": {
    "elapsedMs": 0,
    "route": "in-memory",
    "rowsReturned": 3,
    "sources": [
      {
        "collection": "Customer",
        "database": "chinook",
        "elapsedMs": 0,
        "rows": 3
      },
      {
        "collection": "Country",
        "database": "countries",
        "elapsedMs": 0,
        "rows": 3
      }
    ]
  },
  "records": [
    {
      "data": {
        "country": "United Kingdom",
        "customer": "Ada",
        "region": "Europe"
      }
    },
    {
      "data": {
        "country": "Netherlands",
        "customer": "Edsger",
        "region": "Europe"
      }
    },
    {
      "data": {
        "country": "United States",
        "customer": "Grace",
        "region": "Americas"
      }
    }
  ]
}
```

The databases are the ones mounted on that server: the server hands the document to DALgo, which
joins the sources of the mounted databases.

**Consistency.** A document that one database runs (`route: database`) is one read transaction of
that database. A document read in memory reads each source on its own, so the sources need not
reflect the same moment: a write that lands between two reads can show in one source and not in
the other. There is no transaction across databases, and a source's own external writes during its
one read are not excluded (the same statement the [snapshot paging](#dtql) contract makes).

#### Launch rulings

Three rulings shape what a relational document does at launch.

1. A relational document that names a database with access policies is refused with `422
   authorization_unsupported` before any name is looked up, whichever collections it names. Joins,
   grouping, aliases and subqueries over such a database come after launch; a document of one plain
   collection is still read through the policy.
2. On `/v1/databases/{db}/dtql` a document whose only relational feature is a subquery is
   relational: it is answered with `columns` and `execution` and with no record keys.
3. On `/v1/databases/{db}/dtql` a document of one source whose root names `{db}` itself is read as
   if it named none. The ruling holds for a `database` key written plainly: a key written through a
   YAML alias or a merge key is not recognised, and the document is answered as a relational
   one.

<!-- doc-example method=POST path=/v1/databases/crm/dtql status=422 -->
```yaml
from:
  name: orders
  alias: o
  joins:
    - from: {name: customers, alias: c}
      on:
        - {left: {field: customer_id, source: o}, op: '==', right: {field: id, source: c}}
columns:
  - {field: id, source: o}
```
```json
{
  "error": {
    "code": "authorization_unsupported",
    "message": "database \"crm\" has access policies and is read one source at a time: a relational document (a join, a grouping, an alias, a subquery or a source that names its database) is not run on it"
  }
}
```

#### Launch limits

- A relational document holds the strict field-name rule on every engine: a field name with a
  space, or any name outside the plain-name rule, is `400 invalid_dtql` on a relational document
  even on SQLite, where a document of one plain collection accepts it.
- A relational answer is not passed through the schema coercion of the single-collection path: a
  declared boolean of a SQLite mount is `0` or `1` in a relational row and `true` or `false` in a
  single-collection record.
- The profile has inner and left joins only, at most 8 sources and a subquery nesting of at most 4;
  `limit` is at most 1000 and `offset` at most 10000 on the outermost query (`maxSources`,
  `maxSubqueryDepth`, `maxLimit` and `maxOffset` of `limits`). Other shapes are
  `400 invalid_dtql` with the reason of the classifier.
- The server joins only the databases mounted on it: `externalSources` is false. Window functions
  are not supported. A condition that the storage engine cannot run is `422 query_unsupported`.
- Only the engines in `joinEngines` take part. A GitHub-backed inGitDB mount never does.
- The profile has five aggregate functions (`count`, `sum`, `avg`, `min`, `max`), in any letter
  case. `first` and `last` are not in the profile: a document that uses either, in any position, is
  `400 invalid_dtql` before anything is read, and the message names the function.
- On the in-memory route a join holds at most 10,000 rows and 16 MiB, and a grouping at most
  100,000 groups (`maxInMemoryJoinRows`, `maxInMemoryJoinBytes` and `maxGroups` of `limits`). Beyond that the answer is `422 query_budget_exceeded`, and `error.budget` names
  the bound.

#### Statuses

| Status | `error.code` | Meaning |
| --- | --- | --- |
| `400` | `invalid_key` | A collection name of a source outside the name rule (a control character, a relative path component such as `..`). |
| `400` | `invalid_dtql` | Not a DTQL document, or outside the profile; a source without a database on `/v1/dtql`; a source of another database on the per-database endpoint; a field name outside the strict rule; a column the database does not have; a shape DALgo cannot join. The message holds the reason, clipped. |
| `400` / `414` | `bad_request` | A malformed body or GET form; a GET URI over 8 KiB is `414`. |
| `403` | `forbidden` | With auth on, the token does not grant `records:read` on a collection of a database the document names. |
| `404` | `not_found` | A database that is not mounted, or a collection of a database on an engine that builds SQL that the database does not declare. |
| `422` | `authorization_unsupported` | The document names a database with access policies, or the adapter of a database without them could not compile the document (the answer is the one the adapter gives, with no further detail). |
| `422` | `join_engine_unsupported` | The engine of a database is not in `joinEngines`. |
| `422` | `snapshot_unsupported` | A paging header was sent. |
| `422` | `query_budget_exceeded` | A bound of the request was reached. `error.budget` names it (`name`, `limit`, `route`, and `path` where it applies) and `error.hint` says what to change. |
| `422` | `query_unsupported` | The storage engine cannot run a condition of the document. |
| `501` | `query_unsupported` | The storage engine is not cleared for structured queries (`postgres`, `mysql`, an unknown engine), whatever `joinEngines` says. |
| `503` | `query_capacity` | No slot of the concurrency gate freed within the server's queue wait; `Retry-After: 1`. |
| `504` | `query_timeout` | The query ran longer than `timeoutMs`. |
| `500` | `internal` | A fault of the server, logged and not described. |

<!-- doc-example method=POST path=/v1/dtql status=400 -->
```yaml
from:
  name: Customer
```
```json
{
  "error": {
    "code": "invalid_dtql",
    "message": "this endpoint reads several databases, so every source names its database: collection \"Customer\" does not"
  }
}
```

<!-- doc-example method=POST path=/v1/dtql status=400 -->
```yaml
from:
  database: chinook
  name: ".."
```
```json
{
  "error": {
    "code": "invalid_key",
    "message": "invalid or unsupported DTQL query: relational profile: collection-name at from: invalid key: segment \"..\" contains a relative path component"
  }
}
```

<!-- doc-example method=POST path=/v1/dtql status=422 -->
```yaml
from:
  database: events
  name: Event
```
```json
{
  "error": {
    "code": "join_engine_unsupported",
    "message": "the \"firestore\" storage engine of database \"events\" is not enabled for joins and aggregation"
  }
}
```

<!-- doc-example method=POST path=/v1/dtql status=422 headers=OVDB-Page-Size=10 -->
```yaml
from:
  database: chinook
  name: Customer
  alias: c
```
```json
{
  "error": {
    "code": "snapshot_unsupported",
    "message": "a joined result is returned whole: the paging headers are not supported on a relational query"
  }
}
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

Auth (server binds 127.0.0.1 by default), cursors/offset, projections, group-by
on `/query` (grouping and joins are in the relational DTQL documents above),
collection-group queries, update preconditions, server-side transactions with
read-your-writes across HTTP round-trips (driver buffers writes client-side instead),
optimistic concurrency.
