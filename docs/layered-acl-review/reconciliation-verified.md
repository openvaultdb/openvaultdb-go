R3's fix (normalizeSQLNumber casting to REAL) is confirmed present in source. I have now independently verified every claim needed. Writing the final report.

---

# Reconciliation Report — Layered ACL Blind Review (2026‑09‑09) — CORRECTED FINAL

**Role:** Third-pass independent verifier. This is not the provisional reconciler's output; it corrects and finalizes that provisional report (`reconciliation.md`/`reconciler.json`) against baseline contract text, the acl.schema.json, and both source snapshots, all of which are now readable from this sandbox (`/tmp/layered-acl-review-results-20260909-v1/baseline` and `/remediation`). Every claim below was checked against primary text — contract clauses are quoted, code is cited by path/line, and I flag anywhere I rely on source-reading rather than executed tests.

## 0. What was wrong with the provisional report, and what changed here

| # | Provisional error | Correction |
|---|---|---|
| 1 | Called A1 an unresolved contract ambiguity needing a human product decision | It is an **implementation defect**: the validator is stricter than the already-approved contract. See §3. |
| 2 | Asserted regressions were absent, without searching for them | All six named regression tests exist as source in the remediation snapshot and assert the correct fixed behavior. See §4. |
| 3 | Miscounted overlap and cost-per-finding, and left a self-contradictory `$1.90` figure alongside a correct `$2.85` figure for the same denominator | Correct denominators and arithmetic in §6/§8. |
| 4 | Meta-eval said "each caught real High-severity issues the other affirmatively missed," contradicting its own severity table | Corrected in §7 — only Astra has unique High findings; Opus's uniques top out at Medium. |
| 5 | Cited F5/F7 without checking them against the approved contract, so couldn't tell a real conformance gap from a nice-to-have | Both are **confirmed contract violations** with exact clauses, scoped fixes recommended. See §5. |

Nothing below overturns the provisional report's core technical read of the two consensus bugs (R1/R2) or its verification method (reading current source against reviewer-cited mechanisms); those parts were sound and are preserved.

---

## 1. Scope and method

Read baseline contract docs (`03-policy-format.md`=C1, `04-authorization-contract.md`=C2, `15-contract-completion.md`=C2 completion, `18-mask-supplement.md`, `19-scoped-mask-stages.md`, `contracts/acl.schema.json`), both submitted reviews (`astra-b.md`, `opus-review.md`), the actual usage telemetry (`metrics.json`, `reconciler.json`), the provisional report, the remediation manifest, and the exact pre-fix (`baseline/`) and post-fix (`remediation/`) source trees for every cited file. All source claims below were checked by reading the cited lines directly; no tests were executed. Where I say a fix is "verified," I mean the source now contains the described logic and a regression test asserts it — not that I ran the suite and watched it pass.

---

## 2. Stable crosswalk (unchanged IDs, corrected classification)

| ID | Astra | Opus | Class | Severity | Baseline validity | Remediation status (source-verified) |
|---|---|---|---|---|---|---|
| R1 | ASTRA-B-001 | F2 | Consensus/duplicate | High | Real defect, contract §15 C4 violation | **Fixed** — HTTP test both engines |
| R2 | ASTRA-B-002 | F1 | Consensus/duplicate | Critical | Real defect, primary write path | **Fixed** — deep-copy + regression test |
| R3 | ASTRA-B-003 | — (examined, missed) | Complementary, Astra-unique | High | Real defect | **Fixed** — `normalizeSQLNumber`, conformance test |
| R4 | ASTRA-B-004 | — (different code path, not stated-examined) | Complementary, Astra-unique | High | Real defect | **Fixed** — `ownerActivation` mutex, barrier test |
| R5 | ASTRA-B-005 | — | Complementary, Astra-unique | Medium | Real defect | **Fixed** — component spec |
| R6 | ASTRA-B-006 | — | Complementary, Astra-unique | Medium | Real defect | **Fixed** — component spec |
| R7 | — | F3 | Complementary, Opus-unique | Medium | Real defect (doc says supported) | **Not fixed** (confirmed in source) |
| R8 | — | F4 | Complementary, Opus-unique | Low | Real defect, fails closed | **Not fixed** (confirmed in source) |
| R9 | — | F5 | Complementary, Opus-unique | **Low–Medium, upgraded**: confirmed **C1 conformance violation**, not just a spec-conformance nicety | Real defect against explicit clause | **Not fixed** (confirmed in source) |
| R10 | — | F6 | Optional, Opus-unique | Low | Real defect, diagnostic-only, MVP scope excludes native execution | **Not fixed** (confirmed in source) |
| R11 | — | F7 | Optional, Opus-unique | **Low–Medium, upgraded**: confirmed **contract violation** (18-mask-supplement.md explicitly requires the other code) | Real defect | **Not fixed** (confirmed in source) |
| A1 | — | A1 | **Implementation defect against an already-approved contract** (reclassified from "human decision") | Medium (diagnostic/audit-signal correctness, not a bypass) | Real defect | **Not fixed** (confirmed unchanged in both baseline and remediation `result.go`) |

