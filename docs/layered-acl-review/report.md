# Layered ACL final implementation review

Status: in progress. No final approval, publication or merge is claimed.

Both reviewers receive the same immutable [manifest](manifest.json) and
[rubric](rubric.md), with earlier review material removed from the input.
The input contains committed sources and normative contracts, not live worktrees.
Astra B submitted before seeing Opus results; Opus has no access to this report.
Subsequent fixes are tracked separately for closure and must not be mistaken for
code included in the original blind review.

## Independent reviews

- [Astra B](astra-b.md): gpt-6-astra, high effort. Submitted six findings:
  four high security/correctness, two medium client defects. Not approved.
- Opus: first attempts returned API 429/session limit before any inference.
  A new read-only review started after the stated 11:30 UTC reset. Result pending.
- Separate reconciliation: pending both submitted reviews. No overlap or
  reviewer-value conclusion is valid yet.

## Remediation record

Acceptance here is the implementation owner's provisional assessment; the
independent reconciler will assess the findings and closure evidence.

| Finding | Action | Evidence |
| --- | --- | --- |
| ASTRA-B-001 conditional field evidence disclosure | Accept/fixed | DALgo `6ef1e00`; pinned deciding-field grants, two-owner fallback regression |
| ASTRA-B-002 nested InGitDB candidate aliases pre-image | Accept/fixed | InGitDB `3229699`; ownership takeover denied, nested delete/replacement and map-file evidence isolation |
| ASTRA-B-003 SQLite INTEGER vs DALgo binary64 predicate mismatch | Accept/fixed | SQL `27e1c00`; real secured queries vs evaluator around ±2^53, INTEGER/REAL/float32/text, all comparisons and IN, limit 1 |
| ASTRA-B-004 stale owner reload reinstalls revoked generation | Accept/fixed | InGitDB `3229699`; mounted activation serialization, deterministic reload barrier, authoritative denial after concurrent publication |
| ASTRA-B-005 stale cross-target DataTug selected row | Accept/fixed | Apps `7e3e548`; immutable result target/collection, invalidation and late-response tests |
| ASTRA-B-006 stale allow and missing actual failure blockers | Accept/fixed | Apps `7e3e548`; validated error authorization replaces prior assessment, conflict-specific message; browser E2E 2/2 |

Additional verification found generic query HTTP unsupported-profile errors lost
classification. OVDB and dalgo2openvaultdb now preserve safe unsupported status;
the shared DALgo conformance suite recognizes the documented unsupported boolean
predicate profile instead of treating it as an unexplained denial.

## Usage and value metrics

| Reviewer/run | Input/output/total tokens | Cost | Elapsed | Accepted / unique / high-value findings |
| --- | --- | --- | --- | --- |
| Astra B | Not available from collaboration API | Not available | Exact duration not available | Pending reconciliation; 6 provisionally accepted, including 4 high |
| Opus rejected attempts | 0 / 0 / 0 reported | $0 reported | Initial API response 495 ms | No review produced |
| Opus after reset | Pending actual CLI JSON | Pending | Pending | Pending |
| Reconciler | Not started | Not started | Not started | Not applicable |

Overlap percentage, cost per accepted/unique finding, incremental second-review
value and whether either reviewer alone would have sufficed are pending. Missing
telemetry will remain explicitly unavailable; no price or token estimates are
substituted for measured billing. Conclusions will be evidence from this project,
not a universal model ranking.
