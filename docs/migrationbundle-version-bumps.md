# Updating a MigrationBundle version

A MigrationBundle name may be reused for successive releases. SQL is immutable
**per version**, not per Kubernetes object: update `spec.version` whenever its
resolved source changes. Use a fresh, increasing version; changing the version
does not permit rewriting SQL already recorded in the migration tracking table.

Prefer a new immutable ConfigMap for each release and update the source reference
and version together. If the same ConfigMap is updated first, the old version can
temporarily report `ContentHashChanged`; the subsequent version bump recovers
normally. Regenerate `keystone.sum` when SQL changes. Version bumps still undergo
integrity, lint and approval validation; `executionMode` remains immutable.

The controller pins `status.contentHash` and `status.contentHashVersion` together
before dispatching executions. A different hash at the pinned version is refused.
A new version establishes a new pin and restarts staged-rollout progress, including
soak timers. Previous MigrationExecutions remain available as audit evidence.

Updates with an unchanged spec (including finalizers, annotations, labels and
server-side dry-run comparisons) do not revalidate the external source. They do
not authorize changed SQL: the controller still detects same-version drift.

## Rolling upgrade from legacy status

Apply the additive MigrationBundle CRD **before** rolling out the new manager and
webhook. `contentHashVersion` is optional with no default; old stored objects
remain valid. Without the CRD update the API server may prune the new field.
Apply the updated manager ClusterRole too: event publishing now uses the current
`events.k8s.io` API and requires `create`, `patch` and `update` on its Events. Both
the Helm chart and generated RBAC manifest include these additive permissions.

For a legacy hash the controller recovers its version from the most recent owned
MigrationExecution with that hash, or the last successful `Planned` condition
(also available for bundles that matched zero schemas). This survives a failed
version bump overwriting `Ready` and `observedGeneration`. With no historical
evidence, it conservatively treats the hash as belonging to the current version.
An unchanged legacy source is pinned automatically on reconciliation. A wedged
legacy bump with retained execution/plan evidence also recovers automatically;
do not clear or replace hashes through the status subresource.

During the rollout, an old webhook replica can still refuse metadata updates;
wait until all webhook replicas run the new manager before retrying GitOps sync
or deletion. Do not revert the CRD while the new manager is running. If legacy
execution and plan evidence were removed, retain same-version protection rather
than guessing that a mismatching hash is a legitimate bump.