No conflicting (opposite-conclusion) findings between reviewers. No finding from either report is factually incorrect. **Snapshot dates preserved:** the remediation manifest states only R1/R2 are claimed fixed; I independently re-derived that R3–R6 are also fixed in this snapshot (source-verified, not merely manifest-claimed) and that R7–R11 and A1 remain open **as of this snapshot**. I did not use any claim about later/root-branch fixes to backdate R7, R8, or R10 as closed — whatever their status elsewhere, this snapshot alone proves them open, and that is the only thing scored here.

---

## 3. A1 — corrected: this is a validator defect, not a live ambiguity

Opus described `dalgo/dtql/authorization/result.go:276-278` rejecting any `allowed=true` result carrying restrictions, framed it against §C2's "obligation…applied at the actual query/write boundary" language, and called it a genuine ambiguity needing a human product decision. **It is not ambiguous — the approved contract already resolves it, and the implementation is wrong.**

Evidence, in order of authority:

1. **`contracts/acl.schema.json`**, the closed schema that `15-contract-completion.md` explicitly says defines JSON shapes ("This document…define[s] semantics; closed JSON schemas define JSON shapes"). Its `result` schema has:
   - `mode ∈ {plan,inspect,sample}` ⇒ every restriction's `enforced` must be `false` (dry-run only).
   - `allowed=true` ⇒ `result="allow"` and `coverage.evaluation="complete"`/`truncated=false`.
   - **There is no rule anywhere forbidding `restrictions` from being non-empty when `allowed=true`.** The schema only constrains dry-run modes to `enforced:false`; for `mode="execution"` it places no such ban.

2. **`04-authorization-contract.md`**, `allowed` field definition: *"true only for final complete allow. Never true for residual obligations."* An **enforced** obligation is by definition not residual/outstanding — it was applied. The clause bans outstanding obligations, not a disclosed record of a satisfied one.

3. **`15-contract-completion.md`, §C2**: *"`enforced=false` in every dry run, even after successful evaluation. In a successful execution it is true only when that obligation was applied at the actual query/write boundary. A denied execution has no obligation marked enforced merely because its evaluator ran."* This sentence only makes sense if a **successful (allowed) execution** is expected to be able to report `enforced=true` restrictions — that's the entire point of distinguishing it from a denied execution.

4. **Current code**, confirmed identical in `baseline/dalgo/dtql/authorization/result.go:276-278` and `remediation/dalgo/dtql/authorization/result.go:273-274`:
   ```go
   if result.Allowed && (len(result.Blockers) > 0 || len(result.Restrictions) > 0) {
       return fmt.Errorf("authorization result: allowed result contains blockers or restrictions")
   }
   ```
   This rejects **any** restriction on an allowed result, with no exception for `enforced=true` execution-mode restrictions — stricter than both the schema and the prose contract.

**Conclusion:** A1 is a validator bug against an already-approved wire contract, not a live design question. **Recommended action (smallest scope, no new architecture):** relax the guard to `result.Allowed && (len(result.Blockers) > 0 || anyRestrictionNotEnforced(result.Restrictions))`, i.e. permit restrictions on an allowed result only when every one has `enforced=true`. That is exactly the invariant the contract's own language implies, and it is a one-line change plus a schema-shaped test, not a fresh human product decision. It's worth a maintainer sign-off before merge (it is, after all, a wire-validator relaxation), but the direction is dictated by the existing contract, not open.

---

## 4. Regressions: verified present, not assumed absent

