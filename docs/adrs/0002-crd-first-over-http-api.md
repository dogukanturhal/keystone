# 0002. CRDs, not HTTP APIs, as the control surface

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

The services Keystone replaces (`db-migrator`, `db-provisioner`) exposed HTTP
APIs with saga orchestration, custom state machines, and bespoke auth. New
workflows require new endpoints + API versioning + client library updates.
State lives in per-service Postgres tables that are invisible to Kubernetes.

Meanwhile, the rest of the HexxLock platform is standardised on GitOps:
ArgoCD reconciles everything from Git; nothing in a cluster that isn't in a
manifest. That invariant is also written in our own internal engineering notes
memory — when the platform drifts from Git, debugging is 5× slower.

## Decision

Every Keystone concept is a CustomResourceDefinition, reconciled by a
controller-runtime Reconciler. No HTTP mutation API exists. The only HTTP
endpoints Keystone serves are `/metrics` (Prometheus) and `/healthz` (probes).

State is etcd-backed CR status. Audit trail is CR + Event history. Approval
gates are annotations on CRs. CI integration is via Git commits of CRs.

## Consequences

Easier: ArgoCD reconciles everything automatically. Rollback is `git revert`.
Approval is a CODEOWNERS review. RBAC is standard `kubectl auth can-i`.
Observability is `kubectl get … -o jsonpath=.status.conditions` — no custom
API calls.

Easier: offline/air-gapped support is free — the CRs tar up and apply on the
other side. HTTP APIs would require port access + credentials distribution.

Harder: dynamic behaviour that genuinely needs HTTP semantics (e.g. streaming
plan output during long operations, interactive approval UIs) cannot be built
directly. We mitigate via `keystonectl` as a local dev-loop tool and via the
future PR-preview bot as an external webhook.

Harder: multi-cluster observability needs extra plumbing — Argo CD's cluster-
cache is the aggregation point today; the hub-agent split for Phase 7 works
within that constraint.

## Alternatives considered

- **HTTP API** (db-provisioner pattern): familiar to devs, but loses GitOps
  invariants and duplicates effort already solved by controller-runtime.
- **gRPC API**: no operational advantage over HTTP; same problems.
- **CRD + HTTP hybrid** (CRDs for state, HTTP for actions): splits the
  surface — "where is the source of truth?" has two answers. Rejected.
- **Operator pattern with imperative API** (Atlas Operator style): Atlas
  keeps a CLI that talks HTTP to Atlas Cloud; we explicitly chose no cloud
  dependency.
