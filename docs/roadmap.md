# Keystone — 14-week delivery roadmap

Locked 2026-04-16. Progress tracked in repo issues; phase exits gated by the
exit criteria below, not by date.

## Phase 0 — Bootstrap (week 1) — IN PROGRESS

Stand up the new service skeleton in parallel with the existing `db-migrator`
and `db-provisioner` services. Zero behavior change. Old services keep running
untouched until Phase 8 cuts them over.

**Deliverables**
- [x] `services/keystone/` directory with stub manager binary
- [x] AGPL-3.0-or-later `LICENSE` for operator/engine
- [x] Apache-2.0 `LICENSE-Apache-SDK` for `pkg/sdk/`
- [x] `NOTICE`, `CONTRIBUTING.md` (DCO), `README.md`
- [x] `Dockerfile` (distroless, nonroot)
- [x] `Makefile` with `build / test / lint / tidy / verify`
- [x] Module added to `go.work`
- [ ] Manager builds and runs locally (`make build && bin/keystone-manager --version`)
- [ ] CI builds the image and pushes to Harbor with a 0-replica Deployment

**Exit:** `kubectl get deploy -n keystone-system keystone-manager` shows
`0/0` ready (no-op deployment). `db-migrator` and `db-provisioner` continue
to run unchanged.

## Phase 1 — CRDs + ClusterRegistration (week 2)

- Define CRDs under `api/v1alpha1/`: `ClusterRegistration`, `LogicalDatabase`,
  `DatabaseSchema`.
- Install CRDs via ArgoCD.
- `BootstrapJob` seeds CRs for every database currently hardcoded in
  `db-migrator/cmd/main.go`.

**Exit:** `kubectl get logicaldatabases.keystone.hexxlock.io` lists every
existing DB. No reconciliation yet (controllers come in Phase 2).

## Phase 2 — SchemaController (weeks 3–4) — IN PROGRESS

Reconciles `LogicalDatabase` and `DatabaseSchema`. Adds `DatabaseProvider`
(cluster-scoped) to model the physical PostgreSQL cluster + admin
credentials. Replaces the hardcoded Go list in `db-migrator`.

**Deliverables**
- [x] `DatabaseProvider` CRD with admin-credentials Secret reference,
      SSL mode (`disable` rejected at API), pool sizing.
- [x] `LogicalDatabase.spec.providerRef` + `deletionPolicy` (Retain default).
- [x] `DatabaseSchema.spec.deletionPolicy` (Retain default).
- [x] `internal/postgres` admin client: pool cache keyed by (provider,
      database, hashed creds); idempotent `EnsureRole`, `EnsureDatabase`
      (with race protection on duplicate_database SQLSTATE 42P04),
      `EnsureExtension`, `EnsureSchema`, `DropDatabase`, `DropSchema`.
- [x] Defence-in-depth identifier validation
      (`^[a-z_][a-z0-9_]{0,62}$`); admission webhook + controller both check.
- [x] Default privileges via `ALTER DEFAULT PRIVILEGES` per role/object
      class; revoke-all when privileges list is empty.
- [x] `internal/conditions` helper wrapping apimachinery's
      SetStatusCondition with deterministic ObservedGeneration.
- [x] `LogicalDatabaseReconciler` + `DatabaseSchemaReconciler` with
      finalizer-controlled cleanup.
- [x] Manager wires both controllers; `--system-namespace` flag pins the
      Secret namespace.
- [x] Manifest replicas lifted from 0 to 1.
- [ ] envtest integration tests (deferred to Phase 2.1 follow-up).
- [ ] BootstrapJob seeding existing db-migrator hardcoded list as CRs
      (deferred to a Phase 2.1 follow-up — needs the SchemaController
      proven against a real cluster first).

**Exit:** Adding a new schema requires a Git commit only. ArgoCD green.
ServiceAccount can `kubectl get logicaldatabases -A`. The first sample CR
applied via Argo CD reaches `status.conditions[Ready]=True`.

## Phase 3 — MigrationController + dual-gate policy (weeks 5–6) — IN PROGRESS

