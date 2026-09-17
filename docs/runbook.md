# Keystone Operator — Runbook

> Operational procedures for keystone-manager on call. Pair with
> [`observability.md`](observability.md) for metrics references and SLOs.

---

## Quick reference

| Symptom                                              | Procedure                                  |
|------------------------------------------------------|--------------------------------------------|
| Stage frozen with `Ready=False reason=StageBlocked`  | [Failed stage](#failed-stage)              |
| `DriftReport` exists for a schema                    | [Drift detected](#drift-detected)          |
| `MigrationExecution` stuck in `Expanded`             | [pgroll Expanded waiting](#pgroll-expanded-waiting) |
| `ProductInstance` stuck in `Provisioning`            | [Provisioning stuck](#provisioning-stuck)  |
| `keystone_drift_staleness_seconds` alert             | [Drift inspector stuck](#drift-inspector-stuck) |
| Manager Pod CrashLooping                             | [Manager crashloop](#manager-crashloop)    |

---

## Failed stage

A `MigrationBundle` referencing a `RolloutPolicy` shows
`status.conditions[Ready]=False reason=StageBlocked`. One or more
`MigrationExecution` resources in the active stage hit Failed and the
stage's `abortOnFailure` is true (default).

1. Identify the bundle and stage:
   ```bash
   kubectl get migrationbundles -A -o jsonpath='{range .items[?(@.status.conditions[?(@.type=="Ready")].status=="False")]}{.metadata.namespace}/{.metadata.name} stage={.status.currentStage}{"\n"}{end}'
   ```
2. Inspect the failed executions:
   ```bash
   kubectl get migrationexecutions -A -l keystone.hexxlock.io/bundle=<bundle> --field-selector status.phase=Failed
   ```
3. Read the failure pinpoint:
   ```bash
   kubectl get migrationexecution <name> -n keystone-system \
     -o jsonpath='{.status.failedAtFile}:{.status.failedAtIndex} {"\n"}{.status.failureMessage}{"\n"}'
   ```
4. **Fix path** (pick one):
   - **Patch the SQL**: bump `spec.version` on the bundle (bundles are
     immutable per-version — anti-replay refuses overwrite). Commit a
     new bundle with the fixed SQL.
   - **Skip the failed schema**: remove the matching label from the
     `DatabaseSchema` so the bundle's `schemaSelector` no longer matches
     it. Commit and push.
   - **Roll back the partial schema state manually**: connect to the
     target as the admin role, undo the partial DDL, then `kubectl
     delete migrationexecution <name>` to clear the failed CR. The
     reconciler will recreate it next pass; if you've also fixed the
     SQL, it'll succeed.
5. Once the failed executions are gone or all reach Succeeded, the
   reconciler resumes stage advancement automatically — no manual unblock
   needed.

---

## Drift detected

A `DriftReport` was created. The live PostgreSQL state diverges from
the recorded baseline.

1. Inspect the report:
   ```bash
   kubectl get driftreports -A
   kubectl describe driftreport <schema>-drift -n keystone-system
   ```
2. Read `status.expectedHash` vs `status.observedHash`. The hashes
   alone don't tell you what changed — Phase 5.1 will populate
   `status.findings`. Until then, query directly:
   ```bash
   kubectl exec -n keystone-system deploy/keystone-manager -- \
     psql "$POSTGRES_DSN" -c "\\d+ <schema>.<table>"
   ```
3. **Decide**:
   - **Drift is bad** (manual change someone made): undo the change in
     PostgreSQL. Next inspection auto-resolves the report.
   - **Drift is intentional** (someone applied a migration outside
     Keystone): until Phase 5.1's accept-as-baseline workflow lands,
     manually `UPDATE keystone_baselines SET hash = '<observedHash>'
     WHERE kind = 'structure'` inside the target schema. Source the
     update with `recorded_by = 'manual-acceptance-INC<ticket>'` for
     audit.
4. **Never** `kubectl delete driftreport <name>` to silence — it'll
   regenerate within the inspection interval (default 1h).

---

## pgroll Expanded waiting

A `MigrationExecution` for a `pgroll-expand-contract` bundle is sitting
at `phase=Expanded` and not advancing. This is the deliberate operator
checkpoint.

1. Verify the expand phase succeeded cleanly:
   ```bash
   kubectl get migrationexecution <name> -n keystone-system \
     -o jsonpath='{.status.conditions[?(@.type=="Expanded")].message}{"\n"}'
   ```
2. Confirm app rollout is complete and traffic uses the expanded schema
   (read/write the new column name, or no longer references the
   to-be-dropped column).
3. Trigger contract:
   ```bash
   kubectl annotate migrationexecution <name> -n keystone-system \
     keystone.hexxlock.io/complete=true
   ```
4. Reconciler runs Contract within 30s. Final phase = `Succeeded`.

If you need to **abandon** an Expanded execution (decided not to
contract), the rename's helper trigger and shadow column remain in
place. Either:
- Apply a new bundle that explicitly drops them, or
- Delete the `MigrationExecution` AND manually `DROP TRIGGER` /
  `DROP COLUMN` (Keystone's finalizer cleanup is intentionally minimal).

---

## Provisioning stuck

A `ProductInstance` is in `phase=Provisioning` for more than 5 minutes.

1. Check child resources:
   ```bash
   kubectl get logicaldatabases,databaseschemas -n <ns> \
     -l keystone.hexxlock.io/instance=<instance-name>
   ```
2. Find the laggard:
   ```bash
   kubectl get logicaldatabase <name> -n <ns> \
     -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
   ```
3. Common causes:
   - `DatabaseProvider` admin Secret missing or wrong key —
     `Ready=False reason=CredentialsResolutionFailed`
   - PostgreSQL unreachable — `Ready=False reason=ProviderUnreachable`
   - Bridge mode and the DB does not exist on the provider —
     `Ready=False reason=DatabaseEnsureFailed`

Fix the underlying issue; the `ProductInstance` advances automatically
once children are Ready.

---

## Drift inspector stuck

`keystone_drift_staleness_seconds{schema=...} > 2 *
drift_check_interval` (alerts at 2h with default 1h interval).

1. Confirm the manager is the leader:
   ```bash
   kubectl get lease keystone-manager.keystone.hexxlock.io \
     -n keystone-system -o jsonpath='{.spec.holderIdentity}{"\n"}'
   ```
2. Manager logs:
   ```bash
   kubectl logs -n keystone-system deploy/keystone-manager \
     --tail=200 | grep -i drift
   ```
3. Common causes:
   - Pool dial failures (ProviderUnreachable). Check
     `keystone_drift_checks_total{outcome="error"}` — if non-zero,
     the inspector is hitting the provider but failing.
   - Schema is no longer Ready (skipped by the inspector). Check the
     DatabaseSchema's status.
   - A long-running transaction on the target database is blocking
     the introspection queries. Inspect `pg_stat_activity` for
     long-held locks.

---

## Manager crashloop

```
kubectl logs -n keystone-system deploy/keystone-manager --previous --tail=200
```

Common patterns:

| Log fragment                          | Cause                                       |
|---------------------------------------|---------------------------------------------|
| `unable to acquire leader lease`      | `Lease` resource permission missing — check `keystone-manager` ClusterRole has `coordination.k8s.io/leases` RBAC (regenerate via `make generate`) |
| `failed to create manager`            | Kubeconfig not in-cluster — check the Pod's ServiceAccount + RBAC |
| `metrics server: bind …: address in use` | Port `:8443` collision; either move the metrics port via `--metrics-bind-address` or scale down old pod completely |
| `controller-runtime tls: ...`         | HTTP/2 disabled at our layer; if a metrics scraper is forcing h2, ensure it falls back |

Restore by fixing the underlying RBAC / config and letting the
Deployment's `readinessProbe` recover. Never `kubectl exec` into the
Pod to monkey-patch — see project memory
internal engineering notes.

---

## Escalation

- **Schema corruption** (drift CRITICAL severity, multiple objects
  dropped): page DBA on-call immediately. Do NOT auto-recover via
  Keystone — investigate the source first.
- **Migration applied wrong SQL** (`PriorVersionContentMismatch`
  during retry): the prior application stands. Bump the version and
  ship the corrective migration.
- **Pool exhaustion across providers** (manager OOM, lots of
  `acquire pool` errors in logs): tune `DatabaseProvider.spec.poolMaxConns`
  down for noisy providers.

For incident postmortems, file under `docs/incidents/` with the
`yyyy-mm-dd-<slug>.md` convention.
