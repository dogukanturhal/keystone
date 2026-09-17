# Phase 9 — Enterprise Migration Engine Hardening

Phase 9 closes the gap between Keystone and best-in-class migration
tools (Atlas, pgroll, SchemaHero, Bytebase) while keeping Keystone's
architectural advantages: 53 built-in analyzers, pgroll expand/contract,
N-of-M approval gates, drift detection, and staged rollout.

## 9.4 — Schema Visualization (Mermaid ERD)

**Package:** `internal/viz`

SchemaSnapshot status now includes an `erd` field containing a Mermaid
`erDiagram` string generated from the drift inspector's structural
snapshot at capture time.

- Tables rendered as entities with typed columns
- PK / FK / UK markers from constraint introspection
- FK relationships rendered as `||--o{` edges
- Views excluded (only BASE TABLE entities)
- HexxForge UI and GitLab markdown render natively

```yaml
status:
  erd: |
    erDiagram
        customers ||--o{ orders : "orders_customer_fk"
        customers {
            uuid id PK
            text name
        }
        orders {
            uuid id PK
            uuid customer_id FK
            numeric total
        }
```

## 9.2 — Richer Declarative Differ

**Package:** `internal/migration/declarative`

### New CRD Types

`SchemaDefinitionSpec` gained four new object collections:

| Type | Purpose |
|------|---------|
| `DesiredEnum` | PostgreSQL enum types with ordered value lists |
| `DesiredSequence` | Sequences with datatype, increment, min/max/start, owned-by |
| `DesiredView` | Views with query and CREATE OR REPLACE support |
| `DesiredFunction` | Functions/procedures with args, returns, language, body |

### Differ Enhancements

