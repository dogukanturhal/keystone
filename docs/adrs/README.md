# Keystone Architecture Decision Records (ADRs)

This directory records the tier-1 architectural decisions that shaped
Keystone. Format follows [Michael Nygard's template][nygard] — lightweight,
append-only, one decision per file.

Why these exist: enterprise evaluators ask "why did you choose X over Y?" and
these docs preserve the reasoning so future maintainers (and auditors) don't
have to reverse-engineer intent from the code.

| # | Title | Status |
|---|---|---|
| [0001](0001-agplv3-license.md) | Dual license: AGPL-3.0 operator + Apache-2.0 SDK | Accepted |
| [0002](0002-crd-first-over-http-api.md) | CRDs, not HTTP APIs, as the control surface | Accepted |
| [0003](0003-postgresql-first-multi-engine-later.md) | PostgreSQL-first; pluggable engine abstraction | Accepted |
| [0004](0004-no-kube-rbac-proxy.md) | No `kube-rbac-proxy` sidecar for metrics | Accepted |
| [0005](0005-pgroll-lightweight-subset.md) | Lightweight expand/contract subset over full xataio/pgroll | Accepted |
| [0006](0006-tier-aware-rollout-policy.md) | Tier-aware staged rollout (canary → free → paid) | Accepted |
| [0007](0007-gitops-nothing-in-cluster-not-in-git.md) | GitOps end-to-end; no admin UI that writes to the cluster | Accepted |
| [0008](0008-separate-tracking-table-for-adoption.md) | Configurable tracking table name for adoption on existing DBs | Accepted |
| [0009](0009-dual-strategy-versioned-and-declarative.md) | Support both versioned + declarative strategies | Accepted |
| [0010](0010-defense-in-depth-identifier-validation.md) | Defence-in-depth identifier validation | Accepted |
| [0011](0011-admission-webhook-lints-not-just-reconciles.md) | Lint at admission, not just at reconcile | Accepted |
| [0012](0012-engine-interface-before-mysql.md) | Declare the engine interface before implementing a second engine | Accepted |
| [0013](0013-pgroll-subset-vs-upstream.md) | Ship a pgroll subset before embedding upstream | Accepted |
| [0014](0014-slo-framework-multi-window-burn-rate.md) | SLO framework — multi-window burn-rate alerts | Accepted |
| [0015](0015-migration-directory-integrity-sum.md) | Migration directory integrity via `keystone.sum` | Accepted |
| [0016](0016-approval-workflow-enforcement.md) | Approval workflow enforcement — annotation-based N-of-M | Accepted |
| [0017](0017-plan-gate-apply.md) | Plan-Gate-Apply three-phase commit | Accepted |
| [0018](0018-pgroll-expanded-ops-and-abort.md) | Expanded pgroll operations + explicit Abort lifecycle | Accepted |
| [0019](0019-multi-tenant-batch-orchestration.md) | Multi-tenant batch orchestration — per-schema visibility + throttling | Accepted |
| [0020](0020-intra-bundle-wave-parallelism.md) | Intra-bundle wave parallelism for pgroll operations | Accepted |
| [0021](0021-schema-registry-auto-snapshots.md) | Schema registry — auto-snapshots + structural diff | Accepted |
| [0022](0022-audit-stdout-json-export.md) | Structured JSON audit export for SIEM | Accepted |
| [0023](0023-cel-policy-engine.md) | CEL-based policy expressions for SchemaPolicy | Accepted |
| [0024](0024-credential-rotation-eso.md) | Credential rotation + ESO / Vault integration | Accepted |
| [0025](0025-embeddable-sdk.md) | Embeddable Go SDK (Apache-2.0) | Accepted |
| [0026](0026-schema-source-loaders-and-dev-db-normalizer.md) | Schema-source loaders + the dev-database normaliser | Accepted |

## How to add an ADR

1. Pick the next free number.
2. Copy the template at `_template.md`.
3. Fill in Status, Context, Decision, Consequences.
4. Update this README with a one-line link.
5. Commit with `docs(adr): NNNN <title>`.

[nygard]: https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions
