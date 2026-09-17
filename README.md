# Keystone

> Database lifecycle for tier-1 multi-tenant SaaS.
> GitOps-native. Policy-gated. Zero-downtime.

Keystone is a Kubernetes operator that owns the database lifecycle — schema
migrations, tenant provisioning, staged multi-tenant rollouts, drift
detection, and zero-downtime expand/contract — through declarative CRDs
reconciled by GitOps.

It is the open-source replacement for the homegrown `db-migrator` and
`db-provisioner` services that previously powered the HexxLock platform.

## Status

**Beta — in production use at HexxLock. The CRD API is not yet frozen.**

Fourteen controllers are implemented and reconciling: `LogicalDatabase`,
`DatabaseSchema`, `DatabaseProvider`, `SchemaDefinition`, `SchemaSnapshot`,
`MigrationBundle` (including staged rollout), `MigrationPlan`,
`MigrationExecution`, `ProductInstance`, `DriftReport` and drift acceptance,
`AuditLog`, `AuditEntry` retention, and CR TTL cleanup.

The following are declared in the API but **not** honored by any controller.
They are release blockers, not roadmap items — the API currently accepts
configuration it silently ignores.

| Surface | State |
|---|---|
| `--role=agent`, hub-and-spoke multi-cluster | **Not implemented.** The flag is parsed and logged but never branched on. Every deployment runs as hub and dispatches all executions itself. |
| `defaultIsolation` / `isolation` / `isolationOverride` (pool, bridge, silo) | **Not implemented.** See the warning below. |
| `ClusterRegistration.status` | Never written. The resource is read only for rollout tier lookup. |

### Isolation modes

> **Warning — do not rely on `isolation` for data segregation.** All three
> modes are declared in the API and accepted by CRD enum validation, but no
> controller reads them. Every `ProductInstance` is provisioned as **bridge**
> (shared database, schema-per-tenant), including instances that declare
> `silo`. A workload that requires database-per-tenant isolation for
> compliance reasons is not getting it, and nothing in the status surface
> says so.

See [`docs/roadmap.md`](docs/roadmap.md) for the phased delivery plan.

## Architecture

```
                        ┌───────────────────────────┐
                        │         Git (SSoT)        │
                        │   keystone.hexxlock.io    │
                        │   manifests + policies    │
                        └────────────┬──────────────┘
                                     │ ArgoCD (ApplicationSet, hub-and-spoke)
                                     │
        ┌────────────────────────────┼────────────────────────────┐
        ▼                            ▼                            ▼
┌──────────────┐          ┌─────────────────────┐        ┌──────────────────┐
│  Hub cluster │          │ Workload cluster A  │        │ Workload cluster │
│              │ status   │                     │        │ B, C, …          │
│ • Planner    │◄─────────┤ • keystone-agent    │        │ • keystone-agent │
│ • Policy DB  │  via Git │ • Kyverno admission │        │ • Kyverno        │
│ • Drift      │          │ • Migration runner  │        │ • Migration      │
│   detector   │          │   (pgroll engine)   │        │   runner         │
└──────────────┘          └─────────────────────┘        └──────────────────┘
```

**The diagram above is the target architecture, not the shipped one.** The
binary accepts `--role={hub,agent}` but does not branch on the value: every
deployment runs as a hub and dispatches every migration execution itself.
Cross-cluster agent dispatch is not implemented, and `--role=agent` silently
behaves as `hub`. Today's rollout "tiers" are labels on logical clusters that
share the hub. Status is intended to flow back through Git rather than direct
cross-cluster API calls.

## Core CRDs

| Group / Kind                          | Purpose                                    |
|---------------------------------------|--------------------------------------------|
| `ClusterRegistration`                 | Registers a workload cluster (tier, region)|
| `DatabaseProvider` → `Cluster` → `Shard` | Physical topology                       |
| `LogicalDatabase` → `DatabaseSchema` → `SchemaRole` | Logical topology              |
| `MigrationBundle` → `Plan` → `Execution` | Versioned and declarative migrations    |
| `ProductDefinition` → `ProductInstance` | Tenant lifecycle (isolation modes declared but not enforced — see Status) |
| `TenantAssignment`                    | Tenant → shard binding (consumed by router)|
| `SchemaPolicy` + `RolloutPolicy`      | Lint rules + staged rollout (canary→paid) |
| `DriftReport`                         | Hourly `pg_dump` diff against declared    |

