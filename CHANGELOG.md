# Changelog

All notable changes to Keystone are documented here. The project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The format is loosely based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- `SchemaSnapshot` now records every dimension the drift inspector
  reads. Enums, extensions, sequences, functions, triggers and
  materialized views are captured alongside the tables, and each view
  carries its defining `SELECT`.
  - The capture step copied tables, indexes, constraints and (since the
    entry below) row-level security, and dropped the rest on the floor
    even though the inspector had read them. So a rewritten trigger
    function, a dropped materialized view, an enum whose labels were
    reordered, a sequence narrowed from `bigint` to `integer`, and a
    view whose `WHERE` clause stopped filtering by tenant all produced
    a byte-identical `status.structure`, and `snapshot diff` reported
    "no structural differences" across each of them.
  - A view is the sharpest case: its column list is the same before and
    after the rewrite, so a snapshot holding only the columns cannot
    tell a view that filters by tenant from one that no longer does.
    `viewDefinition` is what makes that visible.
  - New change kinds: `TableKindChanged` and `ViewDefinitionChanged`,
    plus added/removed/changed for each of enums, extensions,
    sequences, functions, triggers and materialized views. Triggers are
    keyed by `(table, name)` for the same reason policies are — the
    name is unique per table, not per schema. Functions are keyed by
    `(name, args)`, since overloads are distinct objects that share a
    name. Enum labels are never sorted before comparison: their order
    *is* the sort order the type compares on, so a reorder is a real
    change, not noise.
  - Structure model version is now 2, and the new dimensions are
    compared only when both snapshots are at 2 or above; below that a
    caveat is returned naming them. Same reasoning as the RLS entry
    below — a model-1 archive that carries no trigger list has not
    observed that a schema has no triggers, and reading it that way
    would report every trigger in the newer snapshot as freshly added.
  - Every new field is `+optional` and none carries a CRD default, so
    archived snapshots still pass admission and still read back exactly
    as written. The one exception is a trigger's `forEachRow`, which is
    required: no archive predates it, so an object that omits it is not
    an old artifact being tolerated but a new one that lost the field,
    and admitting it would silently mean `FOR EACH STATEMENT`.
- `SchemaSnapshot` now records row-level security, and
  `keystonectl snapshot diff` reports changes to it. Each captured
  table carries `rlsEnabled` and `rlsForced`, and the schema's policies
  are captured under `status.structure.policies` with their command,
  permissive flag, roles, `USING` and `WITH CHECK` expressions.
  - Until now the snapshot registry modelled tables, indexes and
    constraints only, while the drift inspector had been reading RLS
    all along — the capture step dropped it. So `DROP POLICY
    tenant_isolation` and `ALTER TABLE … NO FORCE ROW LEVEL SECURITY`
    both left the captured structure byte-identical, and `snapshot
    diff` printed "no structural differences" across a change that
    removes tenant isolation. That is the one thing the registry is
    sold as evidence for.
  - `NO FORCE` in particular is invisible without this: `ENABLE`
    alone exempts the table's owner, so a schema whose application
    connects as the owning role has every policy bypassed for exactly
    the connection the policies exist to constrain, while the policies
    themselves remain present and look correct. The diff spells the
    consequence out rather than printing a bare boolean.
  - New change kinds: `RLSEnabledChanged`, `RLSForcedChanged`,
    `PolicyAdded`, `PolicyRemoved`, `PolicyChanged`. Policies are keyed
    by `(table, name)` — policy names are unique per table, not per
    schema — and a policy's role list is compared order-insensitively,
    since PostgreSQL does not promise a stable order and a reorder is
    not a change.
- `StructuralSnapshot` carries a `modelVersion`, and
  `schemadiff.Diff` returns a `Report` with `Caveats` rather than a
  bare `[]Change`.
  - Snapshots are immutable and retained for a year by default (up to
    a century under a regulatory hold), so the registry permanently
    holds captures from before RLS was modelled. Reading their absent
    policy list as "this schema had no policies" would report every
    existing policy as newly added and, worse, would report a schema
    that had *lost* a policy as having gained several.
  - So RLS is compared only when both snapshots record it, and a
    caveat is returned when they do not. `Caveats` exists because an
    empty change list means "these two agree on everything compared",
    which is only the same as "nothing changed" when nothing was
    skipped — `keystonectl snapshot diff` now says "no differences in
    the dimensions both snapshots record" in that case, rather than
    "no structural differences".
  - `modelVersion` deliberately has no CRD default: structural-schema
    defaulting is applied when an unset field is read back out of
    etcd, so defaulting it to 1 would relabel every pre-RLS snapshot as
    RLS-aware and invert the property it exists to provide. For the
    same reason `rlsEnabled` is optional in the schema even though the
    controller always writes it — a required field would make every
    archived pre-RLS snapshot fail admission against the new CRD.
