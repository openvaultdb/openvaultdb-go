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
It maps the authenticated demo actor to the `reader` role in trusted server
configuration. The bearer token still cannot bypass data policies.

From this OpenVaultDB worktree:

```sh
export GOWORK=/tmp/layered-acl-query-local.work
export OVDB_OWNER_TOKEN="$(openssl rand -hex 24)"
go run ./examples/layered-acl -dir /tmp/my-fresh-acl-demo -listen 127.0.0.1:8899
```

In another terminal with the same token:

```sh
curl -sS http://127.0.0.1:8899/v1/databases/ingitdb/dtql \
  -H "Authorization: Bearer $OVDB_OWNER_TOKEN" \
  --data-binary 'from: {name: customers}
orderBy: [{field: name}]
limit: 1'
```

InGitDB returns `customers/03` with `name: Customer 03`: its tenant-A policy and
OpenVaultDB's Ireland policy both apply. Replace `ingitdb` in the URL with
`sqlite`; that database returns `customers/01`, because its OpenVaultDB policy
requires Ireland and it has no additional tenant policy. Neither returns
`tenant`, `country`, or `secret`. Adding `columns: [{field: secret}]` returns 403.

The example leaves its files for inspection. Restart with the same `-dir`, `-reuse`, and token environment to remount the
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

The first delivery explicitly rejects policy masks and execution-class gates
pending their next work package. It does not silently ignore them. Native SQL,
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
