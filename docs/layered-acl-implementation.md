# Layered ACL query implementation

Implementation was approved on 2026-09-08. The first delivery is real DTQL
queries over HTTP through OpenVaultDB to local InGitDB and SQLite databases,
with database-owned policies enforced using DALgo. Policy viewer and editor
are excluded from MVP.

Work is coordinated locally on `layered-acl-query` worktrees in DALgo,
dalgo2ingitdb, and openvaultdb-go. Local dependency wiring is used during
integration; publication and remote merges are deferred.

## First delivery acceptance

- Mount each engine from a manifest and query it through the real HTTP handler.
- Load persisted policies at their owner; invalid enabled configurations fail closed.
- Apply table, row, and column permissions using the existing DALgo evaluator.
- Apply row predicates before pagination and reject hidden-column query probes.
- Preserve InGitDB enforcement when accessed directly or through OpenVaultDB.
- Intersect OpenVaultDB restrictions with lower-layer restrictions.
- Authenticate callers using existing server authentication; owner tokens do not bypass data ACL.
- Verify persistence by remounting and querying again.
- Return safe authorization errors without exposing private policy contents.

The approved architecture and reviewed contracts are in the DTQL repository's
`design/layered-acl` packet on the local `layered-acl-design` branch. Subsequent
write and Explain Access work continues from those contracts; this first query
delivery does not imply that those later work packages are implemented.

## Progress

- Local worktrees created and existing enforcement/query paths audited.
- Portable policy loading, owner enforcement, and server integration implemented.
- Real HTTP read/probe/remount tests pass for InGitDB and SQLite.
- Explicit projection retains row identity; SQL translation and pagination regressions pass.
- Standalone authenticated HTTP demo passed before and after remount.

## Run the local demonstration

The example starts an authenticated server over both real engines, seeds three
customers, and activates policies after seeding. It requires a fresh directory.
It binds the query credential to a stable typed demo user and DataTug actor,
then resolves the user’s `reader` role in trusted server configuration.

From this OpenVaultDB worktree:

```sh
export GOWORK=/tmp/layered-acl-query-local.work
export OVDB_OWNER_TOKEN="$(openssl rand -hex 24)"
export OVDB_QUERY_TOKEN="$(openssl rand -hex 24)"
go run ./examples/layered-acl -dir /tmp/my-fresh-acl-demo -listen 127.0.0.1:8899
```

In another terminal with the same query token:

```sh
curl -sS http://127.0.0.1:8899/v1/databases/ingitdb/dtql \
  -H "Authorization: Bearer $OVDB_QUERY_TOKEN" \
  --data-binary 'from: {name: customers}
orderBy: [{field: name}]
limit: 1'
```

InGitDB returns `customers/03` with `name: Customer 03`: its tenant-A policy and
OpenVaultDB's Ireland policy both apply. Replace `ingitdb` in the URL with
`sqlite`; that database returns `customers/01`, because its OpenVaultDB policy
requires Ireland and it has no additional tenant policy. Neither returns
`tenant`, `country`, or `secret`. Adding `columns: [{field: secret}]` returns 403.

The example leaves its files for inspection. Restart with the same `-dir`, `-reuse`, and both token environment variables to remount the
generated manifests without reseeding. The normal OVDB mount API also reuses them. Policies are immutable snapshots
until remount; this delivery does not provide live reload or policy editing.

## Configure policies on an existing mount

Add to an OpenVaultDB database manifest:

```yaml
acl:
  enabled: true
  policies: [policies/customers.yaml]
```

Files resolve beneath the manifest directory. Their `target.database` must equal
`database.id`. InGitDB additionally loads its own `.ingitdb/access/manifest.yaml`:

```yaml
enabled: true
database: crm
policies: [customers.yaml]
```

Those files resolve beneath `.ingitdb/access`, independently of OpenVaultDB.
The policy syntax is demonstrated by `examples/layered-acl/main.go`. Supported
policies use `dtql.org/access/v1`, `dalgo-hierarchical-v1`, default deny, path
scopes, row conditions, field allow-lists, and optional user/role/group bindings.
Visibility defaults to public; private is accepted and retained internally.
All HTTP ACL failures use generic diagnostics in this slice.

