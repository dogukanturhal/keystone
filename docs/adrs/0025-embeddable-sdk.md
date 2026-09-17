# 0025. Embeddable Go SDK (Apache-2.0)

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

Keystone's core primitives — analyzer pack (52+ rules), SQL
runner, structural inspector, declarative differ — are individually
useful outside a Kubernetes cluster. Common requests:

- "In my Go test, I want to spin up a real Postgres with
  `testcontainers-go`, apply the app's migrations, and assert the
  resulting schema shape. Without GitOps. Without CRDs. Just a
  function call."
- "My CI job runs the 52-rule lint pack as a pre-commit check on
  every PR that touches `migrations/*.sql`. Same rules the operator
  uses — no subprocess, no Docker."
- "The dev team writes a declarative schema YAML; I want to preview
  the SQL that would run before committing."

Today every one of those workflows has to either run the full
Keystone operator (overkill) or hand-roll a subset of the internals
(bitrot). And two licensing concerns compound the problem:

1. Keystone's operator is AGPL-3.0-or-later (ADR 0001). A
   proprietary Go consumer importing `internal/` inherits AGPL —
   legally a non-starter for most commercial consumers.
2. `internal/` is Go's "don't import this" marker. Consumers can't
   use it from a separate module even if they wanted to.

ADR 0001 anticipated this exact situation: "The public SDK at
`services/keystone/pkg/sdk/` and the CRD Go types imported by
third parties are Apache-2.0." This ADR is the implementation of
that clause.

## Decision

Ship the public SDK at `pkg/sdk/keystone/`, Apache-2.0, with a
stable surface:

```go
import ks "github.com/dogukanturhal/keystone/pkg/sdk/keystone"

ks.Apply(ctx, pool, ks.ApplyOptions{...}) (*ApplyResult, error)
ks.Lint(ctx, bundle, version, files) ([]LintFinding, error)
ks.Inspect(ctx, pool, schema) (*Snapshot, error)
ks.Diff(observed, desired, opts) (*DiffPlan, error)
```

Design invariants:

- **Own Go types** — `SQLFile`, `ApplyResult`, `Snapshot`, `Column`,
  `ObjectDDL`, `DiffPlan`. Internal types (`migration.ResolvedSource`,
  `drift.Snapshot`, `declarative.Plan`) are NOT re-exported. Callers
  import only the SDK package; internal refactors stay invisible.

- **Caller-owned pool** — `*pgxpool.Pool` is passed in, never
  constructed. The SDK doesn't own lifecycle, doesn't cache, doesn't
  pool-manage. That's the caller's problem.

- **No Kubernetes surface** — zero imports from `controller-runtime`,
  `k8s.io/*`, `sigs.k8s.io/*`. The one intrusion is
  `api/v1alpha1.SchemaDefinitionSpec` for `Diff`'s desired-state
  input, which is already Apache-2.0 per ADR 0001 and imports only
  `metav1` for ObjectMeta compatibility. A future phase (v2) could
  introduce a pure-Go DesiredSchema type if that coupling turns out
  to hurt.

- **Idempotent Apply** — `Apply` checks `schema_migrations` before
  running; same-version-same-hash is a no-op success,
  same-version-different-hash is refused. Matches the
  MigrationExecution reconciler's behaviour so SDK + operator agree
  on the invariants.

- **Stability contract** — v1-stable within a major release. Types
  don't rename, retype, or drop; additions happen in minor releases.
  Breaking changes require a module major-version bump.

### Dual-license enforcement

- Every file under `pkg/sdk/` carries the SPDX header
  `Apache-2.0`.
- Every file under `cmd/`, `internal/`, `api/`, `config/` carries
  `AGPL-3.0-or-later`.
- `LICENSE` (root) is AGPL; `LICENSE-Apache-SDK` is Apache-2.0.
- `go.mod` is a single module — the two licenses coexist via SPDX
  headers + two LICENSE files.

## Consequences

Easier: **CI test authors stop forking the runner**. The canonical
"spin up Postgres, apply my migrations" flow is four lines of Go.

Easier: **proprietary adopters unblocked**. A commercial consumer
can import `pkg/sdk/keystone` without inheriting AGPL.

Easier: **lint-only pre-commit hooks**. `ks.Lint(...)` is a pure
function — no pool, no DB, no cluster. Perfect for `.git/hooks`
scripts and GitHub Actions.

Easier: **api/v1alpha1 SchemaDefinitionSpec reuse**. The SDK and
operator share the desired-state type so schemas written in YAML
are consumable by both.

Harder: **two surfaces to evolve in lockstep**. A breaking change
to `internal/migration.Runner.Apply` requires either adapting the
SDK wrapper or shipping the break as a v2. Guarded by the SDK's
own tests (integration + examples) so drift surfaces at CI time.

Harder: **SDK consumers don't get the full Keystone experience**.
No auto-snapshots, no audit chain, no approvals. Documented in the
README as "not in scope" so adopters know what they're giving up.

Soft: **compile-time coupling between SDK and internal**. The SDK
imports internal packages (which Go allows from within the same
module). This is fine for the monorepo model — internal's "don't
import externally" invariant is preserved, while intra-module
consumption keeps the SDK thin. A future split into a separate
module (`github.com/hexxlock/keystone-sdk`) would inherit this
code by git submodule / go replace — postponed until there's
concrete external demand.

## Alternatives considered

- **Separate module from day one** (`keystone-sdk/` with its own
  `go.mod`). Would enforce license boundaries at import-time, but
  doubles CI complexity (two modules to version, tag, publish) and
  doesn't buy anything the SPDX headers don't already give.
  Reconsider if the project splits into a GitHub multi-repo layout.

- **Expose the internal types directly** via type aliases. Rejected
  — Go's `internal/` marker would block cross-module imports even
  with aliases, and we'd still want to pin the external surface.

- **vendor cel-go / pgroll / etc. into the SDK** so it's
  self-contained. Rejected — the SDK is a thin wrapper over the
  same internal implementations the operator uses. Vendoring
  doubles the maintenance burden.

- **Make the SDK the primary authoring interface** (deprecate the
  internal packages). Rejected — the operator's reconcilers have
  Kubernetes-coupled concerns (events, conditions, status patches)
  that the SDK's pool-centric shape doesn't cover. Keep both.

- **Open-source the SDK under AGPL** (same as the operator).
  Rejected — ADR 0001 already made this decision, and this ADR is
  the execution of that plan.
