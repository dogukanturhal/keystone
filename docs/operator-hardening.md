# Keystone operator — hardening guide

This document is the production-grade checklist for running
`keystone-manager` in a hostile-by-default Kubernetes cluster. It
extends `SECURITY.md` (vulnerability policy) with the concrete
deployment controls every tier-1 installation is expected to have.

Applies to: keystone-manager v0.1.x. Some items reference Phase 10+
features still in flight; those are marked **(roadmap)**.

---

## 1. Network isolation

### NetworkPolicy (hard requirement)

The manager pod MUST only:

- Accept inbound traffic from the webhook Service (port 9443/TLS) and
  the metrics Service (port 8443/TLS, authn/authz).
- Egress to:
  - `kube-apiserver` on its Service port
  - Every `DatabaseProvider` host/port declared in the cluster
  - OTLP collector if `--otel-endpoint` is set

A sample policy lives at `config/networkpolicy/manager.yaml`. Apply
it even on single-tenant clusters — the blast radius of a compromised
admin-credential leak is otherwise the full set of reachable PG
clusters.

### kube-apiserver egress

Do not rely on the default `ClusterFirst` DNS. In multi-tenant
clusters a compromised in-cluster workload can publish
`keystone.keystone-system.svc` and intercept the webhook's reply.
Pin the webhook's `clientConfig.caBundle` to a cert issued by
cert-manager; reject all untrusted CAs via the webhook TLS config.

---

## 2. Secret handling

### Admin credentials (DatabaseProvider Secret)

- The Secret MUST live in the `keystone-system` namespace; the
  controller rejects cross-namespace references. This is enforced by
  `LogicalDatabaseReconciler.resolveCredentials` (see ADR 0010).
- Rotate admin passwords at least every 90 days. The manager
  re-reads the Secret on every reconcile; rotation takes effect
  within the next cache invalidation (≤5 min).
- Prefer Vault integration: use `external-secrets-operator` or
  Crossplane `kubernetes.vault.upbound.io` to sync from Vault to the
  Secret. This keeps the Secret object short-lived (TTL-backed).

### Webhook serving cert

- Issued by cert-manager (`Issuer` or `ClusterIssuer`) with a 90-day
  rotation.
- Keep the private key in `PKCS#8` encoding — this is Linkerd's
  compatibility requirement and is documented by the HexxLock
  platform team in internal engineering notes.
- The CA bundle in the `ValidatingWebhookConfiguration` uses
  `cert-manager.io/inject-ca-from` — never pin it by hand; cert
  rotation would break admission silently.

### Migration SQL is NOT sensitive

Migration SQL lives in ConfigMaps, not Secrets. Treat it as
code-reviewable plaintext and protect it via Git + RBAC, not via
Secret confidentiality. If a migration needs a runtime secret (rare),
inject via `pgpass` from a Secret in the provider namespace and
document the pattern.

---

## 3. Pod security

### SecurityContext baseline

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
  seccompProfile:
    type: RuntimeDefault
```

This matches Kubernetes' `restricted` PodSecurity Standard. The
official manifest ships these defaults; don't relax them.

### Resource limits

```yaml
resources:
  requests: { cpu: 100m, memory: 128Mi }
  limits:   { cpu: 1,    memory: 512Mi }
```

The manager is I/O-bound, not CPU-bound. If you run against hundreds
of DatabaseProviders, scale the memory limit — Prometheus's
`keystone_pool_cache_entries` is the signal.

### Distroless image

The shipped image is distroless (static). A shell is not available.
If you need to debug inside the pod, use `kubectl debug` with a
debug-tagged ephemeral container, not an in-image shell.

---

## 4. RBAC scope

### Cluster-scoped resources the manager reads

- `databaseproviders.keystone.hexxlock.io` (cluster-scoped)
- `clusterregistrations.keystone.hexxlock.io` (cluster-scoped)

### Namespace-scoped resources across the cluster

- `logicaldatabases`, `databaseschemas`, `migrationbundles`,
  `migrationexecutions`, `migrationplans`, `driftreports`,
  `productinstances`, `schemadefinitions`, `schemapolicies`,
  `rolloutpolicies` — watched cluster-wide
- `configmaps` — read-only, only for migration source resolution
- `secrets` — read-only, ONLY in `--system-namespace` (default
  `keystone-system`)

### ServiceAccount isolation

Run the manager with a dedicated ServiceAccount. Never reuse the
default namespace SA — other pods in the namespace inherit its RBAC
by default.

### Impersonation

The manager does NOT impersonate users. All API calls go through its
own SA identity; audit logs show the SA, not the author of the
CR. When audit needs to tie a change back to a human, consult Git
history or the admission webhook's `userInfo` that Kubernetes
records.

---

## 5. Supply chain

### Image verification at admission

Enforce cosign-signed images via a Kyverno (or other admission)
policy. **Use keyed signing if your CI provider's OIDC issuer is not
in Fulcio's curated trust list** — most self-hosted GitLab/Gitea/
Forgejo deployments fall in this bucket and keyless will fail with
HTTP 400 "error processing the identity token".

Keyed example (production-ready, matches keystone's own CI default):

```yaml
spec:
  validationFailureAction: Enforce
  rules:
    - name: keystone-signed
      match:
        any:
          - resources:
              kinds: [Pod]
              namespaces: [keystone-system]
      verifyImages:
        - imageReferences:
            - "harbor.example.com/keystone/*"
          required: true
          mutateDigest: true
          verifyDigest: true
          attestors:
            - count: 1
              entries:
                - keys:
                    publicKeys: |-
                      -----BEGIN PUBLIC KEY-----
                      <embed your cosign.pub here>
                      -----END PUBLIC KEY-----
                    rekor:
                      ignoreTlog: true