No ACL configuration preserves legacy behavior. A present configuration requires
an explicit enabled flag. An enabled configuration with missing or invalid
policies fails mounting. An existing InGitDB access directory without its manifest
also fails mounting. Administrators who control the filesystem can change the
configuration; ACL does not constrain the operating-system owner.

`server.WithPrincipalResolver` connects existing authenticated actors to trusted
internal IDs and current roles/groups. It runs on authenticated requests and
fails closed if resolution fails. Without a resolver, only universally bound
policies apply; an application token ID is not automatically a human policy ID.
The token capability check and the data policies must both allow the request.

The file loader supports scoped collection/field masks and execution-class gates. Native SQL,
GraphQL, stored procedure execution, policy management, full structured blocker
collection, Explain Access, policy generations, and write-specific E2E acceptance
remain later work. Policy viewer/editor is excluded from MVP by user direction.

## Local dependency wiring

The task uses an external Go workspace instead of changing published module
versions. WB local dependency propagation requires a publishing stream; this
local-only task uses an explicit `GOWORK` file to honor the requested local
integration workflow. No stream branches or draft PRs are needed.

The workspace contains these `layered-acl-query` worktrees:

- `dal-go/dalgo`
- `dal-go/dalgo2sql`
- `ingitdb/dalgo2ingitdb`
- `openvaultdb/openvaultdb-go`

The current workspace is `/tmp/layered-acl-query-local.work`. For another checkout,
create an external workspace with `go work init` and `go work use` for those four
module directories, then set `GOWORK` to its absolute path. Do not run `go mod tidy`
or `go get` against the linked workspace. A provider-first release/version update
is still required before ordinary CI or an unlinked checkout can build this slice.


## Verification record — 2026-09-08

All four repositories passed their full Go test suites using the linked workspace.
Focused static analysis passed for the changed server packages and DALgo access/DTQL;
InGitDB and the SQL driver passed repository-wide vet. The OpenVaultDB suite includes
`TestLayeredACL_DTQL` against real InGitDB and SQLite mounts, authentication,
restricted projection/filter/order, row filtering before pagination, owner denial,
collection-scoped token enforcement, and remounting.

A separately launched `examples/layered-acl` server was exercised over loopback HTTP,
then restarted with `-reuse` and exercised again:

| Request | Expected and observed |
| --- | --- |
| InGitDB, order by name, limit 1 | `customers/03` |
| SQLite, order by name, limit 1 | `customers/01` |
| Either engine, explicit `name` projection | Correct row key and only `name` data |
| SQLite, offset 1, limit 1 | `customers/03` |
| Either engine, explicit `secret` projection | HTTP 403 `ACCESS_DENIED` |

Temporary smoke servers were stopped after verification. No publication, CI run,
remote merge, or deployment was performed. The Go modules still require the local
workspace until the coordinated dependency release is made. The DALgo repository's
existing 100% pre-push coverage gate was preserved when installing WB hooks; that
publication gate was not run in this local implementation phase.

The SQLite compiler is explicitly selected on OpenVaultDB SQLite mounts. Other
SQL adapters retain their existing compiler until separately integrated and tested.
Enabling this file-ACL profile on unsupported engines or the GitHub InGitDB adapter
fails mounting. The query deadline is cooperative: the engine must honor context
cancellation; it is not a hard process-level CPU or memory sandbox.

## SpecScore implementation tracking

The coordinating plan is DALgo's
`spec/plans/layered-acl-mvp.md` on the local `layered-acl-query` branch.
It contains 24 tasks with dependencies, acceptance criteria and validation.
Use SpecScore task transitions in that worktree; completion records local
implementation commits and evidence. Publication is a separate final task.

## Supported protected query profile

