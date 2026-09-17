# 0006. Tier-aware staged rollout (canary → free → paid)

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

In a multi-tenant SaaS context with a mix of free and paid tiers, applying
a schema change everywhere-at-once is an outage amplifier — a bad migration
lands in production for every tenant simultaneously. The industry pattern
is a staged pipeline: canary (1-2 internal tenants) → free tier (high
parallelism, moderate soak) → paid tier (low parallelism, long soak,
operator approval).

Kubernetes operators like Argo Rollouts model this pattern for Deployments.
Atlas has `for_each` tenant fanout. But neither wires cluster-tier
classification in: "this tenant is on our prod-eu-west-1-free cluster, so
batch with other free-tier tenants; that one is on prod-us-east-1a-paid,
low parallelism."

## Decision

Keystone ships a first-class `RolloutPolicy` CRD. Stages are an ordered
array with:
- `clusterTierSelector`: matches against `ClusterRegistration.spec.tier`
- `parallelism` (default 1 — never all-at-once)
- `soakDuration` (default 10m)
- `requireApproval` (annotation gate between stages)
- `abortOnFailure` (default true — first failure freezes the pipeline)

MigrationBundles reference a RolloutPolicy via `spec.rolloutPolicyRef`.
Bundles without a ref keep the Phase 3 all-at-once behaviour for backward
compat.

## Consequences

Easier: a bad migration in canary freezes the whole pipeline before paid
tenants are touched. The freeze is visible via `Ready=False
reason=StageBlocked`.

Easier: operators can see exactly where a rollout stopped and why —
`status.currentStage` + `status.stageHistory` is the primary signal.

Harder: stage advancement requires operator attention (for approval) or
soak time (for automatic). For rapid iteration, operators set `parallelism:
high` + `soakDuration: 0s` on canary to bypass. We document the "safe
defaults vs fast defaults" trade-off in the runbook.

Harder: the cluster-tier labels must be set correctly on every
`ClusterRegistration`. Misclassified clusters land in the wrong stage
bucket. We mitigate with Kyverno admission policies that require the tier
label.

## Alternatives considered

- **No staged rollout; rely on testing**: accepts some outage risk as the
  cost of simplicity. Rejected for a product that will serve paid tenants
  eventually.
- **Argo Rollouts-style progressive delivery with metric analysis**: more
  sophisticated than we need today; adds metric-provider dependency.
  Revisit in a future phase once Prometheus SLO-based advancement is
  warranted.
- **Fixed percentage rollout** (10%, 50%, 100%): common in CDNs, but
  tenant-scoped migration isn't percentage-of-traffic — it's per-tenant DB
  state. Stages map better to tenant cohorts.
