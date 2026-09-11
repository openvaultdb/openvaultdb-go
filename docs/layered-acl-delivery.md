# Coordinated ACL delivery

Status (2026-09-11): task 24 remains in progress. DALgo PR158 landed at
`8dece8b` and released `v0.80.0`; the prior strict-check question was approved
and resolved. The historical observations below describe the September 9 run
and are not current blockers.

The resumed publication wave rebases the existing implementation onto current
origin/main and opens review PRs. The lead Claude session reviews every PR.
The agent lands only dal-go/*, ingitdb/* and openvaultdb/* after that review
passes, using `wb pr land --approved-by <review comment URL>`; the lead owns
DataTug landing. No policy viewing/editing is included.

Current review PRs: [DALgo #159](https://github.com/dal-go/dalgo/pull/159),
[SQL #181](https://github.com/dal-go/dalgo2sql/pull/181),
[InGitDB #9](https://github.com/ingitdb/dalgo2ingitdb/pull/9).
OpenVaultDB uses immutable reachable adapter commit pseudo-versions for
unlinked review validation. After provider landing, converge to observed
release tags on the same consumer PR before landing; do not hand-tag.

The former DataTug worktrees were discarded by WB on September 11 at
16:46–16:47 UTC. Their retained commits were recovered to origin branches
`feat/layered-acl-query`. Clean replacement worktrees are at each repository's
`.worktrees/acl24-datatug-publication`. Replay exposed the deliberate
contract conflicts documented in datatug/datatug
`spec/research/2026-09-09-layered-acl-reconciliation.md`: authentication,
API envelopes, source targeting, settings ownership and URL conventions.
The old replays were aborted; task 24 now reconciles the source with the current
catalog resolver, fixed secureread session and existing query error envelope.
[DataTug CLI #237](https://github.com/datatug/datatug-cli/pull/237) proves real
remote DTQL reads against SQLite and InGitDB with policies, preserves structured
owner denials and retains the bounded SDK. The app companion displays blocker
codes in the existing query page. Original heads are preserved at each repo's
`recovery/acl-task21-original`. Browser write/Explain convergence with the lead's
current API remains an explicit acceptance item; SDK methods are not a browser
E2E receipt.

Local unlinked validation: DALgo 100.0% coverage and SpecScore 0 violations;
SQL 90.6%; InGitDB 84.8%; OpenVaultDB 59.7%, including conformance and
protected writes. SQL's nested CGO E2E now passes using a temporary rootless GCC toolchain.
Local ownership and Work Logs refreshed; fleet remote claim refresh/release
is unavailable because the configured WB HTTP hub does not support claims.

The [WB graph](layered-acl-release-graph.json) records the six Go repositories
and their fetched main-branch dependencies. It also identifies the nested SQL
`end2end` module; its existing repository-internal replace remains distinct from
an unpublished cross-repository link.

## Ordered wave

1. Complete independent review/reconciliation, regressions, and the unchanged
   repository gates. Preserve exact local commits and acceptance receipts.
2. Validate DALgo with `GOWORK=off` against its own module dependencies. Land
   through WB and observe the exact target CI and immutable provider release.
3. Update the existing SQL and InGitDB implementation worktrees to the released
   DALgo version. Include SQL's nested end2end manifest in dependency convergence.
   Validate and land these provider adapters once each. The HTTP adapter's small
   unsupported-error mapping fix can land in this provider wave as well.
4. Wait for observable immutable adapter releases, then update OpenVaultDB and
   DataTug CLI once with the complete provider set. Align published transitive
   versions with the tested workspace, particularly `dalgo2sqlite v0.1.8`.
   The tested workspace also uses `record v0.1.3` and
   `ingitdb-go/ingitdb v0.6.0`; verify the final selected versions explicitly.
5. Run normal unlinked builds and the real browser→daemon→OVDB→SQLite/InGitDB
   acceptance before final consumer landing. Land DataTug apps with its matching
   daemon contract, and land the approved DTQL specification packet.
6. Observe exact post-target CI/release receipts, synchronize each clean canonical
   checkout through WB, then perform audited cleanup. Mark SpecScore task24
   complete only after these receipts exist.

Use the same WB campaign/PRs on retries. A published provider version is required
before unlinking its consumers; no local replacement or unpublished `go.work`
entry may be hidden from landing validation. Preserve unrelated canonical work.

## Release observations requiring follow-through

`dalgo2openvaultdb` currently disables automatic version bumping in its workflow.
Its mapping fix must be available through an observed immutable published version
or exact reachable commit pseudo-version before OVDB's normal conformance build
can consume it. Do not wait indefinitely for a disabled automatic tagger or claim
an unobserved release. The final receipt must record which supported route was
used.

The source branches may be behind moving default branches. WB prepares isolated
integration candidates against freshly fetched targets and owns conflict and
exact-head validation. A green old source commit alone is not a merge receipt.

## Observed delivery receipts

| Repository | Result | Exact evidence |
| --- | --- | --- |
| datatug/dtql | Landed and cleaned; canonical main synchronized | `c2d048784ef982a0533b1cdfb912f13182ba6b53`; WB receipt `merge-datatug-dtql-main-af26ed96bb47-f7a9cf2ed0f7`; authoritative unprotected direct route, no applicable checks confirmed by stable reread |

No Go provider release is claimed by the DTQL documentation landing.

The independent HTTP adapter fix landed at
`cacd422564fc27844b989e1c80eb96320ce4af4b` through WB receipt
`merge-dal-go-dalgo2openvaultdb-main-8d8affc8e30d-faa17cb17b4b`. Canonical main is
synchronized; [exact-head CI](https://github.com/dal-go/dalgo2openvaultdb/actions/runs/34350377697)
passed lint and build/test, with version bump intentionally skipped. Go resolves
the immutable commit as `v0.3.2-0.20260909113030-cacd422564fc`. Consumer pins
will change once alongside the other ready providers. Its worktree remains in
the local workspace until that convergence, then WB cleanup can finish.

## DALgo receipt recovery

The main DALgo delivery attempt is preserved in receipt
`merge-dal-go-dalgo-main-e3e63fce3822-3fc819a37a2c`. Its original source identity
is recorded in `source_refreshes`; a subsequent source refresh resolved an
overlapping test-file addition. Candidate code validation passed, then SpecScore
identified a stale plan frontmatter status. The source owner corrected that
metadata and the integrated source at `2e3a58c` passes the standalone coverage
gate (reported 100.0%).

WB 0.120.4 rejected audited supersession because the conflict recovery path
compared the operation identity only with the refreshed source set. The narrow
fix landed in [WB PR469](https://github.com/sneat-dev/wb/pull/469) at
`90a1f59c445b465de83cc54754c8793ad24c3b32`, released as `v0.121.1`.
Focused regressions, full orchestrator tests and vet passed; exact candidate
and post-target CI passed. WB's local merge gate recorded the same
repository-relocation test failure on candidate and baseline; no gate was
overridden. The installed Go module resolves the exact merged revision to
`v0.121.1`.

Audited supersession succeeded, retaining the historical receipt and appending
its hash-bound acknowledgement. The successor receipt is
`merge-dal-go-dalgo-main-e3e63fce3822-c23007e4bd4d`. DALgo candidate `2e3a58c`
passed standalone vet, tests, build and SpecScore and was published in
[PR158](https://github.com/dal-go/dalgo/pull/158). An interrupted GitHub API read
was recovered through the same WB prepare/land path. Remote build/tests,
schema freshness and no-breaking-change checks passed. The six stricter remote
lint findings were repaired in `c8adfb6781756938250573e7da531cee02ce9418`.
Standalone golangci-lint reports zero issues, access tests pass, and SpecScore
reports zero violations. The same PR now carries this exact head; its
[remote lint and build/test run](https://github.com/dal-go/dalgo/actions/runs/34360498361)
passes, alongside schema freshness and no-breaking-change. Release-only jobs
are skipped on the PR as configured.

A repeated incomplete JSON response while WB looked up the newly published
head left the successor receipt in `land/conflict` without its PR reference.
GitHub independently confirms PR158 at the exact published head and all checks
terminal. Preserve that receipt and PR; reconcile the interrupted publication
through supported WB recovery before resuming automated delivery. No receipt
has been edited by hand. The branch-policy refusal below is independently
verified and still requires a human decision even after receipt recovery.

WB PR469's source and candidate were retired through audited cleanup. Its
merge receipt is complete after a hash-bound missing-cleanup acknowledgement
corroborated both absent assets and the exact landing. The original source had
been rebased before publication, so GitHub could not look up that unpublished
source SHA during automatic PR reconciliation.

## Historical resolved decision: DALgo strict required checks

The read-only GitHub branch-protection response for `dal-go/dalgo` main on
2026-09-09 reports `strict: false` with these existing checks, all pinned to
GitHub App15368:

- `no-breaking-change`
- `strongo_workflow / Lint`
- `strongo_workflow / Build & test`

WB refuses automatic PR merging without a nonempty, server-enforced strict
up-to-date requirement. Recommended change: enable **Require branches to be up
to date before merging** for this branch, preserving the existing checks and
their App identities. This strengthens the policy for every future PR to
DALgo main; it is a repository-administration decision, separate from the
approved ACL code changes. No branch-protection setting has been changed.

The concrete API payload, after approval and a fresh read confirming the
same checks, is `{"strict":true}` to
`PATCH /repos/dal-go/dalgo/branches/main/protection/required_status_checks`.
Omit `contexts` and `checks` so the existing requirements remain intact.
Read back the setting and App-pinned checks, then resume the existing WB receipt.
Alternatively, a repository administrator may merge the green PR manually;
the agent must still verify the exact landing and finish release/cleanup
through WB. Do not disable required checks or bypass the WB refusal.

The same read-only classic-protection check found DataTug CLI already strict;
SQL, InGitDB adapter, OVDB and DataTug apps returned “Branch not protected.”
Those observations are not substitutes for WB's full ruleset/route evaluation.
