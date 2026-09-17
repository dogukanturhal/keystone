# 0013. Ship a pgroll subset before embedding upstream

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

pgroll (xataio/pgroll) is the authoritative PostgreSQL
expand/contract migration tool. It handles zero-downtime schema
changes via parallel schema versions: the old schema and the new
schema coexist as PG views; clients on the old app see the old shape,
clients on the new app see the new shape; cutover flips a
search_path.

Embedding upstream pgroll pulls in:

- ~15k LOC of Go
- A transitive dependency on PostgreSQL server introspection through
  the pg_get_* function family (present, not a risk)
- A CLI-shaped codebase that wants to be an executable, not a library

Keystone needs three of pgroll's operations: `add_column`,
`drop_column`, `rename_column`. That's Phase 6's scope.

## Decision

Implement a three-operation subset at
`internal/migration/pgroll/` that reproduces the trigger-based
expand/contract pattern without depending on upstream pgroll. The
implementation uses:

- PostgreSQL views to hide old columns during expand
- Triggers to backfill writes across versions
- Explicit search_path flip on cutover

Ship this as `strategy: pgroll-expand-contract` with the three
operations supported. Document the limits — if an operator needs
`add_constraint`, `alter_column_type`, or any other pgroll operation,
they use `strategy: versioned` + hand-written SQL until Phase 14.1
lands upstream pgroll properly.

## Consequences

Easier: audit surface is small (~800 LOC of Go + SQL) — one
maintainer can read the whole implementation in an afternoon. Upstream
pgroll is ~15k LOC that we'd own without the context of having
written it.

Easier: no new Go dep to vendor, no license-compatibility review
(pgroll is Apache-2.0; Keystone is AGPL-3.0-or-later — compatible,
but more deps means more release-audit work).

Harder: operations we didn't implement require the `versioned`
strategy. We call this out in the docs; for the three high-frequency
operations it covers 80% of real migrations (stats from Falcon-ID's
migrations/ folder confirm).

Harder: when upstream adds safety fixes, we have to port them. We
mitigate by keeping the implementation mechanically similar to
pgroll's (same view shape, same trigger names), so a port is diff-ish
rather than re-architecture.

## Alternatives considered

- **Embed pgroll as a library**: attempted briefly — the package
  layout assumes a binary root with CLI flags, and extracting the
  three operations requires importing ~2k LOC of unrelated code.
  Rejected for Phase 6; revisited in Phase 14.1 with a proper upstream
  conversation.
- **Fork pgroll**: forking takes on upstream maintenance responsibility
  with no ongoing benefit. Rejected.
- **Only ship `versioned` strategy**: rejects the whole expand/contract
  story. Zero-downtime rename is a key differentiator vs Atlas; users
  expect it.
- **Ship upstream pgroll as a sidecar**: operational complexity
  (a second container, a second admin credential, coordinated shutdown)
  for a feature that can be ~800 LOC in-process.

## Phase 14.1 (future)

When Phase 14.1 lands, the `pgroll-expand-contract` strategy stays —
renamed to `pgroll-lite` or similar — and a new `pgroll-native`
strategy wraps upstream pgroll via library import (or subprocess, if
library extraction still fails). Existing bundles don't migrate; the
lightweight strategy remains for small shops that don't want the
upstream dep.