- Accept-drift requests are now acted on immediately and always
  answered. `DriftAcceptanceReconciler` watches `DriftReport` for the
  `keystone.hexxlock.io/accept-drift` annotation and re-checks that one
  schema on the spot, instead of leaving the request to be picked up by
  the next periodic sweep.
  - Previously the annotation was only ever read by the sweep, so it
    took effect up to `driftCheckInterval` later — an hour by default —
    and nothing was written back in the meantime. A correct annotation
    waiting on the sweep and an annotation that would never be read
    (wrong key, wrong value, wrong object) were indistinguishable:
    both looked like "I edited it and nothing happened". The only ways
    to tell them apart were to wait an hour and look again, or to
    restart the operator to force a sweep.
  - Refusals now say which of four things went wrong, as
    `Accepted=False` on the report plus a Warning event:
    `MalformedAnnotation` (the value is not the literal `"true"` —
    `"True"` is the mistake operators actually make),
    `SchemaNotFound`, `SchemaNotReady` (a wait, requeued, not a
    failure), `RebaselineFailed`.
  - There is deliberately no `Accepted=True`: acceptance re-baselines
    and deletes the report, so absence of the report is the success
    signal and a lingering `True` would mean the opposite of what it
    said.
  - Acceptance delegates to `DriftController.reconcileSchema` rather
    than calling `acceptDrift` directly, so the annotated route and the
    swept route cannot diverge — the audit entry, the `DriftAccepted`
    event and the snapshot-model-upgrade check that runs ahead of
    acceptance all still apply.
  - Detection stays periodic. The watch is filtered to reports that
    carry the annotation, and to updates where the annotation itself
    changed, so the sweep's own status re-patches — the overwhelming
    majority of `DriftReport` writes — do not wake it. Without that
    filter every sweep would cost an admin inspection per report.

### Fixed

- `SchemaDefinition` no longer reports contradictory status on
  convergence. `Inspected`, `DiffComputed` and `BundleGenerated`
  describe the three stages of a single reconcile, but only the
  bundle-emit lane ever wrote them, so every path that finished
  *without* emitting a bundle inherited whatever the last drifted
  reconcile had left behind. They are now re-stated on all four
  terminal paths — converged, no-matching-schemas,
  destructive-refused, mixed-versions-refused — so all four
  conditions and `Ready` always describe the same reconcile.
  - Observed live on `downstream-service-public-desired` after the RLS differ
    fix landed: `Ready=True (SchemaMatchesDesired)` and
    `pendingOperations: 0` sat next to `DiffComputed=True
    (DriftDetected) "174 statement(s) to apply"` and
    `BundleGenerated=True` naming a `MigrationBundle` that
    `currentBundleRef` had already been cleared of.
  - `LastTransitionTime` does not disambiguate this, because
    apimachinery only advances it on a *status* flip — a condition
    restated with a new reason and message keeps its old timestamp.
    So all four conditions read as equally current and `kubectl
    describe` gives an operator no way to tell a converged
    SchemaDefinition from one that is 174 operations behind.
  - `DiffComputed=True (NoDrift)` is deliberate on the converged
    path: the differ ran and returned an empty plan, which is a
    result, not an absent one. `False` is reserved for "the differ
    never got to run" (`NoMatchingSchemas`).
  - `BundleGenerated` now tracks `Status.CurrentBundleRef` — `False`
    whenever that field is empty. Both refusal paths cleared the ref
    while leaving the condition latched `True`, which reads as an
    apply already in flight and leaves an operator waiting on a
    bundle that will never be created.
  - The condition types are now exported as
    `ConditionTypeInspected` / `ConditionTypeDiffComputed` /
    `ConditionTypeBundleGenerated` rather than repeated as string
    literals across the (now four) call sites.