I searched the remediation snapshot for all six named tests. All exist and assert the correct fixed behavior:

- **R2** — `dalgo2ingitdb/protected_test.go:298` `TestProtectedNestedOwnershipCannotRewritePreImage`: attacker submits nested `meta.ownerID` takeover via `access.NewProtectedUpdate`, asserts inspection does **not** allow, execution returns `ErrAccessDenied`, and the on-disk record bytes are byte-identical to before the attempt.
- **R2 (adapter-level)** — `dalgo2ingitdb/protected_map_test.go:82` `TestProtectedMapNestedCandidatesDoNotMutateEvidence`: three nested-mutation shapes (set/delete/replace-parent), asserts `evidence[0].PreImage` still reads `"victim"`, that mutating a cloned copy doesn't affect the original evidence, and that the backing store record is unchanged.
- **R4** — `dalgo2ingitdb/access_generation_crash_test.go:216` `TestMountedReloadCannotReinstallRevokedGeneration`: a deterministic two-goroutine barrier (`ownerPolicyPublicationHook("reload_before_activation")`) reproduces exactly Astra's paused-Reload/concurrent-Publish schedule, asserts `db.ownerActivation` (a real `sync.Mutex` added at `securedDatabase`) is held across the window, and that after both calls settle the installed snapshot matches the authoritative revoked revision — a subsequent `Get` is denied.
- **R1** — `openvaultdb-go/pkg/server/access_write_test.go:222` `TestProtectedHTTPEvidenceConditionalFieldFallback`: runs a real `httptest.NewServer`, both `sqlite` and `ingitdb` engines, two rows (one matching the conditional deciding rule, one not), asserts the ordinary Get and the `/access/evidence` endpoint both correctly expose `secret` only for the row the conditional rule actually decides, and the other row gets a safe 404 with no `"present"`/`"absent"` leak. This is HTTP-level evidence, not merely an internal-API unit test, and it covers **both** adapters — resolving the "adapter vs HTTP" distinction the task asked me to check: the fix and its test are not adapter-specific.
- **R3** — `dalgo2sql/structured_sql_test.go:195` `TestSQLiteNumericPolicyConformanceBeforeLimit`: seeds values around ±2^53, runs every comparison operator against the shared evaluator (`condeval.Match`) and the actual protected SQL query, asserting they agree. Source-verified fix: `sqlite_emit.go:238` `normalizeSQLNumber` now wraps both operand and literal in `CASE WHEN typeof(...) IN ('integer','real') THEN CAST(... AS REAL) ...`, matching DALgo's float64 evaluator semantics exactly as Astra's suggested fix specified.
- **R5/R6** — `datatug-apps/.../openvaultdb-page.component.spec.ts:185,209,230`: `'clears records and selection when the target changes'`, `'ignores a query response after its target changes'`, `'replaces a prior allow with authorization from a failed request'`. All three assert the exact behavior Astra's repros describe (stale selection cleared, in-flight response after target switch ignored, prior allow replaced by a real denial from `HttpErrorResponse.error.authorization`).

**Limit, stated plainly per the task's instruction:** this is source evidence that the fix logic and its intended regression test both exist and are internally consistent (the test would fail against the pre-fix code, based on reading both). I did not execute `go test`/`ng test`/the toolchain, so I cannot confirm these tests currently pass in CI, only that they exist and are structurally sound. Treat "Fixed" above as "fix and matching regression test present in source," not "verified green in this session."

**Still open, also source-verified (not merely manifest-trusted):**
- R7: `dalgo2ingitdb/query.go` non-GROUP-BY path (lines 60-88) still has no `Offset()` call; only the GROUP-BY branch (line 193) applies it.
- R8: `dalgo/access/coordinator.go:826-830` still branches `Insert → CandidateImage` / else `→ PreImage` exactly as Opus described.
- R10: `dalgo/access/execution.go:85-109` — `allows` still returns a bare `bool`; `executionDenied` still always emits `CodeExecutionClassDenied`.
- R11/F7: `dalgo/access/write.go:216-244` `checkFields` still always denies with `CodeColumnDenied`, with no separate path for opaque/array coverage.
- R9/F5: `openvaultdb-go/pkg/authorizationapi/request.go:424-426` `validSegment` still permits `.` inside a segment.
- A1: confirmed above, unchanged.

