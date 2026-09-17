# 0027. RecordOnly execution mode for adoption baselines

- **Status**: Accepted
- **Date**: 2026-06-04
- **Deciders**: HexxLock platform team

## Context

`keystonectl scaffold --adopt` (ADR 0026) emits an adoption baseline:
the existing schema's current state rendered as `0001_baseline.up.sql`.
Per ADR 0008 the baseline must be **recorded as applied, not executed**
— the database already has those objects.

Two defects surfaced when this was exercised end-to-end (2026-06-04):

1. **The baseline could not pass its own gate.** A real production
   schema's baseline routinely lints at severity=error — function
   bodies with `UPDATE` without `WHERE`, dropped-and-recreated
   triggers, lock-risk DDL. 36 findings on a representative scaffold.
   The MigrationBundleReconciler (and the admission webhook) lint
   every bundle and refuse on severity=error with `LintFailed`, so a
   scaffolded adoption baseline was un-shippable through the front
   door. "Emit lint-clean SQL" is not a fix: the baseline is a
   faithful snapshot of an existing schema, and its content is not
   ours to sanitise.

2. **Recording was a manual psql INSERT.** The scaffold README
   instructed operators to `INSERT INTO <schema>.<tracking-table> …`
   by hand — an imperative, unaudited, GitOps-invisible step that
   directly violates the platform rule that every DB change flows
   through Keystone.

The tier-1 migration tools converged on the same answer years ago:

- **Flyway** — `flyway baseline` writes a `BASELINE`-type row into the
  schema history table; migrations ≤ baselineVersion never execute.
- **Liquibase** — `changelog-sync` marks changesets as executed
  without running them.
- **Atlas** — `atlas migrate apply --baseline <version>` marks the
  version applied and proceeds from the next one.

None of them run execution-safety analysis on an adoption baseline,
because the baseline never executes. Lint exists to gate *execution*.

## Decision

Adoption baselines become first-class via
`MigrationBundle.spec.executionMode`:

- `Apply` (default) — today's behaviour, unchanged.
- `RecordOnly` — the MigrationExecution runner calls
  `Runner.Record(version, contentHash)` (keystone-sdk ≥ go/v0.2.1):
  an upsert of the tracking-table row, zero SQL executed. The audit
  chain records verb `record` instead of `apply`.

Semantics for `RecordOnly`:

- **Lint is advisory, never blocking** — findings still run and are
  published (`status.lintFindings`, admission warnings) so reviewers
  see them, but severity=error neither rejects admission nor sets
  `LintFailed`.
- **Everything else still gates in full**: keystone.sum integrity,
  per-version content-hash immutability, approval policies (ADR 0016),
  CEL expressions, plan-gate-apply. Recording a wrong baseline is
  consequential even though nothing executes.
- **Immutable** — flipping a shipped bundle between modes would
  rewrite the meaning of its tracking-table row; the webhook refuses.
- **Versioned-strategy only, `autoRollback` forbidden** — pgroll
  phases cannot be "recorded", and a never-executed baseline has
  nothing to roll back.
- `ExecutionMode` is **frozen onto the MigrationExecution at fan-out**
  (like `ContentHash`), so later bundle mutations cannot change what
  an in-flight execution does.

`keystonectl scaffold` now emits `migrationbundle.yaml` with
`executionMode: RecordOnly` under `--adopt` (and `Apply` otherwise),
plus a kustomize `configMapGenerator` for the migrations ConfigMap.
The manual-INSERT instruction is deleted from the README.

## Consequences

Easier: adoption is a single `kubectl apply -k .` — declarative,
admission-gated, audited, drift-checked. The DriftController remains
the safety net: if the recorded baseline doesn't match live structure,
drift surfaces on the next check.

Easier: incremental migrations (0002, …) authored with
`keystonectl migrate diff` execute and lint normally on top of the
recorded baseline; nothing downstream changes.

Harder: one more mode in the execution matrix. Mitigated by the
admission guards (strategy/rollback/immutability) and by freezing the
mode onto the execution.

Risk considered — "RecordOnly as a lint bypass": an operator could
mark a destructive bundle RecordOnly to slip it past lint. But a
RecordOnly bundle *executes nothing*, so there is nothing to slip
past; the recorded row only declares history. Approval policies still
apply for scopes that want human sign-off on baselines.

## Alternatives considered

- **Skip lint when `--adopt` is detected by filename/annotation
  convention**: magic-string behaviour, invisible in the API, not
  enforceable at admission. Rejected.
- **`keystonectl scaffold` emits lint-clean SQL**: impossible in
  general — the baseline mirrors an existing schema, including
  whatever its function bodies contain. Rewriting it would break the
  fidelity that makes adoption safe. Rejected.
- **Per-bundle `disabledAnalyzers` list**: solves the wrong problem —
  the baseline shouldn't be execution-linted at all, and a
  rule-by-rule allowlist invites copy-paste lint suppression on
  bundles that *do* execute. (SchemaPolicy-level analyzer tuning
  remains a separate, orthogonal roadmap item.) Rejected.
- **Keep the manual INSERT runbook**: violates the no-manual-DB-change
  rule; unaudited; invisible to GitOps; already caused operator
  friction. Rejected.

## References

- Flyway baselines — https://documentation.red-gate.com/fd/baselines-273973441.html
- Liquibase changelog-sync — https://docs.liquibase.com/commands/utility/changelog-sync.html
- Atlas lint analyzers / baseline — https://atlasgo.io/lint/analyzers ,
  https://atlasgo.io/faq/migrate-baseline-github-action
- ADR 0008 (tracking-table adoption), ADR 0016 (approval policies),
  ADR 0026 (scaffold / migrate-from-code)
