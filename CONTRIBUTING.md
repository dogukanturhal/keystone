# Contributing to Keystone

Thanks for helping. Keystone is an AGPL-3.0-or-later Kubernetes operator for
PostgreSQL schema lifecycle; the CRD types in `api/` are Apache-2.0.

## Before you start

- Skim the [ADRs](docs/adrs/README.md). They explain choices that are not
  obvious from the code (CRD-first, no `kube-rbac-proxy`, tier-aware
  rollouts, the pgroll subset).
- For a new CRD, a new field with behaviour, or a breaking change, open an
  issue first and describe the design. Small fixes, tests and docs can go
  straight to a pull request.
- Security issues do not belong in issues: see [SECURITY.md](SECURITY.md).

## Sign off your commits (DCO)

Every commit needs a `Signed-off-by` line, which certifies the
[Developer Certificate of Origin](https://developercertificate.org/):

```sh
git commit -s -m "controller: fix requeue on missing secret"
```

`make install-hooks` installs a local `commit-msg` hook that checks for the
trailer. To fix a missing sign-off: `git commit --amend -s` or
`git rebase --signoff main`.

## Development setup

You need the Go version in `go.mod`, Docker, and `make`. Tool binaries
(`controller-gen`, `golangci-lint`, `setup-envtest`) are installed into
`./bin` by the Makefile.

```sh
make generate        # CRDs, RBAC, deepcopy
make api-reference   # docs/api-reference.md
make lint
make test            # unit + envtest integration tests
make verify          # everything CI checks
```

`api/` is a separate Go module, so run `go test ./...` there as well.
Generated files (`zz_generated.*`, `config/crd/bases`, `config/rbac/role.yaml`,
`docs/api-reference.md`) must be regenerated and committed in the same pull
request; CI fails if they are stale.

Try changes on a cluster with [docs/getting-started.md](docs/getting-started.md).

## Pull requests

- Branch from `main`; keep the change focused.
- Commit messages: short imperative subject (`area: what changed`), body
  explains why.
- Add or update tests. Controller changes should have a test that fails
  without the change.
- Update `CHANGELOG.md` under `[Unreleased]` for anything user-visible.
- Describe what you ran. If you changed SQL generation or the engine, say
  which PostgreSQL versions you tried.

A maintainer will review; see [GOVERNANCE.md](GOVERNANCE.md) for how
decisions are made and [MAINTAINERS.md](MAINTAINERS.md) for who they are.

## Licensing of contributions

Contributions are licensed under the licence of the part of the tree they
touch: Apache-2.0 for `api/`, AGPL-3.0-or-later elsewhere. New source files
should carry an `SPDX-License-Identifier` header matching their directory.

## Conduct

Participation is covered by the [Code of Conduct](CODE_OF_CONDUCT.md).