```

Sigstore Rekor is skipped on the verifier (`rekor.ignoreTlog: true`)
because keystone CI ships with `COSIGN_TLOG_UPLOAD=false` — there's no
public Rekor entry to consult. If your environment can reach the
public Sigstore infrastructure, drop both flags and use the standard
keyless flow.

Two artifacts ship per release; the policy MUST cover both:

```
harbor.example.com/keystone/keystone-manager:v0.1.x
harbor.example.com/keystone/keystone-tools:v0.1.x      # soak runtime
```

### Cosign keypair rotation

**Cadence: quarterly.** The keystone CI signing key SHOULD be rotated
on a fixed schedule — industry baseline is 90 days. Rotation is also
mandatory if there's any suspicion of compromise (CI runner
exfiltration, exposed `COSIGN_PRIVATE_KEY` artifact, ex-employee with
project Maintainer access).

The seven-step procedure is idempotent and recoverable. Practice it
on a non-production keystone clone before doing it in production.

**Step 1 — Generate a fresh keypair locally.**

```bash
mkdir -p /tmp/keystone-cosign-rotate-$(date +%Y%m%d)
cd /tmp/keystone-cosign-rotate-$(date +%Y%m%d)
COSIGN_PASSWORD='<new-strong-password>' \
  cosign generate-key-pair
ls cosign.key cosign.pub
```

`cosign.key` is the encrypted private key, `cosign.pub` is the public
half. Keep the working dir in tmpfs or shred it after Step 7.

**Step 2 — Provision the new key into CI variables.**

```
hexxlock/keystone → Settings → CI/CD → Variables

  COSIGN_PRIVATE_KEY  (file, protected, masked)    -> upload cosign.key
  COSIGN_PASSWORD     (env_var, protected, masked) -> the password
```

The CI vars MUST be marked **protected** AND the project's protected
tags pattern MUST include `v*`. Without protected tags, file/env vars
marked protected don't inject into tag-triggered pipelines and cosign
silently falls back to keyless mode (which fails on self-hosted
GitLab — see internal engineering notes memory).

**Step 3 — Update the verifier policy + rotation deadline + Secret
in one MR.**

A single MR (or two coordinated MRs) updates THREE locations
together so the cluster's view stays consistent:

1. `platform-bootstrap/clusters/.../kyverno-keystone-policy.yaml` —
   replace the `publicKeys:` block with the contents of the new
   `cosign.pub`. This is the actual admission gate.

2. `<gitops-repo>/keystone/resources/cosign-rotation/
   lease-and-rule.yaml` — bump `spec.renewTime` on the
   `keystone-cosign-rotation-deadline` Lease to today + 90 days
   (RFC3339 with `.000000Z` suffix). Also bump the
   `keystone.hexxlock.io/keypair-issued-at` annotation to today.
   This is what silences the `KeystoneCosignRotationDue` /
   `KeystoneCosignRotationOverdue` AlertManager alerts.

3. `<gitops-repo>/keystone/resources/soak/cronjob.yaml`
   — replace the `keystone-soak-cosign-pubkey` Secret's `cosign.pub`
   stringData with the new public key, and bump the
   `keystone.hexxlock.io/issued-at` annotation. The keystone-soak
   CronJob's `policy-key-drift` check verifies both this Secret
   and the live ClusterPolicy hold the same public key.

Kyverno fetches the policy fresh from the apiserver on every
admission decision — no cache to bust. Once applied, ALL future
admissions verify against the new key. AlertManager picks up the
new Lease renewTime within one Prometheus scrape (~30s).

**Important:** Do NOT yet delete the OLD key from CI vars. Keep both
keys live until Step 7 — the rollback path depends on it.

**Step 4 — Cut a new keystone release.**

```bash
git tag -a v0.1.x -m 'rotation: cosign keypair'
git push origin v0.1.x
```

The tag pipeline:
1. Builds manager + tools images
2. Scans (vuln-scan-image, sbom-image) — must pass
3. **Signs both images with the NEW key** (the one you just put in CI)
4. Self-tests with `cosign verify --key=cosign.pub` (derived from
   `cosign public-key --key=$COSIGN_PRIVATE_KEY` so verify MUST
   succeed if sign succeeded)
5. Bumps the GitOps repository release tag

If verify-signature fails, the new image was signed with the new key
but the OLD key is still in the policy from before Step 3 finished —
revert CI vars to the old key and investigate before proceeding.

**Step 5 — Verify the cluster admits the newly-signed image.**

```bash
kubectl -n keystone-system rollout status \
  deployment/keystone-keystone-operator
