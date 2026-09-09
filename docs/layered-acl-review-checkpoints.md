# Layered ACL implementation review checkpoints

These are bounded implementation reviews during development, not the final
independent Astra/Opus reviews. Reviewers saw evolving local code; final review
will use identified commits and the same rubric. No usage/cost telemetry was
available for these agent messages, so no token or monetary estimate is claimed.

## HTTP and owner-boundary review

Reviewer: `query_security_audit`, implementation agent using the inherited
workhorse model. Read-only review of authorization ingress/projection and core
factory wiring, followed by an explicit correction against the frozen spec.

| Finding | Assessment | Action/evidence |
| --- | --- | --- |
| Self evaluation lacks access:explain | Unsupported by the approved contract; retracted | `04-authorization-contract.md` permits self dry run; `06-identity.md` requires per-owner authority for other-principal Explain. Retain that boundary. |
| Missing/hidden row probes differ | High, accepted | Preserve absent images, defer admission errors, coalesce non-disclosable points to the same generic partial projection. `TestProtectedHTTPReadInspectUpdate` exercises both engines. |
| Empty whole-record revision could escape | Medium, accepted | Evidence success requires a nonempty bounded data revision. |
| Protected factory may return nil DB/coordinator | Medium defensive, accepted | Core rejects incomplete factory results. |

## SQLite storage and predicate review

Reviewer: `portable_policy_loader`, DALgo implementation agent; read-only review
of SQL-owned changes. Severity below is the reconciled engineering assessment,
not a claim that the original severity label was validated independently.

| Finding | Assessment | Action/evidence |
| --- | --- | --- |
| Retained pre-configuration factory can bypass selected profile | Accepted mount-lifecycle hardening | Activate the factory's exposed facade and reject reconfiguration; test retained dynamic transaction worker is never called. Trusted Go code retaining an earlier raw SQL handle remains outside the threat boundary. |
| Row-dependent invalid candidate/type failures reveal hidden row differences | High, accepted | DALgo `PreparationError` evidence carries no images/identity facts; safely evaluate static blockers, mark incomplete, deny execution, and coalesce HTTP disclosure. |
| NOCASE keys defeat unique normalized batch targets | High, accepted | Reject explicit collations in the protected point profile before callback/mutation. |
| Incomparable primary-key comparison panics | Incorrect, retracted | SQL keys are validated strings; an interface holding a slice compared with an interface holding a string has different dynamic types and does not panic. |
| Missing RowsAffected assertion | Medium defensive, accepted | Require exactly one affected row per admitted point mutation. |
| Hidden-column CAS and receipt tests missing | Accepted | Whole-image change invalidates stale token; committed receipt equals the next evidence revision. |
| Bare SQL comparison inherits NOCASE/affinity and widens row ACL before limit | High, accepted | Explicit binary comparisons with affinity suppression and ordered type guards; real secured SQLite tests cover equality/IN and text-vs-number before pagination. |
| Read policy lease released before query completes | Documented boundary | Reads retain an immutable admission snapshot; protected write leases extend through commit. This is not revocation of already-admitted reads. |
| Impure custom evaluators replayed by dry runs | High, accepted enabling fix | Built-ins declare inspection purity; unmarked custom callbacks must produce unsupported/incomplete diagnostics without invocation. Real enforcement retains its existing callback behavior. |

Final review must verify fixes at exact commits, including negative tests and
remaining unsupported-profile limits. These checkpoints do not authorize a
remote push or substitute for end-to-end acceptance.
