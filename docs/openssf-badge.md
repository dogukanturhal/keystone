# OpenSSF Best Practices Badge — Keystone Self-Assessment

SPDX-License-Identifier: AGPL-3.0-or-later

Keystone is an AGPL-3.0-or-later PostgreSQL schema operator.
This document pre-fills the answers for the OpenSSF Best Practices badge application
at https://www.bestpractices.dev/en/projects/new.

Once the GitHub mirror at `github.com/hexxlock/keystone` is live, apply for the badge
at that URL and paste these answers. The badge is free and publicly verifiable.

## Application Details

| Field | Value |
|---|---|
| Project URL | https://github.com/hexxlock/keystone (mirror; canonical: https://github.com/dogukanturhal/platform) |
| Project name | Keystone |
| Description | AGPL-3.0-or-later Kubernetes operator for PostgreSQL schema lifecycle management |
| License | AGPL-3.0-or-later |

## Self-Assessment Answers (Passing Level)

### Basics

**basic_license_location**: The LICENSE file is at the repository root.
Reference: `LICENSE` in `services/keystone/`.

**basic_oss_license**: AGPL-3.0-or-later. Every source file carries the SPDX header
`SPDX-License-Identifier: AGPL-3.0-or-later`.

**interact**: The project accepts bug reports and feature requests via GitLab Issues
at https://github.com/dogukanturhal/platform/issues (label: `keystone`).

**contribution**: Contribution process is documented in `CONTRIBUTING.md` at the
repository root. All commits must carry a `Signed-off-by:` DCO trailer (enforced via
`make install-hooks`).

### Change Control

**repo_public**: The GitLab repository is accessible to authenticated users.
GitHub mirror (planned): publicly accessible.

**repo_track**: Git, hosted at `github.com`. All changes are tracked and
every commit references an issue or MR.

**repo_interim**: Active development on the `master` branch. Release tags follow
`keystone/vX.Y.Z` semver.

**repo_distributed**: Mirrored to `github.com/hexxlock/keystone` (planned).

### Reporting

**vulnerability_report_process**: Documented in `SECURITY.md` at repository root.
Security findings can be reported via GitLab confidential issues or by emailing
`security@hexxlock.com`. Response SLA: 5 business days for acknowledgement.

### Quality

**build**: The project builds via `make build` (Go binary) and `docker build` (OCI image).
CI builds on every MR and merge to master via GitLab CI.

**build_common_tools**: Go (standard toolchain), Make, Docker, Kaniko (CI).

**build_floss_tools**: All build tools are OSS. No proprietary build systems.

**test**: Unit and integration tests run via `make test`. Coverage is reported on
every pipeline run.

**test_invocation**: `make test` (runs `go test -race -count=1 ./...` with envtest).

**test_continuous_integration**: Tests run on every MR pipeline (`keystone:test`-adjacent
via the workspace-level `test:` job in the CI configuration).

**run_tests_as_users**: Tests run without elevated privileges. envtest spins up a local
API server without cluster access.

### Security

**crypto_published**: Keystone does not implement custom cryptography.
All cryptographic operations delegate to the Go standard library and controller-runtime.

**crypto_call**: No custom cryptographic algorithms. TLS handled by controller-runtime
and Kubernetes API machinery.

**crypto_oss**: All cryptographic dependencies are OSS (Go standard library,
`crypto/tls`, `golang.org/x/crypto`).

**crypto_keylength**: RSA >= 2048 bits, ECDSA P-256 minimum, enforced by the Go TLS
stack defaults.

**crypto_working**: No deprecated algorithms in use. Go's `crypto/tls` enforces
TLS 1.2 minimum by default.

**delivery_mitm**: All CI artifacts are published to Harbor over TLS. Images are
cosign keyless-signed with provenance attestations (SLSA Build L3).
See `docs/verify-releases.md`.

**delivery_unsigned**: Releases are signed. See `docs/verify-releases.md`.

**vulnerabilities_critical_fixed**: HIGH and CRITICAL CVEs with published fixes block
the pipeline (`keystone:vuln-scan` with `--exit-code 1`). Response SLA: fix within
one sprint (2 weeks) of a published fix becoming available.

**vulnerabilities_fixed_60_days**: All known HIGH/CRITICAL CVEs are fixed within
60 days of disclosure per the security policy in `SECURITY.md`.

### Analysis

**static_analysis**: gosec (HIGH severity gating) and semgrep (advisory) run on every
MR pipeline. Results surface in the GitLab SAST Security Dashboard and MR widget.

**static_analysis_common_vulnerabilities**: gosec covers OWASP Top 10 and Go-specific
CWEs. semgrep covers `p/golang` and `p/security-audit` rulesets.

**static_analysis_fixed**: All HIGH gosec findings must be fixed or suppressed with a
documented justification (`//nolint:gosec // <reason>`) before merging.

**dynamic_analysis**: Fuzz testing is planned for Phase 12 via `go test -fuzz`.
Not yet active.

## Badge URL (activate after GitHub mirror is live)

```
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/<ID>/badge)](https://www.bestpractices.dev/projects/<ID>)
```

Replace `<ID>` with the project ID assigned by the bestpractices.dev application.
Add this badge to the top-level `README.md` and `services/keystone/README.md`.
