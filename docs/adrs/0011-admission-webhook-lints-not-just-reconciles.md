# 0011. Lint at admission, not just at reconcile

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

Phase 11 shipped 21 analyzers that run during MigrationBundle
reconcile. They populate `status.lintFindings` and refuse to fan out
MigrationExecutions when any finding is severity=error.

That's useful, but it runs *after* the CR is persisted. Practical
consequences:

- A bad bundle sits in etcd until someone `kubectl delete`s it. Its
  name is taken; a retry under the same name fails.
- The admission chain is the bouncer. If lint is only at reconcile, a
  rejected-at-reconcile bundle still passes admission — so things like
  GitOps audit logs, Kyverno policy chains, and `kubectl apply` exit
  codes report "accepted" for something the operator will reject
  moments later.
- Operators debug via `kubectl describe`, which shows conditions but
  not the CR-creation-time rejection path that a webhook gives them.

## Decision

Add a validating webhook (`internal/webhook/migrationbundle_webhook.go`)
that runs the same analyzer registry against the bundle at admission
time. Register via kubebuilder annotations so the
`ValidatingWebhookConfiguration` is generated into
`config/webhook/manifests.yaml`.

Same for `SchemaDefinition` — structural invariants the CRD schema
can't express (unique column names, FK cardinality, PK conflicts) go
through a paired validator.

The reconciler-time lint pass stays. It's the safety net for:

- Bundles that referenced a ConfigMap not yet applied at admission
  (webhook warns, doesn't reject — ordering tolerance).
- Bundles created by operators that bypassed the webhook (disabled via
  `--enable-webhooks=false`, which is strictly for local envtest).
- Any future rule-set extension that webhook admission hasn't picked up.

## Consequences

Easier: kubectl rejection surfaces with a clear error string up front.
GitOps `Application` sync status shows admission errors instead of
"synced then Ready=False". CI that applies manifests against a
cluster sees the rejection immediately, not five minutes later in a
follow-up describe.

Easier: security narrative. Admission is the point where RBAC applies
— a compromised ServiceAccount that can `create migrationbundles` but
shouldn't ship destructive SQL is now blocked by the same rule the
reconciler would have caught later.

Harder: two places to keep in sync. We mitigate by having the webhook
call into `analyze.DefaultRegistry()` — exact same rules, no drift.

Harder: webhook needs TLS certs and a Service. Cert-manager provides
them (same pattern Linkerd uses — see
internal engineering notes). The
`config/webhook/` kustomize bundle references the cert; operators add
cert-manager as a bootstrap dep (already in the platform stack).

## Alternatives considered

- **Reconcile-time only**: cheaper to ship, worse UX. Rejected.
- **Webhook without reconciler pass**: brittle — a bad SQL file can
  appear between admission and reconcile (ConfigMap applied out of
  order). Rejected.
- **Webhook runs a reduced rule set** (e.g., only the
  no-drop-table family): gains little, loses the "one source of
  truth" property. Rejected.
- **Kyverno-based validation** instead of a custom webhook: Kyverno
  can't load the analyzer registry (different runtime), so it'd be a
  reimplementation. Kyverno keeps its current role — coarse policy
  checks that don't need parser state (e.g. "reject any bundle in
  namespace X without label Y").
