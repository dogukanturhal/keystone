# Authoring migrations from code

`keystonectl migrate diff <name>` authors a new versioned, reversible
migration bundle from a **desired schema expressed in code** — the
`atlas migrate diff` of Keystone. You describe the schema you want;
Keystone computes the SQL to get there and writes a lint-clean
`NNN_<name>.up.sql` / `.down.sql` pair plus a regenerated `keystone.sum`.

See [ADR 0026](adrs/0026-schema-source-loaders-and-dev-db-normalizer.md)
for the design, and [migration-authoring.md](migration-authoring.md) for
the standards every generated file satisfies by construction.

## The model

```
desired (yaml:// | sql:// | db://)  ─┐
                                     ├─►  diff  ─►  NNN_<name>.{up,down}.sql + keystone.sum
current (dev-replay | --from db://) ─┘
```

- **Desired** state — what you want — comes from one of three sources.
- **Current** state — what the migration directory already produces — is
  established by replaying it onto a throwaway **dev database**
  (`--dev-url`), so live drift never leaks into the authored migration. For
  adoption/greenfield you can instead diff against a live DB (`--from`).
- A **dev database** (any throwaway PostgreSQL, e.g. a local container,
  running the **same major version as production**) normalises every SQL
  source. Nothing is written to your real database.

## Desired-state sources

| Reference | Meaning |
|-----------|---------|
| `yaml://schema.yaml` | A `SchemaDefinition` (declarative schema-as-code), hand-edited or produced by `keystonectl inspect`. |
| `sql://schema.sql` / `file://schema.sql` | Raw DDL — hand-written, `pg_dump --schema-only`, or emitted by an ORM (see below). Applied to a dev schema and inspected. |
| `db://<dsn>?schema=<s>` | An existing database already holding the desired shape. |

## Declarative (SchemaDefinition or SQL)

```bash
# Edit the desired schema, then:
keystonectl migrate diff add_users_email_index \
    --desired yaml://schema.yaml \
    --dev-url postgres://localhost:5432/devdb \
    --dir migrations
```

`migrate diff` replays `migrations/*.up.sql` onto a scratch schema on
`devdb`, diffs it against `schema.yaml`, and writes the delta as the next
ordinal migration. Re-run after each schema change; an empty diff authors
nothing.

Flags worth knowing:

- `--from db://<dsn>` — diff against a live database instead of dev-replay
  (adoption / greenfield, where there is no migration history yet).
- `--allow-destructive` — permit `DROP TABLE/COLUMN`, `ALTER COLUMN TYPE`.
  Required when the desired schema removes objects; without it the diff
  refuses.
- `--lint error|warn|off` (default `error`) — gate the generated `up.sql`
  through the analyzer. `error` refuses to write a bundle with
  error-severity findings; destructive-class findings are exempted when
  `--allow-destructive` is set.
- `--schema` (default `public`) — the target schema; generated DDL is
  rewritten schema-relative so it is portable.

## From ORM entities

Keystone never models your ORM. Instead the ORM emits SQL, and Keystone's
dev database ingests it — so the workflow above works for any ORM that can
produce a CREATE script.

### .NET — EF Core

Use the [`HexxLock.Keystone.Sdk.EntityFrameworkCore`](../../keystone-sdk/dotnet/src/HexxLock.Keystone.Sdk.EntityFrameworkCore/README.md)
package to export your model's DDL (no database connection needed):

```csharp
await KeystoneEf.WriteCreateScriptAsync(dbContext, "schema.sql");
```

```bash
keystonectl migrate diff add_customer_table \
    --desired sql://schema.sql \
    --dev-url postgres://localhost/devdb \
    --dir migrations
```

Keep the model schema-relative (don't call `HasDefaultSchema`), or pass
`stripSchema:` to `ExportCreateScript`.

### Go — GORM / ent

Use the upstream Atlas providers to print your model's DDL, then feed it in:

```bash
# GORM (standalone mode):
go run -mod=mod ariga.io/atlas-provider-gorm load \
    --path ./models --dialect postgres > schema.sql

# ent:
go run -mod=mod ./ent/migrate/main.go > schema.sql   # or `atlas migrate diff` provider

keystonectl migrate diff add_orders \
    --desired sql://schema.sql \
    --dev-url postgres://localhost/devdb \
    --dir migrations
```

Alternatively, point `--desired db://<dev-dsn>` at a dev database your ORM
has already `AutoMigrate`d / migrated into — Keystone inspects it directly.

## What you get

For every change the differ computes:

- `NNN_<name>.up.sql` — forward statements, `SET LOCAL statement_timeout`
  header, leading comment, schema-relative DDL, `CREATE INDEX CONCURRENTLY`
  for indexes.
- `NNN_<name>.down.sql` — the reverse, applied in reverse order, with
  `IF EXISTS` guards; irreversible operations (e.g. `DROP COLUMN`) are
  marked explicitly rather than silently dropped.
- `keystone.sum` — regenerated over the directory's `*.up.sql` files.

Commit the three files and open an MR — they flow through the normal
Plan-Gate-Apply pipeline.

## Known limitation: identity columns

The inspector models a `GENERATED … AS IDENTITY` column as a plain column
plus a standalone sequence, so a baseline reproduced from an inspected DB
recreates the sequence rather than the `IDENTITY` clause. This is faithful
for **adoption** (the DDL is recorded as already-applied, not executed) but
means a from-empty *replay* of such a baseline yields a sequence-backed
column rather than an identity column. Hand-edit the baseline if you need
the exact `IDENTITY` form on a greenfield apply.
