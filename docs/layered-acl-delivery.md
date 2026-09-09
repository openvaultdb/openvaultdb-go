# Coordinated ACL delivery

Status: queued behind final review closure and required coverage. No pushes,
merges or releases have been performed for this implementation.

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
