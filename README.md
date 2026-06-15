# openvaultdb-go

Reference implementation and Go libraries for [OpenVaultDB](https://openvaultdb.com/), a user-owned application database platform with portable storage backends and DTQL query support.

## `ovdb-server` — reference local server

A local HTTP server implementing the OpenVaultDB wire contract
(`openvaultdb-com/interface/main.tsp`, with the concrete constants from
`INTEGRATION.md`). It lets a wallet register the server, browse the vaults it
hosts, and lets a third-party app obtain a scoped token via the connect flow and
read/write records within its granted scope.

### Run

```bash
go run ./cmd/ovdb-server serve --port 8088
# or build:
go build -o ovdb-server ./cmd/ovdb-server && ./ovdb-server serve --port 8088
```

Flags:

- `--port` (default `8088`) — TCP port.
- `--data-dir` (default `./ovdb-data`) — directory for on-disk vault data.

On startup it prints:

```
OWNER_TOKEN=<random hex>
OpenVaultDB server listening on http://localhost:8088 (data dir: ...)
```

Paste the `OWNER_TOKEN` into the wallet when registering this server. The token
is persisted to `<data-dir>/owner-token` and reused across restarts.

### Owner-token flow

`GET /vaults` and `GET /vaults/{vaultId}/namespaces` require
`Authorization: Bearer <OWNER_TOKEN>`. The wallet validates the server via
`GET /.well-known/openvaultdb`, stores `{ baseUrl, ownerToken }`, then lists
vaults and drills into namespaces with the owner token.

### Connect flow (app → scoped token)

1. App redirects the user's browser to `GET /authorize?client_id&redirect_uri&vault&namespaceId&role&state`.
2. The server renders a dev consent page (Approve/Deny).
3. On **Approve** it mints a one-time `code` bound to
   `(client_id, redirect_uri, vault, namespaceId, role)` and 302s to
   `redirect_uri?code=...&state=...`. On **Deny** it redirects with
   `error=access_denied`.
4. The app's backend calls `POST /token`
   `{ grant_type: "authorization_code", code, client_id, redirect_uri }` and
   receives a scoped opaque bearer token plus its per-collection `scope`.
5. The app calls the record endpoints with the app token. The server **enforces
   scope** — out-of-scope ops (e.g. a `viewer` writing) are rejected `403`.

### Endpoints (full `main.tsp` contract)

| Method | Path | Auth |
|--------|------|------|
| `GET` | `/.well-known/openvaultdb` | none |
| `GET` | `/vaults` | owner token |
| `GET` | `/vaults/{vaultId}/namespaces` | owner token |
| `GET` | `/authorize` | consent page (interactive) |
| `POST` | `/authorize` | consent decision → 302 |
| `POST` | `/token` | one-time code |
| `GET` | `/vaults/{vaultId}/ns/{ns}/collections/{collection}/records` | app token (`read`) |
| `POST` | `/vaults/{vaultId}/ns/{ns}/collections/{collection}/records` | app token (`write`) |
| `PATCH` | `/vaults/{vaultId}/ns/{ns}/collections/{collection}/records/{id}` | app token (`write`) |
| `DELETE` | `/vaults/{vaultId}/ns/{ns}/collections/{collection}/records/{id}` | app token (`delete`) |

`{ns}` is URL-encoded (the namespace id contains `/`, sent as `%2F`); the server
decodes it.

### Seed data (created on first run)

- Vault `{ id: "local", name: "Local Vault", backend: "ingit" }`.
- Namespace `todo-demo.openvaultdb.app/openvaultdb/todos`
  (owner `todo-demo.openvaultdb.app`, collections `["tasks"]`).
- Roles: `editor → { tasks: [read, write, delete] }`, `viewer → { tasks: [read] }`.

### Storage layout

Records are persisted as one JSON file per record in an inGitDB-style layout
under the data dir:

```
<data-dir>/
  owner-token
  vaults/local/ns/<ns>/collections/tasks/
    .ingitdb-collection.yaml      # tasks collection schema
    $records/<id>.json            # one record per file
```

`tasks` fields: `id` (PK, server-generated if absent), `title` (required),
`done` (default `false`), `createdAt` (RFC3339, server-set).

### CORS

Allowed only for the wallet browser origins `http://localhost:5000`,
`http://localhost:8787`, `https://openvaultdb.com` (methods GET/POST/PATCH/DELETE,
headers `Authorization, Content-Type`). The demo app reaches the server
server-side and needs no CORS.
