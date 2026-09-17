# 0024. Credential rotation + ESO / Vault integration

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

`DatabaseProvider.spec.adminCredentialsRef.secretName` points at a
Kubernetes Secret that carries the admin username + password for
the target PostgreSQL cluster. The Secret was previously read on
demand by every consumer (six reconcilers duplicate the same
`resolveCredentials` helper — LogicalDatabase, DatabaseSchema,
MigrationExecution, SchemaSnapshot, SchemaDefinition, Drift).

Two things were missing:

1. **Rotation is invisible to Keystone.** When an operator rotates
   the admin credentials — typically by letting ExternalSecrets
   Operator (ESO) or CSI-secret-store pull a new password from
   Vault into the same Secret — the pool cache in
   `internal/postgres.PoolCache` keyed by
   `SHA256(host|port|db|user|password)` STILL holds a pool opened
   with the old password. Future reconciles read the new Secret,
   compute a new cache key, and open a second pool alongside the
   stale one. The stale pool's connections keep burning
   authentication slots until the manager restarts.

2. **No observability on rotation events.** DatabaseProvider.status
   has no "when was the admin Secret last rotated?" field. SOCs
   auditing credential rotation had to join kubectl get-secret
   events with audit logs by hand.

ESO is the industry-standard bridge from Vault (or AWS Secrets
Manager / GCP Secret Manager / etc.) to Kubernetes Secrets. Rather
than building a direct Vault client into Keystone — which would
require dedicated auth flow, TLS, token renewal, audit integration
— we treat the Secret as the only boundary and make Keystone
**rotation-aware** against it.

## Decision

Ship three additions that make the Secret-as-boundary model
correct under rotation:

### 1. Rotation tracking in DatabaseProvider status

Two new fields + one condition:

- `status.credentialsObservedVersion` — the Secret's
  `metadata.resourceVersion` at the last reconcile.
- `status.credentialsRotatedAt` — timestamp of the most recent
  observed change (empty until the first rotation).
- `ConditionTypeCredentialsReady` — `True` when the Secret exists
  with the required keys; `False` with reason `SecretNotFound` /
  `MissingUsernameKey` / `MissingPasswordKey` otherwise.

### 2. DatabaseProviderReconciler (new controller)

Purpose-built reconciler that:

- `Watches(&corev1.Secret{})` and enqueues the DatabaseProvider
  whenever its admin Secret changes. No polling — rotation
  propagates within one reconcile tick.
- On observed ResourceVersion change, calls `PoolCache.DropMatching`
  with a predicate that matches `host + port` of the provider,
  evicting every cached pool bound to the rotated provider
  regardless of password (which we never hold in CR state).
- Emits a `CredentialsRotated` event for SIEM correlation.

### 3. `PoolCache.DropMatching(predicate)` helper

New method on the pool cache that drops every entry whose
`PoolConfig` satisfies the predicate. Lets the reconciler evict
stale pools by host+port without needing the old password.

### ESO integration story

ESO is NOT a Keystone dependency. The operator treats the Secret
as opaque — whether it was created by kubectl, by a Helm chart,
or by an ExternalSecret syncing from Vault is irrelevant. ESO's
role:

```
 ┌──────────────┐     ┌─────────────────┐     ┌────────────┐     ┌───────────────────┐
 │     Vault    │ ──▶ │ ExternalSecret  │ ──▶ │   Secret   │ ──▶ │ DatabaseProvider  │
 │  kv/keystone │     │ (ESO controller)│     │ admin-creds│     │    Reconciler     │
 └──────────────┘     └─────────────────┘     └────────────┘     └───────────────────┘
       ↑                                                                   │
       │                                                                   │
       │  HashiCorp Vault rotates                                           │
       │  every 24h via its DB secrets engine                               │
       │                                                                   ▼
       └───────── Rotation observed;        ┌───────────────────┐
                 pools evicted via     ◀──  │ PoolCache         │
                 DropMatching(host+port)    │ (in-memory)       │
                                            └───────────────────┘
```

Keystone sees only the Secret. Vault-backed rotation, AWS Secrets
Manager, SOPS — any sync mechanism that updates the Secret works.

## Consequences

Easier: **zero-config rotation correctness**. Operators point ESO
at `adminCredentialsRef.secretName` and don't touch Keystone.
Rotation happens; the reconciler observes; stale pools die.

Easier: **rotation visibility**. SOCs can now run `kubectl get
databaseprovider -o jsonpath='{.items[*].status.credentialsRotatedAt}'`
and correlate with audit log entries from C1.

Easier: **no Vault coupling in Keystone**. Clusters that use
CSI-secret-store, SOPS, or plain kubectl for Secret management all
work. The operator stays minimal — one reconciler, one watch.

Harder: **stale pool in-flight connections**. `DropMatching` calls
`Pool.Close()`, which closes idle connections immediately but lets
in-flight queries drain. A long-running migration executed with
old credentials completes on the old connection before getting
force-closed. Usually desirable (no aborted migration); worst-case
6 extra minutes of stale auth after rotation for the longest
migration.

Harder: **predicate is host+port only**. Two providers sharing the
same host+port but different databases or usernames are evicted
together on either one's rotation. Acceptable in practice — a host
that serves N databases usually rotates its admin creds uniformly.

Soft: **six duplicate `resolveCredentials` implementations
untouched**. The tempting cleanup (single shared helper) was
deliberately skipped — SchemaSnapshotReconciler comments call out
the duplication as intentional for narrow refactor scopes. C5
focuses on the *rotation* gap; consolidation is a separate ADR if
it ever becomes urgent.

## Alternatives considered

- **Direct Vault client in Keystone**. Would need Vault auth
  method (Kubernetes auth or token), TLS config, token renewal,
  and a Vault dependency in go.mod. Rejected — ESO already does
  this better and has broader Vault-adjacent support (AWS SM, GCP,
  etc.).

- **Periodic poll of the Secret instead of Watch**. Works but
  wastes API calls. Watches are Kubernetes-native and propagate
  within milliseconds. Rejected.

- **Hash the Secret contents and compare instead of ResourceVersion**.
  Would detect in-place mutations that keep the same RV — which
  can't happen in Kubernetes (every update bumps RV). Rejected as
  redundant complexity.

- **Store observed password hash in Status**. Enables precise
  DropMatching by hash. Rejected — password hashes don't belong
  in CR status (logs, ETL pipelines, backup tarballs). Host+port
  is enough.

- **Unified shared `credentials.Resolver` helper** replacing the
  six duplicated `resolveCredentials`. Tempting but out of C5's
  scope: the rotation problem is orthogonal to the duplication.
  Marked for a future refactor ADR.
