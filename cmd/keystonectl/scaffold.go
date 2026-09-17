// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dogukanturhal/keystone-sdk/go/authoring"
	"github.com/dogukanturhal/keystone-sdk/go/declarative"
	"github.com/dogukanturhal/keystone-sdk/go/drift"
	"github.com/dogukanturhal/keystone-sdk/go/schemaspec"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// -- scaffold ---------------------------------------------------------
//
// `keystonectl scaffold` onboards an existing PostgreSQL database into
// Keystone's GitOps model. It introspects a live schema and emits the
// full resource set an operator needs to bring that database under
// management: a SchemaDefinition (the declarative shape), topology stubs
// (DatabaseProvider / LogicalDatabase / DatabaseSchema), an adoption
// baseline migration (the schema's current state as 0001), a
// kustomization, and a README. The emitted files are a reviewable
// starting point — TODO markers flag the values an operator must supply
// (credentials secret, cluster ref).

type scaffoldOpts struct {
	dsn           string
	schema        string
	out           string
	product       string
	providerRef   string
	logicalDBRef  string
	schemaRef     string
	adopt         bool
	stripDangling bool
}

func runScaffold(args []string) error {
	var o scaffoldOpts
	fs := flag.NewFlagSet("scaffold", flag.ContinueOnError)
	fs.StringVar(&o.dsn, "dsn", os.Getenv("KEYSTONECTL_DSN"), "PostgreSQL connection string of the database to onboard (or env KEYSTONECTL_DSN)")
	fs.StringVar(&o.schema, "schema", "public", "schema to introspect")
	fs.StringVar(&o.out, "out", "", "output directory for the generated resource set (required)")
	fs.StringVar(&o.product, "product", "", "product/app name used in resource names + labels (default: derived from --schema)")
	fs.StringVar(&o.providerRef, "provider-ref", "", "DatabaseProvider name (default: <product>-provider)")
	fs.StringVar(&o.logicalDBRef, "logicaldb-ref", "", "LogicalDatabase name (default: <product>-db)")
	fs.StringVar(&o.schemaRef, "schema-ref", "", "DatabaseSchema name the SchemaDefinition targets (default: <product>)")
	fs.BoolVar(&o.adopt, "adopt", false, "treat the baseline as adoption state (ADR 0008): the README directs recording it as already-applied rather than executing it")
	fs.BoolVar(&o.stripDangling, "strip-dangling-refs", false, "strip FKs/views/triggers referencing identifiers outside the introspected schema")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `keystonectl scaffold — onboard an existing database into Keystone

Usage:
  keystonectl scaffold --dsn <dsn> --schema <s> --out <dir> [--product name] [--adopt]

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.dsn == "" {
		return errors.New("--dsn or KEYSTONECTL_DSN required")
	}
	if o.out == "" {
		return errors.New("--out directory required")
	}
	o.applyDefaults()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, o.dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// 1. Introspect → desired spec.
	snap, err := drift.NewInspector(pool).Inspect(ctx, o.schema)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	spec := schemaspec.FromSnapshot(snap, schemaspec.Options{SchemaRef: o.schemaRef})
	report := auditDanglingRefs(spec, o.stripDangling)
	report.WriteWarnings(os.Stderr, o.stripDangling)

	// 2. Baseline migration = diff of the empty schema against the spec,
	//    i.e. CREATE everything. Never destructive (observed is empty).
	plan, err := declarative.Diff(&drift.Snapshot{Schema: o.schema}, spec)
	if err != nil {
		return fmt.Errorf("baseline diff: %w", err)
	}
	baseline := authoring.RenderUpDown(plan, authoring.Options{
		Name:        "baseline",
		Version:     "0001",
		Schema:      o.schema,
		StripSchema: o.schema,
		GeneratedBy: "keystonectl scaffold " + version,
	})

	// 3. Write the resource set.
	migDir := filepath.Join(o.out, "migrations")
	if err := os.MkdirAll(migDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", migDir, err)
	}

	cfg := pool.Config().ConnConfig
	if err := o.writeFile("schemadefinition.yaml", o.renderSchemaDefinition(spec)); err != nil {
		return err
	}
	if err := o.writeFile("databaseprovider.yaml", o.renderProvider(cfg.Host, int(cfg.Port))); err != nil {
		return err
	}
	if err := o.writeFile("logicaldatabase.yaml", o.renderLogicalDatabase(cfg.Database)); err != nil {
		return err
	}
	if err := o.writeFile("databaseschema.yaml", o.renderDatabaseSchema()); err != nil {
		return err
	}
	if err := o.writeFile("migrationbundle.yaml", o.renderMigrationBundle()); err != nil {
		return err
	}
	for name, body := range baseline.Files() {
		if err := os.WriteFile(filepath.Join(migDir, name), []byte(body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	sumFiles, err := loadSumInputs(migDir, "*.up.sql")
	if err != nil {
		return err
	}
	if err := sumWrite(migDir, sumFiles); err != nil {
		return err
	}
	if err := o.writeFile("kustomization.yaml", o.renderKustomization()); err != nil {
		return err
	}
	if err := o.writeFile("README.md", o.renderReadme(len(plan.Statements))); err != nil {
		return err
	}

	fmt.Printf("scaffolded %d resources + a %d-statement baseline into %s\n",
		5, len(plan.Statements), o.out)
	fmt.Printf("next: review the TODO markers (credentials, clusterRef), then `kubectl apply -k %s`\n", o.out)
	return nil
}

func (o *scaffoldOpts) applyDefaults() {
	if o.product == "" {
		o.product = sanitizeName(o.schema)
	}
	if o.providerRef == "" {
		o.providerRef = o.product + "-provider"
	}
	if o.logicalDBRef == "" {
		o.logicalDBRef = o.product + "-db"
	}
	if o.schemaRef == "" {
		o.schemaRef = o.product
	}
}

func (o *scaffoldOpts) ownerRole() string { return o.product + "_owner" }

func (o *scaffoldOpts) writeFile(name, body string) error {
	path := filepath.Join(o.out, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func (o *scaffoldOpts) renderSchemaDefinition(spec *keystonev1alpha1.SchemaDefinitionSpec) string {
	// Reuse the inspect path's writer for byte-identical SchemaDefinition
	// output (full kubectl-apply-able CR).
	var sb strings.Builder
	sb.WriteString("# Declarative desired-state for the onboarded schema.\n")
	sb.WriteString("# Re-generate after schema changes with: keystonectl inspect --dsn … --schema " + o.schema + "\n")
	if err := writeSpec(spec, o.schemaRef, "yaml", &sb); err != nil {
		return fmt.Sprintf("# error rendering SchemaDefinition: %v\n", err)
	}
	return sb.String()
}

func (o *scaffoldOpts) renderProvider(host string, port int) string {
	return fmt.Sprintf(`apiVersion: keystone.hexxlock.io/v1alpha1
kind: DatabaseProvider
metadata:
  name: %s
spec:
  engine: postgresql
  host: %q          # introspected from --dsn; confirm it is reachable from the cluster
  port: %d
  sslMode: verify-full   # TODO: confirm; relax only with justification
  adminCredentialsRef:
    secretName: %s-admin-credentials   # TODO: create this Secret (usernameKey/passwordKey)
`, o.providerRef, host, port, o.product)
}

func (o *scaffoldOpts) renderLogicalDatabase(dbName string) string {
	return fmt.Sprintf(`apiVersion: keystone.hexxlock.io/v1alpha1
kind: LogicalDatabase
metadata:
  name: %s
spec:
  name: %q
  providerRef: %s
  clusterRef: ""   # TODO: ClusterRegistration name hosting this database
  ownerRole: %s
`, o.logicalDBRef, dbName, o.providerRef, o.ownerRole())
}

func (o *scaffoldOpts) renderDatabaseSchema() string {
	return fmt.Sprintf(`apiVersion: keystone.hexxlock.io/v1alpha1
kind: DatabaseSchema
metadata:
  name: %s
  labels:
    # Selected by migrationbundle.yaml's spec.schemaSelector — keep in sync.
    keystone.hexxlock.io/scope: %s
spec:
  name: %q
  logicalDatabaseRef: %s
  ownerRole: %s
`, o.schemaRef, o.product, o.schema, o.logicalDBRef, o.ownerRole())
}

// renderMigrationBundle wires the baseline migration declaratively. In
// --adopt mode the bundle is RecordOnly (ADR 0027): the runner upserts
// the tracking-table row without executing — `flyway baseline` /
// Liquibase `changelog-sync` semantics, GitOps-native and audited.
// Without --adopt the baseline executes normally (fresh database).
func (o *scaffoldOpts) renderMigrationBundle() string {
	mode, modeComment := "Apply", "fresh database — the baseline executes"
	if o.adopt {
		mode, modeComment = "RecordOnly",
			"adoption (ADR 0027) — recorded as already-applied, SQL never executes"
	}
	return fmt.Sprintf(`apiVersion: keystone.hexxlock.io/v1alpha1
kind: MigrationBundle
metadata:
  name: %s-baseline
  labels:
    keystone.hexxlock.io/scope: %s
  # If a SchemaPolicy with an ApprovalPolicy matches this scope, add the
  # approver annotation it requires before applying, e.g.:
  #   keystone.hexxlock.io/approval.<policy-name>.<approver-id>: <group>
spec:
  version: "0001"
  strategy: versioned
  executionMode: %s   # %s
  schemaSelector:
    matchLabels:
      keystone.hexxlock.io/scope: %s
  source:
    type: ConfigMap
    configMapRef:
      name: %s-migrations   # generated by kustomization.yaml (configMapGenerator)
`, o.product, o.product, mode, modeComment, o.product, o.product)
}

func (o *scaffoldOpts) renderKustomization() string {
	return fmt.Sprintf(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

# Apply order is enforced by Keystone's controllers via owner refs +
# readiness conditions; listing them here keeps the set together.
resources:
  - databaseprovider.yaml
  - logicaldatabase.yaml
  - databaseschema.yaml
  - schemadefinition.yaml
  - migrationbundle.yaml

# The baseline migration files travel as a ConfigMap; the bundle's
# source.configMapRef points at the generated name. keystone.sum rides
# along for the integrity check (A1). The resolver's default
# filePattern (*.up.sql) ignores the .down.sql key.
configMapGenerator:
  - name: %s-migrations
    files:
      - migrations/0001_baseline.up.sql
      - migrations/0001_baseline.down.sql
      - migrations/keystone.sum

generatorOptions:
  # The bundle pins the ConfigMap by name; content immutability is
  # enforced by keystone.sum + the bundle contentHash gate instead.
  disableNameSuffixHash: true
`, o.product)
}

