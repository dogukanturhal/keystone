# 0009. Support both versioned + declarative strategies

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

Two ecosystems dominate schema management today:

1. **Versioned / imperative** (Flyway, Liquibase, golang-migrate, Goose,
   Alembic): authors write sequential SQL files; the runner applies them
   in order. Pros: maximum control, arbitrary SQL. Cons: hand-authoring
   repetitive ALTERs, drift between desired and actual is invisible.

2. **Declarative / schema-as-code** (Atlas, SchemaHero, Prisma): authors
   describe the desired schema shape; the tool computes the diff and
   generates DDL. Pros: drift detection is inherent, onboarding existing
   DBs is fast. Cons: complex migrations (data migrations, conditional
   logic) are harder to express.

Keystone's users come from both backgrounds. Forcing them into one camp is
a wrong fit — schema authors in Falcon-ID (Go + golang-migrate) expect
versioned SQL; ORM-driven teams (business monolith with EF Core) expect
declarative.

## Decision

Keystone ships both:

- `MigrationBundle.spec.strategy = versioned` — SQL files in a ConfigMap.
  `MigrationBundle.spec.operations` is empty. The existing Phase 3 path.

- `MigrationBundle.spec.strategy = pgroll-expand-contract` — declarative
  per-column operations (`add_column`, `drop_column`, `rename_column`,
  etc.). The Phase 6 path.

- `SchemaDefinition` CRD + `SchemaDefinitionReconciler` (Phase 10) — the
  author declares the desired schema shape; the reconciler computes the
  diff and *generates* a versioned MigrationBundle. Internally this is a
  declarative author → versioned applier bridge.

Both strategies flow through the same reconciler pipeline (analyzers →
admission → plan → execution), so safety and observability are uniform.

## Consequences

Easier: users pick the authoring paradigm that matches their team. Atlas-
style declarative authors use `SchemaDefinition`; golang-migrate authors
use `MigrationBundle` with SQL files.

Easier: the migrations/ folders from the legacy services carry over
directly — same SQL file format, same lexicographic ordering.

Harder: the codebase carries both strategies' runner logic. Mitigated by
strategy-specific code being isolated
(`internal/migration/` for versioned, `internal/migration/pgroll/` for
expand/contract, `internal/migration/declarative/` for schema-as-code).

Harder: documentation must cover both strategies. We mitigate by making
the runbook strategy-agnostic and the strategy choice a single
`MigrationBundle.spec.strategy` field with enum validation.

## Alternatives considered

- **Versioned only**: rejected because onboarding existing DBs is tedious
  and Atlas-style workflows can't be supported.
- **Declarative only**: rejected because complex data migrations (e.g.
  "split one column into two with custom logic") are painful to express
  declaratively.
- **Declarative with escape hatch for raw SQL**: the Atlas approach. We
  ended up with roughly this shape — `SchemaDefinition` generates a
  MigrationBundle with raw SQL, so the escape hatch is explicit and
  reviewable.