`MigrationBundle` / `MigrationPlan` / `MigrationExecution` + `SchemaPolicy`.
Conftest in CI, Kyverno at admission. Self-managed `.Migrate()` calls in
`helpdesk`, `cms`, `comments` are removed.

**Deliverables**
- [x] Four new CRDs: SchemaPolicy, MigrationBundle, MigrationPlan,
      MigrationExecution.
- [x] `internal/migration/source.go` — ConfigMap source resolver +
      content hashing.
- [x] `internal/migration/runner.go` — transactional SQL apply against
      a target schema; `schema_migrations` table compatible with
      golang-migrate format + content_hash + duration_ms columns.
- [x] `MigrationBundleReconciler` — fans out one MigrationExecution per
      matched DatabaseSchema; refuses content-hash drift on an
      already-applied version.
- [x] `MigrationExecutionReconciler` — runs SQL in transaction with
      LOCAL search_path; records via the runner.
- [x] Conftest policy under `policy/conftest/migrationbundle.rego` with
      unit tests; runs at MR time.
- [x] Squawk lint script under `hack/lint-sql.sh`.
- [x] Kyverno ClusterPolicies in
      `<gitops-repo>/keystone/policies/` for admission-time
      enforcement.
- [ ] Squawk integration into the controller's plan phase (Phase 3.1 —
      currently CI-only).
- [ ] MigrationPlan CR generation (Phase 3.1 — bundle jumps straight to
      Execution today).
- [ ] Removal of embedded `.Migrate()` calls in `helpdesk`/`cms`/`comments`
      (Phase 3.2 — touches three other services).
- [ ] envtest integration tests (Phase 2.1).

**Exit:** Every migration flows through Keystone. Squawk + Conftest
block known unsafe patterns at MR time; Kyverno re-blocks at admission.

## Phase 4 — TenancyController (weeks 7–8) — IN PROGRESS

Lifts the `db-provisioner` state machine into CR reconciliation. Tenant
onboarding becomes `kubectl apply ProductInstance` (via ArgoCD
ApplicationSet sourced from a tenant registry). APISIX/SpiceDB/Vault
HTTP integrations are split into Phase 4.1 to keep the surface
reviewable.

**Deliverables**
- [x] ProductDefinition CRD (cluster-scoped) — replaces iam_products
      row. Spec.slug, displayName, defaultIsolation, databases (with
      per-database isolation override + extensions + schemas), apiRoutes,
      lifecycleHooks.
- [x] ProductInstance CRD (namespaced) — replaces iam_product_instances.
      Spec.tenantID (UUID validated), productRef, region,
      isolationOverride, deletionPolicy. Status state machine with phases
      Pending → Provisioning → Migrating → ConfiguringAuth →
      RegisteringRoutes → Active → (Failed / Deactivating / Deactivated).
- [x] TenancyController (ProductInstanceReconciler) — orchestrates the
      lifecycle; templated database/schema names with {{tenantID}},
      {{tenantSlug}}, {{productSlug}} substitution; idempotent
      CreateOrUpdate of child LogicalDatabase + DatabaseSchema with
      ownerReferences for Argo prune; finalizer-controlled deletion
      that waits for children to clean up first.
- [x] Sample ProductDefinition (example product with core + financials
      databases) and ProductInstance (example tenant).
- [ ] APISIX route registration (Phase 4.1 — needs the apisix Go client
      from the legacy db-provisioner).
- [ ] SpiceDB tuple writes (Phase 4.1 — same — needs the existing
      SpiceDB client).
- [ ] Vault dynamic credential issuance for tenant roles (Phase 4.1 —
      currently the SchemaController grants on a static admin Secret).
- [ ] Webhook delivery for lifecycleHooks (Phase 4.1).
- [ ] Scale legacy db-provisioner Deployment to 0 (Phase 4.2 — depends
      on confirming all consumers cut over to ProductInstance CRs).

**Exit:** Tenant onboarding = `kubectl apply` (via ArgoCD ApplicationSet).
No HTTP API call. `db-provisioner` Deployment scales to 0 in Phase 4.2.

## Phase 5 — DriftController (week 9) — IN PROGRESS

Hourly schema fingerprint inspection (via information_schema, NOT
pg_dump — distroless-safe, version-agnostic), diff against per-schema
recorded baseline, emit `DriftReport` CRs and Prometheus metrics.

