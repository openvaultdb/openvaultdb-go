# Independent layered ACL implementation review

Review this immutable source packet independently. Do not read any other
reviewer's output, live agent messages, or prior review reports. The other
reviewer receives this same material and rubric. Submit your findings before
seeing any reconciliation. Do not edit code or start implementation.

## Approved scope

The user approved implementation after design and implementation-plan review.
Priority: real DTQL through OpenVaultDB to local SQLite and InGitDB, with
independently enforced owner policies; then protected UPDATE, safe inspection,
structured blockers, and DataTug query/write/Explain. Policy viewer/editor and
HTTP policy CRUD are excluded from this MVP. No standalone InGitDB server;
OpenVaultDB is its remote server. Generic DALgo point operations support
insert/set/update/delete; the first normalized HTTP write is one-row UPDATE.
Do not demand a new standalone ACL language, external ACL service, or
long-running logical transaction protocol.

DTQL owns the portable format/wire contract, DALgo reuses its existing evaluator,
each data owner enforces its own policies, DataTug connects/displays/explains.
The approved composition is `dalgo-hierarchical-v1`; assess implementation
against its documented semantics rather than replacing it with another global
allow/deny convention. Public-default policy documents are distinct from
owner-authorized diagnostics and data read visibility. Self dry run is allowed;
Explain as another principal requires each owner's authority. Policy admin
alone grants no protected row access.

## Reading order and code map

1. `manifest.json` fixes every repository commit. Files are git exports of
   those commits; paths containing `review` were excluded to avoid anchoring.
2. `dtql/design/layered-acl/15-contract-completion.md` is the normative C1–C6
   completion; read companion architecture, identity, policy and security docs
   and contract fixtures as needed. `dalgo/spec/plans/layered-acl-mvp.md` is the
   tracked implementation plan. Completed features do not imply publication.
3. `openvaultdb-go/docs/layered-acl-implementation.md` describes the actual
   supported profile, remaining limits and runnable example.
4. DALgo: `access/coordinator.go`, assessment/purity/decision/principal/rule/
   field/query/write enforcement, portable document loader/masks/provider,
   `dtql/authorization` wire validation, and corresponding tests. Code shape
   imports wire DTOs through `dtql/authorization`, the cycle-safe implementation
   of the design's proposed `access/contract` package.
5. SQLite: `sqlite_protected.go`, `sqlite_emit.go`, readers, options/factory,
   `sqlite_protected_test.go`, `structured_sql_test.go`.
6. InGitDB: `protected.go`, owner policy publication/recovery, transaction
   boundary, candidate-file revisions, stored-only/computed-field guards and
   corresponding tests.
7. OpenVaultDB: `pkg/authorizationapi`, `pkg/server/access*.go`, records/DTQL/
   identity middleware, `pkg/core/access*.go`, policy stores/leases, mounts,
   `pkg/server/access_write_test.go` and layered/reload/auth tests.
8. DataTug CLI: `pkg/openvaultdb`, `pkg/server/openvaultdb_proxy*`, settings and
   serve command. Apps: `apps/datatug-app/src/app/openvaultdb`, launch-token
   bootstrap/routes, `apps/datatug-app/e2e/openvaultdb` and Playwright config.

## Current evidence and delivery state

- Full DALgo tests/vet passed after the coordinator/purity implementation.
- SQLite full tests/vet passed; later typed unsupported-query projection passed
  focused reader/SQLite tests.
- Full InGitDB tests passed, including same-file multi-row receipt revision,
  hidden sibling CAS, rollback and policy recovery tests.
- OpenVaultDB server/core/mount/policy-store/ingress suites passed. HTTP tests
  prove read/inspect/evidence/UPDATE/CAS/sample/blockers and reconnect on both
  engines. Private/missing point probes use the safe generic projection.
- DataTug unit tests: 19 passed. Real browser→CLI→OpenVaultDB→SQLite/InGitDB
  Playwright flows: 2 passed, including plan/row/top-N Explain, evidence-backed
  UPDATE/reread, token fragment removal and log/header isolation. No HTTP mocks
  replace the final E2E path.
- Coverage work continues separately to meet DALgo's unchanged 100% delivery
  gate. Remote dependencies still use local development links; publication,
  released-version verification and merges are pending. Treat these as known
  delivery gates, not evidence that remote CI has passed.

## Rubric

Assess correctness, security, completeness, unnecessary complexity, reuse of
existing architecture, DTQL/DALgo boundaries, stable identity and trusted
principal propagation, layered enforcement, structured blockers and diagnostic
privacy, DataTug UX, implementation feasibility, testability, MVP scope and
future extensibility without scope creep.

Focus on adversarial operation normalization, masks and exact affected fields;
row/column restrictions before pagination; locks/policy leases and complete
candidate evidence; CAS over hidden fields; no dry-run mutation or impure
callback replay; unavailable-owner aggregation; capability/layer isolation;
private policy cardinality and row existence leaks; actual browser credentials;
owner publication/recovery; and engine scalar/predicate compatibility.

Inspect code and tests, not only documentation. Prefer a concrete repro or
precise code path. Distinguish exploitable behavior from trusted-host setup
misuse, explicit unsupported profiles, and future wishes. Explain whether an
issue violates the approved contract or is an optional improvement. Where
existing tests contradict a suspicion, resolve that discrepancy before
reporting. Do not invent a replacement architecture to address a small defect.

## Required output

Return a concise review with:

- Scope actually examined and any material limits.
- Findings with stable IDs, severity (critical/high/medium/low, or future/out
  of scope), affected file/line, concrete rationale/repro, impact, recommended
  action, and a test that would demonstrate the fix.
- Any contract ambiguity/human decision, with trade-offs.
- Overall readiness, considering the known delivery gates separately.

If no findings at a severity, say so. Do not manufacture a finding to fill the
rubric. Monetary/token metadata will be recorded by the orchestrator where
available; do not estimate it yourself without measurable evidence.