- Bumped `keystone-sdk/go` to `v0.3.1-0.20260806211038-aa0f6070d109`,
  which carries the observed-aware `diffRLS`. The previously pinned
  `3b08a8c` had `diffRLS(plan, schema, desired)` — no observed
  snapshot at all — so it emitted `ALTER TABLE … ENABLE ROW LEVEL
  SECURITY` and `… FORCE ROW LEVEL SECURITY` unconditionally for
  every table declaring `enableRLS: true`. The statements are
  idempotent, so nothing was ever wrong in the database; what was
  wrong was that the plan could never be empty, so a SchemaDefinition
  over an RLS-protected schema never reached `Ready`.
  - Observed live on `downstream-service-public-desired`: 87 RLS-protected
    tables → 87 `ENABLE` + 87 `FORCE` = **174 `pendingOperations`,
    recomputed every 60s indefinitely**, with `Ready=False
    (BundleEmitted)` and `Progressing=True (Applying)` pinned on
    forever. The same SDK differ run out-of-band against the same
    database reported `schema matches desired state; no operations
    pending`, which is what isolated the pin as the variable.
  - The false-drift signal is the real cost: an operator that always
    reports drift on every RLS-protected schema is an operator whose
    drift signal carries no information, and it emits a
    content-addressed MigrationBundle + MigrationExecution per
    distinct plan on the way there.

### Changed

- `MigrationBundleReconciler.success()` and
  `SchemaDefinitionReconciler.markReady()` now apply a change-only
  status-patch gate (`equality.Semantic.DeepEqual` between the
  pre-mutation snapshot and the staged update). Pre-fix both methods
  stamped a fresh `LastPlanTime`/`LastDiffTime` on every tick, which
  always produced a non-empty status diff, which fired the
  `Owns(MigrationBundle)` watch on the SchemaDefinition controller,
  which re-ran `markReady`, which re-stamped `LastDiffTime`, which
  fired the same watch — at ~1.7 reconciles/sec across the live ~50
  bundles plus the ~3 stdout lines/sec and one `AuditEntry` CR per
  bundle reconcile. The 2026-05-08 etcd backlog (20 820 AuditEntries
  drained out-of-band) traced directly to this churn.
  - On a no-op tick: skip the status patch, skip the audit emit, skip
    the `LastPlanTime` mutation, return the long requeue. Persisted
    status is byte-identical to the prior tick — no resourceVersion
    bump, no Owns-watch fan-out.
  - On a real state change: stamp `LastPlanTime` once, patch, emit
    one `AuditEntry`, return.
