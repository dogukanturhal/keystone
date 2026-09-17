# Migrations from code — ORM integration

Keystone authors migrations from a *desired schema*. That schema can come
from a YAML `SchemaDefinition`, a `.sql` file, a live database — or from
your ORM's model, which is what this page is about.

The mechanism is one scheme: **`exec://`**. A provider program prints the
desired schema as PostgreSQL DDL on stdout; Keystone applies that DDL to a
throwaway schema on a dev database, inspects it back with the same
inspector that reads production, and diffs it against your migration
directory. Keystone therefore knows nothing about any ORM's model layer —
it only knows how to read a schema.

That means **any ORM that can render `CREATE` statements already works**,
whether or not we ship a provider for it.

---

## The loop

```console
$ keystonectl migrate diff add_description
authored migrations/002_add_description.up.sql
         migrations/002_add_description.down.sql
1 statement(s), 0 destructive

$ cat migrations/002_add_description.up.sql
ALTER TABLE "Todos" ADD COLUMN IF NOT EXISTS "Description" text;
```

Change an entity, run one command, get a reviewable, reversible, linted
migration. `keystonectl migrate bundle` then turns the directory into the
ConfigMap + `MigrationBundle` you commit to your GitOps tree.

---

## Entity Framework Core

Install the provider as a local tool:

```console
$ dotnet new tool-manifest
$ dotnet tool install --local keystone-ef
$ dotnet keystone-ef --version
```

`keystone-ef` drives `dotnet ef dbcontext script`, which renders your
model's full DDL while bypassing migrations entirely. It needs no database
connection — the schema comes from the model. EF's build output is kept on
stderr so stdout is always pure DDL.

If your model calls `HasDefaultSchema("app")`, pass `--strip-schema app`.
Keystone applies provider output under a scratch schema's `search_path`,
so schema-relative DDL is the shape it wants; the flag also removes the
`DO $EF$ … CREATE SCHEMA app … $EF$;` guard block Npgsql emits.

```yaml
# keystone.yaml
desired:
  program: [dotnet, keystone-ef, --strip-schema, app]
```

Requires `Microsoft.EntityFrameworkCore.Design` in the project and the
`dotnet-ef` tool.

## GORM

```yaml
desired:
  program: [keystone-gorm, --path, ./models]
```

`keystone-gorm` reads model structs statically via `go/ast` — no
reflection, no compilation of your code, no database connection. Install
it with:

```console
$ go install github.com/dogukanturhal/keystone-sdk/go/cmd/keystone-gorm@latest
```

## Anything else

Any command that prints DDL is a provider. No Keystone change is needed:

| ORM | `program:` |
|---|---|
| Prisma | `[npx, prisma, migrate, diff, --from-empty, --to-schema-datamodel, schema.prisma, --script]` |
| Django | `[python, manage.py, sqlmigrate, myapp, "0001"]` |
| SQLAlchemy | `[python, -c, "from models import Base; ..."]` |
| Ecto | `[mix, ecto.dump, --dump-path, /dev/stdout]` |
| plain `pg_dump` | `[pg_dump, --schema-only, --no-owner, mydb]` |

The contract a provider must honour:

- **stdout** is the desired schema as PostgreSQL DDL, and nothing else
- **stderr** is diagnostics
- **non-zero exit** means "I could not render the schema"

Empty stdout is treated as an error, not as an empty schema — otherwise a
broken provider would diff as "drop everything".

---

## keystone.yaml

Discovery walks up from the working directory, so the file sits at the
repository root and is found from any subdirectory. Relative paths inside
resolve against the file's own directory, so `keystonectl` behaves the
same wherever it is invoked from.

```yaml
version: 1

schema: public
devURL: ${KEYSTONE_DEV_URL}      # ${VAR} is expanded — commit the file, not the credential
dir: migrations
schemaRef: todo
lint: error

desired:
  program: [dotnet, keystone-ef, --strip-schema, app]
  # …or a reference instead:
  # ref: sql://schema.sql

bundle:
  namespace: example-prod
  name: todo
  schemaSelector:
    matchLabels:
      app: todo
  rolloutPolicyRef: staged-canary
```

Several setups (local, CI, adoption) go under `env:`, selected with
`--env`. The flat form above is the same shape as one entry, so growing
from one environment to many is additive:

```yaml
version: 1
env:
  default:
    devURL: postgres://localhost:5432/devdb?sslmode=disable
    desired:
      program: [dotnet, keystone-ef]
  ci:
    devURL: ${CI_DEV_URL}
    lint: error
    desired:
      program: [dotnet, keystone-ef, --no-build]
```

Precedence is **flags > environment variable > config > defaults**. A flag
counts as set only if it was written on the command line, so a config file
can override a flag that has a default.

### `prefer program: over exec:// on the command line`

Both work. `program:` is an argv array, so no quoting rules apply and an
argument containing spaces is unambiguous. `exec://dotnet keystone-ef` is
split POSIX-style without a shell — meaning `;`, pipes and redirection are
not smuggle-able through a reference, but also that they are not
available.

---

## Shipping to the cluster

```console
$ keystonectl migrate bundle --out manifests/
wrote manifests/todo-migrations-29ea9c42d4d4.configmap.yaml
wrote manifests/todo-29ea9c42d4d4.bundle.yaml
bundle todo-29ea9c42d4d4 version=002 (2 up, 2 down) → ConfigMap todo-migrations-29ea9c42d4d4
```

Both objects are **content-addressed**: the name carries a hash of the
SQL. Editing a migration produces a *new* ConfigMap and bundle rather than
mutating one in place — the failure mode where a ConfigMap is rewritten
under a bundle name the tracking table already records as applied, leaving
the runner stuck on SQL that no longer exists.

`keystone.sum` is packaged inside the ConfigMap because the controller
verifies the resolved source against it; without it the bundle reports
"integrity unverified".

---

## CI

Nothing forces an author to re-run `migrate diff` after changing an
entity, so code and migrations drift silently and only diverge visibly
once the schema reaches an environment that matters. The
`keystone-migration-drift` template re-runs the diff against a throwaway
PostgreSQL and fails the MR if it would have authored anything:

```yaml
include:
  - project: <your-org>/ci-components
    file: templates/keystone-migration-drift.yml
    ref: main

migration-drift:
  extends: .keystone-migration-drift
  variables:
    KEYSTONE_PROJECT_DIR: services/crm
```

It reads your `keystone.yaml`, so the CI check cannot drift from what you
run locally. It also verifies `keystone.sum`. When the desired source is
`exec://`, point `KEYSTONE_IMAGE` at an image carrying both `keystonectl`
and your ORM toolchain — the default `keystone-tools` image has
`keystonectl` only, which is enough for `yaml://` and `sql://` sources.

---

## Security note

`exec://` runs a program, so it is a capability rather than a parse
result. `keystonectl` grants it because it is developer-driven. The
operator does **not**: `schemasource.ResolveDesired` refuses `exec://`
unless the caller sets `Resolver.AllowExec`, so a reference arriving in a
CR spec can never become code execution inside the operator pod.
