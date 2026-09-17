# 0020. Intra-bundle wave parallelism for pgroll operations

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

A MigrationBundle using the `pgroll-expand-contract` strategy carries
one or more MigrationOperations (capped at 16 per bundle). The engine
applied them strictly in array order: op[0] Expand committed, then
op[1] Expand, then op[2], … — sum of individual latencies for the
whole wall-clock budget.

That's correct for same-table operations: ADD COLUMN then UPDATE
then SET NOT NULL on the same column must serialise. But it's
wasteful when operations target *different* tables — a bundle
adding a column to `users` AND a different column to `orders` AND
a third to `products` has three independent PostgreSQL transactions
that share no rows, no tables, no locks. Running them concurrently
cuts the wall-clock by the parallelism factor.

At tier-1 multi-tenant SaaS scale (50-100 tables touched per release
bundle), serial execution added 30-60 seconds to every rollout. For
a staged rollout across dozens of tenants, that compounds into
minutes of extra apply time. Bytebase and Liquibase don't address
this; Atlas's migration apply is also serial. The research priority
called it out as "no tool does this well".

## Decision

Add a greedy wave scheduler to the pgroll path and an opt-in
`MigrationBundle.spec.parallelism` CRD field (int32, default 0 =
sequential, max 16).

### Wave scheduling

`pgroll.PlanWaves(ops)` groups operations into waves such that every
operation within a wave targets a distinct `op.Table`. Algorithm:
earliest-placement greedy — each op joins the first wave that
doesn't already contain an op on its Table.

Invariants:

1. **Relative order preserved across waves** — for any two ops A, B
   where A precedes B in the bundle and both target the same Table,
   A's wave index is strictly less than B's wave index.
2. **Within a wave, all ops are independent** — different Tables,
   so concurrent PostgreSQL transactions on separate connections
   cannot contend on any relation-level lock.
3. **Deterministic** — same input always produces the same waves;
   no randomness, no hashmap-iteration order sensitivity (the
   algorithm walks in bundle order and places each op in the
   earliest legal wave).

### Execution

The controller reconcile calls `runPgrollWaves(ctx, engine, ops,
phase, reverse, parallelism)`. For each wave:

- `parallelism <= 1` → straight serial for-loop. Zero-cost preserves
  pre-B3 behaviour.
- `parallelism > 1` → `errgroup.Group` with `SetLimit(min(parallelism,
  len(wave)))`. Each op runs in its own goroutine, gets its own pool
  connection, opens its own transaction. First error cancels the
  group; remaining goroutines observe the cancelled context and
  unwind through the engine's existing transaction rollback.

### Reverse traversal for Contract and Abort

The Contract and Abort phases walk waves in reverse order AND reverse
the op order within each wave. This preserves the existing Contract
invariant — a constraint added in op[0] must be validated before
op[1]'s shadow column is dropped — while still permitting
cross-table parallelism inside each reversed wave.

## Consequences

Easier: **wall-clock savings on wide bundles**. A bundle with 8 ops
on 8 distinct tables runs in roughly the slowest op's time rather
than 8× that. At `parallelism: 4` a 16-op bundle finishes in 4×
the slowest op's time rather than 16×.

Easier: **safe defaults**. `parallelism: 0` (default) keeps the pre-
B3 strict-serial behaviour bit-for-bit. Teams that want the serial
trace for auditability or debugging don't have to change anything.

Easier: **no engine changes**. The existing op handlers already open
their own transactions via `pool.Begin()`; they are stateless between
invocations. Concurrency is safe because the Engine struct has no
mutable fields between Apply calls.

Harder: **partial-wave commits on failure**. Under sequential
execution, a failure at op[3] means ops 0-2 are committed and op[4+]
are untouched — clear rollback surface. Under wave parallelism, a
wave of 4 ops might see 3 commit and 1 fail; abort recovery has to
tolerate that. The existing per-op abort handlers are already
idempotent via `IF EXISTS` (ADR 0018), so this is survivable but
requires operators to understand that "failed" can mean "partial".

Harder: **connection pool pressure**. A bundle with `parallelism: 16`
grabs 16 connections at once. Operators must size the CNPG pool to
the bundle's worst-case parallelism plus headroom. The B2
`spec.maxConcurrentExecutions` knob caps cross-schema fanout, but
within a single execution, `parallelism` controls the per-schema
draw.

Harder: **debugging**. A failed wave dumps multiple error messages
in non-deterministic order. The first-error-wins convention in
errgroup is clear for logs but can hide which op failed first if
multiple fail in the same wave. Mitigation: the per-op error
message includes the op index + kind + table, so operators can
cross-reference.

## Alternatives considered

- **Full DAG with explicit per-op dependencies** (author declares
  `dependsOn` on each op). Rejected — operators would have to
  author the DAG; the greedy table-based scheduler infers 95% of
  the win from free information already in the op spec. Keep
  declarative surface minimal.

- **Cross-table FK dependency analysis**. Two ops that touch
  different tables but create an FK from A to B are technically
  dependent — B's column must exist before A's FK. Rejected for
  now: the FK clause is free-form SQL inside `AddConstraintOp.
  Definition`; parsing it reliably requires a SQL parser Keystone
  doesn't ship. Documented caveat: authors writing cross-table FKs
  within one bundle should keep `parallelism: 0` or place the
  dependent op later in the bundle so the greedy planner's
  relative-order invariant holds.

- **Parallelise across MigrationExecutions instead of within one**.
  Already exists via B2's fanout plus RolloutPolicy stage
  parallelism. Different axis of parallelism; orthogonal. Both can
  be enabled simultaneously.

- **Infer parallelism from the bundle shape** (auto-enable when
  every op has a distinct Table). Rejected — silent behaviour
  changes break the "what I deploy is what runs" mental model.
  Opt-in via the spec field keeps the execution model visible at
  `kubectl get migrationbundle -o yaml`.

- **Parallelise the versioned strategy too**. Would require a SQL
  parser to extract per-file table dependencies. Deferred until we
  pick up `pg_query_go` or similar for a broader static analysis
  story; not a one-off for this phase.