- `MigrationBundleReconciler.success()` Ready-path requeue raised
  from `30s` → `5m` (matches Crossplane's circuit-breaker cooldown
  and Argo CD's reconciliation default). Applying-path requeue stays
  at `30s` so per-execution progress surfaces fast. Both intervals
  are jittered ±20% via `requeueWithJitter` so 50+ bundles don't
  synchronise on the period boundary.
- `SchemaDefinitionReconciler.markReady()` log line "schema at
  desired state" demoted from `info` → `V(1)` (debug). A per-tick
  liveness signal belongs in metrics; matches cert-manager / Flux /
  Argo CD verbosity conventions.
- **Behavioral note** — `MigrationBundle.status.lastPlanTime` and
  `SchemaDefinition.status.lastDiffTime` now reflect the wallclock
  of the most recent **state mutation**, not of the most recent
  reconcile-tick. External consumers that watched these fields as a
  controller-liveness heartbeat must migrate to the
  `controller_runtime_reconcile_total` counter or the
  `keystone_audit_retention_pass_duration_seconds_count` Prometheus
  metric. The field semantic shift is the intended fix — a "last
  reconciled at" stamp is the textbook source of the hot-loop this
  MR closes.

### Added

- Structural drift detection now covers **RLS FORCE** and **policies**.
  The inspector recorded `pg_class.relrowsecurity` but not
  `relforcerowsecurity`, and never compared policies at all. Both gaps
  were invisible by construction: the baseline is `sha256` over the
  serialised snapshot, so a bit the snapshot does not carry cannot move
  the hash, and a schema that loses it reconciles clean forever.
  ENABLE-without-FORCE exempts the table's owner from every policy on
  it — and every Keystone-provisioned schema on this platform connects
  as the owning role — so `NO FORCE ROW LEVEL SECURITY` disabled tenant
  isolation while leaving the policies listed in `\dp` and `rowsecurity`
  still `t` in `pg_tables`. New `DriftFinding` kinds: `RLSForced`,
  `RLSUnforced`, `PolicyAdded`, `PolicyDropped`, `PolicyChanged`;
  dropping or weakening is `Critical`, adding is `Info`.
- `SchemaDefinition.spec.tables[].forceRLS` (`*bool`, unset means
  forced) — declares the FORCE bit rather than emitting it
  unconditionally. An explicit `false` now emits `NO FORCE` instead of
  emitting nothing, so turning it off converges the table.

### Changed

- The first reconcile after the drift-snapshot model grows a field no
  longer reports the whole fleet as drifted. Growing the model moves
  every stored hash at once while nothing about any database changed,
  and because the stored baseline is only rewritten on accept-drift,
  the report would have returned on every pass until each schema was
  hand-accepted — downstream-service 86 findings, example-service-main ~200. The
  operator now hashes the observed snapshot projected back onto the old
  model: a match against the recorded baseline proves the model was the
  only thing that changed and the baseline is restated in place;
  anything else takes the ordinary drift path, so the upgrade cannot
  swallow a genuine finding. Self-clearing — it stops firing once the
  stored snapshot carries the field.

- `CRTTLController` — operator-side garbage collector for terminal
  `MigrationBundle` and `MigrationExecution` CRs whose age exceeds the
  retention window configured on the owning `SchemaDefinition` (or the
  operator-wide defaults: 24h on success, 7d on failure). Modelled on
  Argo Workflows' `spec.ttlStrategy.{secondsAfterSuccess,
  secondsAfterFailure}`; the GC walks each SD, classifies each owned
  child by its terminal phase, and deletes everything past its window
  while always preserving the latest Succeeded CR per `(kind, SD)` (and
  per-schema for executions). The 2026-05-08 etcd-bloat outage
  (182 954 AuditEntry CRs / 4.7 GB etcd / 17 h apiserver loss)
  demonstrated the failure mode this controller exists to prevent —
  AuditEntry has its own off-cluster archive path; this GC covers the
  bundle / execution graveyard whose payloads stay on-cluster.
- `spec.cleanup.{retainSuccessFor, retainFailureFor}` on
  `SchemaDefinition` — per-SD overrides as `metav1.Duration` strings
  (`"24h"`, `"7d"`). Both fields optional; nil falls back to the
  operator-wide defaults. Unknown / zero values are rejected at
  admission via the kubebuilder min-duration markers in a follow-up MR.
- `--enable-cr-ttl` manager flag (default `false`). The single global
  gate on the new GC; SD-level config has no effect until the manager
  starts with the flag set. Off by default for first release; per
  `docs/runbooks/cr-ttl-rollout.md` (landing separately) operators flip
  per-cluster after one-week soak observing
  `keystone_cr_ttl_pass_duration_seconds{outcome="disabled"}`.
- `--cr-ttl-check-interval` (default `15m`),
  `--cr-ttl-retain-success` (default `24h`), and
  `--cr-ttl-retain-failure` (default `168h`) manager flags. Tune the
  loop cadence and the operator-wide retention windows that apply when
  the owning SD does not pin its own.
- `controller.crTTL.{enabled, checkInterval, retainSuccess,
  retainFailure}` Helm values (default off + 15m / 24h / 168h). Threads
  the new flags through the Deployment template; the
  `values.schema.json` declares the corresponding object subtree.
- `keystone_cr_ttl_deleted_total{kind, phase}`,
  `keystone_cr_ttl_candidates{kind}`, `keystone_cr_ttl_errors_total`,
  and `keystone_cr_ttl_pass_duration_seconds` Prometheus metrics.
  Operators see GC activity, candidate backlog, error rate, and pass
  latency without enabling pprof.
- `--enable-pprof` manager flag (default `false`). When set, the
  controller registers the standard `net/http/pprof` handlers
  (`/debug/pprof/`, `cmdline`, `profile`, `symbol`, `trace`) on the
  metrics server. They share the metrics server's TLS + TokenReview +
  SubjectAccessReview chain, so a caller still needs a Bearer token
  whose RBAC includes `nonResourceURLs: ["/debug/pprof/*"]` — no new
  attack surface for clients that already have `/metrics`.
- `metrics.pprof.enabled` Helm value (default `false`). Threads the
  flag through the Deployment.
- `keystone-pprof-reader` ClusterRole (rendered when
  `metrics.pprof.enabled=true`) granting `get` on
  `nonResourceURLs: ["/debug/pprof", "/debug/pprof/*"]`. Not bound by
  default — operators bind to a debugging ServiceAccount for the
  duration of an investigation, then unbind. Principle of least
  privilege; permanent bindings would mean any compromise of the bound
  SA's token leaks runtime state.
- The manager refuses to start when `--enable-pprof=true` is paired
  with `--secure-metrics=false`. Without secure-metrics the
  metrics-server's authn/authz FilterProvider is unset and pprof
  would otherwise serve plaintext HTTP — fail-closed instead of a
  silent auth-posture downgrade.

### Performance

- Cache TransformFunc on `AuditEntry` strips `Spec.Before` and
  `Spec.After` (the JSON snapshots of the audited resource state) at
  cache-entry time. Both reconcilers that read AuditEntry from the
  shared informer cache (`AuditLogReconciler`,
  `AuditEntryRetentionController`) ignore these fields; the S3 archiver
  excludes them from its ECS-flavoured wire format by design (see
  `internal/audit/exporter.go:76-79`); the chain-hash canonical
  encoding reads uncached via APIReader from operational tooling. So
  the strip is invisible to every consumer while saving hundreds of
  MiB of resident memory and ~200K heap objects on a representative
  cluster (104K AuditEntries × ~6 KiB combined Before/After).
  Originals stay intact in etcd — only the in-memory cache is leaner.
- Per-type cache scope on `corev1.ConfigMap` via a label-existence
  selector on `keystone.hexxlock.io/schemadefinition`. Pre-change the
  controller-runtime cache held every ConfigMap in the cluster
  (~284 cluster-wide on prod-eu-west-1-hub, several MiB-sized due to
  Argo's last-applied-configuration annotations on every Application
  CM) just to satisfy `SchemaDefinitionReconciler.Owns(&corev1.ConfigMap{})`'s
  reconcile-trigger relationship to its emitted bundle CMs. Post-change
  the cache holds only Keystone-emitted bundle CMs (one per active
  SchemaDefinition; currently 3-5).

  User-authored ConfigMap-sourced bundles fall outside the cache scope
  and are now resolved via APIReader (uncached). The
  `migration.NewConfigMapResolver` constructor accepts a `client.Reader`
  rather than a `client.Client` so call sites pass `mgr.GetAPIReader()`;
  the existing cache-backed `client.Client` still satisfies the
  interface for tests using fake clients.

### Refactor

- Promote three magic-string ConfigMap/MigrationBundle labels to
  exported constants in `api/v1alpha1/annotations.go`:
  `LabelSchemaDefinition`, `LabelSchemaRef`, `LabelSource`. The cache
  selector and the SD reconciler's emit path both reference the
  constants, so a future rename is a single-symbol change instead of a
  string grep.
- `internal/webhook/migrationbundle_webhook.go::MigrationBundleValidator`
  gains an `APIReader client.Reader` field auto-wired by
  `SetupWithManager`; the bundle-source resolver uses it. Falls back
  to `Client` when nil so envtest-style tests don't need to plumb both.

### Why

The 2026-05-07 manager OOM cycle (28 OOMKills in 11 hours, RSS pinned
to the 4 GiB cgroup limit, `go_memstats_heap_objects=14M`) had to ship
a `GOMEMLIMIT` bandage in the GitOps repository without root-cause
data. `/debug/pprof/heap` (off by default) gives operators the
top-retainers view inside one minute instead of one deploy cycle. The
AuditEntry transform shrinks the dominant cache resident set by 100K+
entries × ~6 KiB Before/After payload (the runaway-loop blast radius
from 2026-05-06). The ConfigMap cache scope removes a wildly broad
cluster-wide watch that the SchemaDefinition `Owns` reconcile-trigger
needed only for its own emitted bundle CMs.

## [v0.1.1] — 2026-04-24

Patch release. **Critical fix** — re-enables safe deployment after
the 2026-04-21 etcd-wedge incident.

### Fixed

- `audit.Logger.Append` no longer hot-retries on `IsAlreadyExists`
  when `AuditLog.status.nextSequence` lags behind the true max
  `AuditEntry.spec.sequence`. Root cause of the 2026-04-21
  150-180 writes/sec etcd storm. On the 409 path the retry now
  re-scans `max(AuditEntry.spec.sequence)` and picks `max+1`,
  breaking the loop deterministically.
- Exponential backoff with ±25% jitter between Append retries
  (50ms × 2^attempt, capped at 5s), `context.Done()`-aware so
  manager shutdown doesn't wait out the backoff.
- `MaxRetries` default bumped 5 → 8 (safe now that attempts
  converge instead of repeating).

### Added

- `AuditLogReconciler` (new controller) — 30s-tick background
  reconciler that heals `AuditLog.status.nextSequence` from
  `max(AuditEntry.spec.sequence)` whenever drift is detected.
  Intentionally does NOT Watch AuditEntry events (would
  reintroduce the write-amplification the fix dampens).
  Defence-in-depth partner of the Logger fix.
- Prometheus metrics for alerting on the failure mode:
  - `keystone_audit_append_retries_total{reason}` — alert on
    `reason="alreadyexists"` rate > 1/s for 2m.
  - `keystone_audit_append_total{outcome}` — success / error
    / exhausted.
  - `keystone_audit_log_drift_healed_total` — non-zero rate
    signals concurrent-Append pressure.
- Regression test `TestLogger_Append_RecoversFromStaleAuditLogStatus`
  reproducing the exact 2026-04-21 pattern; verified to fail
  on pre-fix code.

### Deployment notes

The 2026-04-21 incident was silent for 50 minutes because only
raw etcd write load changed. Before re-enabling `keystone-manager`
on-cluster, wire the `keystone_audit_append_retries_total`
counter into your Prometheus stack and alert on it.

Post-deployment follow-ups (tracked as separate issues):
- `maxEntrySequence` should use `APIReader` (quorum read) on
  the 409 path for single-retry convergence.
- `AuditLogReconciler` should detect and refuse to heal on
  gap-creation scenarios (log warning, surface via
  `status.conditions`) rather than silently papering over
  sequence gaps that would break `VerifyChain`.
- Concurrent-Append + exhaustion tests.

## [v0.1.0] — 2026-04-16

First public release. **Alpha**: API surface (`keystone.hexxlock.io/v1alpha1`)
may change between minor releases without a deprecation cycle. Intended
for evaluation deployments and early integrators.

### Added

#### CRDs (12 total)

- `ClusterRegistration` (cluster-scoped) — workload cluster inventory
  with `tier`, `region`, `dataResidency`.
- `DatabaseProvider` (cluster-scoped) — physical PostgreSQL cluster
  reference with admin credentials Secret reference and SSL mode
  (`disable` rejected at API).
- `LogicalDatabase` (namespaced) — declarative `CREATE DATABASE` target.
- `DatabaseSchema` (namespaced) — declarative `CREATE SCHEMA` target
  inside a logical database, with default privileges and search-path
  hints.
- `MigrationBundle` (namespaced) — versioned SQL pack OR pgroll-style
  operations to apply. Supports content-hash anti-replay.
- `MigrationPlan` (namespaced) — controller-generated dry-run snapshot
  (Phase 3.1 will materialise these; today bundles jump straight to
  Execution).
- `MigrationExecution` (namespaced) — per-target run with phase state
  machine (Pending → Running/Expanding → Expanded → Contracting →
  Succeeded | Failed | Aborted).
- `SchemaPolicy` (cluster-scoped) — declarative lint level + blocked
  keywords + approval requirements.
- `ProductDefinition` (cluster-scoped) — platform-author's product
  blueprint with isolation modes and lifecycle hooks.
- `ProductInstance` (namespaced) — tenant × product subscription with
  finalizer-controlled deprovisioning.
- `DriftReport` (namespaced) — drift detection record with severity.
- `RolloutPolicy` (cluster-scoped) — staged rollout pipeline with
  parallelism, soak, and approval gates.

#### Controllers (5 reconcilers + 1 periodic Runnable)

- `LogicalDatabaseReconciler` — finalizer-controlled. Idempotent CREATE
  ROLE / CREATE DATABASE / CREATE EXTENSION via `internal/postgres`.
- `DatabaseSchemaReconciler` — waits for parent LogicalDatabase Ready,
  then ensures schema + grants + ALTER DEFAULT PRIVILEGES.
- `MigrationBundleReconciler` — fans out one MigrationExecution per
  matched DatabaseSchema. Refuses content-hash drift on already-applied
  versions. Supports staged rollout via `spec.rolloutPolicyRef`.
- `MigrationExecutionReconciler` — runs SQL in transaction with LOCAL
  search_path. Two strategies: `versioned` (transactional file apply)
  and `pgroll-expand-contract` (multi-phase with operator complete
  annotation).
- `ProductInstanceReconciler` — orchestrates tenant lifecycle by
  creating child LogicalDatabase + DatabaseSchema CRs with
  `ownerReferences` for Argo CD prune. Templated names with
  `{{tenantID}}` / `{{tenantSlug}}` / `{{productSlug}}` substitution.
- `DriftController` — periodic (default 1h, leader-elected). Inspects
  every Ready DatabaseSchema via `information_schema` (NOT pg_dump —
  distroless-safe). Compares against per-schema `keystone_baselines`
  table; emits DriftReport on mismatch.

#### Engine

- `internal/postgres` — pgx pool cache keyed by hashed credentials.
  Idempotent admin DDL helpers with race-safe duplicate handling.
  Defence-in-depth identifier validation (`^[a-z_][a-z0-9_]{0,62}$`)
  with 31 unit tests covering SQL injection vectors.
- `internal/migration` — ConfigMap source resolver + transactional
  runner with golang-migrate-compatible `schema_migrations` table
  (plus `content_hash`, `duration_ms`, `applied_by` columns).
- `internal/migration/pgroll` — lightweight expand/contract engine for
  `add_column`, `drop_column`, `rename_column`. Trigger-based dual-write
  for renames; multi-statement DDL inside transactions.
- `internal/drift` — schema fingerprint inspector + per-schema baseline
  storage.
- `internal/conditions` — apimachinery `meta.SetStatusCondition` wrapper
  with deterministic `ObservedGeneration`.

#### Policy

- Conftest (Rego) policies under `policy/conftest/` for MR-time
  validation. Unit tests included.
- Squawk lint script under `hack/lint-sql.sh` for SQL files.
- Kyverno `ClusterPolicy` for admission-time validation
  (`<gitops-repo>/keystone/policies/`).

#### Observability

- 4 Prometheus metric families:
  `keystone_drift_detected_total`,
  `keystone_drift_checks_total`,
  `keystone_drift_check_duration_seconds`,
  `keystone_drift_staleness_seconds`.
- Standard `controller_runtime_*` reconcile metrics across all 5
  controllers.
- PrometheusRule + Grafana dashboard manifests in the GitOps repository.
- Runbook (`docs/runbook.md`) and observability guide
  (`docs/observability.md`).

### Architecture

- Multi-cluster via hub-and-agent pattern with `ClusterRegistration`
  inventory (tier-based rollout). Phase 7.1 splits per-cluster agents;
  v0.1.0 ships hub-only.
- ArgoCD-native deployment: CRDs and operator manifests live in the
  source repo; ArgoCD Applications in the GitOps repository source from
  there so CRD schema and controller code version together.

### Security

- AGPL-3.0-or-later for the operator + engine; Apache-2.0 for the SDK.
- Pod Security Standards `restricted` enforcement on the
  `keystone-system` namespace.
- Linkerd injection on `keystone-system` for mTLS to PostgreSQL
  providers and other in-cluster services.
- HTTP/2 disabled by default on the metrics endpoint
  (CVE-2023-44487 + CVE-2023-45288).
- Cross-namespace Secret references rejected at controller and Kyverno
  layers.
- DCO sign-off enforced on every commit.

### Known limitations (deferred)

- Phase 2.1 — envtest integration tests across all 5 controllers.
- Phase 3.1 — `MigrationPlan` CR materialisation + Squawk in
  controller plan phase.
- Phase 3.2 — removal of embedded `.Migrate()` calls in legacy
  `helpdesk` / `cms` / `comments` services.
- Phase 4.1 — APISIX route registration, SpiceDB tuple writes, Vault
  dynamic credential issuance, lifecycle webhook delivery.
- Phase 4.2 — scale legacy `db-provisioner` Deployment to 0.
- Phase 5.1 — per-object diff in `DriftReport.status.findings`,
  acknowledge / accept-as-baseline workflow.
- Phase 6.1 — multi-operation `MigrationBundle`s, `add_constraint` with
  online validation, `alter_column` type changes.
- Phase 6.2 — real `xataio/pgroll` integration as alternative engine.
- Phase 7.1 — cross-cluster execution dispatch, stage-level Prometheus
  metrics.
- Phase 8.1 — OpenTelemetry tracing, multi-window burn-rate alerts.

[v0.1.0]: https://github.com/dogukanturhal/platform/releases/tag/keystone-v0.1.0
[v0.1.1]: https://github.com/dogukanturhal/keystone/releases/tag/keystone-v0.1.1
