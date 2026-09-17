# Contributing to Keystone

Thanks for considering a contribution. Keystone is an AGPL-3.0-or-later
PostgreSQL schema-management operator for Kubernetes. It is maintained by the
HexxLock platform team with the goal of CNCF donation once the API stabilises.
This guide exists so you don't have to guess how the project wants its code.

## Table of contents

- [Before you start](#before-you-start)
- [Developer Certificate of Origin (DCO)](#developer-certificate-of-origin-dco)
- [Local development](#local-development)
- [Branch naming](#branch-naming)
- [Commit message style](#commit-message-style)
- [Pull request workflow](#pull-request-workflow)
- [What not to contribute](#what-not-to-contribute)
- [Where to ask questions](#where-to-ask-questions)

## Before you start

- Read [`docs/adrs/README.md`](docs/adrs/README.md). The ADRs explain the
  non-obvious architectural choices (AGPL, CRD-first, no `kube-rbac-proxy`,
  tier-aware rollouts, etc.) and will answer most "why is it built this way?"
  questions before you file them.
- For substantial changes (new CRD, new isolation mode, new policy class,
  breaking change to an existing CRD), open a design proposal in
  `docs/proposals/` first using `docs/proposals/_template.md`. Code review is
  not the right place to litigate an API.
- For small fixes (bug, typo, test, doc) just open the MR. Don't wait.

## Developer Certificate of Origin (DCO)

Keystone uses the [Developer Certificate of Origin][dco] — the same mechanism
as the Linux kernel and Kubernetes. It is a lightweight alternative to a CLA:
instead of signing a separate document, every commit carries a
`Signed-off-by` trailer certifying that you wrote the contribution or
otherwise have the right to submit it under the project's license.

Install the commit-msg hook once, and every future commit is validated
locally before it lands:

```
make install-hooks
```

This symlinks `hack/git-hooks/commit-msg` into `.git/hooks/`. The hook rejects
any commit missing a `Signed-off-by` trailer whose email matches your
`git config user.email`. CI runs the same check on every MR via the
`dco-check` job.

To sign off a commit manually, use `git commit -s`, which appends:

```
Signed-off-by: Your Name <your.email@example.com>
```

If you forget, amend with `git commit --amend --signoff --no-edit` and
force-push to your branch.

## Local development

Prerequisites:

- Go 1.23+
- `make`, `git`, `docker` (for image builds)
- A Kubernetes cluster for integration testing — `kind` or `k3d` is fine; CI
  uses envtest and does not require a real cluster for unit + integration.

Pinned tool binaries (`controller-gen`, `kustomize`, `envtest`,
`golangci-lint`) are installed into `./bin/` on first use at versions fixed in
the `Makefile`. Do not rely on your system-wide versions.

Common targets:

| Target | What it does |
|---|---|
| `make build`   | Build `bin/keystone-manager` with version stamping |
| `make generate`| Run `controller-gen` — deepcopy, CRDs, RBAC, webhooks |
| `make test`    | Unit + integration tests via envtest, with `-race` |
| `make lint`    | `golangci-lint run ./...` |
| `make verify`  | The full gate CI runs: tidy + generate + lint + test + policy-test |
| `make sbom`    | SPDX SBOM via syft (supply-chain) |
| `make vuln-scan` | Trivy CVE scan, fails on HIGH+CRITICAL with fixes |

Run `make verify` before opening an MR. It is what CI runs; passing locally
saves a round-trip.

### Generated files

`api/**/zz_generated_*.go` and `config/crd/bases/*.yaml` are generated — do
not hand-edit. After changing types under `api/`, run `make generate` and
commit the result in the same MR.

### Migration directory integrity (`keystone.sum`)

Bundles ship a `keystone.sum` sidecar that lists every migration file's
SHA-256 plus a directory-level root hash (see
[ADR 0015](docs/adrs/0015-migration-directory-integrity-sum.md) for the
rationale). Every time you add, edit, or remove a `.up.sql` file inside a
bundle directory, regenerate the sum:

```
keystonectl sum path/to/migrations/
```

This writes `path/to/migrations/keystone.sum`. Commit it alongside the SQL
change in the same MR. Reviewers expect to see the sum diff next to the
SQL diff — an SQL edit without a sum change fails CI (`keystonectl sum
verify`) and is rejected at admission.

The webhook refuses bundles whose sum is malformed or diverges from the
resolved source. A missing sum surfaces as a warning today and will be
upgraded to an error in a future phase once every in-tree bundle carries
one.

### Multi-tenant bundles — tenant labels + throttling

A single MigrationBundle can roll out across dozens of tenant
databases. The B2 support (see
[ADR 0019](docs/adrs/0019-multi-tenant-batch-orchestration.md))
adds two knobs:

- **`spec.maxConcurrentExecutions`** caps in-flight Executions for
  non-staged bundles. Use it to stay under a database's connection
  budget when rolling out to many tenants at once. Default 0 means
  unlimited (pre-B2 behaviour). Ignored for staged bundles —
  RolloutPolicy stage parallelism handles that case.

- **`keystone.hexxlock.io/tenant-id` label on DatabaseSchemas**
  surfaces in `status.schemaProgress` so operators can filter
  per-tenant failures:

  ```
  kubectl get mb my-bundle \
    -o jsonpath='{.status.schemaProgress}' | jq '.[] |
      select(.phase=="Failed") | {tenantID, message}'
  ```

  The label is also usable in RolloutPolicy stage selectors to pick
  a canary tenant (`matchLabels: { keystone.hexxlock.io/tenant-id:
  pilot-acme }`). The label is NOT enforced — unlabelled schemas
  still roll out, just without tenant grouping.

### Embeddable Go SDK

The `api/` module is Apache-2.0-licensed and safe to
import from proprietary Go code (see
[ADR 0025](docs/adrs/0025-embeddable-sdk.md)). Typical use:

```go
import ks "github.com/dogukanturhal/keystone/pkg/sdk/keystone"

findings, _ := ks.Lint(ctx, "my-app", "v1", files)      // pure Go, no DB
result, _  := ks.Apply(ctx, pool, ks.ApplyOptions{...}) // runner + schema_migrations
snap, _    := ks.Inspect(ctx, pool, "public")            // structural introspection
plan, _    := ks.Diff(snap, desiredSpec, ks.DiffOptions{}) // declarative diff
```

Run the SDK's own tests with:

```
GOWORK=off go test ./pkg/sdk/keystone/                          # unit
GOWORK=off go test -tags=integration ./pkg/sdk/keystone/        # testcontainers round-trip
```

SDK surface is v1-stable; breaking changes require a module major
bump.

### Credential rotation + ESO / Vault

`DatabaseProvider.spec.adminCredentialsRef.secretName` points at a
Kubernetes Secret. Any mechanism that updates that Secret
(ExternalSecrets Operator syncing from Vault, SOPS, CSI-secret-store,
plain kubectl) is automatically observed by the
DatabaseProviderReconciler — see
[ADR 0024](docs/adrs/0024-credential-rotation-eso.md). On a
resourceVersion change:

- `status.credentialsObservedVersion` advances to the new RV
- `status.credentialsRotatedAt` records the timestamp
- Cached pools matching the provider's host+port are dropped so
  subsequent reconciles open fresh connections with the new creds
- A `CredentialsRotated` event fires for SIEM correlation

Sample wiring for Vault + ESO is at
`config/samples/keystone_v1alpha1_externalsecret_vault.yaml`. The
operator stays ESO-agnostic — only the Secret shape matters.

### Writing CEL policy expressions

`SchemaPolicy.spec.expressions` accepts a list of CEL rules the
admission webhook evaluates against every matched MigrationBundle
([ADR 0023](docs/adrs/0023-cel-policy-engine.md)). Each rule returns
a boolean — `true` means the rule fires.

Activation variables available inside expressions:

- `bundle`      — MigrationBundle metadata + spec (map-shaped)
- `findings`    — analyzer findings (list of maps with `rule`, `severity`, `file`, `line`, `message`)
- `strategy`    — shortcut for `bundle.spec.strategy`
- `labels`      — shortcut for `bundle.metadata.labels`
- `annotations` — shortcut for `bundle.metadata.annotations`
- `version`     — shortcut for `bundle.spec.version`

Example:

```yaml
spec:
  expressions:
    - name: require-jira-on-prod
      severity: Error
      message: "prod bundles require a jira ticket annotation"
      expression: |
        labels["tier"] == "prod" &&
        !("keystone.hexxlock.io/jira" in annotations)
    - name: warn-on-many-operations
      severity: Warning
      message: "bundle has > 10 operations; consider splitting"
      expression: size(bundle.spec.operations) > 10
```

Compile errors are **blocking** — a typo'd expression stops
admission until fixed. This is deliberate: a silently-disabled
rule is a policy regression authors won't catch until incident
time.

### Audit log streaming for SIEM

Every committed `AuditEntry` CR is mirrored to the manager's stdout
as one newline-delimited JSON record (see
[ADR 0022](docs/adrs/0022-audit-stdout-json-export.md)). Grafana
Alloy / Loki / Promtail sidecars scrape it into the cluster's SIEM
without extra config.

Disable for local dev / envtest with:

```
KEYSTONE_AUDIT_STDOUT=false
```

The emitted record uses ECS-inspired keys so default SIEM parsers
drop it into the right indices:

```json
{"@timestamp":"…","event":{"action":"reconcile","outcome":"success",…},
 "user":{"name":"…","groups":[…]},
 "keystone":{"verb":"…","resource":{…},"chain":{"self_hash":"…"}}}
```

`Before` / `After` JSON diffs are NOT streamed — they stay on the CR
and are retrieved on demand via `kubectl get auditentry/<name>
-o jsonpath='{.spec.before}'`. Keeps SIEM storage cost bounded.

### Browsing the schema registry

Every successful MigrationExecution automatically creates a
SchemaSnapshot (see
[ADR 0021](docs/adrs/0021-schema-registry-auto-snapshots.md)). The
snapshot name is deterministic: `auto-<bundle>-<version>-<schema>`.

Browse with the `keystonectl snapshot` subcommand:

```
# List every snapshot for one schema
keystonectl snapshot list users-acme -n keystone-system

# Diff two snapshots — returns a sorted change list
keystonectl snapshot diff auto-users-v1-acme auto-users-v2-acme -n keystone-system

# Dump a snapshot's structure (tables / columns / indexes / constraints)
keystonectl snapshot show auto-users-v2-acme -n keystone-system
```

Auto-snapshots retain for 365 days by default (`spec.retentionDays`);
manually-created snapshots override per-CR.

### Wave parallelism for pgroll bundles

A pgroll MigrationBundle with operations on multiple distinct tables
can run them concurrently by setting `spec.parallelism`
([ADR 0020](docs/adrs/0020-intra-bundle-wave-parallelism.md)):

```yaml
spec:
  strategy: pgroll-expand-contract
  parallelism: 4        # default 0 = strict serial; 1 = same as 0
  operations:
    - kind: add_column
      table: users
      addColumn: { name: email, type: text, nullable: true }
    - kind: add_column
      table: orders
      addColumn: { name: amount, type: numeric(10,2), nullable: true }
    # ... up to 16 ops
```

The scheduler groups ops into "waves" where each wave targets
distinct tables; waves run sequentially but ops within a wave run
concurrently up to `parallelism`. Same-table ops (e.g. two ALTER on
`users`) always serialise — the wave planner places them in
different waves.

Parallelism only applies to `strategy: pgroll-expand-contract`.
Versioned SQL bundles stay serial (Keystone doesn't parse SQL to
infer table deps).

### Aborting a pgroll migration

pgroll bundles can be rolled back from any non-terminal phase
(`Pending`, `Running`, `Expanding`, `Expanded`) by setting the abort
annotation on the matching MigrationExecution:

```
kubectl annotate migrationexecution <name> \
  keystone.hexxlock.io/abort=true
```

The next reconcile transitions the execution to `Aborting`, runs
per-operation abort handlers in reverse order (drop shadow columns,
triggers, NOT VALID constraints), and settles on `Aborted` (terminal).
Aborting during `Contracting` is refused — that phase drops real
columns and cannot be reversed. See
[ADR 0018](docs/adrs/0018-pgroll-expanded-ops-and-abort.md) for the
per-op rollback semantics.

### Plan-Gate-Apply

Each MigrationBundle fans out into one `MigrationPlan` per matched
`DatabaseSchema`. The plan's `spec.statements[]` shows the DDL that
will run; `spec.approved` (bool) gates the downstream execution.
See [ADR 0017](docs/adrs/0017-plan-gate-apply.md) for the design.

Default: `SchemaPolicy.spec.planApproval=Auto`. The plan reconciler
flips `spec.approved=true` immediately so pre-existing workflows
still run. To require explicit human approval, attach a SchemaPolicy
with `planApproval: Manual`:

```yaml
apiVersion: keystone.hexxlock.io/v1alpha1
kind: SchemaPolicy
metadata:
  name: prod-manual-approval
spec:
  targetSelector:
    matchLabels:
      tier: prod
  planApproval: Manual
```

When a Manual-mode plan is waiting, approve it with:

```
keystonectl plan approve <namespace>/<plan-name>
```

Or via `kubectl`:

```
kubectl patch migrationplan <name> -n <ns> \
  --type=merge -p '{"spec":{"approved":true}}'
```

The MigrationExecution reconciler watches plans and applies within
~1s of the approval flip — no waiting for the 60s poll fallback.

### Approval annotations

When a MigrationBundle matches a SchemaPolicy with `approvalPolicies`,
the webhook counts approver annotations and rejects bundles under
quorum. The annotation shape is fixed (see
[ADR 0016](docs/adrs/0016-approval-workflow-enforcement.md)):

```yaml
metadata:
  annotations:
    keystone.hexxlock.io/author: alice@example.com
    keystone.hexxlock.io/risk-tier: high
    keystone.hexxlock.io/approval.peer-review.bob: schema-reviewers
    keystone.hexxlock.io/approval.peer-review.carol: schema-reviewers
    keystone.hexxlock.io/approval.dba.dave: dba
```

- **Author** is required if any matched `ApprovalPolicy` has
  `disallowSelfApproval: true` — otherwise the self-approval guard
  becomes a no-op.
- **Risk tier** is optional; consumed by `appliesWhen.riskTier` to scope
  approval rules (e.g. "DBA review only on high-risk bundles").
- **Approval key** shape: `<prefix><policy-name>.<approver-id>`. The
  value is the approver's identity group (must match the policy's
  `fromGroups`). Approver-ids are de-duplicated — listing the same id
  twice counts once.

See `config/samples/keystone_v1alpha1_schemapolicy_tiers.yaml` for the
canonical dev / staging / prod example.

### Tests

Unit tests live next to the code they cover (`_test.go`). Integration tests
using controller-runtime's envtest live under `internal/controller/*_test.go`.
Any new controller behaviour needs an envtest; any new validation webhook
needs a table-driven test covering accept + reject + error-message cases.

## Branch naming

Use a type prefix so reviewers can see intent at a glance:

- `feat/<short-description>` — new user-visible capability
- `fix/<short-description>` — bug fix
- `docs/<short-description>` — docs, comments, examples only
- `chore/<short-description>` — tooling, CI, dependencies, refactors without
  behaviour change
- `test/<short-description>` — tests only

Keep the description short and hyphenated: `feat/declarative-drift-mode`,
not `feat/add-the-new-declarative-drift-detection-mode-i-was-thinking-about`.

## Commit message style

Conventional commits with a scope. The scope is the CRD group, subsystem, or
package affected — this matches what the CHANGELOG generator expects.

```
<type>(<scope>): <imperative summary under 72 chars>

<optional body wrapping at 72 chars, explaining *why* not *what*>

Signed-off-by: Your Name <your.email@example.com>
```

Types: `feat`, `fix`, `docs`, `chore`, `test`, `refactor`, `perf`, `build`,
`ci`. Mark breaking changes with `!` after the scope — `feat(api)!: rename
SchemaPolicy.spec.target` — and include a `BREAKING CHANGE:` footer.

Examples from the log:

```
feat(iam): add permission manifests and self-registration for all services
fix(ci): remove dangling example-service references from bump-manifest needs
docs(adr): 0005 pgroll lightweight subset
```

One logical change per commit. MRs with twelve unrelated commits will be
asked to rebase.

## Pull request workflow

Today the canonical repository is GitLab-hosted at
`<your-org>/platform-services`, and changes land via
GitLab Merge Requests. Once the project is published as
`github.com/hexxlock/keystone` the same workflow runs through GitHub Pull
Requests; everything below applies to both.

1. Fork (or create a topic branch if you're on the team).
2. Rebase onto the latest `master` before opening the MR, and again before
   every force-push. Merge commits from `master` back into feature branches
   are rejected.
3. Push and open the MR. The template will prompt for:
   - Summary of the change and motivation.
   - Linked issue or proposal.
   - Test evidence (output of `make verify`, or screenshots for docs).
   - Upgrade / migration notes if user-visible.
4. CI runs `dco-check`, `make verify`, SBOM + vuln-scan, and image build.
   All must be green before review.
5. Review. Expect at least one maintainer +1; contentious changes require two.
   See [`GOVERNANCE.md`](GOVERNANCE.md) for the decision model.
6. **Squash-merge.** The MR becomes a single commit on `master` with the MR
   title as its subject, so write the MR title like a commit message. The
   individual commits on the branch are the author's scratch space and are
   not preserved.

MRs sit idle for 14 days without author response get closed; reopen whenever
you're ready to continue.

## What not to contribute

A few categories of change need a conversation *before* code lands, not
during review:

- **New commercial-adjacent features.** Anything that overlaps the HexxLock
  Cloud product surface (hosted dashboard, SSO, managed pipelines, hosted
  drift detection) — email `maintainers@hexxlock.com` first so we can
  confirm whether it belongs in OSS, in cloud, or split between them. The
  goal is not gatekeeping; it is avoiding the case where a contributor
  spends two weeks on something we then can't merge for business reasons.
- **New database engines.** Keystone is PostgreSQL-first by
  [ADR 0003](docs/adrs/0003-postgresql-first-multi-engine-later.md). Engine
  plug-ins are welcome after the pluggable engine boundary lands in Phase 2;
  before that, engine-specific code should wait.
- **License changes.** Don't. See
  [ADR 0001](docs/adrs/0001-agplv3-license.md).
- **Vendored third-party code.** We depend via go modules, not vendoring.
- **Dependencies with non-permissive licenses.** Permissive (Apache-2.0,
  BSD, MIT, ISC, MPL-2.0) and AGPL-compatible only. If in doubt, ask.

## Where to ask questions

- **Bugs / feature requests**: GitHub Issues once the public repo is live.
  Until then, GitLab Issues on `platform/platform-services` with the label
  `component::keystone`.
- **Design discussions**: GitHub Discussions once the public repo is live.
  Until then, email `team@hexxlock.com`.
- **Security vulnerabilities**: do **not** open a public issue. See
  [`SECURITY.md`](SECURITY.md).
- **Conduct concerns**: see [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md) —
  enforcement contact is `conduct@hexxlock.com`.
- **Commercial / partnership**: `maintainers@hexxlock.com`.

[dco]: https://developercertificate.org/
