## Scope examined

Read the normative contract (`dtql/design/layered-acl/04`, `15`, `18`, `19`) and then the code paths those clauses land in:

- **DALgo `access/`**: `coordinator.go`, `assessment.go`, `policy.go`, `rule.go`, `write.go`, `condition.go`, `fields.go`, `mask.go`, `portable.go`, `file_policy.go`, `document.go`, `session.go`, `query_fields.go`, `principal.go`, `execution.go`, `database.go` (transaction rejection), plus `coordinator_test.go`, `mask_enforcement_test.go`.
- **DTQL wire**: `dalgo/dtql/authorization/result.go`.
- **SQLite**: `sqlite_protected.go`, `sqlite_emit.go`, `reader_base.go`, `database.go`, protected/structured tests.
- **InGitDB**: `protected.go`, `access_generation.go`, `tx_readonly.go`, `tx_readwrite.go` (`applyFieldUpdate`), `query.go`, `protected_test.go`.
- **OpenVaultDB**: `pkg/authorizationapi/request.go`, `pkg/server/access*.go`, `records.go`, `dtql.go`, `identity.go`, `pkg/core/core.go`, `access_layers.go`, `access_sample.go`, `query.go`, `pkg/policystore/*`, `pkg/mount/sqlite.go`, `access_write_test.go`, `layered_acl_test.go`.
- **DataTug**: `pkg/openvaultdb/client.go`, `pkg/server/openvaultdb_proxy.go`, app `openvaultdb/*`, `agent-session.ts`.

**Material limits.** I did not run any tests or builds (no toolchain use in this packet review); all claims are read from source and cross-checked against the committed tests. I did not review the Playwright specs line-by-line, the InGitDB validation-vector fixtures, `condeval` internals beyond `CloneMap`/`Lookup`, or the DataTug settings/serve wiring beyond the proxy registration. Findings are stated against the manifest commits only.

---

## Findings

### F1 — critical — InGitDB contaminates the ACL pre-image with candidate values for nested update paths

**Where:** `dalgo2ingitdb/protected.go:354-379` and the shallow `cloneMap` at `dalgo2ingitdb/protected.go:496-505`, together with `dalgo2ingitdb/tx_readwrite.go:424-441`.

**Mechanism.** `prepare` builds the update candidate as `candidate = cloneMap(pre)`, but that local `cloneMap` copies only top-level keys (`out[k] = v`). `applyFieldUpdate` then *navigates into* existing nested maps (`child, isMap := existing.(map[string]any); m = child`) and assigns in place, so a multi-segment update mutates the map object that `pre` still points at. The evidence record is built afterwards with the same shallow `cloneMap(pre)`, so `ProtectedEvidence.PreImage` carries post-update values for every nested path touched. The coordinator's later `condeval.CloneMap` is a deep copy of already-contaminated data.

**Repro.** InGitDB collection `customers`, record `c1` = `{name:"X", owner:{id:"alice"}}`. Owner policy: `allow update where owner.id == $currentUser` (nested field; `condeval.Lookup` resolves dotted paths — `dalgo/condeval/condeval.go:221`). As `bob`, send the approved one-row UPDATE:

```
PATCH /v1/databases/crm/records/customers/c1
Content-Type: application/vnd.dtql.operation+json
{"id":"u1","action":"update","resource":{...},"executionClass":"dtql",
 "mutation":{"changes":[{"op":"set","path":["owner","id"],"value":"bob"}]}}
```

`prepare` sets `pre.owner.id = "bob"` as a side effect. `evaluateWrite` (`dalgo/access/write.go:151-156`) matches `alternative.Where` against `images.pre` → `"bob" == "bob"` → holds; `Check` defaults to `Where` and holds on the post-image → **allow**. Bob takes ownership of alice's row. Correct behaviour is `ACL_ROW_PREDICATE_FAILED`.

**Impact.** Row-level ownership/tenancy enforcement is bypassable on the primary MVP write path whenever an owner row predicate references a nested field. The same contamination makes `disallowedMaskedMutation` see identical pre/post subtrees, so a removed excluded descendant under a replaced grandparent can go unchecked. Depends only on nested objects decoding to `map[string]any` (the normal YAML/JSON case); other shapes error out and fail closed.

