# Layered ACL MVP acceptance evidence

Status: implemented locally; final reviews, coverage and publication gates are
still active in DALgo's `spec/plans/layered-acl-mvp.md` (tasks 22–24).
Policy viewer/editor is excluded by approved scope.

## Vertical slice

| Acceptance | Executable evidence | Observed |
| --- | --- | --- |
| Real DTQL queries through OVDB to both engines | `pkg/server/layered_acl_test.go`, `pkg/server/access_write_test.go` | Pass on SQLite and InGitDB |
| Owner restrictions compose before paging; hidden filter/order/projection rejected | Layered HTTP tests; SQL structured predicate conformance | Pass |
| Direct InGitDB access retains non-removable owner policies | InGitDB protected profile tests | Pass |
| Plan/inspect never writes; specific row and bounded top-N diagnostics | Protected HTTP tests; DALgo coordinator tests | Pass |
| Exact-field evidence respects each deciding row rule | DALgo conditional evidence regression `6ef1e00` | Pass; forbidden field returns no evidence |
| Real normalized UPDATE agrees with inspection | Protected HTTP tests for both engines | Pass |
| One write returns independent owner blockers with provenance | Protected HTTP multi-owner denial | Pass |
| Hidden/missing point probes redact consistently | Protected HTTP GET/evidence/inspect cases | Pass |
| Fresh evidence, whole-image revision/CAS, stale conflict, reread | Protected HTTP tests; adapter HMAC/receipt tests | Pass |
| Nested updates preserve authorization pre-image | InGitDB `TestProtectedNestedOwnershipCannotRewritePreImage`, map evidence tests | Pass |
| Concurrent publication cannot undo revocation | InGitDB `TestMountedReloadCannotReinstallRevokedGeneration` | Pass |
| Numeric query/evaluator agreement before limit | SQL `TestSQLiteNumericPolicyConformanceBeforeLimit` | Pass |
| Policies and writes survive remount/reconnect | Protected HTTP remount tests | Pass |
| Browser→daemon→OVDB→engine read/Explain/evidence/UPDATE/reread | DataTug Playwright `openvaultdb` project | 2/2 pass |
| Browser holds no source credential, removes launch fragment, cannot redirect proxy | DataTug real-stack tests and proxy unit tests | Pass |
| Selection cannot migrate to another target; actual denial replaces stale allow | Apps `7e3e548` tests | Pass |

Schema discovery, opaque native execution and unsupported SQLite boolean
predicates deliberately fail closed on the documented profile. HTTP offers a
single normalized row UPDATE; generic protected point operations/batches exist
in DALgo without advertising a broader HTTP transaction surface.

## Gate evidence

Tests use the external local Go workspace; no unpublished module replaces are
committed. Final ordinary module builds must run against published dependencies.

| Repository | Verification | Remaining |
| --- | --- | --- |
| DALgo | Legacy packages and DTQL reach 100%; security regression and access tests pass | Access and authorization wire package coverage to unchanged 100% gate |
| dalgo2sql | Full suite and vet pass after `27e1c00` | 90% coverage gate; prior profile 85.4% |
| dalgo2ingitdb | Full suite after `3229699`: 84.7% | CGO-dependent race run when compiler available; gate is 80% |
| dalgo2openvaultdb | Full suite after `cacd422` passes | Published dependency/unlinked build |
| OpenVaultDB | Full suite and HTTP conformance pass after error-mapping fix; coverage 60.6% | Published dependency/unlinked build; gate is 55% |
| DataTug CLI | Focused proxy/config tests pass | Full CGO gate requires C compiler; failed non-CGO profile is not acceptance evidence |
| DataTug apps | 22 unit tests, lint, production build, real browser 2/2 | Unlinked dependency/release integration |

No remote CI, release or merge receipt is represented by this local evidence.
