# Verifying releases

Tagged releases (`v*`) are built by the `Release` workflow in
`.github/workflows/release.yml`. It publishes:

- archives for `keystone-manager`, `keystonectl` and `keystoneadm`, a
  `checksums.txt`, and its cosign bundle (`checksums.txt.sigstore.json`);
- an SPDX SBOM next to each archive;
- a container image at `ghcr.io/dogukanturhal/keystone-manager`, signed with
  cosign (keyless) and carrying a build provenance attestation.

Signatures are keyless: the signing certificate identifies the GitHub Actions
workflow, and the signature is recorded in the public Sigstore transparency
log. Install [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) 2.x first.

## Release archives

```sh
TAG=v0.2.0   # replace with the release you downloaded
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/dogukanturhal/keystone/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

sha256sum --check --ignore-missing checksums.txt
```

## Container image

Look up the digest of the tag you want and pin to it:

```sh
docker buildx imagetools inspect ghcr.io/dogukanturhal/keystone-manager:v0.2.0 | grep Digest
```

Then verify the signature:

```sh
cosign verify ghcr.io/dogukanturhal/keystone-manager@sha256:<digest> \
  --certificate-identity-regexp '^https://github.com/dogukanturhal/keystone/\.github/workflows/release\.yml@refs/tags/v.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

And the build provenance attestation:

```sh
gh attestation verify oci://ghcr.io/dogukanturhal/keystone-manager@sha256:<digest> \
  --repo dogukanturhal/keystone
```

If verification fails, do not run the image; report it as described in
[SECURITY.md](../SECURITY.md).
