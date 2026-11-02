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

## Keep pull requests small

This project has one maintainer who reads every line before merging. Large pull requests can't be reviewed properly, so they are not accepted.

- **One change per pull request.** One bug fix, one feature, or one refactor. Don't mix formatting or unrelated clean-ups into it.
- **Size limit: 400 changed lines and 15 files** (additions plus deletions; `go.sum`, `package-lock.json`, `Cargo.lock` and generated files don't count). Aim for under 200. An automated check fails pull requests over the limit.
- **Bigger change? Open an issue first.** Describe the plan and we'll agree on how to split it into a series of small pull requests that each build and pass tests on their own.
- Exceptions (for example, a large test fixture) only with a maintainer's agreement in the issue.

## AI-assisted contributions

You may use AI tools, under these conditions:

- **Say so** in the pull request description, and name what the tool did (for example "tests drafted with an AI assistant, then reviewed and edited").
- **You are the author.** You must understand every line and be able to explain and change it during review. "The tool wrote it" is not an answer to a review question.
- **Same rules as everyone:** the size limit above, tests, and the licence/sign-off requirements. You confirm you have the right to submit the code.
- Pull requests that look generated without review (code that doesn't fit the codebase, invented APIs, a PR description that doesn't match the diff, or no answers to review questions) are closed without detailed review.

## How your contribution lands

This GitHub repository is a mirror of our main development repository. When a pull request is merged here, we import it there with your authorship and sign-off preserved, and it comes back in the next sync. You don't need to do anything extra.

## Licensing of contributions

Contributions are licensed under the licence of the part of the tree they
touch: Apache-2.0 for `api/`, AGPL-3.0-or-later elsewhere. New source files
should carry an `SPDX-License-Identifier` header matching their directory.

## Conduct

Participation is covered by the [Code of Conduct](CODE_OF_CONDUCT.md).
