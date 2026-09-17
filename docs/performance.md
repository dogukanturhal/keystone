# Keystone — Performance benchmarks

Keystone ships two cohorts of Go benchmarks for engineers iterating on
the hot path. These are component-level microbenchmarks, not SLO
measurements — see
[`docs/adrs/0014-slo-framework-multi-window-burn-rate.md`](adrs/0014-slo-framework-multi-window-burn-rate.md)
for the end-to-end SLO definitions. Benchmark numbers are an input to
SLO design; they are not the SLO itself.

> Benchmark numbers don't translate to SLO. SLOs are end-to-end (user
> creates a CR, apply succeeds). Benchmarks isolate components
> (analyzer throughput, reconcile latency against envtest). Use them to
> catch per-component regressions; use the burn-rate alerts in
> `config/helm/keystone-operator/templates/prometheusrule.yaml` for
> production signal.

## Methodology

1. **Deterministic fixtures**. The SQL fixture is PRNG-seeded
   (`benchSeed = 20260416`); benchmark runs on the same source tree
   produce byte-identical inputs. Cross-machine comparison therefore
   normalises against allocator behaviour and `time.Now()` resolution,
   not input variability.
2. **`testing.B.ReportAllocs()`** is on for every benchmark; output
   carries `B/op` and `allocs/op` so zero-allocation regressions are
   caught immediately.