| Capability | Before | After |
|-----------|--------|-------|
| CREATE TABLE order | Alphabetical | Topological sort (Kahn's algorithm, FK deps) |
| Index creation | `CREATE INDEX` | `CREATE INDEX CONCURRENTLY IF NOT EXISTS` |
| Index drops | `DROP INDEX` | `DROP INDEX CONCURRENTLY` |
| NOT NULL + DEFAULT | Warn only | 3-step: SET DEFAULT → UPDATE backfill → SET NOT NULL |
| FK constraints | Types only | Two-phase: ADD NOT VALID → VALIDATE CONSTRAINT |
| FK drops | None | DROP CONSTRAINT (destructive) |
| Index diff source | Empty map | Full snapshot indexes threaded through |
| Reverse SQL | None | `Plan.ReverseStatements` aligned 1:1 |
| Enums | None | CREATE TYPE IF NOT EXISTS + ADD VALUE IF NOT EXISTS |
| Sequences | None | CREATE SEQUENCE IF NOT EXISTS |
| Views | None | CREATE OR REPLACE VIEW |
| Functions | None | CREATE OR REPLACE FUNCTION |
| Views in observed | Spurious DROP | Filtered (only BASE TABLE) |

### Statement ordering

Enums → Sequences → Tables (toposorted) → Column/FK alters →
Index CONCURRENTLY → Views → Functions → Drops (reverse dep order).

## 9.1 — Dev Database Normalization

**Package:** `internal/devdb`

Keystone's equivalent of Atlas's `--dev-url` pattern. Every migration
is replayed against a real PostgreSQL instance to catch semantic issues
that static analysis misses.

### Architecture

```
SchemaPolicy.spec.devDatabaseRef → DatabaseProvider → pgx Pool
                                                        ↓
MigrationBundleReconciler.runAnalyzers()
  ├── 53 static analyzers (DefaultRegistry)
  └── SemanticAnalyzer (if DevDBPool != nil)
        ├── Acquire temp schema (ks_dev_<random>)
        ├── Replay migration SQL files in order
        ├── On failure → error finding (blocks fanout)
        ├── On success → Inspect schema
        │   ├── checkOrphanedFKs → error if FK targets missing
        │   └── checkEmptyTables → warning if 0-column table
        └── Release (DROP SCHEMA CASCADE)
```

### Design decisions

- **No CNPG ephemeral clusters** — 30-60s bootstrap is too heavy for a
  10-second lint run. Temp schemas inside a long-lived dev database.
- **No Atlas SDK import** — `drift.Inspector` already provides
  information_schema introspection. Keep it native.
- **Distroless safe** — no subprocess, no /bin/sh. Pure pgx.

### Configuration

Add `devDatabaseRef` to a SchemaPolicy:

```yaml
apiVersion: keystone.hexxlock.io/v1alpha1
kind: SchemaPolicy
metadata:
  name: production
spec:
  targetSelector:
    matchLabels:
      tier: production
  devDatabaseRef: dev-provider  # references a DatabaseProvider
```

## 9.5 — OCI Artifact Source

**Package:** `internal/migration`

MigrationBundles can now pull SQL files from OCI artifacts stored in
Harbor (or any OCI-compliant registry), replacing ConfigMaps for large
migration sets that hit the 1MB ConfigMap limit.

### Usage

```yaml
apiVersion: keystone.hexxlock.io/v1alpha1
kind: MigrationBundle
spec:
  version: "2026.04.19"
  source:
    type: OCIArtifact
    ociArtifactRef:
      repository: ghcr.io/dogukanturhal/migrations/billing
      digest: sha256:abc123...  # immutable reference
      pullSecretRef: harbor-robot-creds
```

### Features

- ORAS Go v2 for OCI pulls (no CLI dependency)
- Digest-pinned references for immutability
- dockerconfigjson or plain username/password Secrets
- PlainHTTP flag for dev registries
- keystone.sum integrity verification (same as ConfigMap)
- `CompositeResolver` dispatches to ConfigMap or OCI automatically

## 9.3 — Down-Migration / Reversibility

### Three sources of reversal SQL

1. **Declarative differ** — `Plan.ReverseStatements` auto-generated
   alongside forward statements (Phase 9.2)
2. **Down source** — `spec.downSource` pointing to `*.down.sql` files
3. **pgroll abort** — built-in expand/contract rollback (existing)

### New CRD fields

**MigrationBundleSpec:**
- `downSource *MigrationSource` — rollback SQL (ConfigMap or OCI)
- `autoRollback bool` — auto-rollback on failure when downSource exists

**MigrationExecutionStatus:**
- `reversalStatements []string` — SQL to undo forward migration

**MigrationExecution phases (new):**
```
Running → Failed → RollingBack → RolledBack       (auto-rollback success)
Running → Failed → RollingBack → RollbackFailed   (auto-rollback failure)
```

### Analyzer #52: require-down-migration

Warns when a versioned bundle has no `downSource`. Skips pgroll
(built-in abort) and declarative (auto-reverse). Severity: warning.

## 9.6 — Cross-Bundle Breaking Change Detection

**Package:** `internal/migration/analyze`

### Analyzer #53: cross-bundle-breaking-change

Detects destructive operations in the current bundle that would break
other pending bundles waiting to apply against the same schema.

The analyzer receives:
- `SchemaObjects` — live schema state from drift.Snapshot
- `PendingBundleSQL` — SQL from other Pending/Running bundles

It flags:
- DROP TABLE referenced by a pending bundle
- DROP COLUMN referenced by a pending bundle
- RENAME COLUMN referenced by a pending bundle
- DROP INDEX referenced by a pending bundle

Severity: error (blocks fanout). No-ops gracefully when no pending
bundles exist or schema objects aren't populated.

### Relationship to existing analyzers

| Analyzer | Scope | What it catches |
|----------|-------|-----------------|
| `no-drop-table` | Per-file | Any DROP TABLE in SQL |
| `cross-migration-breaking-change` | Bundle-local | DROP/RENAME against objects created earlier in the same bundle |
| `cross-bundle-breaking-change` | Cross-bundle | DROP/RENAME against objects referenced by other pending bundles |
