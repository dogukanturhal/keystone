# Keystone runbooks

Operational procedures for on-call engineers. Every runbook here is
*executable* — copy-paste commands that fix the stated symptom without
guesswork.

New runbook guidelines:

- One symptom per document; cross-reference rather than duplicate.
- Commands are `kubectl` + `psql` only — no custom tooling required
  beyond what ships with the cluster.
- Include a **before you start** section listing required access
  (VPN, kubeconfig role, which PG clusters you'll touch).
- End with a **verification** step so the operator knows when they're
  done, not just "issue appears resolved".

## Current runbooks

| Runbook | Symptom |
|---|---|
| [runbook.md](../runbook.md) | Master catalog — covers reconcile errors, drift detection, pgroll stalls, ProductInstance provisioning, manager crashloop |
| [example-service-cutover.md](../example-service-cutover.md) | Adopt Falcon-ID's existing PG database into Keystone management |

## Planned (roadmap)

The following runbooks are committed for Phase 12+ but not yet
written. Track progress in [`roadmap-tier1-gaps.md`](../roadmap-tier1-gaps.md).

| Runbook | Trigger |
|---|---|
| `downgrade.md` | Roll back from keystone-manager vN to vN-1 |
| `dr-drill.md` | Quarterly disaster-recovery exercise (restore from etcd + PG backup) |
| `slo-burn.md` | Multi-window burn-rate alert fired — see ADR 0014 |
| `credential-rotation.md` | Rotate admin credentials without a manager restart |
| `tenant-offboard.md` | Remove a tenant's databases + schemas cleanly |

If you hit an operational issue that isn't covered here, add a new
runbook (`docs/runbooks/<name>.md`) in the same MR as the fix. Half
the value of a runbook is it exists at 3am; half is that someone else
wrote it first.

## Related

- [`observability.md`](../observability.md) — metrics, SLOs, dashboards
- [`operator-hardening.md`](../operator-hardening.md) — production deployment controls
- [`SECURITY.md`](../../SECURITY.md) — vulnerability disclosure
