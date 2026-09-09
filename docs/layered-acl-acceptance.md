# Layered ACL MVP acceptance evidence

Status: implementation, review fixes and local gates pass; publication and
unlinked consumer verification remain active in DALgo's `spec/plans/layered-acl-mvp.md` (tasks 22–24).
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
| DALgo | `GOWORK=off` full pre-push coverage gate reports100.0%; standalone vet passes at `f4f8955` | Provider publication and consumer convergence |
| dalgo2sql | Full suite and vet pass after `4bb140a`; coverage 90.1% >=90%; nested `end2end` and `end2end/sqlite` CGO suites pass | Published dependencies/unlinked build |
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

DataTug's integrated candidate `e45adcdc7ff4480a8b63a7e3fc52c63090106220`
(source `5ebb264` onto fetched main `96e7374`) separately passes 26 unit
tests, lint and production build through normal WB admission. WB's candidate
and exact baseline both report the same five pre-existing SpecScore metadata
errors (stale feature index rows and legacy cross-repository references). These
are recorded as baseline debt, not claimed fixed or hidden by the ACL tests.

SQL's separate `end2end` module was tested using an isolated
`/tmp/acl-sql-e2e.work` containing the shared local provider set plus that nested
module. Its repository-internal parent replace was preserved. Both packages
passed in the compiler container through normal WB admission; no tracked
dependency files changed for this check.

DALgo's final standalone command was
`GOWORK=off wb run -- sh .githooks/pre-push`, followed by
`GOWORK=off wb run -- go vet ./...`. The repository's existing gate reports
100.0% for each package and total coverage. This is the tool-reported percentage,
with its normal rounding; no threshold, exclusion, or hook bypass changed.

The integrated DALgo source `2e3a58c` (including fetched main's transaction
identity changes and the plan metadata correction) independently passes the
same standalone pre-push gate. Its full `GOWORK=off go test -race ./... -p=2`
also passes in the compiler container through WB with normal admission.
