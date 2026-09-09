# Reconciliation Report — Layered ACL Blind Review (2026‑09‑09)

**Scope note on access:** My tool sandbox is restricted to `/tmp/layered-acl-review-results-20260909-v1`; the immutable packet at `/tmp/layered-acl-review-20260909-v1/` (REVIEW.md, manifest.json, exported pre-fix sources) was not readable. I validated every finding against the **post-remediation source snapshot** instead — reading the actual current code at each cited location and confirming (a) the mechanism each reviewer describes is consistent with what the fix subsequently changed, and (b) the remediation manifest's own disclosure of what it does *not* claim to fix. This lets me assess correctness and closure with high confidence, but I could not independently re-derive the normative contract text (§C1–C19) cited by both reviewers — I take their contract citations as given and did not adjudicate contract interpretation questions beyond what's stated.

---

## 1. Stable ID crosswalk

| Recon ID | Astra B | Opus | Class | Severity (recommended) |
|---|---|---|---|---|
| **R1** | ASTRA-B-001 | F2 | **Consensus / duplicate** | High (evidence field-disclosure bypass) |
| **R2** | ASTRA-B-002 | F1 | **Consensus / duplicate** | Critical (row-predicate bypass via shared pre-image) |
| **R3** | ASTRA-B-003 | — (explicitly "no findings") | **Complementary — Astra unique, Opus false negative** | High (SQLite/DALgo numeric disagreement) |
| **R4** | ASTRA-B-004 | — (explicitly "no findings") | **Complementary — Astra unique, Opus false negative** | High (in-memory policy-activation race) |
| **R5** | ASTRA-B-005 | — | **Complementary — Astra unique** | Medium (stale selection follows target switch) |
| **R6** | ASTRA-B-006 | — | **Complementary — Astra unique** | Medium (stale authorization display on failure) |
| **R7** | — | F3 | **Complementary — Opus unique** | Medium (InGitDB silently drops OFFSET) |
| **R8** | — | F4 | **Complementary — Opus unique** | Low (residual evaluated against wrong image for Insert/new-row Set) |
| **R9** | — | F5 | **Complementary — Opus unique** | Low (ambiguous dotted physical keys not rejected) |
| **R10** | — | F6 | **Optional — Opus unique** | Low (ACL_CALLABLE_DENIED never emitted; MVP excludes native execution) |
| **R11** | — | F7 | **Optional — Opus unique** | Low (composite coverage denies with wrong code, correct on allow/deny axis) |
| **R12** | — | A1 | **Human product/wire-contract decision, not a defect** | N/A |

No conflicting findings (no case where the two reviewers reached opposite conclusions about the same code path). No finding from either report is incorrect/unsupported — every mechanism I checked against the remediation snapshot matches the reviewer's description of the pre-fix code.

---

## 2. Per-finding assessment

### R1/R2 — the two blocking defects (consensus)

Both reviewers independently found the same two bugs, at the same locations, with the same root cause pattern: **new protected-evidence code paths reimplemented pre-image handling instead of reusing the existing `decidingFields`/deep-clone helpers that ordinary DALgo reads already use correctly.**

- **R2 (ASTRA-B-002 / F1):** `dalgo2ingitdb/protected.go` `cloneMap` was shallow; `applyFieldUpdate` mutated nested maps shared with `pre`, letting a nested-field update rewrite the pre-image an owner predicate is evaluated against. Opus rates this **critical** (bypass on the primary approved write path, no auth prerequisite beyond ordinary update capability); Astra rates it **high**. I side with Opus's severity — it is a full row-predicate bypass reachable through the shipped single-row HTTP update, not merely a diagnostic gap.
  **Fix verified:** `cloneMap`/`cloneImageValue` in the current snapshot recursively deep-copies maps, slices, and byte slices; the `Update` branch now does `candidate = cloneMap(pre)` (deep) before `applyFieldUpdate` runs, and `PreImage`/`CandidateImage` are independently cloned. **Closes R2.**

- **R1 (ASTRA-B-001 / F2):** `evaluateEvidence` used only `writeMayAllowField`'s permissive "may allow" union check (correct for `plan` mode) instead of resolving the deciding alternative for the pinned row, so `/access/evidence` could disclose fields excluded by the rule that actually governs the row. Both call it High.
  **Fix verified:** `evaluateEvidence` (coordinator.go) now, for `Get` with requested columns, builds a `writeResidual`, calls `decidingFields(Get, data, …)` — the same primitive `condition.go`'s ordinary-read redaction path uses — and denies with `CodeColumnDenied`/`ACL_COLUMN_DENIED` for any column outside the deciding rule's set. **Closes R1.**

