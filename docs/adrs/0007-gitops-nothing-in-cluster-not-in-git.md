# 0007. GitOps end-to-end; nothing in a cluster that isn't in Git

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

Production incidents at HexxLock have been traceable to cluster state
diverging from Git — an admin patched a resource via `kubectl` to fix
symptoms, the change wasn't reflected in a manifest, and the next Argo CD
reconcile either reverted the fix (causing a regression) or silently
ignored it (accumulating drift). This lesson is pinned in the memory file
internal engineering notes.

Keystone is supposed to be the schema-management authority. If Keystone
itself can be patched out-of-band, then the cluster's actual schema state
and the Git source of truth diverge, and `DriftReport` CRs become
untrustworthy.

## Decision

Every piece of Keystone state flows through Git → Argo CD → etcd:

- CRDs live under `services/keystone/config/crd/` in the platform repo.
- Applications, PrometheusRule, ServiceMonitor, Kyverno policies live in
  `<gitops-repo>/keystone/`.
- Migrations are authored as `MigrationBundle` YAML + ConfigMap pairs in Git.
- Drift acceptance is an annotation on a DriftReport, which is itself a CR.
- Approvals are annotations on MigrationBundles and MigrationExecutions.

Nothing is created via an admin UI. No HTTP API writes to the cluster
(see ADR 0002). `kubectl apply -f …/keystone-system/…` as a manual step is
acceptable only during bootstrap before ArgoCD owns the App; after
bootstrap, every change is a Git commit.

## Consequences

Easier: rollback is `git revert`. Audit is `git log`. Compliance evidence
is "every production schema change has a Git commit, signed with DCO,
reviewed under CODEOWNERS."

Easier: multi-cluster is trivial — Argo CD can sync the same manifests to
N clusters.

Harder: emergency fixes take longer. An operator can't just `kubectl edit`
to correct an issue; they must commit + push + wait for Argo CD sync.

Harder: feedback loops during authorship are slower than with an
interactive UI. `keystonectl` (ADR not yet written) is the escape hatch —
it runs locally against a dev DB without round-tripping through the
cluster.

## Alternatives considered

- **Hybrid** (Git for declarations, admin UI for approvals): splits the
  source of truth. Rejected.
- **GitOps for most things, kubectl for break-glass**: this is essentially
  where HexxLock was before and where the pinned memory cites pain. We chose
  no break-glass — when genuinely stuck, document the blocker and fix it
  in Git, even if that takes longer.
