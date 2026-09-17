# 0004. No `kube-rbac-proxy` sidecar for metrics

- **Status**: Accepted
- **Date**: 2026-04-16
- **Deciders**: HexxLock platform team

## Context

The Kubebuilder-generated operator template historically deploys
`kube-rbac-proxy` as a sidecar container to gate `/metrics` behind RBAC.
That image is being retired (`gcr.io/kubebuilder/kube-rbac-proxy` images are
unavailable after early 2025), and controller-runtime v0.18+ ships native
`WithAuthenticationAndAuthorization` support that does the same job without
a sidecar.

## Decision

The Keystone manager serves `/metrics` on `:8443` with TLS and
controller-runtime's built-in authentication/authorisation filter. The
authentication filter calls `TokenReview`; the authorisation filter calls
`SubjectAccessReview`. Both are native Kubernetes APIs — no sidecar needed.

Prometheus-Operator ServiceMonitor scrapes it with a ServiceAccount token;
the RBAC binding `keystone-manager-auth-delegator` gives the manager the
right to call TokenReview/SubjectAccessReview.

## Consequences

Easier: one fewer container per manager Pod. Simpler image supply chain — we
manage only our own image, not a sidecar's.

Easier: memory footprint is lower (no second process + its runtime).

Easier: security posture is stronger — fewer third-party images, fewer CVE
alerts to triage.

Harder: upgrading controller-runtime to a version below v0.18 would break the
metrics gate. We pin the dependency and CI catches downgrades.

Harder: some Prometheus-Operator-style scrape configs assume kube-rbac-proxy
listens on `:8443` with a specific TLS cert. Our ServiceMonitor uses
`insecureSkipVerify: true` because the manager serves a self-signed cert per
Pod — the security gate is the authn/authz filter, not TLS server identity.

## Alternatives considered

- **Keep `kube-rbac-proxy` sidecar**: familiar to existing ops, but the image
  source is being retired. Moving target.
- **No auth on metrics at all**: trivially exposes reconcile state to any Pod
  with network access to the manager. Unacceptable.
- **mTLS via Linkerd**: covers pod-to-pod but not user-to-pod (e.g. an
  operator running `kubectl port-forward`). The `WithAuthenticationAndAuthorization`
  filter covers both cases.