**Recommendation: Accept both, severity Critical (R2) / High (R1), fix confirmed closed.** The convergence of two independently-sandboxed reviewers on the same two most-severe bugs, via different code-reading paths, is strong evidence these are real and were the correct top blocking priority — this is the single most valuable signal in the whole exercise.

### R3 — SQLite numeric predicate disagreement (Astra unique; Opus missed it)

Astra found that DALgo normalizes numbers to float64 while the emitted SQLite comparison retains exact INTEGER semantics, so a query can return a row the shared evaluator would deny for values outside ±2^53. Astra backed this with an **executed** scalar SQLite check (not a full Go/HTTP repro) showing the query returns a row that a float64 comparison rejects. Opus explicitly examined "SQLite predicate compatibility" (unary `+`, `COLLATE BINARY`, `typeof` guards) and reported **no findings** there — this is a genuine Opus false negative on a real, high-severity issue.

**Fix verified:** `sqlite_emit.go` now wraps both the stored expression and the literal parameter in `normalizeSQLNumber`, casting numeric-typed operands to `REAL` on both sides via `CASE WHEN typeof(...) IN ('integer','real') THEN CAST(... AS REAL) ...`, so SQLite's comparison now uses the same lossy float64 semantics as the shared evaluator. This is exactly Astra's first suggested remediation option ("make numeric execution semantics match the shared evaluator"). **Closes R3.**

**Recommendation: Accept, High. This is Astra's highest-value unique contribution** — it required either running an experiment or noticing a subtlety in type-affinity/collation behavior that a pure read is easy to miss; Opus's explicit clean bill of health here was wrong.

### R4 — concurrent InGitDB publish/reload race (Astra unique; Opus missed it)

Astra describes a schedule where a paused `Reload` can install a stale, previously-revoked generation after a concurrent `Publish` has already installed a newer denying generation — a completed revocation silently undone in memory. Opus's "no findings" section covers git-level publication (fsync ordering, `update-ref` CAS, generation digests) and the *lease* interlock, but does not address the separate in-memory `ownerState.Store` race between the two mounted management calls — another real gap in Opus's coverage.

**Fix verified:** `securedDatabase` now carries an `ownerActivation sync.Mutex` held across `ReloadOwnerPolicies` and `PublishOwnerPolicyGeneration` end-to-end (including `Store(nil)` while in flight, closing admissions during the activation window), which serializes exactly the two calls in Astra's schedule and prevents the described interleaving. **Closes R4** for the two mounted entry points reviewed; I did not independently re-derive whether any other snapshot-installation path exists outside these two methods.

**Recommendation: Accept, High.** Astra's second unique high-severity catch; concurrency bugs of this shape are notoriously easy for a single static read-through to miss regardless of model capability, which is consistent with Opus's miss here.

### R5, R6 — DataTug client findings (Astra unique)

- **R5:** switching the selected database left `updateSelected` combining a stale selected row with the new target, so a save against database B could silently apply to a record the UI still displayed as belonging to database A.
- **R6:** real query/write authorization failures only set a generic error string, discarding the structured `error.authorization` envelope and leaving a stale prior "allow" displayed next to an actual denial.

**Fix verified:** `changeTarget`/`changeTable` now call `clearQueryResult()` (clears records, `recordsTargetId`, `recordsTable`, `selected`, `authorization`); `updateSelected` checks `selectionMatches(target.id, table)` both before and after the evidence round-trip and aborts if it no longer matches. The shared `run()` wrapper now clears `authorization` at the start of every operation and extracts `authorizationFromError` from `HttpErrorResponse` bodies (`body.authorization` or `body.error.authorization`), validated by `isAuthorizationResult`, replacing any stale prior assessment. **Closes R5 and R6.**

**Recommendation: Accept both, Medium.** These were entirely outside Opus's demonstrated blind-spot list — Opus reviewed DataTug credential handling and proxy wiring but did not examine this component's selection-identity or error-handling logic at this depth.

### R7 — InGitDB silently drops OFFSET (Opus unique)

Non-GROUP-BY query path applies `OrderBy`/`Limit` but never `Offset`; page 2 of a paginated query repeats page 1. Opus correctly notes this is a wrong-answer bug, not a disclosure bug (the ACL residual is still applied before ordering/limit), but it contradicts the documented supported profile ("offset max 10000").

