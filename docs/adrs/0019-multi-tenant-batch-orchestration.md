# 0019. Multi-tenant batch orchestration — per-schema visibility + throttling

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

Keystone already fans out a MigrationBundle across every DatabaseSchema
matched by `spec.schemaSelector`. Two modes coexist:

- **Staged** (`spec.rolloutPolicyRef` set): the RolloutPolicy
  controller walks ordered stages, each with its own
  `schemaSelector`, `parallelism`, `soakDuration`, and
  `abortOnFailure` gate. Canary → free → paid (ADR 0006).
- **Non-staged** (default): a flat loop creates one
  MigrationExecution per matched schema on the first reconcile tick.
  No throttling, no per-tenant visibility.

For tier-1 multi-tenant SaaS, both modes fell short on the *observability*
dimension. The bundle status reported:

- `MatchedSchemas: 42`
- `AppliedSchemas: 37`
- aggregate counts per stage

What operators wanted was "which tenants are still pending? which
failed? what was the error?". Today that means walking the
per-execution MigrationExecution CR list by hand, one kubectl call
per schema. At 42 tenants, triage is five minutes of JSON-path
fu.

The non-staged mode had a second gap: no parallelism cap. A bundle
that matches 80 schemas fires 80 Executions at once, each opening a
pool connection. On small CNPG deployments the connection count
exceeds `max_connections` and half the executions fail with
"too many clients".

## Decision

Two additions that stay within the ADR 0006 canary rollout model:

### 1. Per-schema status detail

New `MigrationBundleStatus.SchemaProgress []SchemaProgress`. One
entry per DatabaseSchema matched by the selector. Each entry
carries:

- `SchemaRef` — the DatabaseSchema's metadata.name
- `TenantID` — value of the `keystone.hexxlock.io/tenant-id` label
  when set; empty otherwise
- `ExecutionRef` — the MigrationExecution name (empty when
  throttled or not yet created)
- `Phase` — mirrors the Execution phase (`Pending`, `Running`,
  `Expanding`, `Succeeded`, `Failed`, `Aborted`, or the B2-new
  `Throttled`)
- `Message` — first non-nominal Ready-condition message from the
  underlying Execution, for one-line failure triage
- `StartedAt`, `CompletedAt` — time markers

Populated on every reconcile by the MigrationBundle controller, via
a new `patchSchemaProgress` helper that follows the established
standalone-patch pattern (same as `patchLintFindings`,
`patchIntegrity`, `patchApproval`) so status fields survive the
downstream merge-patch.

Capped at 256 entries per etcd value-size budget. Bundles matching
more than 256 schemas still work — they just rely on the per-
execution CR list for the tail.

### 2. Non-staged throttling

New `MigrationBundleSpec.MaxConcurrentExecutions int32`. Default 0
(unlimited, pre-B2 behaviour). Non-zero caps the number of
concurrent *active* (non-terminal) Executions. Schemas beyond the
cap get a `SchemaProgress{Phase: "Throttled"}` entry on the current
reconcile tick and are picked up on subsequent reconciles as
in-flight Executions complete.

Staged bundles ignore this field — RolloutPolicy stages already
have a per-stage `parallelism` knob that's a better fit for the
ordered cohort model.

### 3. Tenant label convention

New exported constant `LabelTenantID =
"keystone.hexxlock.io/tenant-id"`. Not enforced; DatabaseSchemas
without the label still reconcile. When set, the value surfaces as
`SchemaProgress.TenantID` so dashboards can group fanouts by tenant
without joining against ProductInstance.

Operators can use the same label in RolloutPolicy stage selectors
to pick canary tenants directly:

```yaml
stages:
  - name: canary-pilot
    schemaSelector:
      matchLabels:
        keystone.hexxlock.io/tenant-id: pilot-customer
    parallelism: 1
  - name: broad-rollout
    schemaSelector:
      matchLabels:
        tier: prod
    parallelism: 10
    abortOnFailure: true
```

## Consequences

Easier: **tenant-grouped triage**. `kubectl get migrationbundle
mb-prod -o jsonpath='{.status.schemaProgress[?(@.phase=="Failed")]}'`
returns the exact tenants that need attention — no cross-CR joins.

Easier: **connection-budget safety**. A CNPG cluster with
`max_connections=100` can cap non-staged bundles at
`maxConcurrentExecutions: 20`, leaving 80 connections for the app
traffic. Previously a 50-tenant bundle would saturate the pool.

Easier: **canary selection by tenant**. A pilot customer's schema
can be labelled `keystone.hexxlock.io/tenant-id: pilot-acme` and
the first stage's selector gates the rollout to just that tenant.

Harder: **status-size budget**. 256-entry cap means fleets of 10k+
tenants per bundle still need to fall back to the per-execution CR
list for the full picture. Dashboards should paginate or scope to
the active cohort rather than loading every entry.

Harder: **tenant-label discipline**. The label is optional, which
means half-labelled environments have partial visibility. The
recommended rollout: enforce via Kyverno or admission policy that
every DatabaseSchema carries the label before enabling
tenant-grouped dashboards.

## Alternatives considered

- **Separate MigrationBundleStatus CRD** (`MigrationBundleProgress`).
  Would have unlimited capacity but splits the triage surface across
  two CRs. Rejected — the 256 cap covers 95% of fleets; the rest
  can fall back to lists.

- **Enforce tenant-id label at admission**. Rejected as ADR-0007
  violation — Keystone's GitOps-first principle means labels are
  authored, not mandated. Kyverno is the right enforcement layer if
  an environment needs the guarantee.

- **ProductInstance as first-class fanout target**. Every Schema
  already belongs to a ProductInstance via LogicalDatabase parent
  references; the `tenantId` could be inferred from that graph
  rather than a label. Rejected because (a) RolloutPolicy stage
  selectors need a Schema-level signal, and (b) the inference walk
  adds reconcile latency. A label is cheaper and discoverable from
  `kubectl get databaseschema --show-labels`.

- **Per-stage throttling carried into non-staged mode**. Rejected
  because RolloutPolicy has a richer model (soak, approval,
  cluster-tier) that overloads `MaxConcurrentExecutions`. Keeping
  non-staged simple (single int) matches the rest of the
  non-staged shape.

- **Auto-throttle based on CNPG `max_connections`**. Tempting but
  requires probing the target database for its connection limit,
  which happens before the pool is acquired for migration. A
  spec field that the operator sets explicitly is more transparent
  and doesn't couple the bundle controller to database-level
  inspection.