| Capability | OpenVaultDB DTQL endpoint | SQLite adapter | InGitDB adapter |
| --- | --- | --- | --- |
| Source | One unaliased root collection | One unparented CollectionRef | One root collection in this profile |
| Projection | Named stored fields | Field or bound scalar constant; quoted aliases at adapter level | Stored fields; computed values unavailable in protected reads |
| Predicate | Structured comparisons, IN, AND/OR | Bound values; field operands; scalar IN members | In-memory structured evaluation |
| Ordering | Named fields | Quoted field identifiers | In-memory field ordering |
| Pagination | Default/max limit 1000; offset max 10000 | Bound LIMIT/OFFSET after WHERE | Policy filtering before ordering/pagination |
| Unsupported endpoint features | Joins, aliases, grouping, functions, cursors, native query text | Unsupported structured sources/expressions fail before execution | Broader legacy adapter features do not expand the HTTP profile |
| Cancellation | Context deadline 10 seconds | database/sql context propagation | Cooperative checks between read/evaluation phases |

All caller projection, filter and ordering fields must be authorized, including
fields omitted from output. Trusted policy predicates may refer to protected
fields. SQLite identifiers use backticks so an unknown field cannot silently
become a string literal under SQLite's double-quoted-string compatibility mode.
Malformed non-scalar bound values fail validation before driver execution.

The response buffer is capped at 8 MiB. Neither pagination nor the deadline is a
hard bound on underlying scan memory or filesystem latency: InGitDB loads stored
records before filtering, and individual file reads, YAML decoding and sorting
are cooperative cancellation boundaries. These limits must not be advertised
as a hard process resource sandbox.

For this profile, computed-field dependency authorization is deferred.
Protected InGitDB reads expose stored values only, including when protection is
added at OpenVaultDB. Formula values must never derive visible output from
hidden input fields. Legacy unprotected mounts retain formula evaluation.


## Scoped masks

Portable policies may now add a mandatory collection gate:

```yaml
collectionMask:
  stages:
    - include: ["*"]
    - exclude: ["sys_*"]
```

An allow rule may use `fieldMask` instead of `fields`, with the same ordered
stage syntax and dotted field patterns. Adjacent stages of the same action
merge; a later include restores only names selected by all preceding stages.
Independent policies and owners continue to intersect.

For nested objects, wildcard reads redact excluded leaves and retain the
structural parents of restored leaves. Explicit whole-object queries require
complete subtree coverage. Parent replacement checks removed and retained
descendants in both images; changing a permitted leaf does not require granting
its structural parent. Arrays remain opaque and require complete coverage.
Masked truncate is unsupported. Legacy `fields` matching is unchanged.

Use the DTQL parse/normalize/serialize APIs for portable editing. Legacy DALgo
serialization refuses policies containing masks rather than dropping them.
Formatting and comments are not preserved by canonical serialization.


## Execution surfaces

`execution: {allow: [{class: dtql}]}` permits the typed DTQL surface and
excludes native SQL, native GraphQL and procedure targets at this policy gate.
An explicitly empty allow array denies every surface; omission adds no gate.
Database action, table, row, column and lower-owner restrictions still apply.

Secure sessions classify structured queries as DTQL before adapter compilation.
Opaque query text cannot be relabeled DTQL. Pure policy assessment can supply a
typed `ExecutionTarget` for a namespaced procedure and evaluate scoped masks
such as `User_*` or include `*` / exclude `sys_*`. This does not authorize an
execution path: native/procedure effects remain unsupported by the protected
HTTP profile. The transport rejects unrecognized execution labels as unknown
DTQL fields.

## Typed identity and current memberships

Set `acl.realm` at each owner (and `realm` in the InGitDB owner manifest).
Policies keep stable binding IDs in that realm; the realm is owner configuration,
not an OAuth provider subject or a new policy-text attribute.

Owner token administration accepts `subject` and `actor` references containing
`realm`, `kind`, and `id`. Both must be supplied together. Grants persist those
references alongside the existing hashed credential, database scope and
capabilities. Legacy client-only grants remain application identities.
The store returns defensive grant copies so resolver code cannot mutate
persisted delegation accidentally.

Configure `server.WithGrantIdentity` with a bootstrap reference from persistent
host configuration and a current `MembershipResolver`. The callback runs on
every authenticated request and returns current role/group IDs and a membership
revision. Resolution failure denies access. Subject policy and actor token
capabilities must both allow the operation; owner administrative capabilities
do not bypass data ACL. A service cannot match a human binding with the same ID.

Hosted deployments retain the established Sneat/Firebase UID and existing
provider-binding infrastructure. Several provider credentials may resolve to
the same stable subject; no provider tokens or linking secrets belong in
policy files. This task provides the trusted mapping contract and credential
tests, not a new OAuth issuance/linking service. DataTug’s actual OVDB connection
transport remains task 21; it must use the provisioned credential and cannot
supply trusted roles or substitute an arbitrary subject.

## Filesystem owner generations

OpenVaultDB supports an opt-in generation store:

```yaml
acl:
  enabled: true
  realm: local
acl_store:
  path: .ovdb/access
```

The path is relative to the mount manifest. Do not combine it with flat
`acl.policies`. An enabled generation mount requires a valid active pointer;
an absent or corrupt generation fails closed.

Trusted embedded administration uses `policystore.Open` and `Store.Activate`
to bootstrap a complete policy set. Mounted owners expose `PublishPolicies`
and `ReloadPolicies`; these methods have no public data-route authority.
Publishing compares the expected internal generation revision, validates the
complete candidate, synchronizes immutable files, then atomically replaces
the active pointer. No-op content reuses a verified generation. Errors after
pointer publication are uncertain publication; the controller reloads the
authoritative pointer before allowing further admissions, or fails closed.

The internal revision covers the entire owner/configuration set. Individual
document revisions cover only canonical policy bytes. Changing a private
document does not change an unchanged public document's revision. Ordinary
clients must never receive the internal generation revision.

Each operation resolves one immutable policy snapshot; a DALgo transaction
pins one snapshot. Explicit reload updates existing mounted handles. External
filesystem administration requires reload/reconnect; it does not implicitly
change a running controller. Protected writes acquire the storage transaction before immutable policy leases,
and retain every lease until storage commit/rollback finishes. Owner publication
cannot activate a new policy during that boundary.

Validation includes process termination before/after pointer publication,
CAS conflicts, corrupt and missing references, duplicate/symlink pointers,
unchanged public document revisions, and a real HTTP query that changes from
allowed to denied on publication and remains denied after remount. Retention
of inactive generations is operator follow-up.

## Authorization transport checkpoint

`POST /v1/databases/{db}/access/evaluate` accepts the frozen C2 request JSON.
It supports metadata-only `plan`, explicit-key `inspect`, and bounded `sample`
through capable local SQLite/InGitDB mounts. The request normalizer is shared
with protected write ingress. It rejects
duplicate/case-aliased/unknown JSON keys, conflicting path identifiers,
overlapping updates, unsupported parameter binding, and request limits before
evaluation. It never reads a row to construct a plan.

