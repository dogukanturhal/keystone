# 0008. Configurable tracking table name for adoption on existing DBs

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

Example Service (HexxLock's IAM service) is the canonical Keystone adoption
target. Its existing database already has a `public.schema_migrations`
table with the golang-migrate standard shape: `(version INTEGER, filename
VARCHAR, checksum, applied_at)`.

Keystone's runner, designed for greenfield, uses `schema_migrations` with
`(version TEXT PRIMARY KEY, content_hash, applied_at, applied_by,
duration_ms, dirty)`. The `version` column has a type mismatch
(`INTEGER` vs `TEXT`) that would require an ALTER during adoption, and the
additional columns need backfill.

Forcing Example Service's table to conform to Keystone's shape means touching
production history. That's a risk we don't need to take.

## Decision

`LogicalDatabase.spec.trackingTableName` is a configurable field. Default
`"schema_migrations"` (golang-migrate convention, right choice for
greenfield). Example Service sets it to `"keystone_schema_migrations"` so
Keystone's tracking table is a new sibling to Example Service's existing table
in the same schema. Both survive; neither is disturbed.

## Consequences

Easier: adoption on databases with existing migrators is a zero-risk drop-in.
No ALTER, no backfill, no history rewrite.

Easier: rollback from Keystone ownership to the prior migrator is a config
change — Keystone's table becomes a historical archive; the prior migrator
resumes writing to `schema_migrations`.

Harder: a single database can drift between Keystone's and a prior
migrator's tracking tables if both run concurrently. We explicitly don't
support that — ADR 0007 mandates Git as the single source of truth, and
the prior migrator's auto-migrate should be disabled during cutover.

Harder: operators monitoring schema migration state must know which table
to query. We mitigate by emitting both tables' high-water marks as
Prometheus metrics post-cutover (Phase 8.2).

## Alternatives considered

- **Force Keystone's canonical schema on every adoption**: breaks
  compatibility with 90% of existing migrator tools (golang-migrate,
  Flyway, Liquibase, Goose, Atlas) that all expect their own shape.
  Rejected.
- **Use a separate schema entirely** (e.g. `keystone_meta.schema_migrations`):
  works but introduces a new schema for one table. ADR 0007 argues for
  minimal schema surface.
- **Support both tables' shapes via a migration adapter**: ~500 LOC of
  code that handles the type-mismatch translation. The separate-table
  approach is simpler and has no downside for the adoption use case.