## Licensing

- **Operator and engine**: GNU Affero General Public License v3.0 or later
  ([`LICENSE`](LICENSE)). The AGPL-3.0 closes the network-service loophole
  exploited by hyperscalers — anyone offering Keystone as a managed service
  must release their modifications. End users running Keystone for their
  own workloads are not affected.
- **SDK** (`pkg/sdk/`): Apache License 2.0 ([`LICENSE-Apache-SDK`](LICENSE-Apache-SDK)).
  Permissive so providers (Crossplane, Terraform, third-party integrations)
  can build against the public API without inheriting AGPL.

## HexxLock Cloud (commercial)

The OSS operator is fully featured for self-hosted use. HexxLock offers a
managed cloud product on top:

- **Pro Seats** — SaaS dashboard, SSO, audit retention, AI plan explanation
- **Pipelines** — hosted CI runners for plan + lint + dry-run
- **Monitoring** — hosted drift detection + SLO dashboards + alerting
- **Enterprise** — dedicated support, SLA, air-gapped installer

Cloud and OSS are interoperable. Migrating between them is a CRD export.

## Audit pipeline (T2 #25)

Keystone implements a dual-write audit pipeline to meet the 7-year WORM
retention mandate (`project_security_stack_tier1_2026_04_27`) while
simultaneously offloading audit storage pressure from etcd
(internal engineering notes).

### Architecture

```
audit.Logger.Append()
  │
  ├── [always] Create AuditEntry CR (etcd)   ← authoritative, chain-hashed
  │
  └── [best-effort] Exporter.Export()
        │
        ├── NATSExporter → NATS JetStream stream KEYSTONE_AUDIT
        │     subject: audit.keystone.<verb>
        │     Nats-Msg-Id: spec.SelfHash  (JetStream dedupe key)
        │
        └── StdoutJSONExporter → Loki / Alloy / Promtail (C1)

Phase 3 audit-archiver (separate service):
  KEYSTONE_AUDIT JetStream stream → pull consumer → MinIO WORM bucket
  (object-lock retention 7 years, per security-stack tier-1 decision)
```

The CR is authoritative. A dropped JetStream publish is best-effort and
recoverable via the backfill tool. Export errors are logged but never
propagate back to the reconciler — a broken NATS broker does not affect
operator availability.

### Enabling NATS export

Add to the operator deployment (or Argo helm-values block in
`<gitops-repo>/keystone/manager-application.yaml`):

```yaml
controller:
  extraEnv:
    - name: AUDIT_NATS_URL
      value: "nats://nats.messaging-system.svc.cluster.local:4222"
    - name: AUDIT_NATS_SUBJECT_PREFIX
      value: "audit.keystone"
```

The operator opts in by setting `AUDIT_NATS_URL`. When the variable is empty
(the default) no NATS connection is attempted and behaviour is identical to
before Phase 4.

Flags also accepted:
- `--audit-nats-url` (overrides `AUDIT_NATS_URL`)
- `--audit-nats-subject-prefix` (default `audit.keystone`)
- `--audit-nats-publish-timeout` (default `5s`)

### Backfilling the existing 109k entries

Use `keystone-backfill` to replay existing AuditEntry CRs into NATS
JetStream. Idempotent — already-published entries are silently deduplicated
by JetStream via the `Nats-Msg-Id` / `SelfHash` header.

