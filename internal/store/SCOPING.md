# Record scoping in openvaultdb-go (why the parent-chain flaw is NOT reachable)

**Verdict:** openvaultdb-go is **not susceptible** to the per-space / parent-record
key-scoping flaw that was root-caused and fixed in the sibling repo
`ingitdb/dalgo2ingitdb` (see its `PARENT-CHAIN-FINDINGS.md`). OVDB uses a
**flat-collection addressing model**: a record is addressed by the 4-tuple
`(vault, namespace, collection, id)` and nothing else. There is no
"subcollection under a parent record" concept anywhere in the stack, so two
same-id records under different parents can never be produced — no parent chain
ever reaches `store.go`.

This doc exists because a prior ecosystem audit flagged
`internal/store/store.go collectionRelPath` as "the same class of flaw." That
note is about the *shape* of the path-mapping function, not an actual reachable
collision. The trace below shows why the flaw cannot be triggered through either
entry path into `store`.

## The flaw, restated

In `dalgo2ingitdb` a subcollection's on-disk path was derived from the schema
layout only and never parameterized by the parent record id, so the dalgo keys
`spaces/family/contacts/c1` and `spaces/work/contacts/c1` (same leaf collection +
id, different parent chain) resolved to the **same file** and clobbered each
other. The fix (Option A) nests subcollection data physically under the parent
record directory. For that flaw to exist you need **two things**: (1) a dalgo key
that carries a parent chain, and (2) a path mapping that ignores it. OVDB has
neither — because no parent-chained key is ever constructed.

## Entry path 1 — remote consumer via the dalgo2ovdb adapter → REST → server → store

A dalgo consumer using `dalgo2ovdb` talks to a running ovdb-server over a REST
API. The adapter reduces every dalgo key to its **leaf collection + id** and
drops any parent chain:

- `dalgo2ovdb/transaction.go:57` — `tx.rest.create(ctx, key.Collection(), body)`
  (create), `:105`/`:109` (Set → patch/create), `:130` (Delete) all pass
  `key.Collection()` — the **leaf** collection name only. `key.Parent()` is never
  read anywhere in the adapter.
- `dalgo2ovdb/query.go:33` — `collection := from.Base().Name()` — leaf only.
- `dalgo2ovdb/client.go:41-46` — the wire URL is
  `/vaults/{vaultId}/ns/{ns}/collections/{collection}/records/{id}`. There is
  **no** parent-record path segment; the protocol has no way to express one.

Even if the adapter were changed to try to smuggle a parent chain through, the
server route cannot receive it:

- `internal/server/server.go:51` —
  `const recBase = "/vaults/{vaultId}/ns/{ns}/collections/{collection}/records"`.
  `{collection}` is a Go 1.22 `net/http.ServeMux` **single-segment** wildcard: it
  matches one path segment and will not match a `/`. A parent chain like
  `spaces/family/contacts` cannot bind to `{collection}`.
- `internal/server/records.go:41` — `collection: r.PathValue("collection")` is a
  single opaque string handed straight to
  `store.{List,Create,Update,Delete}Record` (`records.go:62/84/102/119`).

So through the remote path, `store` only ever receives a **flat** `collection`
string. No parent chain survives.

## Entry path 2 — in-process store.go → dalgo2ingitdb (per-vault)

`store.go` persists records by driving `dalgo2ingitdb` itself, one `dal.DB` per
vault. It always builds a **single-level, sanitized** collection handle and never
a parent-chained key:

- `internal/store/store.go:216` — `collectionID(nsID, collection)` flattens the
  `(namespace, collection)` pair into one dotted, sanitized token
  (e.g. `todo-demo.openvaultdb.app.todos.tasks`).
- `internal/store/store.go:402,425,456,478` — every record op builds its key as
  `dal.NewKeyWithID(colID, id)` — a **root** key with one collection segment and
  an id. `dal.Key.Parent()` is never set, so the parent-chain resolver in
  `dalgo2ingitdb` (the code that had the flaw) is never exercised on a nested
  key. There is no code path in `store.go` that constructs a child key.

## Where OVDB's scoping actually lives

OVDB's analogue of "per-space isolation" is the **vault**, and it is enforced by
**physical directory separation**, not by a parent-record path segment:

- `internal/store/store.go:196-198` — `vaultDir(vault)` roots each vault at
  `<dir>/vaults/<vault>/`.
- `internal/store/store.go:318-327` — `openDatabases()` opens a **separate**
  `dalgo2ingitdb` `dal.DB` per vault, each rooted at its own `vaultDir`.

So the closest OVDB equivalent of the ingitdb collision scenario
(`spaces/family/contacts/c1` vs `spaces/work/contacts/c1`) is "same
namespace+collection+id in vault `family` vs vault `work`," and those resolve to
two different vault roots —
`vaults/family/.../tasks/$records/c1.json` vs `vaults/work/.../tasks/$records/c1.json`
— and cannot collide. `TestFlatCollectionModel_NoParentRecordCollision` in
`scoping_test.go` locks this in.

## Consequence

No behavior change is warranted in openvaultdb-go. Adding parent-record nesting
here would be dead code: there is no consumer-visible way to address a
subcollection, so there is nothing to nest. If OVDB ever grows a real
subcollection concept (a `{collection...}` multi-segment route, or an adapter
that forwards `key.Parent()`), the Option A fix from `dalgo2ingitdb` should be
applied at `store.go`'s path layer **and** the REST protocol extended to carry
the parent chain — at that point this document must be revisited.
