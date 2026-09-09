# Layered ACL MVP acceptance evidence

Status: implemented locally; review fixes, coverage and publication gates are
still active in DALgo's `spec/plans/layered-acl-mvp.md` (tasks 22–24).
Policy viewer/editor is excluded by approved scope.

## Vertical slice

| Acceptance | Executable evidence | Observed |
| --- | --- | --- |
| Real DTQL queries through OVDB to both engines | `pkg/server/layered_acl_test.go`, `pkg/server/access_write_test.go` | Pass on SQLite and InGitDB |
| Owner restrictions compose before paging; hidden filter/order/projection rejected | Layered HTTP tests; SQL structured predicate conformance | Pass |
| Direct InGitDB access retains non-removable owner policies | InGitDB protected profile tests | Pass |
| Plan/inspect never writes; specific row and bounded top-N diagnostics | Protected HTTP tests; DALgo coordinator tests | Pass |
| Exact-field evidence respects each deciding row rule | DALgo `6ef1e00` and `TestProtectedHTTPEvidenceConditionalFieldFallback` on both engines | Pass; forbidden field returns no evidence |
| Real normalized UPDATE agrees with inspection | Protected HTTP tests for both engines | Pass |
| One write returns independent owner blockers with provenance | Protected HTTP multi-owner denial | Pass |
| Hidden/missing point probes redact consistently | Protected HTTP GET/evidence/inspect cases | Pass |
| Fresh evidence, whole-image revision/CAS, stale conflict, reread | Protected HTTP tests; adapter HMAC/receipt tests | Pass |
| Nested updates preserve authorization pre-image | InGitDB `TestProtectedNestedOwnershipCannotRewritePreImage`, map evidence tests | Pass |
| Concurrent publication cannot undo revocation | InGitDB `TestMountedReloadCannotReinstallRevokedGeneration` | Pass |
| Numeric query/evaluator agreement before limit | SQL `TestSQLiteNumericPolicyConformanceBeforeLimit` | Pass |
| InGitDB query/point predicate agreement | InGitDB `ffb1f12`, `TestProtectedQueryPredicateMatchesPointAuthorization`: numeric/text, boolean/text, missing/null, nested fields, IN and ordering | Pass; denied rows removed before limit |
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
| DALgo | Legacy packages and DTQL reach 100%; security regression and access tests pass | Access coverage to unchanged 100% gate; all other packages now 100% |
| dalgo2sql | Full suite and vet pass after `4bb140a`; coverage 90.1% >=90% | Published dependencies/unlinked build |
| dalgo2ingitdb | Full suite after `ffb1f12`: 84.7%; focused ownership/evidence/reload tests pass with race detector in compiler container | Published dependencies/unlinked build; gate is 80% |
| dalgo2openvaultdb | Full suite after `cacd422` passes | Published dependency/unlinked build |
| OpenVaultDB | Full suite and HTTP conformance pass after error-mapping fix; coverage 60.6% | Published dependency/unlinked build; gate is 55% |
| DataTug CLI | Full CGO suite passes in isolated compiler container; coverage 59.1% >=55.5% | Published dependencies/unlinked verification |
| DataTug apps | 26 unit tests, lint, production build, real browser 2/2 at Apps5ebb264 | Unlinked dependency/release integration |

These test rows record local evidence. Remote delivery receipts are tracked
separately in `layered-acl-delivery.md`.


The isolated local image `layered-acl-go-cgo:local` supplied GCC/libc headers
for CGO tests without changing the host toolchain. InGitDB's focused race command
ran through WB without admission overrides. The CLI verification agent reported
using `WB_ADMISSION_LOAD_FLOOR=100` for its container invocation; this is a
resource-admission process deviation, not an approved gate change. The complete
suite result is recorded as observed; subsequent work must respect normal WB
admission and no coverage threshold was changed.
