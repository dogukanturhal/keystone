# 0014. SLO framework — multi-window burn-rate alerts

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

Keystone's Prometheus rules at `config/prometheus/` currently alert on
simple thresholds — reconcile_duration_seconds p95 over 5 minutes,
pool_acquire_errors over 10 per minute. Those fire during transients
(pod restart, upstream PG brownout) that aren't actually SLO
violations.

Google SRE's multi-window multi-burn-rate alerting is the industry
convention: a slow-burn alert that fires only when the error budget
is burning fast enough to be exhausted before the SLO window
refreshes. Saturates less on transients, escalates genuine
long-running incidents.

## Decision

Adopt SLOs for the two load-bearing user-visible signals:

| SLO | Objective | Window | Budget |
|---|---|---|---|
| **Reconcile success** | 99.5% of reconciles complete without error | rolling 28 days | ~3.4 h of error time |
| **MigrationExecution apply latency** | p99 apply latency ≤ 30 s for versioned bundles under 10 statements | rolling 7 days | fixed threshold |

Alerts fire on two burn-rate windows per SLO:

- **Fast**: 1-hour burn rate >= 14.4x (page on-call; 2 % of 28-day
  budget consumed in an hour)
- **Slow**: 6-hour burn rate >= 6x (ticket; 5 % consumed in 6 h)

This is the SRE-book recipe. The multipliers come from the 28-day
window and the desired lead time on each alert class.

Implementation lives in
`config/prometheus/prometheus-rules-slo.yaml` (new file in Phase 10.2
sub-task) and expands the existing ServiceMonitor metrics with the
derived recording rules.

## Consequences

Easier: on-call has a smaller page load. Transients don't page;
sustained failure does.

Easier: SLO status is visible in Grafana as a single dashboard tile
(budget consumed, window, forecast to exhaustion).

Easier: post-incident reviews compare burn against budget in concrete
numbers instead of "lots of errors".

Harder: SLO definitions are a commitment. If reconcile-success drops
below 99.5% long-term, we can't hide behind "that was a bad week" —
the error budget is the contract with users.

Harder: two more Prometheus rules to maintain. Mitigated by keeping
the rule file short (recording rules express the math, alerts express
the threshold — total ~30 lines per SLO).

## Alternatives considered

- **Simple threshold alerts** (current state): noisy and doesn't
  distinguish a transient spike from a sustained regression. Rejected.
- **Single long-window burn-rate** (e.g. 6h at 6x only): doesn't page
  fast enough when an incident is genuinely on fire. Rejected.
- **Single short-window burn-rate** (1h at 14.4x only): misses slow
  regressions (2 % / hour is fast; 1 % / hour over 6 hours is the
  same total budget burn but would be silent). Rejected.
- **More granular SLOs** (per-reconciler, per-provider, per-tenant):
  gives the oncall 20 dashboards and no clear target. Rejected for
  v1; each team can layer their own SLO on top of the exposed
  metrics.
- **SLI definition via sum(rate(keystone_reconcile_total{result!="success"}[5m]))
  / sum(rate(keystone_reconcile_total[5m]))** vs. the per-object
  condition-based count: we use the rate-based version because
  condition-based SLIs double-count long-held Ready=False states. Rate
  captures "work we accepted and failed to do", which is what users
  experience.

## Operational notes

- Dashboards link: grafana.example.com/d/keystone-slo (created with
  the rules).
- Runbook entry: `docs/runbooks/slo-burn.md` (TODO — Phase 10.2 ships
  it with the rules).
- Error budget policy: budget exhaustion freezes non-urgent feature
  merges until the incident is closed and remediation committed. This
  is the same bar HexxLock platform applies to other tier-1 services.
