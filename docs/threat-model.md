# OpenVaultDB MVP Threat Model

Status: MVP (2026-07-09). This is a lightweight threat model for the current local-dev server.
Hosting is explicitly out of scope; production hardening is roadmap.

---

## Network exposure

OpenVaultDB binds to `127.0.0.1:6832` by default. Only processes on the local machine can
reach it. Override with `ovdb serve --addr <host:port>`; binding to `0.0.0.0` or a public
interface exposes the API to the network with **no authentication** — do not do this on a
machine reachable from untrusted networks.

There is no TLS in MVP. Binding to a non-loopback address transmits data in plaintext.

## Authentication and authorisation

There is **no auth**. Any local process that can reach the bound address can read and write all
databases. This is intentional for local-dev use. OpenVaultDB is not production-hosting ready.

Future: bearer token auth per-server and per-database ACLs (see roadmap). Until then, rely on
OS-level process isolation and the loopback default.

## Path traversal

The server validates all key path segments and database IDs before touching the filesystem or
engine.

- **Database IDs** must match `^[a-zA-Z0-9][a-zA-Z0-9_-]*$` (enforced in manifest validation at
  load time). IDs used in URL routing are checked against this same constraint; an unknown ID
  returns 404, never a filesystem probe.
- **Key segments** (collection names and record IDs) are percent-decoded individually, then
  checked: empty string, `.`, and `..` are all rejected with HTTP 400. Absolute path components
  cannot appear because segments are not joined as filesystem paths — they are passed to the
  DALgo driver as typed key components.
- **Storage paths** in manifests are resolved relative to the manifest file's directory;
  absolute paths in the manifest are accepted. The server does not sanitise manifest paths
  (manifests are operator-controlled, not user-supplied).

No known path traversal vectors remain, but the mitigations above assume the DALgo drivers
themselves do not perform unsafe path construction from key components. `dalgo2ingitdb` stores
records at `<collection>/<id>.yaml` derived from the key, using the ID directly as a filename.
IDs that contain `/` after percent-decoding would be rejected by the key parser before reaching
the driver.

## Accidental data exposure

**Binding to 0.0.0.0 / a public interface.** If `--addr` is overridden to a routable address,
all data in all mounted databases is readable and writable with no credentials. The manifest
lists every database. The inferred-schema endpoint (`GET /v1/databases/{db}/inferred-schema`)
additionally leaks observed field names and types from all partial/schemaless writes.

**Inferred schema endpoint.** For partial and schemaless databases the inferred catalogue
records field names, types, first/last seen timestamps, and sample counts derived from written
records. If the server is exposed to untrusted clients this endpoint reveals the shape of the
data even without access to the records themselves.

**Git push publishing data.** The inGitDB engine can be configured to push commits to a git
remote (`storage.ingitdb.push: sync|async`). If that remote is a public repository, all record
data (stored as plaintext YAML files) becomes publicly readable. Push is opt-in per manifest and
defaults to `none` — this is a user decision, not something OpenVaultDB does automatically.
Before enabling push, ensure the remote is private or the data is suitable for public exposure.

## Secrets in manifests

Manifest files have no credential fields by design. The `storage.ingitdb` block accepts
`push`, `remote`, and `branch` — none of which take tokens or passwords. Git remotes use
ambient credentials (SSH keys, git credential helpers, `~/.netrc`, etc.). **Do not embed
authentication tokens in git remote URLs** (e.g. `https://token@github.com/...`) — that would
store the token on disk in plaintext as part of the manifest.

## inGitDB-specific risks

- **Plaintext records on disk.** inGitDB stores each record as a YAML file in a directory tree.
  There is no encryption at rest. Anyone with read access to the data directory can read all
  records. Protect the directory with OS-level permissions if the data is sensitive.
- **Full git history.** Because each write batch is a git commit, deleted records remain
  recoverable from git history (`git log`, `git show`, `git fsck`). Deleting a record via the
  API removes the file but does not rewrite history. Treat the git repo as containing a
  permanent log of all writes.
- **Non-atomicity of batches on mid-batch IO failure.** The server runs a pre-flight validation
  pass over the full batch before writing (checking insert conflicts, update targets, schema
  constraints) and then applies all ops inside one `RunReadwriteTransaction`. For inGitDB this
  maps to one git commit; if an IO error occurs after some files are written but before the
  commit completes, the partial state may be present on disk as uncommitted changes.
  This window is narrow (errors here are OS-level IO failures, not logic errors — pre-flight
  catches those), but it is not zero. The integrity implication: a partial write will show up as
  dirty working tree in the git repo. Recovery: inspect `git status`, resolve the partial state
  manually, and restart the server. This is a known MVP limitation.

## What is not production-ready

| Capability             | MVP status                                      |
|------------------------|-------------------------------------------------|
| TLS                    | Not implemented                                 |
| Authentication         | Not implemented                                 |
| Per-database ACLs      | Not implemented                                 |
| Rate limiting          | Not implemented                                 |
| Audit log              | Not implemented                                 |
| Multi-tenant isolation | Not implemented                                 |
| Hosting / public API   | Out of scope (local-dev only)                   |

