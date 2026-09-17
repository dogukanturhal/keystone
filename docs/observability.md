# Keystone Observability — Metrics, Logs, SLOs

> Pair with [`runbook.md`](runbook.md) for the on-call procedures that
> reference these signals.

---

## Endpoint

`https://keystone-manager-metrics.keystone-system.svc.cluster.local:8443/metrics`

Authenticated via TokenReview + authorised via SubjectAccessReview
(controller-runtime's built-in `WithAuthenticationAndAuthorization` —
**no `kube-rbac-proxy` sidecar**, see Phase 1 commit). Prometheus
Operator scrape config goes via a `ServiceMonitor` (Phase 8 manifests in
`<gitops-repo>/keystone/observability/`).

---

## Keystone-specific metrics

### Drift inspection

| Metric                                       | Type      | Labels                          | Notes |
|----------------------------------------------|-----------|---------------------------------|-------|
| `keystone_drift_detected_total`              | counter   | namespace, logical_database, schema | Increments per detected drift. `rate(...)` over a window approximates "how often this schema drifts". |
| `keystone_drift_checks_total`                | counter   | namespace, logical_database, schema, outcome (`clean`\|`drift`\|`error`) | Coverage + error-rate signal. |
| `keystone_drift_check_duration_seconds`      | histogram | namespace, logical_database, schema | Tune `--drift-check-interval` if p99 approaches it. |
| `keystone_drift_staleness_seconds`           | gauge     | namespace, logical_database, schema | 0 after a successful inspection; alerts at `> 2× interval`. |

### Controller-runtime defaults (per Reconciler)

Standard signals automatically exposed by the framework:
- `controller_runtime_reconcile_total{controller, result}`
- `controller_runtime_reconcile_errors_total{controller}`
- `controller_runtime_reconcile_time_seconds{controller}` (histogram)
- `controller_runtime_active_workers{controller}`
- `controller_runtime_max_concurrent_reconciles{controller}`
- `controller_runtime_webhook_requests_total{webhook, code}`

Per-controller names: `logicaldatabase`, `databaseschema`,
`migrationbundle`, `migrationexecution`, `productinstance`. The drift
controller is a `manager.Runnable` (not a Reconciler) so it does not
emit these — it has its own dedicated `keystone_drift_*` family.

### Process / runtime

`go_*`, `process_*`, `controller_runtime_webhook_*` — standard.

---

## SLOs

Targets set against the controller-runtime defaults plus our drift
metrics. Burn-rate alerting follows the Google SRE workbook
multi-window pattern (1h fast burn + 6h slow burn).

| SLI                                                    | SLO                  | Error budget |
|--------------------------------------------------------|----------------------|--------------|
| `controller_runtime_reconcile_time_seconds` p99 (per controller) | < 5s              | 0.1% / 30d |
| `keystone_drift_check_duration_seconds` p99            | < 30s                | 1% / 30d  |
| `keystone_drift_staleness_seconds` max                 | < 3600s              | — (raw threshold) |
| MigrationExecution-to-Succeeded for `versioned` strategy | p99 < 5m end-to-end | 1% / 30d  |
| MigrationExecution-to-Expanded for `pgroll-expand-contract` p99 | < 30m              | 1% / 30d  |
| Failed reconcile rate (`controller_runtime_reconcile_errors_total / _total`) | < 0.5% per controller | 0.1% / 30d |

The PrometheusRule shipping in `<gitops-repo>/keystone/observability/prometheusrule.yaml`
encodes the alert side; SLO recording rules go in the same file.

---

## Logging

Structured zap, JSON, ISO8601 timestamps, no sampling. Log levels:

- `error` — reconcile failures, status patch failures, pool dial
  failures.
- `warn` — drift detected, stage blocked.
- `info` — controller startup, reconcile success at level 0, lifecycle
  transitions.
- `debug` (V(1)) — per-reconcile success traces, hash comparisons.
- `trace` (V(2)) — pool acquire/release, statement timings.

Default level is `info`. Bump to `debug` via `--zap-log-level=debug` on
the manager Deployment.

