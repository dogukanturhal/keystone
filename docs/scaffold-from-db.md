# Scaffolding from an existing database

`keystonectl scaffold` onboards a brownfield PostgreSQL database into
Keystone's GitOps model. It introspects a live schema and emits the full
set of resources you need to bring that database under management — plus an
adoption baseline migration — as a reviewable starting point.

This is the reverse direction of
[migration-from-code.md](migration-from-code.md): there you go *code →
migration*; here you go *database → code + resources*. Both rest on
[ADR 0026](adrs/0026-schema-source-loaders-and-dev-db-normalizer.md).

## Usage

```bash
keystonectl scaffold \
    --dsn postgres://user:pw@db.internal:5432/appdb \
    --schema public \
    --out ./onboarding/appdb \
    --product app \
    --adopt
```

The introspection is **read-only**; nothing is written to the source
database.

## What it emits

```
onboarding/appdb/
├── databaseprovider.yaml      # physical connection (host/port from --dsn; TODO: credentials Secret)
├── logicaldatabase.yaml       # logical database + owner role (TODO: clusterRef)
├── databaseschema.yaml        # the managed schema
├── schemadefinition.yaml      # declarative desired-state (the inspected shape)
├── migrations/
│   ├── 0001_baseline.up.sql   # the schema's current state as CREATE DDL
│   ├── 0001_baseline.down.sql
│   └── keystone.sum
├── kustomization.yaml
└── README.md                  # apply order + MigrationBundle wiring
```

The baseline `up.sql` is the full schema reconstructed as schema-relative
CREATE DDL (tables, columns, primary keys, **indexes, and foreign keys** —
the two-phase `ADD CONSTRAINT … NOT VALID` / `VALIDATE` form for FKs).

## Flags

- `--out` (required) — output directory.
- `--product` — name used for resource names + the owner role (default:
  derived from `--schema`).
- `--provider-ref` / `--logicaldb-ref` / `--schema-ref` — override the
  generated resource names.
- `--adopt` — emit the baseline as **adoption** state ([ADR 0008](adrs/0008-separate-tracking-table-for-adoption.md),
  [ADR 0027](adrs/0027-record-only-adoption-baselines.md)): the generated
  `migrationbundle.yaml` carries `spec.executionMode: RecordOnly`, so the
  operator records version `0001` as already-applied in the tracking table
  without executing it — the objects already exist. Lint findings on the
  baseline are advisory (`status.lintFindings`), never blocking; integrity,
  immutability, and approval gates still apply.
- `--strip-dangling-refs` — drop FKs/views/triggers that reference
  identifiers outside the introspected schema (e.g. a realm-scoped subset of
  a platform DB). Warnings are always printed.

## After scaffolding

1. Review every `TODO` marker — at minimum the admin-credentials Secret name
   in `databaseprovider.yaml` and the `clusterRef` in `logicaldatabase.yaml`.
2. Create the credentials Secret.
3. If a `SchemaPolicy` with an `ApprovalPolicy` matches the scope label, add
   the required approver annotation to `migrationbundle.yaml`.
4. `kubectl apply -k onboarding/appdb` — the kustomization generates the
   migrations ConfigMap and applies the `MigrationBundle` (already wired;
   `RecordOnly` under `--adopt`, executing otherwise) alongside the
   topology resources. No manual tracking-table SQL — ever.

From then on, author incremental changes with
[`keystonectl migrate diff`](migration-from-code.md) — version `0002`
onward layer on top of the baseline.

## Re-generating the SchemaDefinition

`schemadefinition.yaml` is produced by the same path as `keystonectl
inspect`. Re-run after out-of-band schema changes to refresh the declarative
desired-state:

```bash
keystonectl inspect --dsn "$DSN" --schema public --schema-ref app > schemadefinition.yaml
```

## Known limitation: identity columns

See the note in [migration-from-code.md](migration-from-code.md#known-limitation-identity-columns).
For `--adopt` (the common scaffold case) it is immaterial — the baseline is
recorded as already-applied, not executed.