## Future auth needs

When OpenVaultDB is extended to support remote hosting or multi-user scenarios, the following
will be needed:

- **Bearer token auth** on all HTTP endpoints (per-server or per-database).
- **Per-database ACLs**: read-only vs read-write grants, scoped to specific collections.
- **TLS** (terminate at the server or at a reverse proxy).
- **Multi-tenant isolation**: database IDs must be scoped to a tenant; cross-tenant key path
  access must be blocked at the routing layer, not relied on by engine-level path validation.
- **Rate limiting** per client or per database to prevent local DoS.
- **Audit log**: structured log of all writes, accessible for compliance.


## Token Admin + Revocation (2026-07-09 update)

`POST/GET/DELETE /v1/tokens` — owner-token-authorized admin API for creating
and revoking scoped application tokens without the consent flow. Revocation
takes effect immediately in memory and is persisted to the auth-store file
(`--auth-store`, mode 0600). Revoked grants are kept in the file with a
`revokedAt` timestamp for the audit trail — they are never used for
authentication. Token hashes are the only credential in the file; the raw
token secret appears only in the `POST /v1/tokens` response and is never
stored. Non-owner callers (app tokens or unauthenticated) get 403/401.

Server-level grants (empty `databaseId`) exist for the `databases:create`
capability (runtime provisioning via `POST /v1/databases` under
`--data-dir`). A server-level grant with records capabilities grants data
access to ALL databases — minting one is an explicit owner decision; the
recommended pattern is a provisioning token carrying `databases:create` only,
with each app receiving a token scoped to just its own created database
(minted automatically on create).

## Auth MVP (2026-07-09 update)

`ovdb serve --auth` adds the first authentication layer (still NOT
production-hosting ready — no TLS, no rate limits, no audit log):

- Owner bearer token (env/flag/generated) for full and admin access;
  compared in constant time; never persisted.
- App tokens via consent → one-time code (5 min) → opaque bearer token (1 h).
  Grants persist with SHA-256 token hashes only (`--auth-store`, mode 0600),
  so the grants file never contains usable credentials.
- Capabilities follow the spec taxonomy with optional collection scoping;
  collection-scoped grants cannot use `/dtql` (target collection unknowable
  before parsing) and cannot enumerate databases; `/v1/status` redacts the
  database list for non-owners.
- Remaining gaps for hosted use: no user identity (OIDC/passkeys per spec),
  no refresh/rotation, consent page has no CSRF protection (dev-only flow),
  tokens transit in clear without TLS termination in front.


## PostgreSQL engine (2026-07-09)

The Postgres DSN carries credentials and is read from the environment variable
named by `storage.postgres.dsn_env` (default `OVDB_POSTGRES_DSN`) — never from
the manifest. Use `sslmode=require` (or stronger) for non-local servers; the
DSN, and thus the password, is visible to anything that can read the ovdb
process environment. ovdb opens exactly one connection pool per mounted
Postgres database and issues only parameterized statements (identifiers are
validated against a safe pattern before quoting), so record data cannot inject
SQL.


## CORS (2026-07-09)

`ovdb serve --cors <origin>` injects CORS headers for browser clients.

**`--cors '*'` (wildcard) is a foot-gun.** With wildcard CORS and auth disabled
(`--auth` off, the default) on a non-loopback address, any browser tab on the
internet can read and write all mounted databases without any credential.  The
server prints a startup warning when it detects this combination, but does not
refuse to start — operator intent is respected.

Recommended patterns:

- Local dev: `--cors http://localhost:4200` (explicit port; auth optional)
- Hosted: `--cors https://sneat.app --auth` (exact origin + bearer tokens)
- Never: `--cors '*'` on a public address without `--auth`

`Access-Control-Allow-Credentials` is never set; the CORS layer never weakens
auth on actual (non-preflight) requests — a 401 or 403 will carry the ACAO
header so the browser can read the error body, but the request is rejected.


## MySQL engine (2026-07-09)

Same posture as the PostgreSQL engine: the DSN carries credentials and is read
from the environment variable named by `storage.mysql.dsn_env` (default
`OVDB_MYSQL_DSN`), never from the manifest. Prefer a TLS-enabled DSN
(`tls=true` or a custom TLS config) for non-local servers. ovdb issues only
parameterized statements; identifiers are validated against a safe pattern
before backtick-quoting, so record data cannot inject SQL.


## GitHub-backed inGitDB (2026-07-09)

The GitHub access token is a credential and is read from the environment
variable named by `storage.ingitdb.github.token_env` (default
`OVDB_GITHUB_TOKEN`), never from the manifest. Scope the token to
`contents:write` on the single target repo (a fine-grained PAT or a GitHub App
installation token) — a broadly-scoped classic PAT would let a compromised
ovdb write to every repo the token can reach. Data written to the repo is
plaintext and, if the repo is public or later made public, world-readable
(same consideration as any inGitDB repo). Each write batch is one commit, so
the full history — including deleted records — is retained and pushed to
GitHub.
