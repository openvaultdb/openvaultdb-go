// Package dalgo2ovdb is a proof-of-concept DALgo adapter over an OpenVaultDB
// (OVDB) vault, mirroring the shape of github.com/dal-go/dalgo2firestore.
//
// Unlike dalgo2firestore (a native SDK wrapper), dalgo2ovdb is a plain HTTP
// client: ovdb-server already speaks DALgo internally (its Store holds
// map[string]dal.DB backed by github.com/ingitdb/dalgo2ingitdb), so this
// package does not re-implement storage — it drives ovdb-server's REST record
// CRUD + OAuth-like connect flow from the outside, exactly as any other
// external app would.
//
// # Scope (PoC, not production)
//
//   - Get / Exists / GetMulti — implemented by listing the whole collection
//     and filtering client-side, because OVDB's REST API has no single-record
//     GET route (only a collection-wide list). See restClient.list.
//   - Set — implemented as an existence probe + POST (create) or PATCH
//     (merge), because OVDB has no upsert/full-replace route. The PATCH branch
//     is a field merge, not a replace: a field present in the old record but
//     absent from the new one survives. This is a real semantic gap versus
//     DALgo's Set() "replace the whole record" contract.
//   - Insert — POST create; if the DALgo key has no ID, OVDB's native
//     server-side id generation is used and the id is written back onto the
//     key (the WithAdapterGeneratedID contract).
//   - Delete — DELETE by id. Clean 1:1 fit.
//   - Query (StructuredQuery only) — maps to the list endpoint. Offset/Limit
//     are applied client-side; Where/GroupBy/Having/OrderBy are NOT pushed
//     down (OVDB's REST API has no filter/sort query params, and DTQL, its
//     would-be query language, does not exist yet). Only SelectIntoRecord is
//     supported.
//   - Update/UpdateRecord/UpdateMulti — NOT implemented. Translating DALgo's
//     field-level update.Update operations onto OVDB's raw-JSON PATCH-merge
//     endpoint is a real gap, left for a future slice.
//   - RunReadonlyTransaction / RunReadwriteTransaction — OVDB has no
//     server-side multi-record transaction. Both are emulated as an
//     immediate-execution wrapper: every write inside the callback hits the
//     REST API right away, sequentially, with NO atomicity and NO rollback.
//     This is the transaction gap the investment plan flagged in advance.
//
// Construct a DB via Connect (runs the connect flow against a running
// ovdb-server and returns a ready-to-use dal.DB) or NewDB (if you already hold
// a Conn, e.g. a persisted/refreshed token).
package dalgo2ovdb