**Why tests missed it.** Every protected InGitDB test uses `update.ByFieldName(...)` (`dalgo2ingitdb/protected_test.go:96,107,184,203,230,272,283`), i.e. single-segment paths, which assign on the cloned top-level map and cannot contaminate. Multi-segment paths only arrive through the HTTP `changes[].path` array (`openvaultdb-go/pkg/authorizationapi/request.go:427-437` permits 1–16 segments), and no test exercises that.

**Action.** Deep-copy the pre-image before deriving the candidate — use `condeval.CloneMap` (as `dalgo2sql/sqlite_protected.go:332` already does) for both `candidate` and `evidence.PreImage`, and stop reusing the shallow local `cloneMap` for images. Consider also making `applyFieldUpdate` copy-on-write.

**Test that demonstrates the fix.** In `dalgo2ingitdb/protected_test.go`, seed `{owner:{id:"alice"}}`, register an owner policy `Allow(Update).Where(WhereField("owner.id", Equal, NewParam("currentUser")))`, run a protected update with `update.ByFieldPath(update.FieldPath{"owner","id"}, "bob")` as `bob`, and assert `errors.Is(err, access.ErrAccessDenied)` **and** that the stored record still has `owner.id == "alice"`. Add an assertion inside the participant policy that the pre-image it sees is `"alice"`.

---

### F2 — high — Evidence field authorization uses a permissive union over write alternatives instead of the deciding rule

**Where:** `dalgo/access/assessment.go:257-267` (`writeMayAllowField`), used by `applyStaticFieldValidation` (`:221-230`); disclosure at `dalgo/access/coordinator.go:570-611`; the missing deciding-rule step at `dalgo/access/coordinator.go:818-869`.

**Mechanism.** `writeMayAllowField` returns true if **any** alternative, or the terminal, may allow the field — correct "may" semantics for `plan` mode, where no row is known. But the protected `Get` evidence path reuses exactly that check and then returns raw pre-image values: `inspectionSession.Evidence` only requires `assessment.Outcome == AssessmentAllow` and reads `valueAtPath(item.PreImage, path)`. `evaluateEvidence` never runs the field restriction for read actions — its write branch is gated on `op.action == Insert|Set|Update|Delete` — and then collapses `Conditional → Allow` and discards `Restrictions`. So the deciding alternative's `fields`/`fieldMask` is never applied, unlike the ordinary record read, which correctly uses `decidingFields` (`dalgo/access/condition.go:94-117`).

**Repro.** One policy on `/customers` with two allow rules for `get`, ordered so the conditional one wins (`matchingRules` sorts by depth, literals, effect, then name):

```yaml
- id: a-own
  effect: allow
  operations: [get]
  where: {op: "==", left: {field: owner}, right: {param: currentUser}}
  fieldMask: {stages: [{include: ["*"]}]}
- id: z-public
  effect: allow
  operations: [get]
  fieldMask: {stages: [{include: ["*"]}, {exclude: ["ssn"]}]}
```

For a row **not** owned by the caller, `decidingFields` picks `z-public` and `ssn` is excluded. But `POST /v1/databases/{db}/access/evidence` with `requiredFields: [["ssn"]]` passes `writeMayAllowField` via the non-deciding `a-own` alternative (`CompleteSubtree("ssn")` is true under `include:["*"]`), the assessment reduces to allow, and the endpoint returns `{"path":["ssn"],"state":"present","value":"..."}`. Same result with legacy `fields:` lists.

**Impact.** Masked/allow-listed field values leak through the authorized point-inspection transport — directly against §18 ("point/write decisions retain the first deciding rule", "never return only the includes as a weaker admission condition") and §15 ("Each owner authorizes Get and the requested concrete fields"). Requires `records:read` on the table plus a multi-rule policy where a non-deciding alternative is broader. Cross-policy intersection is unaffected (the outer loop is per policy), and writes are unaffected (`checkFields` uses the deciding alternative).

**Why tests missed it.** All existing evidence tests use single-rule policies: `dalgo/access/coordinator_test.go:319,345` and `openvaultdb-go/pkg/server/layered_acl_test.go:43-53` each have exactly one allow rule, where union and deciding-rule agree.