Correlation: every reconcile log line carries the resource's
namespace/name in `WithValues`. Trace IDs (Phase 8.1) will surface via
the OpenTelemetry exporter.

---

## Tracing

Phase 8 stops short of OpenTelemetry wiring (Phase 8.1). When it
lands, every reconcile becomes a span; per-statement DDL becomes child
spans; drift inspection becomes a long-running span. Exemplars on the
controller-runtime histograms link directly to traces.

---

## Runtime profiling (`/debug/pprof/*`)

Off by default, opt-in via `--enable-pprof=true` on the manager (or
`metrics.pprof.enabled: true` in the helm chart). When on, the standard
`net/http/pprof` handlers are registered on the same TLS bind address
as `/metrics`, so they share the same TokenReview + SubjectAccessReview
chain. They are NOT bound to the same RBAC: the `/metrics` nonResourceURL
grant does not implicitly grant `/debug/pprof/*`, by design — pprof
exposes function names, ASLR offsets, and allocation call graphs that go
well beyond what metrics scraping justifies.

### Enabling

In `values.yaml` (or via `--set` on `helm upgrade`):

```yaml
metrics:
  pprof:
    enabled: true                 # threads --enable-pprof=true through
    renderReaderClusterRole: true # default; chart renders a separate role
```

This re-renders the manager Deployment with `--enable-pprof=true` and
emits a `keystone-pprof-reader` ClusterRole granting
`get` on nonResourceURLs `[/debug/pprof, /debug/pprof/*]`. The role is
NOT bound — operators bind it on demand for an investigation, then
unbind.

### Investigation flow

```bash
# 1. Bind the reader role to a debugging ServiceAccount.
kubectl create clusterrolebinding keystone-pprof-temp \
  --clusterrole=keystone-keystone-operator-pprof-reader \
  --serviceaccount=monitoring:kube-prometheus-stack-prometheus

# 2. Port-forward + capture profile.
POD=$(kubectl -n keystone-system get pods \
  -l app.kubernetes.io/name=keystone-operator -o name | head -1)
kubectl -n keystone-system port-forward "$POD" 18443:8443 &
TOKEN=$(kubectl -n monitoring create token kube-prometheus-stack-prometheus --duration=600s)

curl -sk -H "Authorization: Bearer $TOKEN" \
  https://127.0.0.1:18443/debug/pprof/heap > /tmp/keystone-heap.pprof
go tool pprof -top -inuse_objects /tmp/keystone-heap.pprof | head -30
go tool pprof -top -inuse_space  /tmp/keystone-heap.pprof | head -30

# 3. Unbind. Permanent bindings would mean any compromise of the bound
# SA's token leaks runtime state — keep the window measured in minutes.
kubectl delete clusterrolebinding keystone-pprof-temp
```

### Endpoints

The standard set is registered:

| Path                    | Purpose                                                      |
|-------------------------|--------------------------------------------------------------|
| `/debug/pprof/`         | Index + named profiles (`heap`, `goroutine`, `allocs`, …)    |
| `/debug/pprof/cmdline`  | The manager's argv (joined with NULs)                        |
| `/debug/pprof/profile`  | 30s CPU sample (engages active sampling — heavy, use sparingly) |
| `/debug/pprof/symbol`   | Address → symbol resolution                                  |
| `/debug/pprof/trace`    | Execution trace (engages active sampling — heavy)            |

Merely registering the handlers adds zero per-request overhead until a
profile or trace endpoint is actually hit. Heap and goroutine pulls are
snapshot reads of live runtime state.

---

## Dashboards

`<gitops-repo>/keystone/observability/grafana-dashboard-keystone-overview.yaml`
provisions the canonical dashboard via Grafana Operator. Panels:

- Per-controller reconcile rate + error rate
- Per-controller reconcile latency p50/p99
- Drift detected / cleared rate by schema
- Drift staleness by schema (gauge with threshold band)
- MigrationExecution backlog by phase
- Pool size + active connections per provider

Dashboard JSON ships with the keystone repo at
`hack/grafana-dashboard-keystone-overview.json`. A ConfigMap in the GitOps
repository wraps it for Grafana Operator to pick up.
