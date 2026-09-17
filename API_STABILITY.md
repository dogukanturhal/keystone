# Keystone API stability commitment

This document is the contract between Keystone maintainers and
operators about how our CRD APIs evolve. It is written before any
production adoption so expectations are set explicitly, not
retrofitted later.

## The rules

Keystone's CRDs follow the Kubernetes deprecation policy:
<https://kubernetes.io/docs/reference/using-api/deprecation-policy/>

| Maturity | Lifetime guarantee | Who should use it |
|---|---|---|
| **`v1alpha1`** (today) | **May be removed in any release without notice.** Breaking changes land in the same version. | Early adopters evaluating; not production |
| **`v1beta1`** (target v0.3) | **Deprecated at most 9 months or 3 minor releases after introduction, whichever is longer. Not removed for 9 months / 3 minors after deprecation.** Conversion webhooks keep old and new served simultaneously. | Production with an understood upgrade cadence |
| **`v1`** (target v1.0) | **Not removed within a major version.** Any schema change is additive or deprecated with a forever-support commitment inside the v-major. | Indefinite production |

## Roadmap

- **v0.1 → v0.2** — `v1alpha1` only. Breaking schema changes are
  documented in release notes and require CR re-application. A
  storage-version migration script ships with each release when
  needed. This is the "alpha is alpha" window; adopters running
  production here accept manual intervention on upgrade.

- **v0.3** — Introduce `v1beta1` for every CRD. Conversion webhook
  (`internal/webhook/conversion_*`) provides round-trip-lossless
  translation between `v1alpha1` and `v1beta1`. Both served; v1beta1
  becomes `storage: true`. `v1alpha1` enters deprecation countdown —
  served for at least 3 subsequent minors, then dropped.

- **v0.5** — `v1alpha1` removed. `v1beta1` only, still with conversion
  framework in place for future `v1`.

- **v1.0** — `v1` is the stable commitment. From here, any schema
  change that would remove a field or tighten validation goes through
  deprecation → warning (`Warning` header + `apiserver_requested_deprecated_apis`
  metric) → removal only in a `v2` major.

## What "breaking change" means

For `v1alpha1` specifically, we may, in any release:

- Remove a field from an existing Spec or Status
- Tighten a field's validation (narrower enum, stricter regex, lower
  MaxLength)
- Rename a field (treated as remove-old + add-new)
- Change the semantics of a field without renaming it
- Remove a CRD kind

For `v1beta1` and `v1`, none of the above happens without a
deprecation window. Field-level annotations
(`// Deprecated: use X instead.`) appear 9 months before removal, and
every deprecated field emits a `Warning:` in the apiserver response
headers when read.

## Conversion webhook

A conversion webhook scaffold ships at
`internal/webhook/conversion_scaffold.go` ahead of any real version
bump. Today it's an identity converter — all versions are
`v1alpha1`, so the trivial passthrough compiles. When `v1beta1` lands,
the same file grows per-kind conversion logic and round-trip tests.

The scaffold exists *now* because adding a conversion webhook after
customers exist is the expensive, risky path. Having it wired into
the CRDs (with `conversion: { strategy: None }` → flip to `Webhook`
when the first translation ships) is preparation, not premature
optimisation.

## Printer columns + observed fields

`status.observedGeneration` is present on every CRD. Consumers use it
to detect stale status (`.metadata.generation > .status.observedGeneration`
means the reconciler hasn't seen the latest spec).

`status.conditions` follows the standard Kubernetes pattern — the
`Ready` condition is authoritative for "is this CR doing what it
should right now". Other conditions (`Available`, `Progressing`,
`Degraded`) are informational and may come and go across versions
without a deprecation window. `Ready` is committed indefinitely.

Every CRD has printer columns for: Ready, primary identifying
field (name / version / tier), and `metadata.creationTimestamp` as
Age. These are stable across minor versions.

## Testing conversion

Before `v1beta1` lands, every kind grows a round-trip test at
`api/v1beta1/conversion_test.go` that asserts:

```
original_v1alpha1 → v1beta1 → v1alpha1 == original_v1alpha1
```

and the equivalent in the other direction. The conversion webhook
fails admission if a round-trip is lossy; CI catches that before
publishing.

## Migration tooling

`keystonectl migrate-versions <from> <to>` is planned for v0.3 (the
same release that introduces `v1beta1`). It reads all CRs in a
namespace, round-trips through the conversion webhook, and re-applies
— giving operators a one-command path to force-migrate storage after
the webhook is live.

## Promise

We don't ship `v1beta1` until all of the following are true:

- At least one external adopter (see `ADOPTERS.md`) has exercised
  `v1alpha1` in production
- Every CRD has a round-trip-lossless conversion tested in CI
- Every CRD has explicit deprecation markers on any field we plan to
  remove in `v1`
- This document has been updated to reflect lessons from alpha usage

We don't ship `v1` until all of the following are true:

- Two minor releases of `v1beta1` have shipped
- No schema changes are pending from the operator roadmap
- An external pentest has been completed (see `docs/pentest-scope.md`)
- The conversion webhook has handled one real upgrade without
  operator intervention

## Updating this document

Changes to the rules or roadmap require an ADR (see `docs/adrs/`) and
update to `GOVERNANCE.md §Decision-making`. This document tracks the
current state; the ADR records how we got here.

Current status: **v0.1.x, `v1alpha1` only, 13 CRDs served.**