```

Argo + Kyverno will:
1. See the bumped manifest tag
2. Pull the new image from Harbor
3. Verify the signature against the policy's NEW publicKeys
4. Admit the Pod

If admission fails ("no matching signatures"), the policy and the
image disagree on which key signed. Re-check Steps 3 and 4 — most
common error is forgetting to actually merge the policy MR before
tagging.

**Step 6 — Run the soak.**

Don't wait for the next scheduled run — manually trigger:

```bash
kubectl -n keystone-system create job \
  --from=cronjob/keystone-soak keystone-soak-rotation-$(date +%Y%m%d)
```

The `cosign-verify` and `policy-key-drift` checks both exercise
the new keypair end-to-end. A green run is the formal "rotation
verified" signal.

If `policy-key-drift` fails, the keystone-soak-cosign-pubkey Secret
in `keystone-system` still holds the OLD public key. Update it:

```bash
kubectl -n keystone-system patch secret keystone-soak-cosign-pubkey \
  --type=merge \
  -p "$(jq -nc --arg pub "$(base64 -w0 < cosign.pub)" \
    '{data:{"cosign.pub":$pub}}')"
```

Then re-run the soak.

**Step 7 — Retire the old key.**

After at least 24 hours of green soak runs:

1. Remove the OLD `COSIGN_PRIVATE_KEY` and `COSIGN_PASSWORD` values
   from CI vars. (Step 2 replaced them in-place; this just confirms
   the old binary blob is gone from the variable history.)
2. Shred local working dir: `shred -u cosign.key && rm -rf
   /tmp/keystone-cosign-rotate-*`
3. Annotate the rotation in the operator runlog / SECRETS store with
   the rotation date and the new public key fingerprint
   (`cosign public-key --key cosign.key | sha256sum`).

Rotation complete. Next quarter re-uses this runbook unchanged.

### Compromise response

If the private key leaks (CI artifact exfiltration, ex-employee, lost
laptop), perform Steps 1-7 immediately and additionally:

1. Audit Harbor for any image tags signed with the compromised key
   that were NOT produced by your CI. Any unexpected tag with a valid
   signature is a confirmed supply-chain incident.
2. Rotate `HARBOR_PASSWORD` and the Kyverno verifier-fetch credentials
   if the same operator had access to both.
3. Open a public advisory if the leaked key has been used to sign a
   downstream-consumed image.

The CrowdSec community blocklist + the audit-chain emission together
provide post-hoc evidence of any unauthorized push attempts. Pull the
AuditLog snapshot and Harbor access log for the suspected window
before acting.

### SBOM export

Every release attaches `keystone-sbom.spdx.json` AND
`keystone-tools-sbom.spdx.json` to the tag (one per image). Both are
also bound to the image digest as cosign attestations
(`cosign verify-attestation --type spdxjson IMAGE`) so an admission
policy can require their presence the same way it requires the
signature.

### CVE posture

CI fails on HIGH+CRITICAL CVEs with a published fix (`trivy fs
--ignore-unfixed` on every pipeline; `trivy image --ignore-unfixed`
on default-branch + tag). When a CVE with no upstream fix lands, we
add an entry to `.trivyignore` with explicit per-CVE attack-surface
analysis and a removal trigger; entries without that documentation
are rejected at code review.

---

## 6. Audit log forwarding

### Kubernetes audit log

Keystone's first-class audit trail is `kube-apiserver` audit logs
(`audit-policy.yaml`). Enable:

- Metadata-level logging for every verb on the `keystone.hexxlock.io`
  API group
- Request-level logging for CREATE/UPDATE/DELETE verbs on
  `migrationbundles`, `migrationexecutions`, `driftreports`

Forward these to your SIEM. Retain for at least 1 year — GDPR and
PCI DSS both require longer retention for change-management
records, but the default `kube-apiserver` rotation is days.

### Drift acknowledgments

Drift acknowledgment is an annotation on `DriftReport`:

```yaml
annotations:
  keystone.hexxlock.io/drift-accepted-by: "alice@example.com"
  keystone.hexxlock.io/drift-accepted-at: "2026-04-16T10:00:00Z"
  keystone.hexxlock.io/drift-reason: "pre-existing schema, owned externally"