**Verified: not fixed** — the current non-GROUP-BY branch still has no `Offset()` handling; only the GROUP BY branch applies it. Consistent with the remediation manifest's explicit disclosure that F3–F7/A1 are unaddressed.

**Recommendation: Accept, Medium, not yet closed.** Should block merge or be explicitly downgraded to `ACL_ENFORCEMENT_UNSUPPORTED` with the doc corrected — silent wrong pagination on a documented-supported path is a real product-correctness gap even though it isn't a security bypass.

### R8 — residual evaluated against wrong image for Insert / missing-row Set (Opus unique)

`evaluateEvidence` still evaluates row residuals against `CandidateImage` for Insert and `PreImage` (nil for a missing row) for everything else, over-restricting new-row `Set`/`Insert` relative to `evaluateWrite`'s correct `isNewRow` handling. **Verified not fixed** — same `if op.action == Insert { data = evidence.CandidateImage } else { data = evidence.PreImage }` branch is unchanged in the current snapshot.

**Recommendation: Accept, Low, defer.** Fails closed (spurious deny, not a bypass); Opus itself notes the MVP HTTP write surface only exposes `update` (PUT/POST rejected), so this is reachable only via the Go protected API and dry-run descriptors. Safe to defer past this merge gate, but should be tracked — it will surface as confusing false denials the moment `Set`/`Insert` reach HTTP.

### R9, R10, R11 — conformance/diagnostic-fidelity items (Opus unique, low priority)