DALgo implements the common wire contract in `dtql/authorization` (the
cycle-safe equivalent of the design's proposed `access/contract` location).
`access.AssessPlan` shares collection/evaluation machinery with enforcement.
The HTTP projection retains owner conjunctions and portable source predicates
where authorized, without exposing resolved principal variables. Private
policies are coalesced into owner summaries; document revisions are visible
only with authorized references. Bounded overflow returns incomplete,
non-authorizing diagnostics. Reconstructed real-query failure diagnostics
explicitly remain partial until the admission coordinator can retain the
original pinned execution assessment.

`WithOwnerAuthorization` binds each owner's diagnostic authority independently.
The default bootstrap/token authority applies to OpenVaultDB only. Lower-owner
administration and key-scoped protected inspection require explicit trusted
owner bindings. Policy administration does not imply permission to inspect
unreadable rows. `WithAccessInstanceID` supplies a persistent deployment ID.

`GET /access/layers` and `GET /access/policies?layerId=...` expose authorized
metadata only. No policy viewer/editor or HTTP policy mutations are enabled.
Custom policies without publishable document metadata retain their mandatory
owner representation. No host file paths, private generation revisions, or
raw evaluator exceptions are part of these responses.

Verification: strict request validation, real InGitDB/SQLite plan and query
tests, public-reference/private-policy separation, hidden query-field denial,
and the existing identity/reload/server suites. The protected HTTP test also proves read/inspect/UPDATE agreement, whole-record
CAS, no dry-run mutation, key-safe sampling and multiple owner blockers.


## Protected HTTP operations

`POST /v1/databases/{db}/access/evidence` takes
`{apiVersion,resource,requiredFields}`. It requires ordinary data read authority
for every requested field at every owner. Returned fields distinguish present
(including null) from absent; withheld fields never masquerade as absent. A
successful response contains an opaque whole-record `dataRevision`, with no
raw hidden image. Policy-admin authority grants no evidence bypass.

`PATCH /v1/databases/{db}/records/{table}/{id}` with content type
`application/vnd.dtql.operation+json` accepts a normalized C2 operation:

```json
{
  "id": "u1",
  "action": "update",
  "resource": {"databaseId": "crm", "path": "/customers/01"},
  "executionClass": "dtql",
  "mutation": {
    "changes": [{"op": "set", "path": ["name"], "value": "Updated"}],
    "ifDataRevision": "<revision from evidence>"
  }
}
```

The URL and operation must name the same record. Candidate schema validation,
row/column checks and revision preconditions use one private storage boundary.
Success is `{authorization,dataRevision}` after commit. A readable stale
revision returns 409 `data_revision_conflict`; retry requires fresh evidence.
A write-only actor may execute an authorized normalized operation, but cannot
obtain row evidence or hidden denial facts. DataTug's selected-row editor
always obtains fresh evidence and supplies the revision.

SQLite uses `BEGIN IMMEDIATE`; InGitDB uses its repository lock and rollback
journal. Whole-image revisions are HMACs with a random per-mount secret, so
hidden low-entropy values cannot be guessed from a public digest. Reconnection
invalidates old revisions conservatively. DALgo supports bounded atomic point
operation batches internally; this HTTP MVP advertises only one-row normalized
UPDATE. Legacy dynamic write/batch ingress returns 422 on the protected profile.
No speculative write is used to implement a dry run.

Private images never leave the coordinator. Missing, denied and non-disclosable
point diagnostics use a generic redacted denial with explicitly unavailable
row evidence. Owner-authorized diagnostics can retain each independently
blocking policy reference and exact changed column paths. Actual token data
capabilities remain required in addition to all owner ACL policies.

Sampling applies the target's and actual requester's read restrictions before
pagination, then rechecks visibility inside inspection. Any changed visibility
aborts row diagnostics rather than exposing a revoked key. Ordering ends in
SQLite's configured `id` primary key or InGitDB's `$id` key expression (not an
ordinary stored `id` field). The read policies must authorize the ordering
reference. N is at most 100, offset is unsupported, and a completed all-allow or
empty sample remains conditional with `allowed:false` and `exhaustive:false`.

## Supported-profile limits

Protected SQLite point operations require a real table with a single TEXT
primary key and supported scalar columns. Generated/default columns, triggers,
foreign keys, CHECK clauses and explicit collations are conservatively
unsupported. Ambiguous key collations must not bypass unique-target checks.
Row-dependent malformed data produces incomplete safe assessment, never an
empty authoritative image. InGitDB rejects unsupported computed/transform/FK
profiles before protected execution. Provision schema before activating ACL;
filtered schema discovery is currently 422 instead of revealing unfiltered
names. Native SQL/GraphQL/procedure execution, policy UI/HTTP CRUD, external
ACL services and long-running logical transactions are excluded.

The local example now provisions customer read/write credentials and policies
for both engines. Secrets remain environment variables. Reusing fixtures does
not silently expand an existing grant: create a fresh fixture for the write
example, or update its credential through authorized owner administration.
