# 0023. CEL-based policy expressions for SchemaPolicy

- **Status**: Accepted
- **Date**: 2026-04-19
- **Deciders**: HexxLock platform team

## Context

SchemaPolicy accreted hardcoded fields over A1-B4: MinLintLevel,
AllowedStrategies, MaxStatementsPerMigration, BlockedKeywords,
ApprovalPolicies, Integrity, PlanApproval. Each one answers a
specific governance question and ships as a typed CRD field +
dedicated controller logic.

The limit of this approach is visible at scale. Operators ask for
policies like:

- "Reject any prod bundle whose source ConfigMap name doesn't match
  `<module>-migrations-v*`"
- "Warn on pgroll bundles with more than 5 operations targeting the
  same table"
- "Require the `keystone.hexxlock.io/jira` annotation on every
  bundle labelled `change-risk=high`"
- "Forbid any bundle that drops a column and lacks a warning-level
  approval"

Each new hardcoded field means CRD bump + controller code + docs +
Go test + release cut. Flyway Enterprise solved this with a
policy-as-code engine; Atlas solved it with `atlas.pkl`. Both
converge on the same pattern: let operators write rules in a
well-known expression language and have the controller evaluate
them at admission time.

CEL (Common Expression Language) is the obvious choice:

- Same language Kubernetes already uses for
  `ValidatingAdmissionPolicy` and CRD `x-kubernetes-validations`.
  Operators who have written either already know the syntax.
- Sandboxed — no I/O, no unbounded loops, no arbitrary code
  execution. Safe to run at admission time on untrusted input.
- `cel-go` is already an indirect dep via the kube-apiserver client
  libraries; no new supply-chain surface.

## Decision

Add `SchemaPolicy.spec.expressions []CELRule` — a list of CEL rules
the admission webhook evaluates against every matched
MigrationBundle. Each rule declares:

```yaml
expressions:
  - name: require-jira-on-prod
    severity: Error
    message: "prod bundles require a jira ticket annotation"
    expression: |
      labels["tier"] == "prod" &&
      !("keystone.hexxlock.io/jira" in annotations)
```

Expressions must evaluate to a boolean; `true` means the rule
fires. Severity selects the admission action:

- **Warning** — appended to admission warnings; bundle admits.
- **Error** (default) — rejects admission with the rule's Message.

### Activation variables

The webhook exposes a stable set of variables. Adding new ones is
an ADR-bump event so policy authors' expressions don't silently
break.

| Variable      | Type                      | Source                                 |
|---------------|---------------------------|----------------------------------------|
| `bundle`      | `dyn` (map-shaped)        | MigrationBundle metadata + spec        |
| `findings`    | `list<dyn>`               | analyzer findings for the current apply |
| `strategy`    | `string`                  | `bundle.spec.strategy` shortcut        |
| `labels`      | `map<string,string>`      | `bundle.metadata.labels` shortcut      |
| `annotations` | `map<string,string>`      | `bundle.metadata.annotations` shortcut |
| `version`     | `string`                  | `bundle.spec.version` shortcut         |

**Deliberately absent**: `bundle.status.*`. Admission runs before
the status exists, and exposing it would let expressions gate on
reconcile-time state that can race with admission.

### Compilation + caching

`internal/policy/cel/evaluator.go` compiles each expression on
first use and caches the `cel.Program` by raw expression text.
Subsequent evaluations hit the cache — typical admission sees
sub-millisecond CEL cost for a 5-rule policy.

Compilation errors surface as blocking `Verdict{Kind: "compile"}`
entries. They do NOT silently disable — a typo'd expression stops
admission until fixed. The trade-off is correctness over
availability: a broken rule that silently passes is a policy
regression an author won't notice until the incident.

## Consequences

Easier: **operators govern without shipping controller code**. A
new rule is a YAML edit; it takes effect on the next reconcile.

Easier: **consistency with Kubernetes-native patterns**. Teams
that already write `ValidatingAdmissionPolicy` CRs apply the same
mental model to Keystone.

Easier: **expressions compose with existing fields**. CEL rules run
AFTER hardcoded gates (lint, integrity, approvals). An author can
keep the typed gates for the common case and add CEL only where
the typed surface is insufficient.

Harder: **expression safety as a moving target**. CEL itself is
sandboxed, but a policy author can still craft expensive regexes
or deeply nested `findings.exists(...)` loops. Mitigation: the
default `cel.NewEnv` sets reasonable cost caps; admission has its
own timeout from the webhook server.

Harder: **version-skew risk across activation variables**. Today's
policies reference `labels["tier"]`; if a future phase renames the
convention, existing rules silently stop matching. Mitigation:
activation variables are ADR-tracked; breaking changes require a
deprecation cycle.

Soft: **two policy languages in the same CRD**. Hardcoded fields
(AllowedStrategies, BlockedKeywords) and CEL expressions express
overlapping concerns. Documentation explicitly frames CEL as the
extension point for policies the typed fields don't cover — we do
NOT rewrite existing typed fields as CEL rules.

## Alternatives considered

- **Rego / OPA**. Rego is more expressive but ships a separate
  engine with a steeper learning curve. Kubernetes picked CEL for
  ValidatingAdmissionPolicy; matching that choice keeps the
  cognitive overhead low for operators. Rejected.

- **Jsonnet / Starlark expressions**. Neither has the Kubernetes-
  ecosystem momentum CEL has. Rejected.

- **Plain Go plugins (`.so` modules)**. Supply-chain nightmare,
  no sandboxing, no portability. Rejected immediately.

- **Extend hardcoded CRD fields instead of adding CEL**. The
  organic growth path — but three more years of this leads to a
  200-field SchemaPolicy. CEL bounds the CRD surface. Accepted as
  complementary: hardcoded fields stay for common cases; CEL covers
  the long tail.

- **Admission-only evaluation** (no reconcile-time re-run).
  Accepted for this phase — CEL rules don't gate reconcile the way
  the approval engine does, because CEL expressions run against
  admission-time state and re-running at reconcile would not catch
  new drift. If a future phase needs reconcile-time CEL, it's a
  straight copy of the evaluator wire-up.
