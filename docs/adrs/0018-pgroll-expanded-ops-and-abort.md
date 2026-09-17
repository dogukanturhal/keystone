# 0018. Expanded pgroll operations + explicit Abort lifecycle

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team
- **Supersedes (partially)**: ADR 0005 §"Three operations" list; extends the op set without reversing the ADR's core trade-off

## Context

ADR 0005 shipped Keystone's pgroll subset with three operations
(`add_column`, `drop_column`, `rename_column`) plus the explicit
lock-in: **no view-level schema versioning, apps stay on `public`,
full pgroll is deferred to Phase 14.2 as an opt-in alternative
engine**.

The subset grew organically to five ops (also `add_constraint`,
`alter_column_type`). In use, two gaps emerged:

1. **`NOT NULL` transitions are a daily footgun.** `ALTER COLUMN SET
   NOT NULL` scans every row under AccessExclusive; the safe pattern
   is `ADD CHECK (col IS NOT NULL) NOT VALID` → `VALIDATE CONSTRAINT`
   → `SET NOT NULL` → `DROP CHECK`. Authoring that by hand inside a
   `versioned` migration is error-prone and reviewers miss the
   NOT VALID omission. A first-class `set_not_null` operation makes
   the correct pattern the default.

2. **Mid-flight rollback is impossible.** Once a bundle enters
   `Expanding` or sits at `Expanded`, the only way out is forward
   (Contract) or delete the `MigrationExecution` and leave shadow
   columns, triggers, and NOT VALID constraints in the database.
   Reviewers had no "cancel migration" button — even when the expand
   phase was clearly the wrong change.

Industry research (xataio/pgroll, Bytebase, Atlas) treats "instant
rollback = cancel migration" as table stakes. The competitive gap
was load-bearing in evaluator feedback.

## Decision

Two additions to the pgroll subset:

### 1. New operations

- **`set_not_null`** — Expand adds `ADD CONSTRAINT chk_<col>_notnull
  CHECK (<col> IS NOT NULL) NOT VALID` (metadata only, no scan).
  Contract runs `VALIDATE CONSTRAINT` (SHARE UPDATE EXCLUSIVE),
  `SET NOT NULL` (instant because the proven CHECK exists), then
  `DROP CONSTRAINT` (cleanup). Abort drops the CHECK.

- **`drop_not_null`** — Single-phase operation. Expand runs
  `ALTER COLUMN DROP NOT NULL` (metadata only, brief
  AccessExclusive). Contract is a no-op. Abort is a no-op plus a
  warning — once the column is nullable, NULL rows may exist, and
  re-establishing NOT NULL without a data audit is unsafe.

### 2. Abort lifecycle

- New execution phase `Aborting` (transient) alongside the existing
  terminal `Aborted`.
- New annotation `keystone.hexxlock.io/abort=true`. Set on a non-
  terminal execution, the controller flips its phase to `Aborting`
  on the next reconcile.
- New engine phase `PhaseAbort`. Each operation's handler declares
  what Abort means: drop shadow columns, drop triggers, drop
  functions, drop NOT VALID constraints. All handlers use `IF EXISTS`
  so repeat aborts are safe.
- Aborts are **refused during `Contracting`**. Once Contract starts
  dropping real columns the data is gone; rolling back would require
  backups the controller doesn't have. The controller emits a
  `Warning` event (`AbortRefused`) and lets the contract finish.
- Abort completes as `Aborted` — same terminal state as `Succeeded` /
  `Failed` from the controller's perspective: no further reconciles,
  retained for the audit window, kubectl describe carries the
  rollback narrative.

Per-op abort semantics:

| Operation | Expand side-effects | Abort handler |
|-----------|---------------------|---------------|
| `add_column` | ALTER TABLE ADD COLUMN | DROP COLUMN IF EXISTS |
| `drop_column` | COMMENT ON COLUMN (marker) | COMMENT ON COLUMN IS NULL |
| `rename_column` | ADD new column + trigger + function | DROP trigger + function + new column |
| `add_constraint` | ADD CONSTRAINT … NOT VALID | DROP CONSTRAINT IF EXISTS |
| `alter_column_type` | ADD shadow + trigger + function | DROP trigger + function + shadow |
| `set_not_null` | ADD CONSTRAINT … CHECK NOT VALID | DROP CONSTRAINT IF EXISTS |
| `drop_not_null` | DROP NOT NULL | No-op + warning (best-effort only) |

## Consequences

Easier: **NOT NULL migrations are one line of YAML**. Authors write
`kind: set_not_null, column: email` and the engine picks the
lock-safe pattern. Reviewers count fewer footguns.

Easier: **cancel-mid-flight UX**. `keystonectl plan approve` already
opens the A4 gate; `kubectl annotate migrationexecution <name>
keystone.hexxlock.io/abort=true` closes it cleanly before Contract
runs. The shape matches `kubectl delete pod --grace-period=0` as a
mental model.

Easier: **test coverage**. A `//go:build integration` testcontainer
suite now covers every operation's expand / contract / abort path
against real PostgreSQL. The package had zero tests before this ADR.

Harder: **two-phase testing story**. Engine behaviour is now
exercised in two places: controller envtest (phase transitions) +
engine testcontainer (SQL effects). A behaviour change needs both
sets of tests updated.

Harder: **Abort is not a full time machine**. `drop_not_null` is the
canonical example — once NULLs are allowed, a rollback cannot
restore the invariant. The warning message spells this out, but
operators must internalise that "Abort" means "undo what Expand did"
not "restore pre-bundle state for every op kind".

## What this ADR does NOT change

- **No view-level schema versioning**. ADR 0005's decision still
  stands: apps stay on `public`. Full pgroll with parallel `public_v1`,
  `public_v2` views remains the Phase 14.2 `strategy=pgroll-native`
  opt-in. The rationale — HexxLock services don't want to change
  their search_path — has not moved.

- **No new authoring constraint**. Bundles already supported up to 16
  operations; the enum expansion doesn't change that.

- **No new RBAC**. All handlers execute against the same pooled
  connection that existing ops already used.

## Alternatives considered

- **Full view versioning (pgroll-native)**. Would deliver the
  research priority "old+new schema versions coexist via PG views"
  literally. Rejected for this phase per ADR 0005 §"Alternatives
  considered" — forces app coordination for every migration. Still
  scoped for Phase 14.2 as the alternative engine.

- **Abort-as-contract-equivalent (just run Contract)**. Tempting for
  uniformity but wrong semantically: Contract for `rename_column`
  DROPs the OLD column; Abort must drop the NEW column + trigger,
  leaving the OLD column intact. Different SQL, same ownership.

- **Abort via `kubectl delete` on the execution**. Already possible
  but leaves shadow columns behind. The annotation-driven path
  cleans up before the CR disappears, which is what operators want.

- **Dedicated `CancellationRequest` CRD**. Overkill — annotations
  are idiomatic for "please react to this" signals (ArgoCD's sync
  annotation, cert-manager's renew annotation). A new CRD would
  duplicate the execution's identity.
