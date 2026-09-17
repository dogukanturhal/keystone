# 0026. Schema-source loaders + the dev-database normaliser

- **Status**: Accepted
- **Date**: 2026-05-27
- **Deciders**: HexxLock platform team

## Context

ADR 0009 committed Keystone to supporting the declarative / schema-as-code
strategy alongside versioned migrations, citing "drift detection is inherent,
onboarding existing DBs is fast" as the payoff. Two author-facing workflows
realise that payoff: **scaffold from a database** (an existing schema →
the full Keystone resource set) and **author migrations from code** (a
desired schema expressed in code → a versioned, reversible bundle). The
library primitives existed — `drift.Inspector` reads a schema into a
`Snapshot`, `declarative.Diff` turns a `(Snapshot, SchemaDefinitionSpec)`
pair into forward + reverse SQL — but nothing tied them into the GitOps
bundle format, and "from code" had no concrete meaning.

The hard question is what "code" is. Keystone has no ORM/entity layer; its
declarative source of truth is the `SchemaDefinition`. But teams' real
source of truth is often a SQL DDL file, a live database, or an ORM model
(EF Core, GORM, ent). Re-implementing each ORM's model→DDL translation
inside Keystone would be a large, fragile, version-coupled surface.

Atlas solved the same problem with two ideas we adopt: a **dev database**
that computes diffs against a throwaway schema, and **external schema
providers** where the ORM emits SQL that the dev database ingests — Atlas
never models the ORM itself.

## Decision

Introduce a **schema-source loader** abstraction (`schemasource`) whose
references — `yaml://`, `sql://` / `file://`, `db://` — all resolve to a
desired `SchemaDefinitionSpec`, and make a **dev database the universal
normaliser**: any SQL DDL (hand-written, `pg_dump`, or emitted by an ORM)
is applied to a throwaway scratch schema via the production
`migration.Runner`, read back by the existing `drift.Inspector`, and
converted with `schemaspec.FromSnapshot`. The current state for
`migrate diff` is established the same way — by replaying the existing
migration directory onto the dev database (canonical) or by inspecting a
live database directly (`--from db://`, for adoption). The computed
`declarative.Plan` is rendered into a versioned, reversible bundle by
`authoring.RenderUpDown`.

ORM support therefore requires **no ORM-specific code in Keystone**: the
ORM emits SQL (EF Core via `GenerateCreateScript`, shipped as
`HexxLock.Keystone.Sdk.EntityFrameworkCore`; GORM/ent via their existing
provider programs), Keystone ingests it through the dev database.

## Consequences

- "Migrations from code" works for both declarative schemas and ORM
  entities through one mechanism; adding a new ORM is a documentation task,
  not a code change.
- The reusable primitives live in `keystone-sdk` (Apache-2.0) — `schemaspec`,
  `schemasource`, `authoring` — keeping `keystonectl` (AGPL) a thin
  orchestrator and letting third-party tools build the same workflows.
- A dev database becomes a hard dependency of the canonical `migrate diff`
  and of `sql://` desired resolution. This matches Atlas and is cheap
  (a local container), but it is a new operational requirement for authors.
- The dev-replay snapshot embeds a random scratch-schema name in its
  catalog-derived DDL; the loader rewrites it to the target schema so the
  differ does not see spurious index churn. The differ's `renderCreateTable`
  was also extended (via `diffTableObjects`) to emit a *new* table's indexes
  and foreign keys, which the create path previously dropped — required for
  faithful baselines.
- The dev database must run the same PostgreSQL major version as production
  for the normalisation to be faithful (a documented constraint).

## Alternatives considered

- **Re-implement ORM model introspection inside Keystone** (read EF Core's
  `IRelationalModel`, parse GORM struct tags). Rejected: large, fragile,
  per-ORM, and version-coupled — exactly what Atlas avoids by going through
  SQL + a dev database.
- **Parse SQL DDL into a `SchemaDefinitionSpec` directly** (no dev database).
  Rejected: a full PostgreSQL DDL parser is a major undertaking and would
  drift from PostgreSQL's own canonicalisation; round-tripping through a real
  server is exact and reuses `drift.Inspector`.
- **Diff against the live database only** (no dev-replay). Rejected as the
  default: live drift would be baked into authored migrations. Retained as
  the `--from db://` mode for adoption and greenfield.
- **Add a `DiffSnapshots(observed, desired *Snapshot)` path** to avoid the
  `Snapshot → Spec` conversion for DB-sourced desired state. Rejected:
  reworking the 2900-line differ to accept two snapshots is far riskier than
  reusing `schemaspec.FromSnapshot`, which is already exercised by
  `keystonectl inspect`.
