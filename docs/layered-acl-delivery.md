# Coordinated ACL delivery

Status: DALgo PR158 is published; CI lint repair and a repository branch-policy
decision precede provider publication.
The approved DTQL specification packet and independent HTTP adapter fix have
landed. The main ACL provider/consumer wave is not yet published.

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
schema freshness and no-breaking-change checks passed; six stricter remote
lint findings are being repaired before requesting the policy decision below.

## Required human decision: DALgo strict required checks

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
