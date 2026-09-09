# Layered ACL final implementation review

Status: both blind reviews and independent reconciliation submitted. Remediation
closure and repository gates remain in progress; no publication or merge is claimed.

Both reviewers receive the same immutable [manifest](manifest.json) and
[rubric](rubric.md), with earlier review material removed from the input.
The input contains committed sources and normative contracts, not live worktrees.
Astra B submitted before seeing Opus results; Opus has no access to this report.
Subsequent fixes are tracked separately for closure and must not be mistaken for
code included in the original blind review.

## Independent reviews

- [Astra B](astra-b.md): gpt-6-astra, high effort. Submitted six findings:
  four high security/correctness, two medium client defects. Not approved.
- [Opus](opus.md): submitted seven findings and one contract ambiguity. The
  two blocking findings independently confirm ASTRA-B-001 and ASTRA-B-002.
  Actual runtime selected claude-opus-5, with a small Haiku helper charge.
- [Independent Sonnet reconciliation](reconciliation-verified.md) confirms all
  numbered findings and identifies A1 as an implementation defect against the
  approved contract. Its closure assessment uses the fixed remediation snapshot;
  subsequent commits are recorded below.

## Remediation record

The reconciler accepted all numbered findings. The table below records current
implementation closure separately from the immutable snapshot it assessed.

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

| Run | Tokens: input including cache / output / total | Runtime list-price cost | Elapsed | Accepted / unique numbered findings |
| --- | --- | --- | --- | --- |
| Astra B | Unavailable | Unavailable | Unavailable | 6 / 4; two unique high |
| Opus | 19,379,740 / 53,404 / 19,433,144 | $14.246995 | 979.527 seconds | 7 / 5; zero unique high; A1 separately accepted |
| Reconciler provisional | 1,353,319 / 19,722 / 1,373,041 | $0.7437484 | 220.21 seconds | Not applicable |
| Reconciler verified | 4,128,007 / 29,482 / 4,157,489 | $1.6723536 | 368.672 seconds | Not applicable |

Two duplicate mechanisms among eleven distinct numbered findings give 18.18%
Jaccard overlap. Opus cost about $2.04 per accepted numbered finding and $2.85
per unique numbered finding. These counts exclude its separately accepted A1
contract observation; including it would change the denominator. No Astra cost
per finding or monetary comparison is measurable from available telemetry.

Astra added two unique high-severity fixes (numeric query semantics and owner
activation concurrency) and two medium client fixes. Opus added pagination,
new-row admission, path normalization and diagnostic conformance findings.
Opus alone missed material defects in this packet; both reviews added useful
coverage. Use both for security-sensitive changes of this kind, and choose a
single focused review for lower-risk work where appropriate. This is evidence
from one project, with unequal experiment tooling, not a universal model ranking
or a demonstrated cost-optimal choice.

The first reconciliation could not read the sibling baseline directory. A fresh
independent verification received the same baseline copied into its readable
input directory. Both raw reports and usage records are retained. The verified
report corrects the first report's missing-test claims, A1 framing, and arithmetic.
One sentence in verified section9 mistakenly calls R1/R2 the only high/critical
items; its own crosswalk correctly also rates R3/R4 high. Use the crosswalk for
severity counts. Root's later finding ROOT-002 was not a reviewer contribution.

## Further findings and closure

| Finding | Current action | Evidence |
| --- | --- | --- |
| Opus F1/F2 | Same mechanisms as ASTRA-B-002/001; fixed | InGitDB3229699, DALgo6ef1e00; HTTP evidence regression OVDBe75f9d0 |
| Opus F3 ignored InGitDB offset | Accept/fixed | InGitDB3604dab and OVDB0718d7c; filtering/order/offset/limit and remount regressions pass |
| Opus F4 new-row Where incorrectly overrides Check | Accept/fixed | DALgocc16e7d; Insert and missing Set distinct Where/Check regressions |
| Opus F5 ambiguous physical dotted fields | Accept/fixed | OVDB9d86678; concrete field segments reject literal dots, nested segment arrays and dotted row IDs stay valid |
| Opus F6 callable-mask failure classified as execution class | Accept/fixed | DALgocc16e7d; class/namespace/name distinction and plan regression |
| Opus F7 opaque composite failure reason | Accept/fixed | DALgoce68a45; shared write field checks distinguish unsupported composite coverage from definite exclusion, paired regressions |
| Opus A1 allowed execution with enforced restrictions | Accept/fixed | DALgocb67d4f and Apps5ebb264; success permits fully enforced restrictions, rejects outstanding obligations/blockers; client tests/lint pass |
| ROOT-001 direct DALgo write wrapper drops assessed blockers | Accept/fixed | DALgo23f487d preserves ordered Decisions with legacy first Decision |
| ROOT-002 InGitDB query evaluator differs from policy evaluator | Accept/fixed | InGitDBffb1f12; shared condeval replaces legacy coercion; real query/point regressions cover numeric/text, boolean/text, nested fields, missing/null, IN, ordered comparisons and filtering before limit; full suite passes84.7% |

[Runtime telemetry](opus-runtime.json) and [normalized metrics](metrics.json)
retain actual counters. Input totals include repeated cached contexts, not unique
source text. The CLI cost is a list-price estimate from its runtime, not proof of
an incremental subscription charge. Astra had read-only executable experiments
available and used one; Opus used only Read/Glob/Grep. This tool difference limits
any model-only interpretation of the results.

## Focused post-review predicate follow-up

Astra B independently reviewed InGitDB `ffb1f12`, ran the conformance cases,
and identified ASTRA-B-D1 (medium): synthetic `$id` injection in query predicates
differed from stored-field point evaluation. Accepted and fixed in `3b86e58`: the
protected profile rejects `$id` and `$id.*` predicates before record reads with
`dal.ErrNotSupported`. Exact resource paths and deterministic `$id` ordering
remain available; no synthetic value enters a stored policy image. Tests cover
both absent and conflicting stored `$id` values. Astra B source-reviewed the
follow-up and reported D1 closed with no blockers; it did not rerun tests.

This was a sequential delta review, not part of the original blind comparison.
Its tokens/cost/elapsed telemetry is unavailable and its finding is excluded
from the original review overlap and value counts.
