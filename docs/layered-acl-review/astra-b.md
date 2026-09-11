Model identifier: gpt-6-astra
Reasoning effort: high
Token/cost telemetry: unavailable

Review completed independently against the immutable packet and manifest commits. **Not ready to approve: four high-severity security/correctness findings and two medium-severity client findings.** No critical or low findings identified.

1. **ASTRA-B-001 — High: evidence reads bypass the actual row’s field restrictions.**

   **Locations:** `dalgo/access/assessment.go:257`, `dalgo/access/coordinator.go:842`, `dalgo/access/coordinator.go:604`.

   `writeMayAllowField` accepts a requested field if *any* alternative permits it. That is useful for static planning, but `evaluateEvidence` never resolves the deciding alternative’s fields for Get. `Evidence` subsequently returns the requested value directly from the complete pre-image.

   **Concrete repro:** Configure a higher-priority conditional Get rule exposing `secret` on owned rows, followed by an unconditional Get rule exposing only `name`. Request evidence for `secret` on somebody else’s row. Static validation accepts the field because the conditional alternative permits it; the terminal allow removes the row residual; evidence returns the secret. Ordinary Get correctly uses `decidingFields` in `access/condition.go:91` and redacts it.

   **Impact:** Authenticated callers with ordinary record-read capability can retrieve protected values through `/access/evidence`, on either adapter, without diagnostic or administrative authority.

   **Fix/test:** Resolve each owner’s deciding field set against the pinned pre-image before permitting every requested evidence path. Reuse ordinary-read semantics, including complete-subtree mask checks. Add paired Get/evidence tests with mutually exclusive conditional field grants, terminal fallback, and two owners. The hidden field request must fail entirely, never return its value or an “absent” assertion. Existing evidence tests cover a single applicable field grant or complete row denial, not this combination.

2. **ASTRA-B-002 — High: InGitDB nested UPDATE corrupts the authorization pre-image.**

   **Locations:** `dalgo2ingitdb/protected.go:356`, `protected.go:374`, `protected.go:384`, `protected.go:496`; `tx_readwrite.go:426`.

   The adapter’s `cloneMap` copies only top-level entries. A nested candidate therefore shares child maps with `pre`. `applyFieldUpdate` mutates those shared maps before the adapter records its supposedly complete pre-image.

   **Concrete repro:** Store `meta.ownerID: victim`. Allow UPDATE where `meta.ownerID == currentUser`, permitting `meta.ownerID` and `name`. As another user, submit changes setting `meta.ownerID` to that user and updating `name`. Candidate construction also changes the pre-image’s owner ID. Both the pre-image predicate and final check now see the attacker’s ID and allow the operation.

   **Impact:** Supported nested HTTP mutations can bypass owner row predicates. Inspection can also report false authorization because it assesses a fabricated pre-image. This does not require computed fields or transforms.

   **Fix/test:** Deep-copy complete JSON-shaped images before candidate mutation and throughout evidence cloning. Add protected adapter and HTTP tests asserting that a nested ownership change on another principal’s record denies, preserves disk bytes, and leaves the original evidence unchanged. Repeat for nested deletion/parent replacement and both supported record-file layouts.

3. **ASTRA-B-003 — High: SQLite numeric predicates can return rows DALgo denies.**

   **Locations:** `dalgo2sql/sqlite_emit.go:174`; `dalgo/condeval/condeval.go:262` and ordered comparison beginning around line 312.

   DALgo normalizes numbers through JSON to float64. SQLite’s emitted comparison retains exact INTEGER comparison semantics. Type guards and binary collation do not reconcile this difference.

   **Concrete repro:** Store INTEGER `score = 9007199254740993`; authorize rows where `score > 9007199254740992`. DALgo rounds both operands to the same float64 and rejects the predicate. SQLite selects the row. Secured query readers redact fields but do not re-evaluate row predicates before returning the result.

   **Executed evidence:** A WB-governed Python SQLite check using the emitted predicate returned `[('hidden',)]` with both integer and floating-point bound parameters. The corresponding float64 comparison returned `False`. This was a scalar engine check, not a full Go/HTTP exploit test.

   **Impact:** Query and point-inspection authorization disagree; query execution can disclose denied rows before pagination. The documented supported profile does not exclude such stored INTEGER values.

   **Fix/test:** Make numeric execution semantics match the shared evaluator, or explicitly fail closed for an enforceably restricted numeric profile. Add conformance tests around ±2^53 and INTEGER/REAL mixtures that compare protected query results with DALgo Get/inspection, including `limit: 1`. Rejecting only large literal parameters is insufficient because the stored value causes this example.

