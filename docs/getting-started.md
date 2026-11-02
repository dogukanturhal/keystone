# Getting started

This walks through a local install on [kind](https://kind.sigs.k8s.io/): the
operator, a throwaway PostgreSQL, and one database, schema and migration
managed through custom resources. It takes about five minutes once the image
has built.

Everything here is for a test cluster. The files under `hack/quickstart/`
keep data in an `emptyDir`, hard-code an admin password and turn the
admission webhooks off.

## Prerequisites

- `docker`
- [`kind`](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- `kubectl` 1.27 or newer

## 1. Cluster and operator image

There is no published image before the first tagged release, so build it:

```sh
git clone https://github.com/dogukanturhal/keystone && cd keystone

kind create cluster --name keystone
docker build -t keystone-manager:dev .
kind load docker-image keystone-manager:dev --name keystone
```

## 2. Install the CRDs, the operator and PostgreSQL

```sh
kubectl apply --server-side -k config/crd
kubectl apply -k hack/quickstart
kubectl -n keystone-system rollout status deploy/postgres deploy/keystone-manager
```

CRDs go in first, with server-side apply, because large CRD schemas can
exceed the size limit of the client-side `last-applied` annotation.

`hack/quickstart/kustomization.yaml` takes `config/default` and changes four
things: one replica instead of two, the locally built image, `--enable-webhooks=false`
(the webhooks need a serving certificate from cert-manager), and no
certificate volume. `postgres.yaml` runs PostgreSQL 17 with a self-signed
certificate, because a `DatabaseProvider` does not accept `sslMode: disable`.

## 3. Create a database, a schema and a migration

```sh
kubectl apply -f hack/quickstart/sample.yaml
```

`sample.yaml` contains:

| Resource | What it does |
|---|---|
| `DatabaseProvider/quickstart-pg` | Points at the PostgreSQL service and the admin `Secret`. Cluster scoped. |
| `LogicalDatabase/quickstart` | Creates the database `quickstart` and its owner role. |
| `DatabaseSchema/app` | Creates schema `app` inside it; the `module: app` label is what the bundle selects. |
| `ConfigMap/app-v1-sql` | Two numbered `*.up.sql` files. |
| `MigrationBundle/app-v1` | Applies version `v1` of that SQL to every schema labelled `module: app`. |

Watch it settle:

```sh
kubectl -n keystone-system get logicaldatabase,databaseschema,migrationbundle
kubectl -n keystone-system get migrationplan,migrationexecution
```

The `LogicalDatabase` and `DatabaseSchema` should become ready, and the
bundle creates one plan and one execution for the `app` schema. A bundle that
has no `keystone.sum` file reports integrity `Missing`; that is expected here
and is covered in [ADR 0015](adrs/0015-migration-directory-integrity-sum.md).

## 4. Check the database

```sh
kubectl -n keystone-system exec deploy/postgres -- \
  psql -U postgres -d quickstart -c '\dt app.*' -c '\d app.notes'
```

You should see `app.notes` with `id`, `body` and `created_at`, plus the
migration tracking table (`schema_migrations` by default).

## 5. Ship a second migration

Add a file to the ConfigMap and bump the bundle version. Same-version content
changes are refused on purpose.

```sh
kubectl -n keystone-system patch configmap app-v1-sql --type merge -p \
  '{"data":{"003_add_author.up.sql":"ALTER TABLE app.notes ADD COLUMN IF NOT EXISTS author text;\n"}}'
kubectl -n keystone-system patch migrationbundle app-v1 --type merge -p \
  '{"spec":{"version":"v2"}}'
```

## Clean up

```sh
kind delete cluster --name keystone
```

## Troubleshooting

- `kubectl describe` on the failing resource shows the condition message and
  events. `kubectl -n keystone-system logs deploy/keystone-manager` has the
  controller logs.
- A `DatabaseProvider` stuck not-ready usually means the admin `Secret` is
  not in `keystone-system` (cross-namespace references are rejected) or the
  PostgreSQL pod is not ready yet.
- `docs/runbook.md` lists the common failure modes.

## Next steps

- Run it against a real cluster: [CloudNativePG](https://cloudnative-pg.io/)
  works well as the PostgreSQL side; point `DatabaseProvider.spec.host` at its
  `-rw` service and use `sslMode: verify-full`.
- Install with the Helm chart in `config/helm/keystone-operator` and enable
  the webhooks with cert-manager.
- Author migrations from a desired schema with `keystonectl`:
  [migration-from-code.md](migration-from-code.md).
- Staged rollouts across tenant tiers: [ADR 0006](adrs/0006-tier-aware-rollout-policy.md).