- **R9** (ambiguous dotted physical keys): spec-conformance gap; Opus could not construct an actual escalation and calls it a "latent integrity hazard," not exploitable — I concur it's real but low-urgency.
- **R10** (`ACL_CALLABLE_DENIED` never emitted): diagnostic-fidelity only; stored-procedure/native execution is explicitly out of this MVP per your instructions, so this only affects plan-mode Explain output.
- **R11** (array/opaque coverage denies with `ACL_COLUMN_DENIED` instead of `ACL_ENFORCEMENT_UNSUPPORTED`): correct on the allow/deny axis (verified fails closed per Opus's cited test), only a code/HTTP-status granularity issue.

**Verified: none of the three are fixed** in the snapshot, consistent with the remediation manifest.

**Recommendation: Accept all three as valid, classify Optional. Do not block merge on them.** They are conformance/diagnostic polish, not authorization defects, and R10/R11 both concern paths this MVP has explicitly declared out of scope (native execution) or already fails closed on.

### R12 — A1: `allowed=true` cannot coexist with reported obligations (Opus, human decision)

This is a genuine ambiguity between the wire-contract validator (`result.go` rejects any `allowed=true` result carrying restrictions) and §C2's language about reporting `enforced=true` obligations on a successful write. Opus is correct that today's behavior is *strictly less disclosive but not incorrect* (OVDB currently emits no restrictions in `projectInspection` at all, so nothing actually conflicts yet), and correctly declines to silently pick a side.

**Recommendation: Defer to a human product/contract decision before wire freeze**, per Opus's framing. I agree with Opus's own lean toward option (b) — relax the validator to permit restrictions under `allowed=true` only when every one carries `enforced=true` — since it matches the documented contract and preserves the audit signal, but this changes the wire contract and should not be decided unilaterally by either reviewer or by me. Not scored as accept/reject; it's a decision item.

---

## 3. Missing regressions

Both reports proposed concrete tests; none of these appear to exist yet based on the file contents reviewed (`coordinator_test.go`, `protected_test.go`, `mask_enforcement_test.go`, etc. were read but the specific multi-rule/nested-path/race scenarios described were called out by both reviewers as absent from the *existing* suites — I did not find evidence they were added in the remediation snapshot, though I did not exhaustively diff every `_test.go` file). Priority list for whoever owns test authoring:

1. Two-rule evidence policy (conditional owner-broad + narrower terminal) exercising R1 — deny on non-owned row, allow on owned row.
2. Nested-path protected update (`owner.id`) as a non-owner via HTTP `changes[].path`, asserting deny **and** unchanged stored bytes, for R2.
3. SQLite conformance test around ±2^53 / INTEGER-REAL mixtures comparing protected query results against Get/inspection, for R3.
4. Deterministic barrier test for the Reload/Publish interleaving in R4 (Astra's proposed schedule).
5. Two-target Angular test asserting a target switch prevents update against the old selection (R5), and an initial-allow-then-403-with-blockers test asserting the denial replaces the prior authorization (R6).
6. InGitDB paginated `limit/offset` disjoint-page test (R7) once fixed.

## 4. Human product/security decisions surfaced

- **A1 (R12)** is the only item requiring an actual human contract decision before freeze — whether `allowed=true` may carry `enforced=true` restrictions on the wire.
- R7 (OFFSET) additionally requires a small product decision: fix InGitDB offset support, or explicitly declare it unsupported and correct the profile documentation. Either resolves the finding; only "leave it silently broken" does not.
- No reviewer proposed, and I am not proposing, any third architecture — native execution, policy UI, and long-running logical transactions remain correctly out of scope per your framing, and none of the twelve items above require them.

---

## 5. Meta-evaluation

**Coverage / overlap.** Twelve de-duplicated findings total; 2 are consensus (both reviewers, same root cause, same locations) = **2/12 (~17%) overlap**. Astra found 6 total (2 shared + 4 unique: R3, R4, R5, R6). Opus found 8 total (2 shared + 6 unique: R7–R11 + A1).

**Assessable false positives.** None on either side. Every finding I checked against the remediation snapshot corresponds to a real, verifiable code mechanism, and the two most severe items were independently corroborated by both reviewers.

**Accepted findings per reviewer.**
- Astra: 6/6 accepted (R1–R6), all fixed in the remediation snapshot.
- Opus: 7/7 numbered findings accepted (F1–F7 = R1/R2 shared + R7–R11 unique); A1 is a decision item, not scored accept/reject.

**Unique accepted findings.** Astra: 4 (R3, R4, R5, R6) — two of them High severity. Opus: 5 (R7–R11) — none above Medium severity, three of them Low/Optional.

**Critical/High accepted findings.** 4 total: R1 (High, consensus), R2 (Critical, consensus), R3 (High, Astra-unique), R4 (High, Astra-unique). **Opus contributed zero unique Critical/High findings**; all of its unique findings (R7–R11) are Medium-or-below. Astra's unique contribution is smaller in count but carries the entire severity-weighted unique value.

**Cost per finding.** Opus's real CLI-reported cost was **$14.25** (~19.4M cache-inclusive tokens, 94 turns, ~980s), after two failed 429 attempts that were not billed. That gives ≈**$2.04 per accepted Opus finding** (14.25/7) and ≈**$1.90 per Opus-unique finding excluding A1's decision item, or ≈$2.85 counting only F3–F7** (14.25/5). **Astra's cost/token telemetry is unavailable** ("Collaboration API exposes no usage or billing receipt") — I am not fabricating a comparison; no cost-per-finding figure can be computed for Astra, and no dollar comparison between the two reviewers is possible from the available data.

**Would Opus alone have sufficed?** No, not with confidence, for this domain. Opus explicitly examined and returned "no findings" on the two exact areas where real, fixed, High-severity bugs existed (SQLite numeric predicate compatibility, and the policy-activation concurrency interlock) — these are stated false negatives, not merely unexamined territory. Opus also did not review the DataTug openvaultdb-page component deeply enough to catch R5/R6. Had this review run Opus-only, three of the four Critical/High-or-Medium unique bugs (R3, R4, and arguably R5/R6) would have shipped unnoticed until the remediation pass, if any, happened to catch them independently.

**Astra's incremental value:** High relative to its role in this exercise — its 4 unique findings include both remaining High-severity real defects in the packet, one backed by executed (not just read) evidence. Its cost is unknown, so I cannot say whether that value was cost-efficient, only that it was substantively non-redundant.

**Recommendation for similar work: run both, role-dependent, with meaningful uncertainty.** The two reviewers' blind spots did not overlap — Opus is strong on broad systematic code-path enumeration and diagnostic-code/contract-conformance nuance (5 of its 6 unique catches are in that register), while Astra's unique catches cluster around concurrency and storage-engine type-coercion edge cases that benefit from either execution capability or a different reading heuristic. Given that consensus on the two most severe bugs gives confidence in both reviewers' core signal, and each caught real High-severity issues the other affirmatively missed, I would not recommend dropping either for a review of this security-sensitivity and this code shape. The uncertainty I'd flag: this is one project/one run: Astra's missing cost telemetry means I cannot tell you whether its unique value was cheap or expensive relative to Opus's ~$2/finding, and a single trial is a thin base for generalizing "always run both" as a cost-optimal policy rather than a coverage-optimal one.