4. **ASTRA-B-004 — High: concurrent InGitDB publication/reload can reinstall revoked policies.**

   **Locations:** `dalgo2ingitdb/database.go:179–185`, `database.go:196–212`; `access_generation.go:60`.

   The filesystem lock protects controller publication/reload, but is released before the mounted handle installs the returned snapshot. No mutex or generation check serializes that installation.

   **Concrete schedule:** Reload A captures allowing generation G1 and returns from the controller; pause A before `ownerState.Store`. Publisher B commits denying generation G2, reloads it, installs G2, and returns success. Resume A: it stores G1. Subsequent admissions use the old permissions despite committed HEAD and the successful publication identifying G2. The publication method’s own reload/store has the same race.

   **Impact:** A completed revocation can be undone in memory indefinitely until another reload/remount. Cooperating concurrent management calls suffice; no malicious callback or filesystem tampering is needed.

   **Fix/test:** Serialize mounted publication/reload through snapshot installation, ensuring activation corresponds to the authoritative generation before reopening admissions. Add a deterministic barrier test for the schedule above and assert that no post-publication read/write can use G1. Existing examined publication tests exercise sequential activation and crash recovery.

5. **ASTRA-B-005 — Medium: changing the selected database can redirect a previously selected row’s UPDATE.**

   **Locations:** `datatug-apps/apps/datatug-app/src/app/openvaultdb/openvaultdb-page.component.html:18`; `openvaultdb-page.component.ts:171–194`.

   Database changes update `targetId` without clearing or rebinding records and selection. `updateSelected` combines the old selected key with the *current* target, then obtains fresh evidence from that target.

   **Repro/impact:** Query database A, select `customers/01`, switch to database B containing the same key, and save. The page still displays A’s selected record, but modifies B’s record. Fresh CAS succeeds because it was obtained from B; CAS does not protect selection identity.

   **Fix/test:** Bind query results and selections to immutable target/database/collection identity; clear them when that identity changes. Capture the full mutation target before asynchronous work. Add a two-target UI test asserting that switching targets prevents updating the old selection.

6. **ASTRA-B-006 — Medium: real query/write authorization failures discard structured blockers and can leave a stale allow displayed.**

   **Locations:** `datatug-apps/apps/datatug-app/src/app/openvaultdb/openvaultdb-page.component.ts:274–275`, `:295–316`.

   HTTP failures only set an error message. They do not consume `error.authorization` or replace the previous authorization result. OpenVaultDB’s protected-write denial envelope frequently has structured authorization without a message, so the UI shows a generic failure while retaining an earlier successful assessment.

   **Impact:** The approved DataTug structured-error/Explain flow does not explain actual failed operations and can display an unrelated prior allow beside a denial.

   **Fix/test:** Validate and display authorization envelopes from failed query/write responses, clear stale assessments when starting a new operation, and render revision conflicts distinctly. Test an initial allow followed by a real 403/404 with blockers; the current denial must replace the previous result.

Scope examined: normative C1–C6 completion, tracked implementation plan and supported-profile documentation; DALgo coordinator, assessment, conditional field/read/write enforcement, principal/execution classification; SQLite compiler and protected storage; InGitDB protected preparation, mutation, publication/reload; OpenVaultDB normalization, inspection/evidence/write/sample projection; DataTug proxy credentials and client interaction/error handling, plus relevant tests.

Limits: source review was targeted, not exhaustive. I did not modify exported code, consult other reviews/live worktrees, rerun full repository suites, or execute full HTTP/browser repros. Apart from the recorded SQLite experiment, the findings provide precise code-path repros and proposed regression tests.

No human product decision is needed to resolve these defects. Numeric compatibility has implementation trade-offs, but silently different authorization semantics are outside the approved contract. Explicit unsupported native/computed/schema profiles, omitted policy administration UI, and the single-row HTTP write scope are not findings.

Coverage, dependency publication, unlinked builds, CI, and merges remain separate known delivery gates; their eventual success would not resolve these findings.
