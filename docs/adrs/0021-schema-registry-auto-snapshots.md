# 0021. Schema registry — auto-snapshots + structural diff

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

Keystone already has a `SchemaSnapshot` CRD that captures a
DatabaseSchema's applied-migration history, a structural fingerprint
(SHA-256), and a Mermaid ERD for human viewing. The piece missing
from a "schema registry" story was:

1. **Snapshots were never automatic.** Every capture required an
   operator to `kubectl apply` a SchemaSnapshot CR. In practice
   operators forgot until drift alerts fired, and the last-known-
   good state was lost.

2. **Snapshots were not machine-diffable.** The captured ERD is
   rendered as Mermaid markdown — good for rendering in dashboards,
   not parseable back into tables + columns. The fingerprint only
   answers "did this schema change?", not "what changed?".

3. **No CLI for the registry.** Operators triaging "what schema did
   we ship with bundle X@v2?" had to walk per-snapshot `kubectl get
   -o yaml` output by hand.

Atlas Cloud's paid registry tier covers (1) and (2); Bytebase's
registry covers (3). Keystone needed all three to claim feature
parity for tier-1 multi-tenant SaaS operators.

## Decision

Three additions, all stacking on the existing SchemaSnapshot CRD:

### 1. Structural snapshot field

New `SchemaSnapshotStatus.Structure *StructuralSnapshot` — a
deterministic, JSON-serialisable mirror of `drift.Snapshot`:

```
Structure:
  tables:
    - name: users
      kind: BASE TABLE
      columns:
        - name: id
          ordinal: 1
          dataType: uuid
          udtName: uuid
          nullable: false
        - name: email
          ordinal: 2
          dataType: text
          udtName: text
          nullable: true
  indexes:
    - name: idx_users_email
      table: users
      type: btree
      definition: CREATE INDEX idx_users_email ON users USING btree (email)
  constraints:
    - name: pk_users
      table: users
      type: PRIMARY KEY
      definition: PRIMARY KEY (id)
```

Byte-identical serialisation across captures of identical schemas.
Populated at every snapshot capture alongside ERD + fingerprint.

### 2. Auto-snapshot on every successful MigrationExecution

`markSucceeded` gains an `ensureAutoSnapshot` call that creates a
`SchemaSnapshot` CR with:

- Deterministic name `auto-<bundle>-<version>-<schema>` (K8s 253-
  char trim). Idempotent retries reattach to the original CR.
- `spec.reason = "auto: applied <bundle>@<version>"`.
- `status.autoSource = "<bundle>@<version>"` — filterable via label
  selector AND JSON-path on the status subresource.
- `spec.retentionDays = 365` (operator can change per-snapshot
  afterwards).
- `AlreadyExists` is silently swallowed; other errors emit a
  Warning event but do NOT flip the execution back from Succeeded
  — the DDL already committed, the snapshot is a nicety.

Controller-reference owner: the MigrationExecution. Deleting the
execution cascades to its snapshot (intentional — auto-snapshots
follow their originating execution's retention policy).

### 3. `keystonectl snapshot` subcommand

Three operations against the cluster via the same kubeconfig loader
that serves `keystonectl plan approve`:

- `list <schemaRef> -n <ns>` — table of snapshots for a given
  DatabaseSchema (name, phase, capturedAt, autoSource).
- `diff <snap-a> <snap-b> -n <ns>` — runs the new `schemadiff`
  package over the two snapshots' `Status.Structure` fields. Output
  is a sorted change list: `TableAdded users`, `ColumnRetyped
  users.age: integer → bigint`, etc.
- `show <name> -n <ns>` — dumps `Status.Structure` as YAML,
  excluding the multi-megabyte ERD field.

## Consequences

Easier: **continuous registry, no discipline required**. Every
applied migration leaves an immutable artifact behind. Bundle X@v2
has a snapshot named `auto-X-v2-<schema>` for every target schema
— no "we forgot to snapshot" recovery stories.

Easier: **machine-readable diff**. The structural snapshot is a
typed Go struct; the differ is a pure function. Any caller that
knows the `StructuralSnapshot` shape can compute changes — dashboards,
alerting rules, approval webhooks.

Easier: **CLI triage**. `keystonectl snapshot diff auto-X-v1-users
auto-X-v2-users` answers "what changed between v1 and v2 of the
users schema" in one command. Compare to five minutes of kubectl
+ jq + diff.

Harder: **snapshot CR count grows linearly with migrations**.
Every successful bundle × every target schema = one snapshot. A
50-tenant fleet running 100 migrations/year creates 5000 snapshots.
Mitigation: `retentionDays: 365` default + a future cleanup job
(Phase C-TBD) that prunes old auto-snapshots by creation timestamp.
`auto` label makes filtering trivial.

Harder: **structural snapshot size**. Per-snapshot ~50-200 KB.
A large multi-tenant CNPG cluster might hit etcd's 1.5 MiB CR
ceiling on schemas with 1000+ tables. Mitigation: the existing
`MaxItems=2048` cap on Tables + `MaxItems=4096` on
Indexes/Constraints. Beyond that, the schema is almost certainly
mis-designed.

Soft: **two structural formats in flight**. `drift.Snapshot`
(internal, not serialised) and `StructuralSnapshot` (CRD). They
mirror each other; the reconciler translates at capture time via
`structuralSnapshotFromDrift`. Drift between the two would cause
fingerprint ≠ structure. Noted in the test suite.

## Alternatives considered

- **Separate SchemaRegistry CRD indexing snapshots**. Rejected —
  SchemaSnapshot already IS the registry. Adding a wrapper just to
  list + count is complexity without value. Labels + the `snapshot
  list` CLI cover the "index" story.

- **Reconstruct structure from ERD + fingerprint**. Rejected —
  Mermaid ERD is lossy (no data types, no nullable flags, no
  indexes/constraints). The ERD is for rendering; structure is for
  analysis.

- **Auto-snapshot ONLY for manual bundles** (not auto-generated
  bundles from declarative differs). Rejected — the point is to
  capture the post-apply state regardless of how the bundle was
  authored.

- **Store raw DDL (CREATE TABLE statements)**. Tempting for copy-
  paste restore, but PG's `pg_dump` is the right tool for that.
  The structural form is smaller, typed, and diffable; ops who
  want DDL run `pg_dump` at snapshot time and stash it out-of-band.

- **Configurable auto-snapshot opt-out via SchemaPolicy**.
  Deferred — no evidence anyone needs to opt out. If the snapshot
  volume becomes painful before the cleanup job lands, we revisit.