```sh
# Dry-run: count entries without publishing
keystoneadm backfill \
  --kubeconfig ~/.kube/config \
  --nats-url nats://nats.messaging-system.svc.cluster.local:4222 \
  --dry-run

# Full backfill
keystoneadm backfill \
  --kubeconfig ~/.kube/config \
  --nats-url nats://nats.messaging-system.svc.cluster.local:4222

# Resume from a checkpoint after interruption
keystoneadm backfill \
  --kubeconfig ~/.kube/config \
  --nats-url nats://nats.messaging-system.svc.cluster.local:4222 \
  --from-sequence 50000
```

Progress is logged every 1,000 entries. Re-running from the same
`--from-sequence` is safe.

### Phase 5a — WriteCR toggle (T2 #25)

Phase 5a adds a `WriteCR bool` field to `audit.Logger` (default `true`) and
a corresponding flag / env-var to the manager. It is the surgical toggle that
lets the operator stop writing `AuditEntry` CRs to etcd once the Phase 4 NATS
path has been proven live for 7 continuous days.

#### Architectural note: chain-head relocation

In Phase 4 (dual-write), the etcd `AuditLog.Status.LastEntryHash` is the
authoritative chain-head pointer. In Phase 5a (`WriteCR=false`) that pointer
moves to the Phase 3 archiver's `_chain/head.json` object in the MinIO WORM
bucket. The archiver writes a new head pointer on every drained message.
`Sequence` and `PrevHash` are therefore `0`/`""` in NATS-published specs when
`WriteCR=false` — ordering is recovered by the archiver from the NATS
JetStream delivery sequence, not from the spec fields.

`SelfHash` (SHA-256 of the canonical spec) is always computed regardless of
`WriteCR`, so the `Nats-Msg-Id` JetStream deduplication key remains stable and
backfill replays are idempotent.

#### Boot-time mode log

At startup the manager emits exactly one of these structured log lines
(field `msg`):

| `AUDIT_WRITE_CR` | `AUDIT_NATS_URL` | Log message |
|---|---|---|
| `true` (default) | _(empty)_ | `audit: CR-only (legacy) — AuditEntry CRs written to etcd; no NATS exporter` |
| `true` | set | `audit: dual-write (Phase 4) — CR authoritative + NATS shadow` |
| `false` | set | `audit: NATS-only (Phase 5a) — chain-head in MinIO; etcd CR writes disabled` |
| `false` | _(empty)_ | **fail-fast** — manager refuses to start (no durable audit sink) |

#### Enabling Phase 5a

Add to the operator deployment (or Argo helm-values block in
`<gitops-repo>/keystone/manager-application.yaml`):

```yaml
controller:
  extraEnv:
    - name: AUDIT_WRITE_CR
      value: "false"
    - name: AUDIT_NATS_URL
      value: "nats://nats.messaging-system.svc.cluster.local:4222"
```

Flags also accepted:
- `--audit-write-cr=false` (overrides `AUDIT_WRITE_CR`)

#### Phase 5a cutover sequence

1. Confirm Phase 4 NATS dual-write has been running cleanly for **7 continuous
   days** with zero publish errors in `keystone_audit_append_total{outcome="error"}`.
2. Confirm the Phase 3 archiver has drained all entries into MinIO and
   `_chain/head.json` is being updated on every message.
3. Confirm MinIO object-lock / WORM retention policy is active on the bucket
   (`s3api get-object-lock-configuration`).
4. Set `AUDIT_WRITE_CR=false` alongside `AUDIT_NATS_URL` and roll the manager.
   Verify the boot log shows `audit: NATS-only (Phase 5a)`.
5. Monitor `keystone_audit_append_total{outcome="success_nocr"}` — this counter
   rises when `WriteCR=false` and is zero in all other modes. Confirm it matches
   the expected per-minute audit event rate.

#### Phase 5b (future MR)

Phase 5b removes the now-idle CR write code paths (AuditLog singleton,
`maxEntrySequence` scan, AuditEntry CR create, AuditLog status patch) and
retires the `AuditEntry` CRD after the `AuditEntryRetentionController` has
drained the CR backlog. Do not merge Phase 5b until the retention drain is
confirmed complete.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). All commits must be signed off
under the [Developer Certificate of Origin][dco].

[dco]: https://developercertificate.org/