**Action.** In `evaluateEvidence`, after the residuals hold, compute the deciding alternative for the pre-image (reuse `decidingFields`) and validate `op.columns` against it; deny with `ACL_COLUMN_DENIED` otherwise. Keep `writeMayAllowField` for `AssessPlan` only. Do not clear `Restrictions` for a read whose field obligation was not evaluated.

**Test that demonstrates the fix.** `dalgo/access/coordinator_test.go`: build the two-rule policy above, evidence-read `["ssn"]` on a row owned by someone else, assert `session.Evidence` returns `ErrAccessDenied` and `Assess` reports `ACL_COLUMN_DENIED`; then assert the owned row still returns the value. Mirror it end-to-end in `openvaultdb-go/pkg/server/access_write_test.go` on both engines.

---

### F3 — medium — InGitDB silently ignores query OFFSET, contradicting the documented profile

**Where:** `dalgo2ingitdb/query.go:72-77` (non-GROUP BY path applies `OrderBy` then `Limit`, never `Offset`; `Offset` is handled only in the GROUP BY branch at `:193-199`, which the DTQL profile rejects).

**Mechanism/repro.** `openvaultdb-go/pkg/core/query.go:152` accepts `offset` 0–10000, and the DataTug page lets the user type arbitrary DTQL (`openvaultdb-page.component.ts:70`). `from: {name: customers}\nlimit: 5\noffset: 5` against an InGitDB mount returns rows 1–5 again instead of 6–10.

**Impact.** Not a disclosure defect — the ACL residual is ANDed into `Where` and applied before ordering and limit, so no unauthorized row appears. It is a silent wrong-answer: paging in the DataTug UI repeats page 1, and `openvaultdb-go/docs/layered-acl-implementation.md:188` advertises "offset max 10000" and "Policy filtering before ordering/pagination" for InGitDB without disclosing the gap. The contract's own rule is "Adapter inability to enforce bounds fails before execution" — silent truncation of a paging parameter is the opposite.

**Action.** Apply `sq.Offset()` in the non-GROUP BY path (slice after ordering, before limit), or reject non-zero offset for the InGitDB protected profile with `ACL_ENFORCEMENT_UNSUPPORTED` and correct the doc table.

**Test.** `dalgo2ingitdb`: seed 10 records, run `limit 5 / offset 5` with a deterministic order, assert the returned keys are the second page. Plus an OVDB HTTP test on the `ingitdb` engine comparing page 1 and page 2 key sets for disjointness.

---

### F4 — low — Row `where` residual is evaluated against the wrong image for Insert and for Set of a missing row

**Where:** `dalgo/access/coordinator.go:824-841`.