**Deliverables**
- [x] DriftReport CRD (namespaced) with severity (info|warning|
      critical), expected/observed hashes, findings slice, conditions.
- [x] internal/drift/inspector.go — information_schema queries for
      tables, columns, indexes, constraints; canonical Snapshot struct;
      deterministic SHA-256 hash.
- [x] internal/drift/baseline.go — keystone_baselines table per schema
      (one row per kind); ReadBaseline / WriteBaseline.
- [x] DriftController — manager.Runnable with leader election; ticks
      every CheckInterval (default 1h, --drift-check-interval flag);
      first observation seeds the baseline (no false-positive flood at
      first deploy); subsequent observations compare and create/update
      DriftReport.
- [x] DatabaseSchema.status.conditions[DriftFree] flipped True/False
      on every check.
- [x] DriftReports auto-resolve (delete) when drift clears; event
      recorded for audit.
- [x] Prometheus metrics: keystone_drift_detected_total (counter),
      keystone_drift_checks_total{outcome} (counter),
      keystone_drift_check_duration_seconds (histogram),
      keystone_drift_staleness_seconds (gauge — alert on staleness > 2×
      interval).
- [ ] Per-object diff in DriftReport.status.findings (Phase 5.1 —
      hash-only detection today).
- [ ] Acknowledge / accept-as-baseline workflow on DriftReports
      (Phase 5.1 — currently the controller silently re-baselines on
      first observation only).
- [ ] PrometheusRule + Alertmanager routing for staleness + drift
      severity (Phase 5.1 — manifests live in the GitOps repository).

**Exit:** A manual DB change in test fires a DriftReport within one
hour and a `keystone_drift_detected_total` counter increment within
one inspection interval.

## Phase 6 — pgroll expand/contract (weeks 10–11) — IN PROGRESS

Lightweight expand/contract engine for zero-downtime column changes.
NOT a full xataio/pgroll port — apps stay on their original
`search_path` (no parallel schema versions); we cover the three
highest-value column-level operations using triggers + helper columns.

**Deliverables**
- [x] MigrationBundle.spec.operations field (mutually exclusive with
      spec.source). One operation per bundle in Phase 6 (multi-op
      deferred to Phase 6.1).
- [x] MigrationOperation kinds: add_column, drop_column, rename_column.
- [x] Per-op specs: AddColumnOp (name, type, default, nullable,
      enforceNotNullInContract), DropColumnOp (name), RenameColumnOp
      (from, to).
- [x] internal/migration/pgroll/engine.go — Engine.Apply(op, phase)
      dispatches to per-kind handlers. Defence-in-depth identifier +
      type validation. Multi-statement DDL inside transactions.
- [x] add_column: Expand adds nullable column with optional default
      (PG 11+ instant fast-path); Contract optionally SETs NOT NULL
      after operator-confirmed backfill.
- [x] drop_column: Expand only writes a COMMENT marking the column for
      drop (apps continue working); Contract issues DROP COLUMN.
- [x] rename_column: Expand reads source column type from
      information_schema, ADDs new column matching that type, COPYs
      data, INSTALLs BEFORE INSERT/UPDATE trigger that mirrors writes
      between the names; Contract DROPs trigger + function + old column.
- [x] New ExecutionPhase values: Expanding, Expanded, Contracting.
- [x] Two-phase reconciler: Expanded waits for explicit operator
      annotation `keystone.hexxlock.io/complete=true` before running
      Contract. Re-queues every 30s until the annotation arrives.
- [x] Crash-recovery: Contracting state re-enters the engine with
      idempotent IF EXISTS / IF NOT EXISTS clauses.
- [x] Sample bundle for the lead.name → lead.full_name canonical case.
- [ ] Multi-operation bundles (Phase 6.1).
- [ ] add_constraint with online validation (Phase 6.1).
- [ ] alter_column type changes via shadow column (Phase 6.1).
- [ ] Real xataio/pgroll integration as an alternative engine for
      teams that need parallel schema versions (Phase 6.2 — major
      dependency lift).
- [ ] envtest tests covering all three operations under simulated
      write load (Phase 2.1 still).