```

The manager records the ack but does NOT verify the email against
a user directory. Upstream SIEM correlation plus apiserver audit
logs are the trust chain.

---

## 7. Tenant isolation

### Namespace scoping

Put every tenant's `LogicalDatabase` + `DatabaseSchema` in a
tenant-specific namespace. Namespace deletion cascades to the CRs;
the finalizer ensures external DB cleanup runs first.

### Network policies between tenants

Manager pod is in `keystone-system`; its NetworkPolicy should permit
egress to the databases it's been declared to manage, nothing else.
A compromised CR in tenant `A` should not be able to coerce the
manager into touching tenant `B`'s database — today the control is
at the `LogicalDatabase.spec.providerRef` level, validated against
the provider's declared allowlist.

### Pool-per-namespace (roadmap)

v0.1 shares the `pg.PoolCache` across all reconcilers. A Phase 12
enhancement scopes pools per LogicalDatabase owner namespace so a
pool-exhausting tenant can't starve another.

---

## 8. Operational signals

The default dashboard lives at
`grafana.example.com/d/keystone-operator`. Watch:

| Metric | Alert threshold | Runbook |
|---|---|---|
| `keystone_reconcile_errors_total` by controller | sustained > 0/min | runbook.md#reconcile-error-spike |
| `keystone_pool_acquire_duration_seconds` p99 | > 5s | runbook.md#pool-acquire-slow |
| `keystone_drift_staleness_seconds` | > 2h | runbook.md#drift-inspector-stuck |
| `keystone_manager_up` | == 0 for > 2m | runbook.md#manager-crashloop |

---

## 9. Upgrade story

### Running manager

- Upgrade is a standard Deployment rollout. Use `maxUnavailable: 0`
  and leader election (`--leader-elect=true`) so the quiescing pod
  completes its in-flight reconciles before the new one takes over.
- CRD upgrades are additive in v1alpha1. Field removals land in
  v1beta1 with a conversion webhook (Phase 13 roadmap).

### Downgrade

Supported only between adjacent minor versions. The rules:

- No CRD field removals in a patch release
- Deprecations land N-1 minors before removal
- The deprecation appears in release notes, ADRs, and CRD field
  descriptions

Rollback runbook: `docs/runbooks/downgrade.md` (roadmap).

---

## 10. Disaster recovery

### State that must be restored

- CRs in etcd (restored via cluster backup — velero or similar)
- Migration history in PG (per-database `schema_migrations` or
  `keystone_schema_migrations`)

The manager keeps NO authoritative state of its own. All operational
state derives from CRs + the databases themselves. A fresh manager
pod on a restored etcd rebuilds pool cache + reconciler state
automatically.

### DR drills

Run a DR drill quarterly: delete the manager Deployment, delete its
ServiceAccount, delete every pod in `keystone-system`, restore from
etcd snapshot, verify all `LogicalDatabase` CRs converge to
Ready=True within 10 minutes. Document drill outcomes in the DR
log; a runbook entry `docs/runbooks/dr-drill.md` is planned.

---

## 11. What's NOT hardened yet (transparent gaps)

- **Per-tenant rate limiting** on reconcile — a tenant spamming
  MigrationBundles can starve others. (Phase 12 follow-up.)
- **CRD conversion webhook** — v1alpha1 only, no in-cluster rollback
  path yet. (Phase 13.)
- **Admission webhook failure policy** is currently `fail` — a
  webhook outage blocks all writes. Acceptable for v0.1; high-
  availability (2 replicas + PDB + service-level affinity) lands in
  Phase 12.
- **Static analysis of operator code** (semgrep, gosec) — CI runs
  trivy + golangci-lint only. Add `gosec ./...` to CI in Phase 12.

These are documented honestly rather than hidden. Tier-1 graduation
(Phase 16) requires all of them closed.
