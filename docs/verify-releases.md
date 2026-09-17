# Verifying Keystone Release Signatures and SLSA Provenance

SPDX-License-Identifier: AGPL-3.0-or-later

Keystone images published to `ghcr.io/dogukanturhal/keystone-manager` are:

1. Cosign keyless-signed (Fulcio OIDC, identity from `github.com`).
2. Attested with a SLSA v1.0 provenance predicate (`https://slsa.dev/provenance/v1`) via `cosign attest`.

All signatures and attestations are anchored to the public Sigstore Rekor transparency log.

## Prerequisites

Install `cosign` v2.4.1 or later:

```
curl -sSfL -o /usr/local/bin/cosign \
  https://github.com/sigstore/cosign/releases/download/v2.4.1/cosign-linux-amd64
chmod +x /usr/local/bin/cosign
cosign version
```

## Step 1 — Resolve the image digest

Always pin to a digest, not a tag, before verifying:

```sh
IMAGE=ghcr.io/dogukanturhal/keystone-manager:v0.1.0
DIGEST=$(cosign digest "${IMAGE}")
echo "Digest: ${DIGEST}"
# e.g. sha256:abcdef1234...
IMAGE_AT_DIGEST="${IMAGE%:*}@${DIGEST}"
```

## Step 2 — Verify the image signature

```sh
cosign verify \
  --certificate-identity-regexp 'https://github.com/dogukanturhal/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "${IMAGE_AT_DIGEST}"
```

A successful output looks like:

```
Verification for ghcr.io/dogukanturhal/keystone-manager@sha256:... --
The following checks were performed on each of these signatures:
  - The cosign claims were validated
  - Existence of the claims in the transparency log was verified
  - The code-signing certificate claims were validated
```

If verification fails with "no signatures found", the image was not built from a
tagged GitLab release pipeline. Do not use it in production.

## Step 3 — Verify the SLSA provenance attestation

```sh
cosign verify-attestation \
  --type slsaprovenance1 \
  --certificate-identity-regexp 'https://github.com/dogukanturhal/.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "${IMAGE_AT_DIGEST}" \
  | jq '.[0].payload | @base64d | fromjson'
```

The decoded payload will include the in-toto statement. Key fields to inspect:

| Field | Expected value |
|---|---|
| `predicateType` | `https://slsa.dev/provenance/v1` |
| `predicate.buildDefinition.buildType` | `https://gitlab.com/gitlab-org/gitlab-runner@v1` |
| `predicate.buildDefinition.externalParameters.source` | `<CI_REPOSITORY_URL>@<commit-sha>` |
| `predicate.buildDefinition.externalParameters.entrypoint` | `services/keystone/Dockerfile` |
| `predicate.runDetails.builder.id` | `https://github.com/dogukanturhal/platform/-/runners` |
| `predicate.runDetails.metadata.invocationId` | GitLab job URL |

## Step 4 — Inspect the full provenance JSON

The raw `provenance.json` file is archived as a CI artifact on every release pipeline
at `keystone-provenance-<short-sha>`. Download it from the GitLab CI pipeline page
under the `keystone:provenance` job artifacts.

## Automated verification in Kubernetes (Kyverno)

The platform ships `ClusterPolicy` resources under
`<gitops-repo>/kyverno/policies/` that enforce signature and
provenance verification for every image deployed into the cluster. No additional
consumer-side setup is needed for in-cluster workloads.

## Troubleshooting

**"Error: no matching signatures"** — The image was built outside of a `keystone/v*`
tag pipeline (e.g. a branch or MR build). Only tagged releases are signed and attested.

**"tlog entry not found"** — The Rekor transparency log may be temporarily unavailable.
Retry after a few minutes. Do not skip verification.

**digest mismatch** — Always fetch the digest fresh from the registry; do not hardcode it.
