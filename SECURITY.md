# Security policy

Keystone is infrastructure software that holds database credentials and
executes schema changes against production PostgreSQL clusters. We take
vulnerability reports seriously and want them in a private channel before
anyone else sees them.

## Reporting a vulnerability

Email **security@hexxlock.com** with:

- A description of the issue.
- The Keystone version(s) affected (`keystonectl version` or the image tag).
- A reproduction — CRD manifest, cluster shape, what you observed, what you
  expected.
- Your assessment of impact (who can trigger it, what they gain).
- Whether you are willing to be credited publicly in the advisory.

Please encrypt anything sensitive. Our PGP key is published at
`https://hexxlock.com/.well-known/security.asc`
(fingerprint also linked from the HexxLock website under `/security`). If you
cannot encrypt, email us first to arrange a secure channel — do **not** post
exploit details to a public issue tracker, mailing list, social media, or
chat while waiting.

We acknowledge reports within **two business days**. If you have not heard
from us in that window, resend — mail filters are the usual culprit.

## Disclosure timeline

We operate on a **90-day responsible disclosure** window from the moment we
acknowledge the report:

| Day | What happens |
|---|---|
| 0   | Report acknowledged; maintainer triage starts. |
| 0–7 | Severity assessed (CVSS v3.1); CVE requested if applicable. |
| 7–60| Fix developed, reviewed, tested under embargo. |
| 60–75 | Patch prepared; downstream operators notified under embargo at T-14. |
| ~90 | Coordinated public release: patched version, advisory, CVE. |

If a fix is straightforward we release sooner. If the fix needs an
API change or carries migration risk we may negotiate an extension with the
reporter, but never unilaterally. If exploitation in the wild is observed,
we release immediately and skip the remaining embargo.

Reporters are credited in the advisory by name or handle unless they ask not
to be. No bug bounty is offered at this stage.

## Scope

**In scope** — we treat these as vulnerabilities:

- The operator binary (`keystone-manager`) and any container image we publish.
- The CRD Go types (`api/`), and the client SDKs in the keystone-sdk repository.
- Admission webhooks and validation logic.
- Generated RBAC manifests (anything under `config/rbac/`).
- The CLI (`keystonectl`) and its embedded credentials handling.
- The Helm chart and Kustomize bases we ship.
- Supply-chain artefacts we publish: the SBOM, the cosign signature, the
  Harbor image manifest.

**Out of scope** — please do not report these as security issues, but
regular bugs are welcome:

- Misconfiguration of the user's own cluster (missing NetworkPolicies,
  permissive RBAC granted by the user, exposed kubeconfigs).
- PostgreSQL misconfiguration (weak passwords on upstream DB, unencrypted
  traffic, superuser credentials handed to non-admin tenants) — **unless**
  Keystone itself can be coerced into creating the misconfiguration.
- Denial of service against a single reconcile loop via deliberately
  oversized CRs; submit as a performance issue.
- Vulnerabilities in dependencies that do not affect Keystone's attack
  surface (report upstream; we will track and bump).
- Social engineering, physical access, and anything requiring a
  pre-compromised cluster-admin.

If you are not sure, report it anyway and let us sort scope.

## Supported versions

Keystone is pre-1.0. Only the **latest minor release** receives security
fixes. Once 1.0 ships we will extend to the latest two minors; the current
policy reflects the reality that pre-1.0 users are expected to track
releases.

| Version | Supported? |
|---|---|
| 0.1.x (latest minor) | Yes — fixes published as 0.1.N+1 |
| 0.0.x and earlier    | No  — upgrade to 0.1.x |

Support status is re-evaluated at each minor release and noted in the
release announcement.

## Supply-chain assurances

Every tagged release publishes:

- **SBOM** — SPDX JSON at `bin/keystone-sbom.spdx.json`, generated with
  `syft` (see `make sbom`). Attached to the GitHub / GitLab release and to
  the container image as an OCI referrer.
- **Vulnerability scan** — `trivy fs` + `trivy image` with
  `--severity HIGH,CRITICAL --ignore-unfixed --exit-code 1`. The release
  pipeline fails if either reports a fixable HIGH or CRITICAL.
- **Cosign signature** — keyless, OIDC-signed via the GitLab CI identity.
  Verify with:

  ```
  cosign verify ghcr.io/dogukanturhal/keystone-manager:<tag> \
    --certificate-identity-regexp 'https://github.com/dogukanturhal/.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

- **Pinned tool versions** — `controller-gen`, `kustomize`, `envtest`,
  `golangci-lint`, `syft`, `trivy`, `cosign` are all version-pinned in the
  `Makefile` and installed under `./bin/`. CI rejects drift.
- **DCO** — every commit is signed off (see `CONTRIBUTING.md`). Unsigned
  commits cannot land on `master`.

These are verification inputs for anyone shipping Keystone into regulated
environments (FedRAMP, SOC 2, PCI) and for the in-toto / SLSA L3 attestation
roadmap.
