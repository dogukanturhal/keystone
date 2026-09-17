# 0012. Declare the engine interface before implementing a second engine

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

Keystone today is PostgreSQL-only. Phase 13 of the tier-1 roadmap adds
MySQL. The engine layer — CREATE DATABASE, CREATE ROLE, schema
inspection — today lives under `internal/postgres/`.

Adding MySQL after the fact usually plays out in one of two ways:

1. **Refactor-as-you-go**: edit the first MySQL call site to dispatch
   by engine name, add more dispatches over time. Ends with a
   non-interface of `if engine == "postgres" { pg.… } else { mysql.… }`
   scattered across reconcilers.
2. **Big-bang refactor when MySQL lands**: extract the interface and
   move all call sites in one PR. Risky, hard to review.

We've been burned by option 1 in other HexxLock packages. Option 2 is
viable but deferring creates an unreviewed assumption that the engine
shape is clear — it isn't until you've written two implementations.

## Decision

Declare the engine interface now at `internal/engine/engine.go`, even
though `internal/postgres/` isn't yet refactored to implement it.
Purpose:

- Lock the method set under review before we write a MySQL driver
  under pressure.
- Make the contract explicit in the codebase — future PRs touching
  `internal/postgres/` get the interface as a checklist.
- Enable early discovery of engine-specific concepts that DON'T
  generalise (see "Alternatives considered" below).

The postgres package stays where it is for now. Phase 13.1 does the
mechanical move (`internal/postgres/` → `internal/engine/postgres/`)
with no behaviour change; Phase 13.2 adds `internal/engine/mysql/`
implementing the same interface.

## Consequences

Easier: a reviewer looking at the interface can comment "this looks
like a PostgreSQL method, not a cross-engine method" before it's
calcified into every controller. The contract is narrow (10 methods)
and documented at declaration site.

Easier: documentation of "what an engine is" is now a single file
developers can point at, not a scrape of `pg.Admin` call sites.

Harder: interface + implementation diverge if the postgres package
drifts. Mitigated by a `go vet`-style check in Phase 13.1's follow-up
CI job (`go test -run Interface ./internal/engine/` will compile-check
that `*postgres.Admin` satisfies `engine.Engine` once the move lands).

Harder: the Snapshot type is currently duplicated — the
drift-inspector has its own, and `engine.Snapshot` is a forward
declaration. Phase 13.1 consolidates them.

## Alternatives considered

- **No interface, dispatch on engine name at call sites**: the option-1
  failure mode above. Rejected.
- **Wait until MySQL implementation actually lands**: defers the
  design discussion to a moment when PR pressure biases toward
  "whatever compiles". Rejected.
- **Include pgroll-specific operations in the Engine interface**:
  ADR 0005's lightweight pgroll subset lives above the engine (operates
  on SQL strings + pgroll views). Keeping it out of Engine means
  MySQL doesn't have to implement something that only makes sense for
  Postgres. Pgroll-native support (Phase 14.1) slots in as its own
  ExecuteOperations interface if ever needed.
- **Include cloud-provider-native calls** (AWS RDS ModifyDBInstance,
  etc.): out of scope — Keystone operates on running databases, not
  provisioning control planes.
