# The release train — three modules, two repositories

Keystone's schema vocabulary is spread across three Go modules that
depend on each other in one direction:

```
github.com/dogukanturhal/keystone/api        (keystone repo, api/go.mod)
        ↑
github.com/dogukanturhal/keystone-sdk/go     (keystone-sdk repo, go/go.mod)
        ↑
github.com/dogukanturhal/keystone            (keystone repo, ./go.mod)
```

The split is deliberate: `api/` is its own module precisely so the SDK can
import the CRD types without forming a require-cycle back onto the
operator. The cost is that a change to the schema vocabulary — a new
`DesiredColumn` field, say — has to travel the chain in order.

This page is how to make that cheap.

---

## Day to day: pseudo-versions, not tags

**Do not tag a module just to unblock the next one.** Go has a mechanism
for exactly this. From the [Go modules
reference](https://go.dev/ref/mod#pseudo-versions):

> Pseudo-versions may refer to revisions for which no semantic version
> tags are available. They may be used to test commits before creating
> version tags, for example, on a development branch.

So once the dependency's MR is **merged and pushed**, the dependent repo
pins it by branch and Go resolves it to a canonical version:

```console
$ go get github.com/dogukanturhal/keystone-sdk/go@main
go: added github.com/dogukanturhal/keystone-sdk/go v0.2.5-0.20260801T142233-4f7212ec9a01
```

That is a real, immutable, proxy-served version — not a floating
reference. `main` never reaches `go.mod`; the go command rewrites it,
because "canonical versions are required outside the main module".

The keystone repo wraps this:

```console
$ make sync-sdk                    # pin to the SDK's latest pushed main
$ make sync-sdk SDK_REF=go/v0.3.0  # or to a release tag
```

### Order of merges

For a change that touches the schema vocabulary:

1. **keystone** — merge the `api/` change. (Same repo as the operator, but
   a separate module, so it publishes independently.)
2. **keystone-sdk** — `go get …/keystone/api@main`, then merge.
3. **keystone** — `make sync-sdk`, then merge.

Each MR is green on its own. Nothing is tagged.

A change that touches only the SDK skips step 1. A change that touches
only the operator skips 1 and 2.

### Why the build fails before you do this

`GOWORK=off go build ./...` — which is what CI runs, and what the Makefile
forces — resolves the SDK from the proxy at whatever `go.mod` pins. Code
that uses a symbol added in an unreleased SDK commit therefore fails with
`undefined: …`. That error means "the train hasn't reached this car yet",
not "your code is wrong".

Locally, `go.work` papers over it by using the sibling checkout. That
divergence is intentional and is why `GOWORK := off` is set in the
Makefile — see [go-workspace-note.md](go-workspace-note.md). Reproduce CI
with `GOWORK=off` before pushing.

---

## Release time: the tag train

Tags are for consumers outside this train (other services embedding the
SDK, the `keystone-ef` NuGet package, image builds). Cut them when the
surface is stable, not on every merge.

Tag in dependency order, verifying each step before moving on:

```console
# 1. api  — tag lives in the keystone repo, path-prefixed per
#    https://go.dev/doc/modules/managing-source (subdirectory modules
#    must prefix the tag with the module's subdirectory).
git -C keystone tag api/v0.1.3 && git -C keystone push origin api/v0.1.3

# 2. sdk  — repin to the tagged api, verify, then tag.
cd keystone-sdk/go
go get github.com/dogukanturhal/keystone/api@api/v0.1.3
go mod tidy && go build ./... && go test ./...
git commit -am "chore(deps): pin keystone/api v0.1.3" && git push
git tag go/v0.3.0 && git push origin go/v0.3.0
#    Slash, not hyphen: Go's path-suffix rule makes `go-vX.Y.Z`
#    silently unresolvable. The CI comment in the CI configuration has the detail.

# 3. operator — repin to the tagged SDK, verify, merge.
cd keystone
make sync-sdk SDK_REF=go/v0.3.0
GOWORK=off go build ./... && GOWORK=off go test ./...
```

The `.NET` packages ride the same train on a `dotnet-vX.Y.Z` tag, which
packs every packable project — `HexxLock.Keystone.Sdk`,
`…​.EntityFrameworkCore`, and the `keystone-ef` tool.

### Verify before tagging

Both publish jobs re-run the full suite at the tagged commit, so a broken
tag fails loudly rather than silently serving bad code from the proxy.
There is no un-tagging once the proxy has cached a version — it is
immutable. Check `go build ./... && go test ./...` first.

---

## Should the api/SDK boundary be collapsed? — No. Measured 2026-08-01.

Go's [official guidance](https://go.dev/doc/modules/managing-source) says:

> You publish code in a module when the code should be versioned
> independently from code in other modules.

`keystone/api` and `keystone-sdk/go` are *not* versioned independently —
every schema-vocabulary change moves both — which reads like a mis-drawn
boundary and suggests collapsing them so the train loses a car. That was
proposed, then measured, and the measurement says don't.

**What the SDK actually imports from `keystone/api`**: 443 references
across 24 non-test files. Most are the schema vocabulary
(`SchemaDefinitionSpec`, `DesiredColumn`, `DesiredTable`), which could in
principle move. But `keystone/sdk.go`, `keystone/enroll.go` and
`migration/oci.go` use the **custom resources themselves** —
`SchemaDefinition`, `DatabaseSchema`, `LogicalDatabase`,
`LogicalDatabaseList` — against a controller-runtime client. The SDK is
not merely a schema library that happens to share some structs; it is a
Kubernetes client for Keystone's CRs, and a client needs its API types.

**What `keystone/api` costs**: `k8s.io/apimachinery`, `k8s.io/api`,
`sigs.k8s.io/controller-runtime` and their transitive set. No operator
machinery, no controllers, no database drivers. It is the same
api-submodule pattern cert-manager and CloudNativePG publish, and it is
exactly the dependency set a CR client needs anyway.

So the two modules are coupled because the SDK genuinely depends on the
API, not because the boundary is in the wrong place. Collapsing it would
mean either duplicating the CR types in the SDK or splitting the SDK into
schema-vocabulary and Kubernetes-client modules — which *adds* a module
rather than removing one. The current shape is right; the train is the
price of the SDK being a real client, and pseudo-versions already reduce
that price to merge ordering.

Revisit only if the SDK stops speaking to the Kubernetes API.

If a future change does put several co-versioned modules in **one**
repository, adopt tooling rather than a bespoke script: OpenTelemetry-Go's
[multimod](https://github.com/open-telemetry/opentelemetry-go-build-tools/tree/main/multimod)
is the reference implementation, defining *module sets* in a
`versions.yaml` that are "incremented in lockstep" with `verify`,
`prerelease` and `tag` commands. It operates within a single repository,
so it does not fit the current two-repo topology.