func (o *scaffoldOpts) renderReadme(stmtCount int) string {
	adoptNote := ""
	if o.adopt {
		adoptNote = `
### Adoption mode (--adopt)

This database already exists, so its baseline is **recorded as applied,
not executed** (ADR 0008 / ADR 0027). ` + "`migrationbundle.yaml`" + ` carries
` + "`spec.executionMode: RecordOnly`" + `: the runner upserts the
` + "`(version, content_hash)`" + ` row into the schema's tracking table and
executes nothing — the declarative equivalent of ` + "`flyway baseline`" + ` /
Liquibase ` + "`changelog-sync`" + `. No manual SQL against the tracking table
is needed (or sanctioned).

Because the baseline never executes, lint findings on it are advisory:
they appear in the bundle's ` + "`status.lintFindings`" + ` but do not block.
Integrity (keystone.sum), per-version immutability, and approval
policies still gate the bundle in full.

From then on, ` + "`keystonectl migrate diff`" + ` authors *incremental*
migrations (0002, 0003, …) on top of this baseline — those execute
normally and lint normally.
`
	}
	return fmt.Sprintf(`# %s — Keystone onboarding

Generated by `+"`keystonectl scaffold`"+` from an existing PostgreSQL
schema (`+"`%s`"+`). This is a reviewable starting point: search for
`+"`TODO`"+` and fill in the deployment-specific values (admin credentials
Secret, ClusterRegistration ref) before applying.

## Files

| File | Purpose |
|------|---------|
| `+"`databaseprovider.yaml`"+`   | Physical connection to the database server |
| `+"`logicaldatabase.yaml`"+`    | The logical database + owner role |
| `+"`databaseschema.yaml`"+`     | The schema this set manages |
| `+"`schemadefinition.yaml`"+`   | Declarative desired-state (%d objects) |
| `+"`migrationbundle.yaml`"+`    | Wires the baseline migration (see executionMode) |
| `+"`migrations/0001_baseline.{up,down}.sql`"+` | The schema's current state as a baseline migration |
| `+"`migrations/keystone.sum`"+` | Integrity manifest over the migration dir |

## Apply

1. Create the admin-credentials Secret referenced by `+"`databaseprovider.yaml`"+`.
2. Set `+"`clusterRef`"+` in `+"`logicaldatabase.yaml`"+`.
3. If a SchemaPolicy with an ApprovalPolicy matches scope `+"`%s`"+`, add the
   required approver annotation to `+"`migrationbundle.yaml`"+`.
4. `+"`kubectl apply -k .`"+` — the kustomization generates the
   `+"`%s-migrations`"+` ConfigMap from `+"`migrations/`"+` and applies the
   MigrationBundle alongside the topology resources.
%s
## Authoring further migrations

Edit `+"`schemadefinition.yaml`"+` (or point at updated ORM/SQL), then:

    keystonectl migrate diff <change_name> \
      --desired yaml://schemadefinition.yaml \
      --dev-url postgres://…/devdb \
      --dir migrations
`,
		o.product, o.schema, stmtCount, o.product, o.product, adoptNote)
}

// sanitizeName lowercases and underscores a schema name into a product slug.
func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	out := strings.Trim(sb.String(), "_")
	if out == "" {
		return "app"
	}
	return out
}
