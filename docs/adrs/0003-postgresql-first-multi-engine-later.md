# 0003. PostgreSQL-first; pluggable engine abstraction for later

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

HexxLock's stack runs exclusively on PostgreSQL today: CNPG-managed clusters
in-cluster, Falcon-ID + business services all on PG 16. Designing multi-engine
abstractions upfront would slow v0.1.0 and produce speculative interfaces that
don't match the engines we'd actually support.

Atlas supports 8+ engines, which is a large differentiator. But Keystone's
unique positioning is *not* "generic DB schema tool" — it's "multi-tenant
SaaS lifecycle on Kubernetes, which happens to include schema management."
For that positioning, PG coverage first is sufficient for v1.

## Decision

v0.1.0 ships PostgreSQL only. All DDL helpers live in `internal/postgres/`.
The public CRD `DatabaseProvider.spec.engine` is an enum with only
`postgresql` permitted today.

An `internal/engine/` abstraction exists (Phase 13.1) to make MySQL / other
engine addition a clean one-PR affair when demand lands, but the abstraction
is deliberately not filled in yet — YAGNI.

## Consequences

Easier: v0.1.0 is shippable in weeks instead of months. The abstraction layer
can be refined after MySQL demand is real, avoiding guessing what MySQL-
specific quirks matter.

Easier: test matrix is 1 engine × N features, not N × M. Quality of each
feature is correspondingly higher.

Harder: users on MySQL / MSSQL / Oracle / Snowflake / Spanner cannot adopt
Keystone. For them, Atlas is the right tool. We lose those evaluations.

Harder: some design choices (e.g., using `information_schema` dialect directly
rather than going through an engine driver) may need refactoring when MySQL
lands. We mitigate by keeping dialect-specific code in `internal/postgres/`
rather than leaking into reconcilers.

## Alternatives considered

- **Multi-engine from day 1**: would have delayed v0.1.0 by 3-6 months with
  speculative abstractions based on engines we don't operate. Historical
  experience (Atlas started PG-first too) validates the narrower initial
  scope.
- **Delegate to Atlas for non-PG**: Atlas has better non-PG coverage;
  embedding Atlas as a sub-engine is possible but licensing + dep weight
  argues against it today. Revisit in Phase 14 when pgroll integration
  lands.
