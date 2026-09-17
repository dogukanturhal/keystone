# 0005. Lightweight expand/contract subset over full xataio/pgroll

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

`xataio/pgroll` is the industry reference for zero-downtime PostgreSQL
schema migrations. It implements expand/contract using per-version schema
namespaces + views that hide old columns — a clean, powerful model. The
trade-off is that applications must participate: they connect via a
search_path-qualified schema name (`public_v2`, `public_v3`, …), so the
application Deployment needs a coordinated rollout.

Keystone's users today (HexxLock's own services) don't want to change their
search_path. Their apps connect to `public` and expect the live schema there
to evolve.

## Decision

Keystone's Phase 6 ships a **lightweight subset** of expand/contract
semantics — not a pgroll port. Three operations:

- `add_column`: Expand ADDs nullable + DEFAULT; Contract optionally SETs
  NOT NULL after backfill.
- `drop_column`: Expand comments the column as scheduled-for-drop; Contract
  issues `ALTER TABLE DROP COLUMN`.
- `rename_column`: Expand adds the new column + installs a BEFORE-trigger
  that mirrors writes between old and new; Contract drops the trigger and
  the old column.

Apps stay on their original search_path. No parallel schema versioning. No
pgroll dependency.

Full `xataio/pgroll` integration is explicitly scoped for Phase 14.2 as an
**alternative** engine, opt-in via `MigrationBundle.spec.strategy=pgroll-native`.

## Consequences

Easier: users adopt immediately without changing application connection
config.

Easier: Keystone's codebase stays small. Phase 6 is ~300 LOC; a full pgroll
embed would be several thousand.

Harder: some operations (e.g. changing a column's nullability with complex
semantics, combining drop+add in one transaction) are harder or require
manual staging. For those, users write the SQL directly via a versioned
`MigrationBundle`.

Harder: we have two expand/contract implementations in the codebase once
Phase 14.2 lands (lightweight subset + real pgroll). Documentation must
explain which to pick.

## Alternatives considered

- **Full `xataio/pgroll` from day 1**: forces app coordination for every
  migration. Wrong trade-off for our user base.
- **No expand/contract at all**: users would fall back to raw ALTER
  statements with implicit locking. Unacceptable for production workloads.
- **Block-and-wait scheme** (maintenance windows only): incompatible with
  24/7 SaaS operation. Rejected.
