# keystone-operator

[![Version](https://img.shields.io/badge/Version-0.1.0-blue)](./Chart.yaml)
[![AppVersion](https://img.shields.io/badge/AppVersion-0.1.0-informational)](./Chart.yaml)
[![License](https://img.shields.io/badge/License-AGPL--3.0--or--later-brightgreen)](../../../LICENSE)

Database lifecycle for tier-1 multi-tenant SaaS. GitOps-native.
Multi-cluster. Zero-downtime.

This chart deploys [Keystone](../../../README.md), the AGPLv3 Kubernetes
operator that owns PostgreSQL schema migrations, tenant provisioning,
isolation modes, staged rollouts, drift detection, and zero-downtime
expand/contract — via declarative CRDs.

## TL;DR

```bash
helm install keystone \
  oci://ghcr.io/dogukanturhal/charts/keystone-operator \
  --version 0.1.0 \
  --namespace keystone-system --create-namespace
```

> The canonical distribution is kustomize (`config/default`). This chart
> is a consumer-facing packaging; it must track the kustomize bundle's
> behaviour. Divergences are bugs — file them in
> [github.com/dogukanturhal/platform](https://github.com/dogukanturhal/platform/-/issues).

## Prerequisites

- Kubernetes >= 1.28
- [cert-manager](https://cert-manager.io/) (required unless
  `webhook.certManager.enabled=false` and you supply the CA bundle
  manually)
- Optional: [Prometheus Operator](https://prometheus-operator.dev/) for
  the `ServiceMonitor` / `PrometheusRule` templates
- A reachable PostgreSQL instance (Keystone will not provision the
  server itself)

## Install

### From OCI (HexxLock Harbor)

```bash
helm install keystone \
  oci://ghcr.io/dogukanturhal/charts/keystone-operator \
  --version 0.1.0 \
  --namespace keystone-system --create-namespace
```

### From a local checkout

```bash
helm install keystone ./config/helm/keystone-operator \
  --namespace keystone-system --create-namespace
```

### Production values (example)

```yaml
replicaCount: 2
image:
  repository: ghcr.io/dogukanturhal/keystone-manager
  tag: "0.1.0"
controller:
  leaderElect: true
  role: hub
  driftCheckInterval: 1h
webhook:
  certManager:
    enabled: true
    issuerRef:
      kind: ClusterIssuer
      name: platform-ca
networkPolicy:
  enabled: true
  extraEgressNamespaceSelectors:
    - matchLabels: { app.kubernetes.io/name: cnpg }
metrics:
  serviceMonitor:
    enabled: true
  prometheusRule:
    enabled: true
podDisruptionBudget:
  enabled: true
  minAvailable: 1
```

## Admin credentials

**This chart does NOT accept database admin credentials through values.**
Admin credentials are supplied via Secrets you manage (preferably via an
external secret store — ESO / Crossplane vault provider). Each
`DatabaseProvider` CR references its own Secret by name:

```yaml
apiVersion: keystone.hexxlock.io/v1alpha1
kind: DatabaseProvider
metadata:
  name: my-postgres
spec:
  host: pg.example.com
  port: 5432
  sslMode: require
  maintenanceDatabase: postgres
  adminSecretRef:
    name: my-postgres-admin  # Secret you create separately
    usernameKey: username
    passwordKey: password
```

See [`docs/operator-hardening.md` §2](../../../docs/operator-hardening.md)
for credential rotation and Vault integration.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `systemNamespace` | string | `keystone-system` | Namespace Keystone reads Secrets from (controller rejects cross-namespace refs — ADR 0010). |
| `replicaCount` | int | `2` | Number of manager replicas. Leader election keeps one active. |
| `image.repository` | string | `ghcr.io/dogukanturhal/keystone-manager` | Manager image repository. |
| `image.tag` | string | `""` | Image tag. Defaults to `Chart.appVersion` when empty. |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy. |
| `image.pullSecrets` | list | `[]` | Image pull secret references. |
| `controller.leaderElect` | bool | `true` | Enable leader election. |
| `controller.leaderElectionID` | string | `keystone-manager.keystone.hexxlock.io` | Lock identifier. |
| `controller.role` | string | `hub` | Hub/spoke role. |
| `controller.watchNamespaces` | string | `""` | Comma-separated namespace scope; empty = cluster-wide. |
| `controller.driftCheckInterval` | duration | `1h` | DriftController scan interval. |
| `controller.enableWebhooks` | bool | `true` | Enable admission webhook server. |
| `controller.enableHTTP2` | bool | `false` | Enable HTTP/2 (off by default — CVE-2023-44487). |
| `controller.extraArgs` | list | `[]` | Extra manager CLI args. |
| `resources.requests.cpu` | string | `100m` | CPU request. |
| `resources.requests.memory` | string | `128Mi` | Memory request. |
| `resources.limits.cpu` | string | `1` | CPU limit. |
| `resources.limits.memory` | string | `512Mi` | Memory limit. |
| `podSecurityContext` | object | restricted PSS | Pod securityContext. |
| `containerSecurityContext` | object | restricted PSS | Container securityContext. |
| `metrics.enabled` | bool | `true` | Expose Prometheus metrics. |
| `metrics.secure` | bool | `true` | TLS + SAR authz on metrics endpoint. |
| `metrics.serviceMonitor.enabled` | bool | `false` | Create Prometheus Operator ServiceMonitor. |
| `metrics.prometheusRule.enabled` | bool | `false` | Create PrometheusRule with ADR 0014 SLO alerts. |
| `metrics.prometheusRule.reconcileSuccessObjective` | number | `0.995` | Reconcile success SLO. |
| `metrics.prometheusRule.migrationApplyP99Seconds` | number | `30` | Migration apply latency SLO (p99). |
| `webhook.enabled` | bool | `true` | Deploy admission webhook. |
| `webhook.failurePolicy` | string | `Fail` | Admission failure policy. |
| `webhook.certManager.enabled` | bool | `true` | Issue webhook cert via cert-manager. |
| `webhook.certManager.issuerRef.kind` | string | `Issuer` | Issuer kind. |
| `webhook.certManager.issuerRef.name` | string | `selfsigned` | Issuer name. |
| `webhook.certManager.privateKey.encoding` | string | `PKCS8` | Required for Linkerd compatibility. |
| `serviceAccount.create` | bool | `true` | Create ServiceAccount. |
| `rbac.create` | bool | `true` | Create ClusterRole + ClusterRoleBinding. |
| `networkPolicy.enabled` | bool | `true` | Manager pod NetworkPolicy. |
| `networkPolicy.extraEgressCIDRs` | list | `[]` | External PG egress CIDRs. |
| `networkPolicy.extraEgressNamespaceSelectors` | list | `[]` | In-cluster PG namespace selectors. |
| `podDisruptionBudget.enabled` | bool | `true` | Create PDB (only when replicaCount >= 2). |
| `podDisruptionBudget.minAvailable` | int/string | `1` | PDB minAvailable. |
| `otel.endpoint` | string | `""` | OTLP tracing endpoint. |
| `otel.insecure` | bool | `false` | OTLP over plaintext. |

See [`values.yaml`](./values.yaml) for the full set, including the
complete type-checked schema in [`values.schema.json`](./values.schema.json).

## Upgrade notes

### From kustomize

The chart is drop-in compatible with the kustomize bundle for the default
ServiceAccount / RBAC names. Leader election lock lives in the same
namespace. You can migrate in-place:

1. `helm install keystone … --set serviceAccount.name=keystone-manager`
2. Delete the kustomize-managed Deployment + Service +
   ClusterRoleBinding; the chart takes over.
3. Delete the kustomize-managed `ValidatingWebhookConfiguration` after
   verifying the chart's replacement is serving.

### Across chart versions

- **Minor bumps** are additive (new fields, new templates). Safe to
  rolling upgrade.
- **Major bumps** carry breaking changes in values.yaml structure; see
  `CHANGELOG.md` in the repository root.

## Uninstall

```bash
helm uninstall keystone --namespace keystone-system
```

CRDs installed from `crds/` are **not** removed by `helm uninstall`
(Helm policy). Remove manually after verifying no CRs depend on them:

```bash
kubectl get crd -o name | grep keystone.hexxlock.io | xargs kubectl delete
```

> Deleting CRDs cascades to every `LogicalDatabase`, `DatabaseSchema`,
> etc. in the cluster. External databases are retained only when their
> CRs had `deletionPolicy: Retain`. Re-read
> [`docs/runbook.md`](../../../docs/runbook.md) before destructive cleanup.

## Development

Run `helm lint` + `helm template` smoke-test from the service root:

```bash
helm lint config/helm/keystone-operator/
helm template test config/helm/keystone-operator/ > /tmp/out.yaml
kubectl apply --dry-run=client -f /tmp/out.yaml
```

Regenerate this README's values table from the source `values.yaml`:

```bash
helm-docs --chart-search-root config/helm/keystone-operator/
```

(`helm-docs` is optional; the table above is hand-maintained for now.)

## License

AGPL-3.0-or-later. See [`LICENSE`](../../../LICENSE) and
[`NOTICE`](../../../NOTICE).
