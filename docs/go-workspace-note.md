# Why Keystone is not in `go.work`

Keystone deliberately stays out of the repo-root `go.work` file. The older
services in this monorepo pin `google.golang.org/genproto` at the 2020-era
single-package layout (`v0.0.0-20200423170343-7949de9c1215`). Modern
`sigs.k8s.io/controller-runtime` (which Keystone depends on) requires the
2023+ split layout (`google.golang.org/genproto/googleapis/api`,
`.../googleapis/rpc`, etc.). When both layouts are in the same workspace,
the Go toolchain raises an ambiguous-import error.

## How to develop on Keystone

Run all `go` commands from inside `services/keystone/`. The local `go.mod`
is self-contained:

```bash
cd services/keystone
go build ./...
go test ./...
make verify
```

If you need workspace mode for IDE tooling (gopls cross-module navigation),
either:

1. **Open just this directory in your editor** so gopls reads the local
   `go.mod` directly, or
2. **Use the `keystone-only` workspace stub**:

   ```bash
   GOWORK=$PWD/go.work.local make build
   ```

   where `go.work.local` lists only `./` (Keystone) plus `../../shared/*`.

## When can Keystone re-join `go.work`?

When the legacy services upgrade their `google.golang.org/genproto`
dependencies to the split layout. That is tracked in the broader monorepo
modernisation work and is not on the Keystone roadmap.

## SSO of truth

`services/keystone/go.mod` is authoritative. Do not edit `replace`
directives there to point at workspace siblings — Keystone must stay
buildable as a standalone module so the OSS release pipeline can produce
a self-contained tarball.
