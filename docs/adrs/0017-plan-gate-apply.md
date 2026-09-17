# 0017. Plan-Gate-Apply three-phase commit

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

Keystone's reconciler ships a two-phase flow:

1. **Plan** — MigrationBundleReconciler resolves the source, runs the
   analyzer pack, and creates one `MigrationPlan` per matched
   `DatabaseSchema`. The plan captures the DDL that will run.
2. **Apply** — MigrationExecutionReconciler consumes the plan (via
   `spec.planRef`) and calls the SQL runner.

Missing between them: a **gate** that lets operators pause between
seeing the plan and running it. SchemaHero and the Atlas operator
both make this explicit — the plan materialises, the operator
reviews, then flips an approval bit. The execution doesn't fire
until the gate opens.

Before this ADR, plans were auto-approved at creation time via a
stub at `migrationplan_helpers.go:110-114` (`AutoApprovedPhase31`).
The `ConditionTypeApproved` existed but was always True. A Manual
gate required either a webhook hack or a full CRD rework.

The related work from A3 is the *annotation-based bundle approval*
gate (who said yes). This ADR is about the *plan-level* gate (what
SQL is about to run on which schema). They are complementary:

- A3 asks "has this bundle been approved by the right humans?"
- A4 asks "has the computed DDL for this (bundle, schema) pair been
  reviewed and released?"

Both matter. A3 catches malicious bundles; A4 catches "the computed
plan surprised me, I want to look before it runs".

## Decision

Add a `spec.approved bool` gate to `MigrationPlan`, and a new
`MigrationPlanReconciler` that owns the gate's state based on matched
SchemaPolicies.

```
kind: SchemaPolicy
spec:
  targetSelector:
    matchLabels:
      tier: prod
  planApproval: Manual     # default Auto
```

Behaviour:

- **Auto (default)** — the reconciler patches `spec.approved=true`
  immediately after the plan is created. Existing pre-A4 deployments
  see no behaviour change.
- **Manual** — the reconciler leaves `spec.approved=false`. The
  `ConditionTypeApproved` flips to False with reason
  `AwaitingApproval`, pointing at the blocking policy. A reviewer
  flips `spec.approved=true` via `keystonectl plan approve`,
  `kubectl patch`, or a GitOps commit that updates the plan.
- **MigrationExecution gate** — the execution reconciler reads the
  referenced plan BEFORE acquiring the pool. If `spec.approved !=
  true`, the execution stays Pending with a `Ready=False,
  reason=AwaitingPlanApproval` condition and requeues after 60s. A
  `Watches` on `MigrationPlan` ensures approval flips propagate
  within ~1s (no waiting for the 60s poll).

When multiple matched SchemaPolicies disagree, **Manual wins**. The
"strictest policy wins" convention already holds for
`integrity.requireSumFile` and `approvalPolicies.requiredApprovers`;
this keeps it uniform.

The plan's `spec.statements[]` stays the source of truth for what
will run — the existing per-statement 4 KiB cap is the right
trade-off for CR size vs reviewability. Full SQL lives in the
source ConfigMap and is re-verified at apply time via the existing
ContentHash pin (TOCTOU defence).

## Consequences

Easier: **explicit review UX**. Operators run `kubectl get
migrationplan -o yaml <name>` and see exactly what the controller
will execute. No shelling out to `psql` to reconstruct the diff.

Easier: **GitOps-native approvals**. `spec.approved` is a regular
field — an ArgoCD Application can gate a manifest that flips it,
complete with its own approval workflow. No side-channel needed.

Easier: **backward compatibility for existing deployments**. Auto
mode is the default; bundles today continue to flow without
operator changes. Manual mode is strictly opt-in via SchemaPolicy.

Harder: **two approval concepts in play**. A bundle can be
SchemaPolicy-approved (A3) AND plan-gated (A4). Both must pass.
Documentation takes extra care to distinguish them — the A3 gate is
about "who said this bundle is fit to ship"; A4 is about "has the
operator accepted this specific computed plan for this schema".

Harder: **Watch cost**. The MigrationPlanReconciler watches
SchemaPolicies so policy flips propagate; the
MigrationExecutionReconciler watches MigrationPlans so approval
flips propagate. Two additional watch channels on the manager —
low-volume objects, negligible at enterprise scale.

Soft consequence: **plan deletion pauses executions**. If an
operator deletes a plan mid-flight, the execution reconciler
interprets the missing plan as "not approved" and requeues. That's
a safer default than "apply the cached plan" since the deleted
plan's statements are no longer reviewable.

## Alternatives considered

- **Reuse `ConditionTypeApproved` directly**. Conditions are
  controller-owned by convention; flipping them from human CLI is a
  smell. A spec field is the idiomatic Kubernetes pattern (compare
  `CertificateSigningRequest.spec.approved` / `oc adm policy`
  actions). Rejected.

- **Annotation-based gate** (e.g., `keystone.hexxlock.io/
  plan-approved: true`). Works, but annotations are stringly-typed
  and don't generate printer columns. `spec.approved` (bool) gives
  `kubectl get` a real column and integrates with
  OpenAPI-driven UIs. Rejected.

- **Bundle-level gate (no per-plan gate)**. The plan snapshot is
  per-(bundle, schema). A bundle-level approval means the operator
  approves every future plan the bundle might generate across every
  schema — too coarse for tier-1 production. Rejected.

- **Combine with A3 (single gate for bundle + plan)**. A3 is at
  admission time (before plans exist); A4 is at reconcile time.
  Merging them would force A3 to wait for every plan to be
  reviewed before the bundle is admitted, which defeats the
  point of admission-time validation. Rejected.

- **Auto-delete the plan on execution success** (to reduce clutter).
  Plans are audit artifacts — the Example Service plan for March 2025
  should still be queryable in March 2026. Retention is the right
  answer, even if `kubectl get migrationplan` gets noisy.