**Mechanism.** `evaluateEvidence` evaluates `d.Residuals` against `evidence.CandidateImage` for `Insert` and against `evidence.PreImage` for everything else, then separately runs `evaluateWrite`. For a `Set` of a missing row the pre-image is `nil`, so the residual (the OR of the rules' `Where`s) is matched against no data and fails, even though `evaluateWrite`'s `isNewRow` branch (`write.go:129-141`) correctly admits the row via `Check`. For `Insert`, the residual adds `Where(post)` on top of `evaluateWrite`'s `Check(post)`, so a rule with distinct `where` and `check` is enforced more strictly than the declared semantics.

**Impact.** Over-restrictive, fails closed. Reachable only through the Go protected API (`securedWriteSession.Set/Insert`) and dry-run `set`/`insert` descriptors; the MVP HTTP write path is `update` only, and `records.go:71` rejects PUT/POST on coordinator mounts. Manifests as a spurious `ACL_ROW_PREDICATE_FAILED` in Explain for a legitimate upsert.

**Action.** Skip residual evaluation for new-row cases and let `evaluateWrite` own the new-row admission (it already implements `Alternatives` → `Check` → `Terminal`).

**Test.** `dalgo/access/coordinator_test.go`: policy `Allow(ReadWrite).Where(ownerID == $currentUser)` with no terminal; `NewProtectedSet` for a key with `Exists:false` and candidate `{ownerID:"u1"}` as `u1`; assert the assessment allows and `Execute` succeeds.

---

### F5 — low — Ambiguous physical dotted keys are not rejected at protected normalization

**Where:** `openvaultdb-go/pkg/authorizationapi/request.go:424-437` — `validSegment` rejects `/ \ %` and `.`/`..` as whole segments but permits `.` *inside* a segment, so `path: ["address.internal"]` and `path: ["address","internal"]` both validate.

**Mechanism/impact.** §18 requires: "A literal dotted field pattern always means nested segments, never a literal dot in a physical key. Reject ambiguous physical keys at protected normalization/evidence boundaries as specified in C1." Downstream, `disallowedUpdates` and `disallowedMaskedMutation` re-join with `.` and mask matching re-splits on `.`, so a physical dotted key is authorized as if it were a nested path. I could not construct an escalation: parent-prefix mask semantics mean every dotted string is checked against the same pattern set and the deny direction holds (`exclude:["notes"]` still blocks `notes.x` as a single segment), and SQLite rejects the unknown column outright. InGitDB will however create a top-level physical key literally named `a.b`, whose later authorization decisions are indistinguishable from nested `a`→`b`. Spec-conformance gap and a latent integrity hazard rather than an exploitable one.

**Action.** Reject `.` inside any column/change path segment (and inside `rowId` if physical-key ambiguity matters for the engine) in `Operation.Normalize`.

**Test.** `openvaultdb-go/pkg/authorizationapi/request_test.go`: assert `Normalize` rejects `changes[].path == ["address.city"]` and `resource.columns == [["address.city"]]`.

---

### F6 — low — `ACL_CALLABLE_DENIED` is defined but never emitted

**Where:** `dalgo/access/execution.go:96-109` — `compiledExecutionGate.allows` returns a single bool, and `executionDenied` always sets `CodeExecutionClassDenied`, so a stored-procedure denial caused by the *name mask* (not the class) reports the class code. C7 requires `ACL_CALLABLE_DENIED` with `scope: operation` for that case, and "the same code/scope across Explain and real route dispatch".

**Impact.** Diagnostic fidelity only; procedure execution is unsupported in MVP so this affects plan-mode Explain. **Action:** have `allows` distinguish "no entry for this class" from "class matched, name mask rejected" and emit the callable code. **Test:** assert `AssessPlan` on a `stored_procedure` target whose namespace matches but whose name fails the mask yields `ACL_CALLABLE_DENIED`.

---

### F7 — low — Array/opaque composite coverage failures report `ACL_COLUMN_DENIED` instead of `ACL_ENFORCEMENT_UNSUPPORTED`

**Where:** `dalgo/access/fields.go:363-369` (`allowsValue` routes slices/arrays/structs to `allowsWhole`) and `write.go:216-244` (`checkFields` always denies with `CodeColumnDenied`).

**Impact.** Fails closed and is correct on the allow/deny axis (`mask_enforcement_test.go:91-102` proves opaque containers do not leak descendants). §18 nevertheless specifies "Unknown/opaque composite coverage or unsupported arrays yields ACL_ENFORCEMENT_UNSUPPORTED", and the two codes reduce differently (deny vs indeterminate, and 403 vs 422 at the HTTP binding). **Action:** classify a refusal whose cause is an unsupported composite value separately from a mask exclusion. **Test:** protected update touching an array-valued path under a mask, asserting `ACL_ENFORCEMENT_UNSUPPORTED`/422 rather than 403.

---

### No findings at these points

I specifically looked for and did **not** find defects in: mask stage semantics (`CompiledMask.Allows` reproduces every row of the §19 truth table, `**d` normalizes to `*d`, `address.**` decides identically to `address.*`, and `CompleteSubtree` conservatively rejects a parent whose descendant exclusion could apply); enforcement of masks before result publication (the enumerable fast path is disabled for masks via `fieldSet.enumerable` returning `ok=false`); row filters reaching storage before pagination on both engines; CAS coverage of hidden fields (InGitDB HMACs the whole record file, SQLite the whole `SELECT *` row); dry-run mutation (`inspectionSession` structurally cannot satisfy `ExecutionSession`, and inspection takes a shared lock over a read-only tx); impure callback replay (`CanInspectPolicy` gating); rejection of public dynamic `RunReadwriteTransaction` before invoking the worker (`database.go:191-194`); unavailable-owner aggregation (all leases still evaluated; definitive deny dominates indeterminate; errors are not converted to denials); private-policy cardinality (one unconditional owner aggregate is always emitted; hidden policies never contribute a placeholder); row-existence projection (missing/denied/private point probes all reach the same 404 `resource_unavailable` + `redactPoint` generic `ACCESS_DENIED`); policy-lease/publication interlock (`AcquirePolicyLease` holds `RLock` across the whole storage boundary, `Activate` takes the write lock, and the coordinator releases leases outside the storage callback); filesystem and Git generation publication (digest-verified reuse, fsync ordering, `update-ref` CAS with expected old head, clean-subtree requirement, recovery that never activates an unreferenced generation); SQLite predicate compatibility (unary `+` plus explicit `COLLATE BINARY` and `typeof` guards; unsupported operators fail closed, and the unsafe `emitSQL` string path is unreachable because the protected factory only exists for `StructuredQueryDialect == "sqlite"`); adversarial request normalization (duplicate keys, `null`, case-insensitive field matching, unknown fields, overlapping parent/child changes, and columns-vs-mutation mismatch are all rejected); and DataTug credentials (the OVDB bearer never leaves the CLI, the browser holds only an agent session token checked with `subtle.ConstantTimeCompare` behind an `Origin` check, redirects are not followed, the launch fragment is stripped via `replaceState`, and the proxy rejects browser-supplied `subject`/`simulation`).

---

## Contract ambiguity / human decision

**A1 — `allowed=true` cannot coexist with enforced obligations.** `dalgo/dtql/authorization/result.go:276-278` rejects any result with `allowed=true` that carries restrictions. But §C2 says of a successful execution: "In a successful execution it is true only when that obligation was applied at the actual query/write boundary" — i.e. an allowed execution should be able to report `enforced=true` restrictions. Today OVDB emits no restrictions in `projectInspection` at all, so nothing conflicts in practice and the behaviour is strictly less disclosive.

*Trade-offs.* (a) Keep the validator rule and drop the `enforced=true` reporting idea: simplest, keeps "allowed means nothing outstanding" easy to audit, but permanently loses the audit signal that a specific obligation was applied. (b) Relax the validator to allow restrictions when `allowed=true` **and** every one has `enforced=true`: matches §C2 and gives DataTug something to show after a write, at the cost of a subtler invariant clients must get right. I recommend (b) with the strengthened invariant, but it is a wire-contract change and needs a human decision before freeze.

**Not findings, recorded for the record.** `ownerAllows` (`openvaultdb-go/pkg/server/access.go:51-57`) returns `true` for every non-`inspect-protected` capability when no `OwnerAuthorization` hook and no `authCfg` are configured — that is an entirely unauthenticated dev mount, trusted-host setup, not an exploitable path. Separately, because `Controller.Activate` takes the write lock while `AcquirePolicyLease` spins on `TryRLock`, a policy publication concurrent with in-flight protected operations will make those operations time out and fail closed; correct but worth knowing operationally.

---

## Overall readiness

The architecture holds up well against the approved contract: `dalgo-hierarchical-v1` is reused rather than replaced, the scoped-mask evaluator extends the existing `fieldSet` machinery instead of forking one, the coordinator seam is genuinely capability-scoped and lifetime-checked, each owner re-authorizes independently, and the safe 404/`ACCESS_DENIED` projection is applied consistently across get/head/evidence/update/sample. The wire validation on both ingress and egress is unusually thorough. I found no reason to question the composition, the boundaries, or the MVP scope.

Two defects should block merge:

- **F1 is a genuine authorization bypass** on the primary approved write path and needs a fix plus the nested-path regression test before this ships. It is a small, local fix (one shallow copy).
- **F2 is a field-disclosure bypass** on the evidence transport and needs the deciding-rule check wired into `evaluateEvidence`.

F3 should be fixed or explicitly declared unsupported and documented, because the implementation doc currently claims offset support on InGitDB. F4–F7 are conformance and diagnostic-fidelity items that can follow.

Both blocking defects share a shape worth noting for whoever fixes them: the *ordinary* DALgo paths get the per-row deciding rule and a deep pre-image right (`decidingFields`, `condeval.CloneMap`), and the *new* protected paths reimplement the same step less carefully. Prefer converging on the existing helpers over adding new logic.

The known delivery gates — DALgo's 100% coverage gate, local development module links, publication and released-version verification, pending merges — are separate from the above and I have treated them as such; nothing in this review depends on remote CI having passed, and I did not treat the reported local test runs as evidence for the two uncovered paths (I verified from the test sources that neither is exercised).