**Exit:** Column rename on a non-trivial table demonstrated under
write load with zero application errors.

## Phase 7 — RolloutPolicy + multi-cluster (week 12) — IN PROGRESS

Tier-aware staged rollout. MigrationBundles fan out through ordered
stages instead of all-at-once. Cluster tier resolved via
DatabaseSchema → LogicalDatabase.spec.clusterRef →
ClusterRegistration.spec.tier.

**Deliverables**
- [x] RolloutPolicy CRD (cluster-scoped) with ordered stages.
- [x] RolloutStage spec: schemaSelector (label match against
      DatabaseSchema.metadata.labels), clusterTierSelector
      (canary|free|paid|internal), parallelism (default 1, refuses to
      default to "all at once"), soakDuration, requireApproval,
      abortOnFailure.
- [x] MigrationBundle.spec.rolloutPolicyRef (optional — empty preserves
      Phase 3 all-at-once behaviour).
- [x] MigrationBundle.status.currentStage + StageHistory (per-stage
      StartedAt, SoakStartedAt, CompletedAt, Targets, Succeeded, Failed).
- [x] reconcileStaged in MigrationBundleReconciler — per-stage filter,
      parallelism cap, soak timer, optional approval annotation
      (keystone.hexxlock.io/approve-stage-<name>=true), automatic
      advancement.
- [x] Cluster-tier resolution helper walks DatabaseSchema →
      LogicalDatabase → ClusterRegistration in the controller cache.
- [x] AbortOnFailure freezes the stage on first failure (status:
      Ready=False reason=StageBlocked); operator must investigate.
- [x] Sample RolloutPolicy (canary 1-parallel 5m → free 10-parallel 15m
      → paid 3-parallel 30m approval-required) + companion second
      ClusterRegistration (prod-eu-west-1-free) so staged rollout has a
      non-canary target.
- [ ] Cross-cluster execution dispatch — Phase 7 currently runs all
      executions from the hub manager regardless of cluster tier (the
      hub-and-only model from Phase 0). Phase 7.1 splits the agent
      out to per-cluster reconcilers; today's "tier" is a label
      attached to logical clusters that share the hub.
- [ ] Stage-level Prometheus metrics (Phase 7.1 — staleness +
      stage-duration histograms).

**Exit:** Canary-first rollout demonstrated end-to-end. A bad migration
hits canary tenants only and freezes the rollout before paid tenants
are touched.

## Phase 8 — Hardening + GA (weeks 13–14) — IN PROGRESS

Operational hardening + v0.1.0 release.

**Deliverables**
- [x] `docs/runbook.md` — failed stage, drift detected, pgroll
      Expanded waiting, provisioning stuck, drift inspector stuck,
      manager crashloop, escalation playbook.
- [x] `docs/observability.md` — metrics catalog, SLOs (per-controller
      reconcile latency p99 < 5s, migration p99 < 5m versioned / 30m
      pgroll, drift staleness < 1h, error rate < 0.5%).
- [x] `CHANGELOG.md` — v0.1.0 entry.
- [x] `hack/chaos-test.sh` — kill manager mid-migration, verify
      idempotent recovery within 90s.
- [x] `<gitops-repo>/keystone/observability/` —
      ServiceMonitor (auth via TokenReview), PrometheusRule (5 alerts +
      3 recording rules), Grafana dashboard ConfigMap, Argo CD
      Application (sync wave 2).
- [x] Manager fallback version bumped to v0.1.0.
- [ ] `git tag keystone-v0.1.0` — done after merge.
- [ ] Public OSS publication to `github.com/hexxlock/keystone`
      (Phase 8.1 — manual step; needs DCO bot setup, CI mirror, and
      go module path migration).
- [ ] Phase 8.1 — OpenTelemetry tracing + multi-window burn-rate
      alerts.
- [ ] Phase 8.1 — delete legacy `services/db-migrator` and
      `services/db-provisioner` (deferred — Example Service may still
      reference them per session memory; needs explicit cutover
      verification).

**Exit:** v0.1.0 tagged on master; Argo CD shows
`keystone-observability` synced; `hack/chaos-test.sh` passes against
the cluster.
