# 0010. Defence-in-depth identifier validation

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

Every table/column/schema/role/index name flows through Keystone from:

1. User-authored `MigrationBundle` / `SchemaDefinition` CR spec
2. Admission webhook (CRD validation + Kyverno policies)
3. Reconciler
4. DDL generation in `internal/postgres`

PostgreSQL identifier rules (`[a-z_][a-z0-9_]*`, max 63 chars) are enforced
at multiple layers. A single-layer enforcement is a single point of failure:
if admission webhooks are disabled (cluster-wide emergency) or bypassed (a
CR applied via a compromised ServiceAccount), a malicious identifier could
reach the DDL generator.

Parameter binding (the normal SQL injection mitigation) doesn't apply to
DDL identifiers — `CREATE TABLE $1` isn't valid SQL. Identifiers land in
the DDL string directly.

## Decision

Every identifier is validated at **three** layers:

1. **API level**: `kubebuilder:validation:Pattern=^[a-z_][a-z0-9_]*$` on
   every identifier-typed field in the CRD schema. OpenAPI validation
   rejects bad names at `kubectl apply` time.

2. **Admission level**: Kyverno ClusterPolicies re-enforce the same patterns
   (ADR 0004's policy package). Defence if the CRD schema was patched out
   or validation was disabled.

3. **Controller level**: `internal/postgres/quote.go::ValidateIdentifier`
   is called immediately before every DDL emission. Defence if the CR was
   applied via a compromised webhook or admission was bypassed.

Additionally, every identifier passes through `pg.QuoteIdentifier` — which
handles embedded double quotes correctly — before being spliced into DDL.
So even if validation somehow accepted a weird name, the quoting prevents
SQL breakout.

31 unit tests in `internal/postgres/quote_test.go` cover semicolons,
backticks, NUL bytes, Unicode tricks, oversize, and other known bypass
vectors.

## Consequences

Easier: security review is concentrated in one package
(`internal/postgres/quote.go`). Auditors inspect 50 LOC of regex + 31 tests,
not every DDL site across the codebase.

Easier: adding a new DDL-generating helper is mechanical — call
`ValidateIdentifier` + `QuoteIdentifier` and move on.

Harder: developers sometimes forget to call `ValidateIdentifier` when
adding new DDL paths. Mitigated by code review (every PR touching
`internal/postgres/` or `internal/migration/pgroll/` is reviewed by a
security-aware maintainer) and by staticcheck hooks flagging direct
string-concatenation into DDL.

Harder: the validation pattern is slightly more restrictive than PG's own
identifier grammar (we refuse uppercase, hyphens, and double-quoted
identifiers with special characters, even though PG permits them with
careful quoting). We consider this a feature — uniformity beats flexibility.

## Alternatives considered

- **Single-layer validation** (admission only): faster to implement but a
  single bypass loses the whole defence. The cluster scan memory lists
  known admission-bypass scenarios (stale webhook CAs, disabled
  validation).
- **Parameterised DDL** (e.g. `CREATE TABLE "$1"` with some wrapper
  library): PostgreSQL doesn't support this, and no standard exists. Any
  library claiming to provide it is string-templating under the hood —
  same defence posture, more opacity.
- **Stricter identifier set** (reject common reserved words): Phase 11.3's
  `no-reserved-identifier` analyzer is the soft enforcement. Hard
  enforcement at the quote layer would break too many legitimate cases
  (see GORM's default struct → table convention producing `user`, which
  is reserved).
