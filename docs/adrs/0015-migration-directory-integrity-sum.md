# 0015. Migration directory integrity via `keystone.sum`

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

A MigrationBundle's SQL files come from a `ConfigMap`, OCI artifact, or
Git path. Once applied, the bundle records a `ContentHash` in its
status so a later reconcile that sees a different hash at the same
`spec.version` is refused (the admission webhook and controller both
check it). That pair already makes the bundle *tamper-evident* from
the operator's point of view — but only *after* the first apply.

What it does not do:

- Make tampering visible to code review **before** it reaches the
  cluster. A reviewer looking at a GitLab MR that edits
  `migrations/002_users.up.sql` has no way to tell whether the author
  also edited `001_init.up.sql` unless they open every file.
- Force a merge conflict when two branches both add a new migration.
  Today the apply order is implicit (lexicographic filename), so two
  branches can each commit `20260418_foo.up.sql` and `20260418_bar.up.sql`
  independently; both merge cleanly into `master`, and whichever lands
  last might run in a different order than its author tested.
- Give the `keystonectl` CLI a way to report "this directory is
  corrupt" without connecting to the cluster.

Atlas, Bytebase, and Liquibase all solve this with the same primitive:
a committed sum file beside the migrations that lists every file's
hash plus a directory-level hash. Atlas calls theirs `atlas.sum`.

## Decision

Ship `keystone.sum` — an in-tree, committed, flat-list-with-root
integrity artifact co-located with migrations. Format mirrors Atlas
for familiarity but uses hex (matching the existing `ContentHash`
encoding) instead of base64:

```
h1:<hex-sha256-of-body>
001_init.up.sql h1:<hex-sha256>
002_users.up.sql h1:<hex-sha256>
```

- Line 1: `h1:` + SHA-256 of the body (all subsequent lines, newline-
  terminated, sorted by filename).
- Lines 2..end: `<name> h1:<hex>` per file, sorted.
- Per-file hash: SHA-256 over `name || 0x00 || content || 0x00` — the
  single-file form of the existing `HashFiles` primitive, so root and
  content hashes share a mental model.

The sum file is the authoritative artifact. It rides inside the
`MigrationSource` (ConfigMap data key, OCI entry, Git path). The
resolver strips it out of `Files` before linting; the controller and
webhook verify it at admission and reconcile.

Reactions to each outcome:

| Status    | Controller     | Webhook        | Policy gate                  |
|-----------|----------------|----------------|------------------------------|
| Valid     | Ready path     | Admits         | —                            |
| Missing   | Ready path (Unknown condition) | Warning | Error when `SchemaPolicy.spec.integrity.requireSumFile=true` |
| Malformed | Ready=False    | Rejects        | Always an error              |
| Mismatch  | Ready=False    | Rejects        | Always an error              |

`keystonectl sum <dir>` generates the file locally. `keystonectl sum
verify <dir>` re-checks an existing one and exits non-zero on
divergence. Per-commit Git hooks or a GitLab CI job call the verify
mode so drift is caught before push.

## Consequences

Easier: **code review**. When `002_users.up.sql` changes, `keystone.sum`
changes too — a reviewer sees a diff across both files and knows
immediately that the author meant this edit. When a reviewer sees a
SQL file diff *without* a sum diff, they know someone bypassed the
tooling.

Easier: **merge conflicts on concurrent migration creation**. The sum
file's root hash changes on any add/remove/reorder, so two branches
each adding a migration both touch the same line in `keystone.sum` and
Git refuses to auto-merge.

Easier: **client-side verification**. The same primitive that the
controller uses lives in `internal/migration/sum.go` and the CLI
imports it. A dev can run `keystonectl sum verify migrations/` on any
laptop without cluster access.

Harder: **transient adoption window**. Existing bundles have no
`keystone.sum`. We ship Phase A1 in advisory mode — `SumMissing`
surfaces as an `Unknown` condition, not `False`. Phase A3 flips the
policy default to require the file once every in-tree bundle carries
one.

Harder: **`keystone.sum` becomes part of the API surface**. Changing
the format is breaking. Rollouts of a v2 format would use a new prefix
(`h2:`) and support both during a deprecation window.

Softly harder: **one more file to regenerate**. Authors must run
`keystonectl sum <dir>` after every SQL change. The commit-msg hook
(shipped alongside this ADR) runs `sum verify` and rejects commits
whose sum is out of date.

## Alternatives considered

- **Merkle tree with inclusion proofs.** Atlas-style flat list is
  enough for `~100` migration files: the full content is always
  available at resolve time, so sparse inclusion proofs have no use
  case. A true Merkle tree adds constant complexity for no enterprise
  benefit today. Revisit if an OCI-artifact layer wants to ship
  partial bundles (unlikely — bundles are indivisible).

- **CRD-embedded sum (spec field).** Would let `kubectl apply` carry
  the sum without a file. Rejected because it separates the sum from
  the SQL it covers — reviewers looking at a ConfigMap edit wouldn't
  see the sum change unless they also scrolled to the bundle CR. The
  whole point is proximity.

- **Detached signature (cosign) per migration file.** Heavier: needs
  keys, a signer identity, Rekor. Legitimate future work for supply-
  chain sign-off on shipped OCI artifacts; overkill for the daily
  authoring loop this phase addresses.

- **Git hook only, no server-side verification.** The controller and
  webhook would admit tampered bundles as long as the attacker also
  updated the sum file. The point of server-side verification is that
  it refuses to apply SQL that diverges from the committed sum even
  when the committer is compromised — a layered defence, not a
  client-side convenience.

- **Defer to Atlas's own `atlas.sum`.** Shells out to a separate
  binary on every reconcile, couples Keystone to Atlas's format
  decisions, and pulls in Apache-2.0 + commercial Atlas tooling that
  ADR 0005 consciously avoided. Implementing the primitive in-tree is
  ~200 LoC and keeps the dependency graph clean.
