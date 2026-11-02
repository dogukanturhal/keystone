# Security policy

Keystone holds database credentials and runs schema changes against
PostgreSQL servers. Please report vulnerabilities privately.

## Reporting a vulnerability

Use GitHub private vulnerability reporting: open the repository's **Security**
tab and choose **Report a vulnerability**
(<https://github.com/dogukanturhal/keystone/security/advisories/new>).

If you cannot use GitHub, email **dogukanturhal@hexxlock.com**. Do not post
details in a public issue, discussion or chat.

Please include:

- What the issue is and which Keystone version or image tag is affected.
- A way to reproduce it: the manifests, the cluster shape, what you saw and
  what you expected.
- Who can trigger it and what they gain.
- Whether you want to be credited.

We acknowledge reports within two business days.

## Disclosure

We follow a 90-day coordinated disclosure window from acknowledgement.

| Day | What happens |
|---|---|
| 0 | Report acknowledged, triage starts. |
| 0-7 | Severity assessed (CVSS v3.1); a CVE is requested if it applies. |
| 7-60 | Fix developed and reviewed privately. |
| ~90 | Patched release and advisory published together. |

A simple fix ships sooner. If a fix needs an API change we may agree a longer
window with you, never without telling you. If the issue is being exploited we
release immediately. Reporters are credited unless they ask not to be. There
is no bug bounty.

## Supported versions

Keystone is pre-1.0. Only the latest minor release receives security fixes.

## Scope

In scope: the operator (`keystone-manager`) and the images we publish, the
admission webhooks and validation logic, generated RBAC under `config/rbac/`,
the CLIs (`keystonectl`, `keystoneadm`), the Helm chart and Kustomize bases,
the `api/` Go types, and release artifacts (checksums, SBOM, signatures).

Out of scope: vulnerabilities in PostgreSQL, Kubernetes or other
dependencies (report those upstream; tell us if Keystone's use of them makes
it worse), denial of service by someone who already has cluster-admin, and
findings that need a malicious `DatabaseProvider` admin credential holder to
do what that credential already allows.

## Verifying releases

Release assets carry a checksum file signed with cosign keyless signing, and
an SBOM. Container images are signed and have a build provenance attestation.
See [docs/verify-releases.md](docs/verify-releases.md) for the commands.

Dependencies are watched by Dependabot and CodeQL, and `govulncheck` runs on
every pull request.
