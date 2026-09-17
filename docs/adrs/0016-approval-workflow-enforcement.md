# 0016. Approval workflow enforcement — annotation-based N-of-M

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

`SchemaPolicy.spec.approvalPolicies` landed in a previous phase as a
fully typed CRD surface: each policy declares `requiredApprovers`,
`fromGroups`, `disallowSelfApproval`, and a conditional
`appliesWhen` (bundle selector, lint severity, risk tier). The types
were there; enforcement was not. Every `MigrationBundle` in the
cluster was implicitly auto-approved, and the only use of
`ConditionTypeApproved` was a stub at
`internal/controller/migrationplan_helpers.go:110-114` that set it
to True with reason `AutoApprovedPhase31` regardless of the policy
set.

Tier-1 enterprise expects the standard production gate: dev
auto-admits, staging takes one peer review, prod takes two peers
plus a DBA. Without enforcement, the CRD was aspiration, not
governance.

Two related questions had to be answered before enforcement could
land:

1. **How do approvers prove approval?** Bytebase uses a hosted web
   flow. Atlas Cloud uses signed URLs. Flyway Enterprise uses JIRA
   tickets. Each couples the operator to a specific SaaS.
2. **Where is trust rooted?** If the approval evidence lives in the
   cluster (annotations, secrets, CRs), any principal with write
   access to the bundle can forge it. If it lives outside the
   cluster (JWTs from an external IdP, Sigstore signatures), the
   operator needs a key-management side-channel that works on air-
   gapped clusters.

## Decision

Approvals are encoded as annotations on the `MigrationBundle` object:

```
keystone.hexxlock.io/author = <identity>
keystone.hexxlock.io/risk-tier = <low|medium|high|critical>      # optional
keystone.hexxlock.io/approval.<policy-name>.<approver-id> = <group>
```

The webhook + controller walk the annotations, count distinct
approver-ids per `ApprovalPolicy.Name`, filter by `FromGroups`,
subtract the author when `DisallowSelfApproval=true`, and compare
against `RequiredApprovers`. When any applicable policy is under-
quorum, admission rejects and reconcile sets
`ConditionTypeApproved=False`.

Trust is rooted in **the Git review gate**, not in the cluster. The
expected rollout is:

1. Author opens an MR that edits the `MigrationBundle` YAML.
2. CODEOWNERS enforces who can approve the MR.
3. CI merges only after the required MR approvals exist — the merge
   bot writes the corresponding `keystone.hexxlock.io/approval.*`
   annotations to the YAML as part of the merge commit.
4. ArgoCD applies the merged manifest; the webhook sees the
   annotations and admits.

Inside the cluster any principal with RBAC write on
`migrationbundles` could, in principle, add annotations. That is
acceptable: the cluster-side RBAC scope already grants them the
ability to create the bundle outright. The annotation layer exists
to carry proof *from Git to cluster*, not to defend against
cluster-admin actors.

A future phase (12.2) will replace raw annotations with short-lived
signed JWTs from an approval-issuer webhook — the trust boundary
moves inside the cluster then. The CRD surface does not change; the
webhook gains a verification step.

## Consequences

Easier: **no external dependency**. Annotations round-trip through
any GitOps stack. Works on air-gapped clusters. Maps cleanly onto
`kubectl describe` and `kubectl get -o yaml`.

Easier: **review visibility**. A reviewer reading the MR sees who
approved (from the annotations) alongside what's being changed.
Compare to JWTs, where the approval payload is opaque until
decoded.

Easier: **policy composition**. The existing `appliesWhen` struct
composes across RiskTier / BundleSelector / MinLintSeverity cleanly
in Go without a separate rule engine.

Harder: **CI bot scope**. Something has to write the approval
annotations to Git after MR approvals. Today this is manual (the
approvers add the annotations when they approve); a future phase
adds a GitLab bot that does it automatically from MR approval
metadata.

Harder: **self-approval enforcement requires author identity**.
`DisallowSelfApproval=true` only works if the bundle carries the
`keystone.hexxlock.io/author` annotation. The committer pipeline
must write it (from `git config user.email`). Absence of the
author annotation turns DisallowSelfApproval into a no-op.

Softly harder: **the bundle author can set
`keystone.hexxlock.io/author` to a different identity**. True.
That is an audit-log concern, not a webhook concern — the
`AuditEntry` chain (ADR 0001-adjacent) records who actually applied
the CR via the admission request's UserInfo. Cross-referencing
the author annotation against UserInfo is tracked for Phase 12.2.

## Alternatives considered

- **Hosted approval UI (Bytebase-style)**. Rejected: Keystone's
  GitOps-first principle (ADR 0007) forbids a UI that writes state
  to the cluster. A read-only dashboard is fine; an approval flow
  that bypasses Git review is not.

- **Sigstore / cosign-signed annotations**. Rejected for A3.
  Requires a per-approver identity in Fulcio and a transparency-log
  side-channel. Legitimate for the OCI-artifact flow later (signing
  released bundles), but overkill for daily authoring.

- **Kyverno policy as enforcement layer**. Kyverno can match an
  annotation key pattern but cannot execute the Go-typed
  `ApprovalCondition` (BundleSelector, MinLintSeverity, RiskTier
  composition). Would have required re-implementing the evaluator
  in JMESPath/CEL — two sources of truth. Rejected.

- **Blocking RBAC on MigrationBundle patch**. Rejected: the whole
  operator mediates cross-cluster schema changes, so denying
  patch-by-role would break too many workflows. Approval evidence
  as CR annotations leaves RBAC at its natural scope.

- **Sync-on-condition (reconcile waits until approved, no hard
  reject at admission)**. Rejected in favour of the ADR 0011
  both-layers model. Admission rejects early; reconcile re-checks
  and sets the condition so operators can see exactly what is
  missing without going back to kubectl describe on the webhook's
  admission review.