---

## 5. F5 and F7 independently re-assessed against the approved contract

**F5 — ambiguous dotted physical keys.** Opus flagged this as a spec-conformance gap it "could not construct an escalation" for. Checked against **C1** (`03-policy-format.md`, §"Paths, fields, expressions", line 74):

> *"A concrete request uses field segment arrays on the wire, making nested `address.city` unambiguous. **Literal dots in a segment**, ambiguous pattern escapes and whole-object replacements under descendant-only grants **are rejected** by the portable profile until lossless common escaping is specified."*

This is not a "would be nice" — C1 explicitly says such segments **are rejected**. The current `validSegment` (`request.go:424-426`) rejects `/ \ %` and whole `.`/`..` segments but not an interior `.`, so `path:["address.internal"]` (one physical segment literally containing a dot) validates when C1 requires it not to. **This is a confirmed C1 violation**, upgraded from "spec-conformance nicety" to "contract non-conformance," though I concur with Opus that no live escalation is demonstrated (the deny direction still holds, per Opus's own analysis, and SQLite rejects the unknown column outright) — it is a real gap, not a proven exploit.

Distinguishing exact paths from physical row IDs, as instructed: `validSegment` is called only from `validField` (`request.go:427-437`), which is used for `columns`, `changes[].path`, and evidence `requiredFields` — i.e., segmented field-path arrays. `resource.RowID` uses the separate `validString` check (`request.go:295`) and is never passed through `validSegment`. **The fix must not touch `rowId`** — a dot inside a physical record ID is an ordinary opaque key, not a nested-field ambiguity, and C1's "literal dots in a segment" clause is about field-path segments, not record identifiers. **Recommended smallest-scope action:** reject `.` inside any element of a `field` array in `Operation.Normalize`/`validSegment`; leave `rowId` untouched. No new syntax, no new service — exactly Opus's originally proposed fix, now with contract citation and an explicit boundary against touching row IDs.

**F7 — array/opaque composite coverage denies with the wrong code.** Checked against **18-mask-supplement.md**, "Post-review contract clarification":

> *"For fieldMask writes, enumerate affected paths from pre-image and final candidate below every touched parent... **Unknown/opaque composite coverage or unsupported arrays yields `ACL_ENFORCEMENT_UNSUPPORTED`.**"*

This is an exact, unconditional textual match to Opus's F7 citation. `write.go:216-244`'s `checkFields` always denies with `CodeColumnDenied` regardless of whether the refusal is an ordinary mask exclusion or an unsupported composite value — a **direct contract violation**, not merely a "nicer diagnostic" as the provisional treated it. I confirmed (per Opus's cited `mask_enforcement_test.go:91-102`, unchanged in remediation) that the current behavior is still correct on the **allow/deny axis** (fails closed) — this is definitively a known-exclusion-vs-unprovable-coverage code/classification defect, not evidence of an actual authorization hole. **Recommended smallest-scope action:** in `checkFields`/`allowsValue`, have the slice/array/struct routing (`fields.go:350-354` `allowsWhole`) return a distinguishable "unsupported composite" signal instead of a plain bool, and have the caller emit `ACL_ENFORCEMENT_UNSUPPORTED` (scope `column`) for that case specifically, leaving ordinary mask-exclusion denials as `ACL_COLUMN_DENIED`. No new architecture.

---

## 6. Corrected overlap denominators

- Astra findings: **6** (ASTRA-B-001…006).
- Opus numbered findings: **7** (F1…F7). A1 is explicitly not F-numbered in Opus's own report and is handled separately (§3).
- Shared/consensus (same mechanism, same locations): **2** (R1 = ASTRA-001/F2, R2 = ASTRA-002/F1).
- **Unique numbered findings: 6 + 7 − 2 = 11** (R1–R11).
- **A1 is a twelfth, separately classified item** (§3: implementation defect, not a decision item) — the provisional report's "12 de-duplicated findings" count was numerically right but mischaracterized what A1 *was*.

Overlap rate: **2/11 (~18%)** of unique numbered findings are consensus duplicates (the provisional's "2/12 (~17%)" blended A1 into the denominator inappropriately; A1 isn't a numbered finding either reviewer could have "duplicated").

---

## 7. Severity-weighted unique contribution — corrected

The provisional's own crosswalk table already showed this; its prose meta-eval ("each caught real High-severity issues the other affirmatively missed") contradicted it. Recomputed honestly:

| Reviewer | Unique findings | By severity |
|---|---|---|
| Astra | R3, R4, R5, R6 (4) | **2 High** (R3, R4), 2 Medium (R5, R6) |
| Opus | R7, R8, R9, R10, R11 (5) | 1 Medium (R7), **4 Low** (R8, R9(upgraded but still low-severity as a defect, not an exploit), R10, R11) |

**Astra carries all of the unique Critical/High-severity contribution in this exercise; Opus's unique contribution is real but caps at Medium.** This is a factual, not a value, claim — Opus's numbered findings include genuine, contract-citable defects (§5 upgraded two of them), but none of Opus's *unique* catches reach High.

**On why, without overreaching:**
- **R3** (SQLite numeric): Opus's own "No findings" section explicitly claims to have examined "SQLite predicate compatibility (unary `+`, explicit `COLLATE BINARY`, `typeof` guards...)" — the exact mechanism Astra's finding concerns. This is a **stated, examined-area miss**, not merely unexamined territory.
- **R4** (concurrent reload/publish): Opus's "No findings" section covers a **different** mechanism — `Controller.Activate`'s write lock vs. `AcquirePolicyLease`'s `TryRLock` (a per-request coordinator lock), not the in-memory `ownerState.Store`/`securedDatabase` race across the two *management* entry points (`database.go:179-212`, `access_generation.go:60`) that Astra's repro exploits. I therefore do **not** call this a stated false negative the way R3 is — it is more accurately a coverage gap in a related-but-distinct code path Opus did not claim to examine.
- Per `metrics.json`'s `comparabilityNotes`: *"Opus restricted to Read/Glob/Grep. Astra had read-only WB execution and used one SQLite scalar experiment."* R3 in particular was caught via an **executed** scalar check — a **tool-access** difference (Astra could run something; Opus could only read), not evidence that either model is less capable of spotting numeric-coercion bugs by reasoning alone. I am not attributing R3 or R4 to model ability; the honest attribution is tooling and, for R4, examined-path scope.

---

## 8. Cost — corrected arithmetic, actual data only

From `metrics.json` (Opus reviewer's own CLI-reported usage, this is the second-pass **reviewer**, not the reconciler):
- Total cost: **$14.246995** (not the rounded "$14.25" the provisional used throughout, though the rounding itself wasn't the error).
- Astra: cost/token telemetry **unavailable** ("Collaboration API exposes no usage or billing receipt") — no dollar comparison is possible, and I am not estimating one.

Corrected denominators (§6):
- **Per accepted numbered finding:** $14.246995 / 7 (F1–F7) = **$2.0353/finding**.
- **Per Opus-unique numbered finding:** $14.246995 / 5 (F3–F7) = **$2.8494/finding**.
- The provisional's **"$1.90 per Opus-unique finding excluding A1"** figure does not correspond to any defensible denominator (5 unique numbered findings gives $2.85, not $1.90; 7.5 would be needed to reach $1.90, which is not a real count) — it is confirmed to be an **arithmetic error**, as the task flagged, sitting inconsistently next to the correct $2.85 figure for the same underlying set in the same sentence of the provisional report.
- A1 does not have a per-finding cost assigned, since it's a defect requiring a fix in `result.go`, not a separately "found" item distinct from the F-series review effort.

**Original (first) reconciler's own cost, reported separately per instructions**, from `reconciler.json`: **$0.7437484 total** (model usage: `claude-sonnet-5` $0.7422104 + `claude-haiku-4-5` $0.001538; 220,721 ms API duration, 33 turns, 19,706 output tokens against ~1.28M cache-read input tokens). This is distinct from the $14.246995 Opus-reviewer cost above and from whatever cost is attached to this verification pass by the caller.

---

## 9. Human decisions vs. baseline-validity items vs. remediation-closure items (kept distinct)

**Actual human decision needed:** none of the twelve items requires a fresh product/architecture decision. A1 (§3) needs a maintainer to approve the one-line validator relaxation (routine review, not a design call, since the contract already specifies the target behavior). R7 needs a small product choice only in the sense of "fix InGitDB offset, or explicitly downgrade the documented profile and emit `ACL_ENFORCEMENT_UNSUPPORTED` for nonzero offset" — either resolves it; leaving it silently broken does not.

**Baseline validity (was this ever a real defect against the *approved* contract, independent of whether it's since been fixed):** all 11 numbered findings plus A1 are real defects against baseline contract text, confirmed by direct clause citation above (§3, §5) or by reviewer mechanism-vs-current-code correspondence (§2, §4) for the rest. None are false positives, hypothetical-future-profile complaints, or out-of-scope architecture requests.

**Remediation closure (was it fixed in *this* snapshot):** R1–R6 yes (source + regression test present); R7–R11 and A1 no (confirmed unchanged in remediation source, independently re-verified in this session, not merely trusted from the manifest's self-disclosure or from any out-of-snapshot "root" claim).

No high-severity or security-relevant item is called closed here without both a source-level mechanism check and a corresponding test read directly (§4). R1 and R2 are the only Critical/High items in the packet, both closed with adapter-and-HTTP-level tests across both storage engines.

---

## 10. Meta-evaluation

**Unique useful findings:** 11 numbered (R1–R11) + A1, all real. **Overlap:** 2/11 unique numbered findings (~18%) are consensus. **Assessable false positives:** none on either side — every mechanism checked against source matches the reviewer's description, and two (F5, F7) were upgraded from "conformance nicety" to confirmed contract violations on independent re-check.

**Accepted / unique / high, by reviewer:**
- Astra: 6/6 accepted; 4 unique (R3–R6); **2 unique High**, 2 unique Medium. 0 false positives, 0 misses claimed against Astra's own stated scope.
- Opus: 7/7 numbered findings accepted, plus A1 reclassified as a valid defect (not merely a decision item — a point in Opus's favor, since flagging it at all was correct even though its framing needed correction here); 5 unique numbered (R7–R11), **0 unique High/Critical**, 1 unique Medium, rest Low. Two of Opus's five unique findings (F5, F7) were upgraded to confirmed contract non-conformance on independent verification — Opus's instinct that these mattered was right even where its own text hedged ("spec-conformance gap," "correct on the allow/deny axis").

**Costs (actual data only):** Opus reviewer $14.246995 total, $2.04/accepted-numbered-finding, $2.85/unique-numbered-finding. Astra: cost unmeasurable, no comparison possible. First-pass reconciler: $0.7437 (reported separately, not comparable to the reviewer costs above since it's a different task).

**Would Opus alone have sufficed for this packet?** Not with full confidence, but the case is narrower than the provisional claimed. Opus's own "no findings" section explicitly claims to have examined the exact SQLite numeric-compatibility mechanism where a real, since-fixed High-severity bug existed (R3) — a stated miss in a self-declared examined area. Opus did not (by its own scope list) examine the DataTug `openvaultdb-page` component's selection-identity/error-handling logic deeply enough to catch R5/R6, and did not examine the specific in-memory reload/publish race Astra found (R4) — these are coverage gaps rather than contradicted claims. Running Opus alone would very likely have missed R3, R4, R5, and R6 (one confirmed High false negative, one High coverage gap, two Medium coverage gaps) while still correctly finding the two most severe consensus bugs (R1/R2) and 5 lower-severity Opus-unique items.

**Astra's incremental value:** high and concentrated exactly where it matters most (the only two unique High findings in the packet), but its cost is unmeasurable, so "high value" cannot be turned into "cost-efficient" — only "substantively non-redundant," as the provisional correctly noted.

**Future recommendation, role-dependent, one-project caveat:** run both for security-sensitive layered-ACL work of this shape. The two reviewers' blind spots did not overlap: Opus is stronger on broad code-path enumeration and diagnostic/contract-conformance nuance (its unique catches, once verified against contract text, include two genuine conformance violations); Astra's unique catches cluster around concurrency and storage-engine type-coercion edges, which in this run correlated with **having execution capability** (one SQLite scalar check) rather than any demonstrated model-reasoning advantage — a tooling variable, not a capability verdict. This is one project, one run, with Astra's cost entirely unmeasured; treat "run both" as a coverage-optimal recommendation with real uncertainty about whether it is cost-optimal, exactly as the provisional's caveat said, and that part of the provisional's framing is preserved here unchanged.