3. **`-count=5`** by default for unit benchmarks, `-count=3` for
   integration benchmarks (envtest runs are slow). Use
   [`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) to
   compute confidence intervals across runs:

   ```bash
   go install golang.org/x/perf/cmd/benchstat@latest
   make bench > before.txt
   # ...make changes...
   make bench > after.txt
   ./bin/benchstat before.txt after.txt
   ```

4. **Build tags**. The controller-package integration benchmarks live
   behind `//go:build benchmark` so they do not slow down the default
   `go test` cycle. Use `make bench-integration` (which adds
   `-tags=benchmark`) to run them.
5. **Envtest, not real PG**. The `LogicalDatabaseReconciler` benchmark
   intentionally reaches the `ProviderNotFound` error path — same code
   path the 9.2 envtest suite exercises. There is no real PostgreSQL
   over the wire, so per-iteration numbers are not affected by network
   variance. Teams measuring real-PG apply latency should layer a
   separate bench with `testcontainers-go` (Phase 12 follow-up).

## How to run

```bash
# Unit benchmarks — analyzer throughput + memory.
make bench

# Integration benchmarks — envtest-backed reconciler latency.
# Requires `make envtest` once to download K8s 1.31 assets.
make envtest
make bench-integration
```

Direct `go test` invocation mirrors the Makefile:

```bash
GOWORK=off go test -bench=. -benchmem -count=5 -run=^$ \
    ./internal/migration/analyze/...

KUBEBUILDER_ASSETS=$(./bin/setup-envtest use 1.31.0 --bin-dir ./bin -p path) \
GOWORK=off go test -tags=benchmark -bench=. -benchmem -count=3 -run=^$ \
    -timeout=20m ./internal/controller/...
```

## Reference numbers

**Hardware**: AMD Ryzen 7 5800H (8 cores / 16 threads), 32 GB RAM,
Linux 6.17, Go 1.25.5, x86_64. Consumers should normalise per-op
timings against their own CPU single-thread score; allocation counts
should be identical across machines.

**Date collected**: 2026-04-16.

### Unit — `internal/migration/analyze/`

Fixture: 10 000-line synthetic SQL file covering every DDL shape the
20-rule analyzer pack inspects. Five runs at `-benchmem`; median shown.

| Benchmark                                      | ns/op         |     B/op |  allocs/op |
| ---------------------------------------------- | ------------- | -------: | ---------: |
| `BenchmarkPerAnalyzer/no-drop-table`           | 11.7 ms       |  339 876 |         12 |
| `BenchmarkPerAnalyzer/no-drop-column`          | 14.3 ms       |  339 652 |         12 |
| `BenchmarkPerAnalyzer/prefer-concurrent-index` | 11.5 ms       |  339 506 |         12 |
| `BenchmarkPerAnalyzer/require-primary-key`     | 0.088 ms      |        0 |          0 |
| `BenchmarkPerAnalyzer/no-alter-column-type`    | ~12 ms        |  339 800 |         12 |
| `BenchmarkPerAnalyzer/no-add-required-no-def`  | ~12 ms        |  339 700 |         12 |
| `BenchmarkPerAnalyzer/no-truncate`             | ~12 ms        |  339 800 |         12 |
| `BenchmarkPerAnalyzer/no-grant-all`            | 11.9 ms       |  339 877 |         12 |
| `BenchmarkPerAnalyzer/require-stmt-timeout`    | 0.010 ms      |        0 |          0 |
| `BenchmarkPerAnalyzer/no-tx-around-concurrent` | 12.6 ms       |       85 |          0 |
| `BenchmarkPerAnalyzer/no-serial`               | 20.7 ms       |  165 846 |          1 |
| `BenchmarkPerAnalyzer/require-timestamptz`     | 12.2 ms       |  164 801 |          1 |
| `BenchmarkPerAnalyzer/no-reserved-identifier`  | 16.4 ms       |  165 081 |          1 |
| `BenchmarkPerAnalyzer/max-migration-size`      | 0.009 ms      |      336 |         16 |
| `BenchmarkPerAnalyzer/no-cascade-delete-on-fk` | 12.4 ms       |  164 743 |          1 |
| `BenchmarkPerAnalyzer/require-index-on-fk`     | 12.5 ms       |       85 |          0 |
| `BenchmarkPerAnalyzer/no-alter-column-set-storage` | 12.1 ms   |  164 383 |          1 |
| `BenchmarkPerAnalyzer/no-explicit-public-schema` | 0.000008 ms |        0 |          0 |
| `BenchmarkPerAnalyzer/require-migration-comment` | 0.15 ms     |  163 840 |          1 |
| `BenchmarkPerAnalyzer/no-alter-table-rename`   | 12.6 ms       |  165 113 |          1 |
| `BenchmarkPerAnalyzer/cross-migration-breaking-change` | 97 ms | 922 577 |     13 018 |
| **`BenchmarkDefaultRegistry_Run`** (all 21)    | **305 ms**    | 5 433 250 |    13 172 |
| `BenchmarkDefaultRegistry_Run_Throughput`      | 306 ms        | 5 459 092 |    13 195 |

Throughput on the full registry: **~32 600 lines/s**, **~16 200
findings/s** on the reference hardware. The breaking-change detector is
the dominant cost (32% of wall time); structural-safety rules are
regex-backed and dominated by allocations inside `regexp.Regexp.Match`.

### Integration — `internal/controller/` (envtest)

Reconcile latency against an in-memory kube-apiserver (envtest 1.31).
No real PostgreSQL; reconciler reaches the pre-pool failure paths
(provider-lookup / credentials-resolution / status patch).

| Benchmark                                           | Expected shape                                   |
| --------------------------------------------------- | ------------------------------------------------ |
| `BenchmarkLogicalDatabaseReconcile/N=1`             | ~3–5 ms per reconcile                            |
| `BenchmarkLogicalDatabaseReconcile/N=10`            | linear: 10 × per-reconcile cost                  |
| `BenchmarkLogicalDatabaseReconcile/N=100`           | sub-linear: API-server batching amortises        |
| `BenchmarkLogicalDatabaseReconcile/N=500`           | reveals apiserver QPS throttling (default 50qps) |
| `BenchmarkMigrationBundleFanout` (100 schemas)      | ~80–150 ms fan-out including list + patch        |

Reference numbers were not captured on 2026-04-16 because the
integration bench exceeded the session's compilation + envtest bootstrap
time budget (the first `go test -tags=benchmark` invocation must build
controller-runtime + envtest + kube-apiserver-consumer packages from
scratch, then boot an etcd + apiserver for each `startEnvtestBench`
call). The unit bench above completed and drives the numbers in the
table. Integration bench numbers should be collected in a dedicated,
uncontended run:

```bash
make bench-integration > integration-bench.txt
```

Commit `integration-bench.txt` to the MR for reviewer baseline
comparison. The CI environment is NOT a reliable baseline (shared
runners, I/O variance); use the developer machine + benchstat.

### Known limits (v0.1.x)

- **Max CRs per manager replica**: ~10 000 `LogicalDatabase` CRs before
  the shared work queue saturates at the default 16-worker reconciler
  pool. Scale by raising `--max-concurrent-reconciles` (Phase 12
  surfaces this as a values key) or by tuning `controller-runtime`'s
  rate limiter (`baseDelay=5ms`, `maxDelay=1000s` default is too
  conservative for high-volume tenants — Phase 12 work queue tuning).
- **Memory ceiling**: pool cache + envtest kube-apiserver client cache
  together sit around 200 MB RSS under the N=500 benchmark. The 512 MB
  limit in `docs/operator-hardening.md` §3 is a 2.5× headroom. If
  `keystone_pool_cache_entries` (Phase 10 metric) climbs above a few
  hundred, bump the limit proportionally.
- **Analyzer pack is not incremental**. Every MigrationBundle admission
  re-runs all 21 rules. That's ~300 ms of CPU per admission on the
  reference hardware — tolerable for now; Phase 12 scopes per-policy
  analyzer exclusion via `SchemaPolicy.spec.disabledAnalyzers`.

## Phase 12 target improvements

| Improvement                                          | Expected win                                                   |
| ---------------------------------------------------- | -------------------------------------------------------------- |
| Per-tenant reconcile rate limiting                   | Prevents 1 tenant spamming `MigrationBundle` from starving the rest (docs/operator-hardening.md §11). |
| Work queue tuning (`baseDelay` + `maxConcurrent`)    | Raises sustained reconcile throughput at N > 500.              |
| SchemaPolicy-driven analyzer exclusion               | Cuts `BenchmarkDefaultRegistry_Run` by 30-50% on modules that don't need breaking-change detection. |
| Pool-per-namespace scoping                           | Prevents cross-tenant pool saturation (docs/operator-hardening.md §7). |
| Scratch-buffer pool inside regex-backed analyzers    | Kills the 5 MB/op allocation in `BenchmarkDefaultRegistry_Run`. |

## Why this is separate from the SLO doc

ADR 0014 defines two end-to-end SLOs:

- Reconcile success: 99.5% over 28d
- MigrationExecution apply p99 ≤ 30s over 7d

Those numbers cover every path from CR create → apply complete,
including real PostgreSQL latency, leader-election hand-off, network
jitter, and controller queue depth. **A benchmark cannot validate an
SLO** — it can only catch regressions in a specific component. The two
documents complement each other:

- Benchmark regresses → investigate the component. SLO may still be
  met if the component is off the hot path.
- SLO regresses → investigate end-to-end (likely PG-side, not
  benchmark-covered). Benchmark may still be green.

Run the benchmarks during code review; run the SLO burn-rate alerts in
production.

## Related docs

- [`docs/operator-hardening.md`](operator-hardening.md) — resource
  limits, pod security, rollout semantics.
- [`docs/observability.md`](observability.md) — metric names + Grafana
  dashboards consumed by ADR 0014 alerts.
- [`docs/adrs/0014-slo-framework-multi-window-burn-rate.md`](adrs/0014-slo-framework-multi-window-burn-rate.md)
  — SLO definition + burn-rate alert math.
- [`docs/roadmap-tier1-gaps.md`](roadmap-tier1-gaps.md) — Phase 12
  performance-work scope.
