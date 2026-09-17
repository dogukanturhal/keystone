// SPDX-License-Identifier: AGPL-3.0-or-later
//
// keystonectl — local CLI for Keystone. Atlas-parity ergonomics for
// schema authors who want fast iteration without round-tripping through
// the cluster.
//
// Subcommands:
//
//   keystonectl inspect \
//       --dsn postgres://user:pw@host/db \
//       --schema public \
//       --schema-ref crm
//       → writes a SchemaDefinition YAML to stdout
//
//   keystonectl diff \
//       --dsn postgres://user:pw@host/db \
//       --schema public \
//       --desired path/to/schema-definition.yaml
//       → prints the SQL statements needed to reach desired state
//
//   keystonectl lint path/to/*.sql
//       → runs the 20-rule analyzer registry over the SQL files;
//         exits non-zero if any finding is at severity=error
//
//   keystonectl migrate diff <name> \
//       --desired yaml://schema.yaml --dev-url postgres://…/devdb
//       → authors a versioned, reversible migration bundle from a desired
//         schema (declarative YAML, SQL DDL, or a live DB). See
//         docs/migration-from-code.md.
//
//   keystonectl scaffold --dsn postgres://…/db --schema public --out ./out
//       → onboards an existing database: emits the full Keystone resource
//         set + an adoption baseline. See docs/scaffold-from-db.md.
//
// Design notes:
//   - single binary, built from this one file + the existing internal/
//     packages. No transitive cluster dependencies.
//   - Connects directly to PostgreSQL — no K8s API involvement.
//   - All commands are read-only against the target DB except `apply`
//     (not implemented here; explicit MigrationBundle CR is the GitOps
//     path. keystonectl stays a dev-loop tool).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/dogukanturhal/keystone-sdk/go/analyze"
	"github.com/dogukanturhal/keystone-sdk/go/declarative"
	"github.com/dogukanturhal/keystone-sdk/go/drift"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
	"github.com/dogukanturhal/keystone-sdk/go/schemaspec"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/schemadiff"
)

// keystonectlScheme is the runtime scheme used by the plan subcommand
// when talking to the apiserver. Populated once at init.
var keystonectlScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(keystonectlScheme))
	utilruntime.Must(keystonev1alpha1.AddToScheme(keystonectlScheme))
}

// loadRESTConfig resolves a *rest.Config from an explicit path, the
// KUBECONFIG env, the standard in-cluster path, or ~/.kube/config in
// that order. Mirrors what `kubectl` does under the hood.
func loadRESTConfig(explicit string) (*rest.Config, error) {
	if explicit != "" {
		return clientcmd.BuildConfigFromFlags("", explicit)
	}
	// clientcmd's default loader handles KUBECONFIG env + recommended
	// home dir + in-cluster fallback.
	loading := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loading, overrides).ClientConfig()
}

// newKeystoneClient returns a controller-runtime client wired to the
// Keystone scheme. Used by plan subcommands; other subcommands stay
// apiserver-agnostic (DB only).
func newKeystoneClient(cfg *rest.Config) (ctrlclient.Client, error) {
	return ctrlclient.New(cfg, ctrlclient.Options{Scheme: keystonectlScheme})
}

var (
	version = "v0.1.0"
	commit  = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "inspect":
		if err := runInspect(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "diff":
		if err := runDiff(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "migrate":
		if err := runMigrate(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "scaffold":
		if err := runScaffold(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "lint":
		if err := runLint(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "sum":
		if err := runSum(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "plan":
		if err := runPlan(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "snapshot":
		if err := runSnapshot(os.Args[2:]); err != nil {
			exitErr(err)
		}
	case "version", "--version", "-v":
		fmt.Printf("keystonectl %s (commit %s)\n", version, commit)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `keystonectl — Keystone schema CLI

Usage:
  keystonectl <command> [flags]

Commands:
  inspect    Dump a live schema as a SchemaDefinition YAML
  diff       Compute SQL to reach desired state from live
  migrate    Author a versioned migration from a desired schema (migrate diff <name>)
  scaffold   Onboard an existing database: emit the full Keystone resource set
  lint       Run the 20-rule analyzer pack over SQL files
  sum        Generate or verify keystone.sum (directory integrity)
  plan       Inspect or approve a MigrationPlan (Plan-Gate-Apply)
  snapshot   Browse schema registry (list | diff) — requires cluster
  version    Print version

Run "keystonectl <command> --help" for command-specific flags.
`)
}

// -- inspect ----------------------------------------------------------

type inspectOpts struct {
	dsn               string
	schema            string
	schemaRef         string
	schemaSelector    string // comma-separated key=value pairs, e.g. "scope=example-service,tier=tenant"
	name              string // SchemaDefinition metadata.name override (required when --schema-selector is set)
	output            string
	stripDanglingRefs bool // strip FKs/triggers/views that reference identifiers not declared in the same SD
	failOnDangling    bool // exit non-zero when any dangling ref is detected (warnings still print regardless)
}

func runInspect(args []string) error {
	var o inspectOpts
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	fs.StringVar(&o.dsn, "dsn", os.Getenv("KEYSTONECTL_DSN"), "PostgreSQL connection string (or env KEYSTONECTL_DSN)")
	fs.StringVar(&o.schema, "schema", "public", "schema name to inspect")
	fs.StringVar(&o.schemaRef, "schema-ref", "", "DatabaseSchema CR name the output references (single-target form; default: inferred from --schema). Mutually exclusive with --schema-selector.")
	fs.StringVar(&o.schemaSelector, "schema-selector", "", "Label selector for fan-out form, comma-separated key=value pairs (e.g. \"scope=example-service,tier=tenant\"). Mutually exclusive with --schema-ref. Requires --name.")
	fs.StringVar(&o.name, "name", "", "SchemaDefinition metadata.name (required when --schema-selector is set; default: <schema-ref>-desired in --schema-ref mode)")
	fs.StringVar(&o.output, "output", "yaml", "output format: yaml | json")
	fs.BoolVar(&o.stripDanglingRefs, "strip-dangling-refs", false,
		"strip foreignKeys to undeclared tables, views with empty queries, and triggers calling undeclared functions before emitting the SchemaDefinition. Warnings are always printed to stderr regardless; this flag controls whether the offending entries are also removed from the output. Use when the source DB contains platform-only references the inspected schema cannot resolve (e.g. realm-scoped subset of a platform DB).")
	fs.BoolVar(&o.failOnDangling, "fail-on-dangling", false,
		"exit non-zero when any dangling reference is detected during inspect. Pairs with --strip-dangling-refs in CI to enforce reviewer attention; standalone (without --strip-dangling-refs) flags drift but lets the caller decide what to do.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.dsn == "" {
		return errors.New("--dsn or KEYSTONECTL_DSN required")
	}

	// Mode resolution. Default = single-target (legacy behavior). Setting
	// --schema-selector flips to fan-out and disallows --schema-ref to
	// avoid the admission-validation foot-gun (the SD webhook rejects an
	// SD that has both — see the
	// XValidation rule on SchemaDefinitionSpec, types_schemadefinition.go).
	if o.schemaRef != "" && o.schemaSelector != "" {
		return errors.New("--schema-ref and --schema-selector are mutually exclusive (the SD webhook rejects an SD with both set)")
	}
	var selectorLabels map[string]string
	if o.schemaSelector != "" {
		var perr error
		selectorLabels, perr = parseSchemaSelector(o.schemaSelector)
		if perr != nil {
			return fmt.Errorf("--schema-selector: %w", perr)
		}
		if o.name == "" {
			return errors.New("--name is required in --schema-selector mode (the legacy <schema-ref>-desired naming has no input to derive from)")
		}
	} else {
		if o.schemaRef == "" {
			o.schemaRef = o.schema
		}
		if o.name == "" {
			o.name = o.schemaRef + "-desired"
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, o.dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	insp := drift.NewInspector(pool)
	snap, err := insp.Inspect(ctx, o.schema)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	spec := schemaspec.FromSnapshot(snap, schemaspec.Options{
		SchemaRef:      o.schemaRef,
		SelectorLabels: selectorLabels,
	})
	report := auditDanglingRefs(spec, o.stripDanglingRefs)
	report.WriteWarnings(os.Stderr, o.stripDanglingRefs)
	if o.failOnDangling && report.Total() > 0 {
		// Warnings already printed; exit non-zero so CI gates can refuse
		// to commit a SchemaDefinition with unresolved cross-DB refs.
		return fmt.Errorf("inspect found %d dangling references (use --strip-dangling-refs to remove, or --no-fail-on-dangling to permit)", report.Total())
	}
	return writeSpec(spec, o.name, o.output, os.Stdout)
}

// danglingReport collects the curation-class drift the inspector is
// allowed to detect in the snapshot-to-spec pipeline. The classes
// correspond to three problem patterns first identified by a
// service-specific curation script that this inspector extension
// absorbs upstream as a generic feature.
type danglingReport struct {
	// danglingFKs[i] = "table.fk_name -> referencesTable"
	danglingFKs []string
	// emptyViews[i] = view name (query was empty/whitespace)
	emptyViews []string
	// danglingTriggers[i] = "trigger_name on table -> function()"
	danglingTriggers []string
}

func (r *danglingReport) Total() int {
	return len(r.danglingFKs) + len(r.emptyViews) + len(r.danglingTriggers)
}

// WriteWarnings emits one stderr line per detected issue. The `stripped`
// flag determines the verb in the message: STRIPPED if the issue was
// removed from the spec, KEPT if only flagged. Format is grep-friendly
// (`level=warn class=… …`) so CI logs stay parseable.
func (r *danglingReport) WriteWarnings(w io.Writer, stripped bool) {
	verb := "kept"
	if stripped {
		verb = "stripped"
	}
	for _, x := range r.danglingFKs {
		fmt.Fprintf(w, "level=warn class=dangling-fk verb=%s detail=%q\n", verb, x)
	}
	for _, x := range r.emptyViews {
		fmt.Fprintf(w, "level=warn class=empty-view verb=%s detail=%q\n", verb, x)
	}
	for _, x := range r.danglingTriggers {
		fmt.Fprintf(w, "level=warn class=dangling-trigger verb=%s detail=%q\n", verb, x)
	}
	if r.Total() == 0 {
		return
	}
	if stripped {
		fmt.Fprintf(w, "level=info class=summary detail=%q\n",
			fmt.Sprintf("stripped %d dangling-fks, %d empty-views, %d dangling-triggers from spec",
				len(r.danglingFKs), len(r.emptyViews), len(r.danglingTriggers)))
	} else {
		fmt.Fprintf(w, "level=info class=summary detail=%q\n",
			fmt.Sprintf("flagged %d dangling-fks, %d empty-views, %d dangling-triggers — pass --strip-dangling-refs to remove from output",
				len(r.danglingFKs), len(r.emptyViews), len(r.danglingTriggers)))
	}
}

// auditDanglingRefs walks the SchemaDefinitionSpec and collects the
// three curation classes (dangling FKs, empty views, dangling triggers).
// When `strip` is true, the offending entries are also removed from the
// spec in-place. The returned report holds human-readable identifiers for
// each issue so the caller can print warnings.
//
// "Dangling" means: the reference targets an identifier that is not also
// declared by this same SchemaDefinition. For FKs that's an undeclared
// referencesTable; for triggers, an undeclared function. The differ
// emits SQL that would fail at apply time when these references can't
// resolve in the target DB — typically because the source schema is the
// platform's full schema but the SD will be applied to a realm-scoped
// subset that legitimately omits the platform-only target tables. Empty
// view queries surface the same way: the inspector couldn't capture the
// view definition (postgres returns "" for some window-function or CTE
// patterns) and a CREATE VIEW with empty body always fails.
//
// The check is deterministic and idempotent — calling it twice on the
// same spec produces the same report. Useful for both the inspector
// emit path and any post-load validation in the operator's webhook.
func auditDanglingRefs(spec *keystonev1alpha1.SchemaDefinitionSpec, strip bool) *danglingReport {
	report := &danglingReport{}
	if spec == nil {
		return report
	}

	declaredTables := make(map[string]bool, len(spec.Tables))
	for _, t := range spec.Tables {
		declaredTables[t.Name] = true
	}
	declaredFuncs := make(map[string]bool, len(spec.Functions))
	for _, f := range spec.Functions {
		declaredFuncs[f.Name] = true
	}

	// R1 — strip foreignKeys whose referencesTable is not declared.
	for i := range spec.Tables {
		tbl := &spec.Tables[i]
		var keep []keystonev1alpha1.DesiredForeignKey
		for _, fk := range tbl.ForeignKeys {
			if declaredTables[fk.ReferencesTable] {
				keep = append(keep, fk)
				continue
			}
			report.danglingFKs = append(report.danglingFKs,
				fmt.Sprintf("%s.%s -> %s", tbl.Name, fk.Name, fk.ReferencesTable))
		}
		if strip {
			tbl.ForeignKeys = keep
		}
	}

	// R2 — strip views whose query is empty or whitespace-only. The
	// inspector COALESCEs missing definitions to "" rather than fail,
	// so this is the surface where an empty-body view shows up.
	{
		var keep []keystonev1alpha1.DesiredView
		for _, v := range spec.Views {
			if strings.TrimSpace(v.Query) == "" {
				report.emptyViews = append(report.emptyViews, v.Name)
				continue
			}
			keep = append(keep, v)
		}
		if strip {
			spec.Views = keep
		}
	}

	// R3 — strip triggers whose function isn't declared. Functions can
	// live outside the public schema (e.g. audit.log_changes) which the
	// inspector doesn't capture; the resulting trigger then references
	// a function the differ won't emit, so CREATE TRIGGER ... EXECUTE
	// FUNCTION fn() fails at apply time.
	{
		var keep []keystonev1alpha1.DesiredTrigger
		for _, tr := range spec.Triggers {
			if tr.Function != "" && declaredFuncs[tr.Function] {
				keep = append(keep, tr)
				continue
			}
			report.danglingTriggers = append(report.danglingTriggers,
				fmt.Sprintf("%s on %s -> %s()", tr.Name, tr.Table, tr.Function))
		}
		if strip {
			spec.Triggers = keep
		}
	}

	return report
}

// parseSchemaSelector turns "k1=v1,k2=v2" into a label map. Empty pairs
// and missing equals signs are rejected — selector authoring is
// security-relevant (an empty selector matches everything in-namespace),
// so silent fallback would be a foot-gun.
func parseSchemaSelector(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("expected key=value, got %q", pair)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			return nil, fmt.Errorf("empty key in %q", pair)
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, errors.New("at least one key=value pair required (empty selectors match everything in-namespace and are rejected by admission)")
	}
	return out, nil
}

func writeSpec(spec *keystonev1alpha1.SchemaDefinitionSpec, name, format string, w io.Writer) error {
	switch format {
	case "yaml":
		// Emit the full K8s-style object so the output is `kubectl apply -f`-able.
		obj := map[string]interface{}{
			"apiVersion": "keystone.hexxlock.io/v1alpha1",
			"kind":       "SchemaDefinition",
			"metadata": map[string]string{
				"name": name,
			},
			"spec": spec,
		}
		raw, err := yaml.Marshal(obj)
		if err != nil {
			return err
		}
		_, err = w.Write(raw)
		return err
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(spec)
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}

// -- diff -------------------------------------------------------------

type diffOpts struct {
	dsn      string
	schema   string
	desired  string
	format   string
	showSQL  bool
	showWarn bool
}

func runDiff(args []string) error {
	var o diffOpts
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.StringVar(&o.dsn, "dsn", os.Getenv("KEYSTONECTL_DSN"), "PostgreSQL connection string")
	fs.StringVar(&o.schema, "schema", "public", "schema name to inspect")
	fs.StringVar(&o.desired, "desired", "", "path to SchemaDefinition YAML (desired state)")
	fs.StringVar(&o.format, "format", "sql", "output format: sql | summary")
	fs.BoolVar(&o.showWarn, "warnings", true, "print differ warnings to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.dsn == "" || o.desired == "" {
		return errors.New("--dsn and --desired required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, o.dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	desired, err := loadSchemaDefinition(o.desired)
	if err != nil {
		return err
	}
	observed, err := drift.NewInspector(pool).Inspect(ctx, o.schema)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	plan, err := declarative.Diff(observed, desired)
	if err != nil {
		// ErrDestructiveRefused is still informative — print the plan
		// so the operator can decide whether to flip allowDestructive.
		var dr *declarative.ErrDestructiveRefused
		if errors.As(err, &dr) {
			fmt.Fprintf(os.Stderr, "warning: %s\n", err.Error())
		} else {
			return fmt.Errorf("diff: %w", err)
		}
	}
	if o.showWarn && len(plan.Warnings) > 0 {
		for _, w := range plan.Warnings {
			fmt.Fprintf(os.Stderr, "warn: %s\n", w)
		}
	}
	if plan.Empty() {
		fmt.Fprintln(os.Stderr, "schema matches desired state; no operations pending")
		return nil
	}
	switch o.format {
	case "sql":
		for _, s := range plan.Statements {
			fmt.Println(s + ";")
			fmt.Println()
		}
	case "summary":
		fmt.Printf("%d statement(s); %d destructive\n", len(plan.Statements), plan.DestructiveOps)
		for _, s := range plan.Statements {
			fmt.Printf("  - %s\n", firstLine(s))
		}
	default:
		return fmt.Errorf("unsupported format %q", o.format)
	}
	return nil
}

func loadSchemaDefinition(path string) (*keystonev1alpha1.SchemaDefinitionSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// Accept either a full SchemaDefinition CR (with apiVersion/kind/spec)
	// or a bare spec dict.
	var full struct {
		Spec keystonev1alpha1.SchemaDefinitionSpec `yaml:"spec" json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &full); err == nil && len(full.Spec.Tables) > 0 {
		return &full.Spec, nil
	}
	var spec keystonev1alpha1.SchemaDefinitionSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return &spec, nil
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// -- lint -------------------------------------------------------------

func runLint(args []string) error {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	schema := fs.String("schema", "public", "target schema (influences some analyzers)")
	jsonOut := fs.Bool("json", false, "emit findings as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		return errors.New("at least one .sql file path required")
	}

	m := &analyze.Migration{
		BundleName:   "keystonectl-lint",
		Version:      "local",
		TargetSchema: *schema,
	}
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			// Recursive: accept a directory, pick up *.up.sql.
			_ = filepath.Walk(p, func(path string, i os.FileInfo, err error) error {
				if err != nil || i.IsDir() || !strings.HasSuffix(path, ".sql") {
					return nil
				}
				body, _ := os.ReadFile(path)
				m.Files = append(m.Files, analyze.FileBody{Name: path, Body: string(body)})
				return nil
			})
			continue
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		m.Files = append(m.Files, analyze.FileBody{Name: p, Body: string(body)})
	}

	findings, err := analyze.DefaultRegistry().Run(context.Background(), m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "analyzer error: %v\n", err)
	}
	errorCount := 0
	for _, f := range findings {
		if f.Severity == keystonev1alpha1.LintLevelError {
			errorCount++
		}
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]interface{}{
			"findings":    findings,
			"error_count": errorCount,
			"total_count": len(findings),
		})
	}
	for _, f := range findings {
		fmt.Printf("%-8s  %-40s  %s:%d  %s\n", f.Severity, f.Rule, f.File, f.Line, f.Message)
	}
	fmt.Fprintf(os.Stderr, "\n%d finding(s) total, %d error(s)\n", len(findings), errorCount)
	if errorCount > 0 {
		os.Exit(1)
	}
	return nil
}

func exitErr(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// -- sum --------------------------------------------------------------
//
// `keystonectl sum <dir>` writes dir/keystone.sum. `keystonectl sum
// verify <dir>` re-computes the sum and exits non-zero if it diverges.
// The file that lands in <dir> is the authoritative artifact — commit
// it alongside the SQL migrations and keep it in the same ConfigMap /
// OCI artifact / Git path so the operator sees it at reconcile time.

type sumOpts struct {
	pattern string
	verify  bool
}

func runSum(args []string) error {
	var o sumOpts
	fs := flag.NewFlagSet("sum", flag.ContinueOnError)
	fs.StringVar(&o.pattern, "pattern", "*.up.sql",
		"glob pattern for migration files (matches MigrationBundle.spec.source.configMapRef.filePattern)")
	fs.BoolVar(&o.verify, "verify", false,
		"verify the directory against its committed keystone.sum (exit 1 on mismatch)")
	// Usage message has to make the common case (one positional dir arg)
	// obvious, so do not rely on flag parsing to hint at it.
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `keystonectl sum — generate or verify keystone.sum

Usage:
  keystonectl sum [--pattern=GLOB] <dir>           # write <dir>/keystone.sum
  keystonectl sum --verify [--pattern=GLOB] <dir>  # verify existing sum

Flags:
`)
		fs.PrintDefaults()
	}

	// Shortcut: support `keystonectl sum verify <dir>` as an alias for
	// `keystonectl sum --verify <dir>`. It reads like a subcommand and
	// matches `atlas migrate hash` ergonomics.
	if len(args) >= 1 && args[0] == "verify" {
		o.verify = true
		args = args[1:]
	}

	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) != 1 {
		fs.Usage()
		return errors.New("exactly one directory argument required")
	}
	dir := paths[0]

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	files, err := loadSumInputs(dir, o.pattern)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no files matching %q in %s", o.pattern, dir)
	}

	if o.verify {
		return sumVerify(dir, files)
	}
	return sumWrite(dir, files)
}

// loadSumInputs walks <dir> non-recursively, picks up files matching
// pattern, and returns them as a name→content map suitable for
// migration.BuildSum. We deliberately stay non-recursive to match the
// ConfigMap flat-data model: nested directories are out of scope for
// Phase A1.
func loadSumInputs(dir, pattern string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	files := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == migration.SumFilename {
			// Never include keystone.sum in its own hash set.
			continue
		}
		match, err := filepath.Match(pattern, name)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
		}
		if !match {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		files[name] = string(body)
	}
	return files, nil
}

func sumWrite(dir string, files map[string]string) error {
	raw := migration.MarshalSum(migration.BuildSum(files))
	path := filepath.Join(dir, migration.SumFilename)
	// 0644 — the sum is committed to Git and read by the operator; it
	// is not a secret. Explicit so readers don't guess.
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d files)\n", path, len(files))
	return nil
}

// -- plan -------------------------------------------------------------
//
// `keystonectl plan approve <ns>/<name>` sets
// MigrationPlan.spec.approved=true — the Plan-Gate-Apply release
// valve for Manual-mode plans. Thin wrapper over `kubectl patch` so
// operators don't have to remember JSON-merge syntax; no kubectl
// dependency — the CLI speaks to the apiserver directly via
// controller-runtime's REST config loader.

func runPlan(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, `keystonectl plan — inspect or approve MigrationPlans

Usage:
  keystonectl plan approve <namespace>/<name>   # set spec.approved=true
  keystonectl plan approve <name> -n <ns>       # alternative form

Flags:
  -n, --namespace <ns>   namespace when using single-name form (required)
`)
		return errors.New("subcommand required (approve)")
	}
	switch args[0] {
	case "approve":
		return runPlanApprove(args[1:])
	default:
		return fmt.Errorf("unknown plan subcommand %q", args[0])
	}
}

func runPlanApprove(args []string) error {
	fs := flag.NewFlagSet("plan approve", flag.ContinueOnError)
	ns := fs.String("namespace", "", "namespace (when target is <name> without slash)")
	fs.StringVar(ns, "n", "", "alias for --namespace")
	kubeconfig := fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig (defaults to $KUBECONFIG or in-cluster)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return errors.New("expected exactly one target (namespace/name or name)")
	}
	target := pos[0]
	namespace, name := splitNamespacedName(target, *ns)
	if namespace == "" || name == "" {
		return fmt.Errorf("target %q must be namespace/name or a name with --namespace", target)
	}

	cfg, err := loadRESTConfig(*kubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	cli, err := newKeystoneClient(cfg)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var plan keystonev1alpha1.MigrationPlan
	if err := cli.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: name}, &plan); err != nil {
		return fmt.Errorf("get plan %s/%s: %w", namespace, name, err)
	}
	if plan.Spec.Approved {
		fmt.Fprintf(os.Stderr, "plan %s/%s is already approved\n", namespace, name)
		return nil
	}
	original := plan.DeepCopy()
	plan.Spec.Approved = true
	if err := cli.Patch(ctx, &plan, ctrlclient.MergeFrom(original)); err != nil {
		return fmt.Errorf("patch plan: %w", err)
	}
	fmt.Fprintf(os.Stderr, "plan %s/%s approved (mode=%s)\n",
		namespace, name, plan.Status.ApprovalMode)
	return nil
}

// splitNamespacedName parses "ns/name" or returns ("", name) for a
// bare name. When only a name is given, fallback is the explicit
// --namespace flag.
func splitNamespacedName(target, fallbackNS string) (string, string) {
	if i := strings.Index(target, "/"); i >= 0 {
		return target[:i], target[i+1:]
	}
	return fallbackNS, target
}

// -- snapshot ---------------------------------------------------------
//
// `keystonectl snapshot list <schema> -n <ns>` shows every
// SchemaSnapshot that captured <schema>, sorted newest-first. Handy
// for "which auto-snapshot recorded the state after bundle X v2?".
//
// `keystonectl snapshot diff <snap-a> <snap-b> -n <ns>` loads two
// snapshots and prints the schemadiff Change list. Compact enough
// for PR comments; detailed enough for triage.
//
// `keystonectl snapshot show <name> -n <ns>` dumps the parsed
// Structure as YAML — an alternative to `kubectl get snap <name>
// -o yaml` that omits the giant ERD field.

func runSnapshot(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, `keystonectl snapshot — browse the schema registry

Usage:
  keystonectl snapshot list <schema> -n <ns>
  keystonectl snapshot diff <snap-a> <snap-b> -n <ns>
  keystonectl snapshot show <name> -n <ns>
`)
		return errors.New("subcommand required (list | diff | show)")
	}
	switch args[0] {
	case "list":
		return runSnapshotList(args[1:])
	case "diff":
		return runSnapshotDiff(args[1:])
	case "show":
		return runSnapshotShow(args[1:])
	default:
		return fmt.Errorf("unknown snapshot subcommand %q", args[0])
	}
}

func runSnapshotList(args []string) error {
	fs := flag.NewFlagSet("snapshot list", flag.ContinueOnError)
	ns := fs.String("namespace", "keystone-system", "namespace to search")
	fs.StringVar(ns, "n", "keystone-system", "alias for --namespace")
	kubeconfig := fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: snapshot list <schemaRef>")
	}
	schemaRef := fs.Arg(0)

	cfg, err := loadRESTConfig(*kubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	cli, err := newKeystoneClient(cfg)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var list keystonev1alpha1.SchemaSnapshotList
	if err := cli.List(ctx, &list, ctrlclient.InNamespace(*ns)); err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	fmt.Printf("%-40s  %-12s  %-24s  %s\n", "NAME", "PHASE", "CAPTURED", "SOURCE")
	for i := range list.Items {
		s := &list.Items[i]
		if s.Spec.SchemaRef != schemaRef {
			continue
		}
		captured := "-"
		if s.Status.CapturedAt != nil {
			captured = s.Status.CapturedAt.Format("2006-01-02T15:04:05Z")
		}
		source := s.Status.AutoSource
		if source == "" {
			source = "(manual)"
		}
		fmt.Printf("%-40s  %-12s  %-24s  %s\n", s.Name, s.Status.Phase, captured, source)
	}
	return nil
}

func runSnapshotDiff(args []string) error {
	fs := flag.NewFlagSet("snapshot diff", flag.ContinueOnError)
	ns := fs.String("namespace", "keystone-system", "namespace")
	fs.StringVar(ns, "n", "keystone-system", "alias for --namespace")
	kubeconfig := fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: snapshot diff <snap-a> <snap-b>")
	}
	nameA, nameB := fs.Arg(0), fs.Arg(1)

	cfg, err := loadRESTConfig(*kubeconfig)
	if err != nil {
		return err
	}
	cli, err := newKeystoneClient(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var a, b keystonev1alpha1.SchemaSnapshot
	if err := cli.Get(ctx, ctrlclient.ObjectKey{Namespace: *ns, Name: nameA}, &a); err != nil {
		return fmt.Errorf("get %s: %w", nameA, err)
	}
	if err := cli.Get(ctx, ctrlclient.ObjectKey{Namespace: *ns, Name: nameB}, &b); err != nil {
		return fmt.Errorf("get %s: %w", nameB, err)
	}
	if a.Status.Structure == nil {
		return fmt.Errorf("snapshot %s has no structure captured (phase=%s)", nameA, a.Status.Phase)
	}
	if b.Status.Structure == nil {
		return fmt.Errorf("snapshot %s has no structure captured (phase=%s)", nameB, b.Status.Phase)
	}

	report := schemadiff.Diff(a.Status.Structure, b.Status.Structure)

	// Caveats first, and on the way out too when there is nothing else
	// to print. An operator reading "no differences" is entitled to
	// assume every dimension was looked at; when one wasn't, saying so
	// once at the top of a long change list is not enough.
	for _, c := range report.Caveats {
		fmt.Fprintf(os.Stderr, "warning: %s\n", c)
		fmt.Fprintf(os.Stderr, "         %s modelVersion=%d, %s modelVersion=%d\n",
			nameA, report.FromModelVersion, nameB, report.ToModelVersion)
	}

	if len(report.Changes) == 0 {
		if len(report.Caveats) > 0 {
			fmt.Fprintf(os.Stderr,
				"no differences between %s and %s in the dimensions both snapshots record\n",
				nameA, nameB)
		} else {
			fmt.Fprintf(os.Stderr, "no structural differences between %s and %s\n", nameA, nameB)
		}
		return nil
	}

	fmt.Fprintf(os.Stderr, "%d change(s) from %s → %s:\n\n", len(report.Changes), nameA, nameB)
	for _, c := range report.Changes {
		fmt.Println("  " + c.String())
	}
	if len(report.Caveats) > 0 {
		fmt.Fprintln(os.Stderr)
		for _, c := range report.Caveats {
			fmt.Fprintf(os.Stderr, "warning: %s\n", c)
		}
	}
	return nil
}

func runSnapshotShow(args []string) error {
	fs := flag.NewFlagSet("snapshot show", flag.ContinueOnError)
	ns := fs.String("namespace", "keystone-system", "namespace")
	fs.StringVar(ns, "n", "keystone-system", "alias for --namespace")
	kubeconfig := fs.String("kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: snapshot show <name>")
	}
	name := fs.Arg(0)

	cfg, err := loadRESTConfig(*kubeconfig)
	if err != nil {
		return err
	}
	cli, err := newKeystoneClient(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var snap keystonev1alpha1.SchemaSnapshot
	if err := cli.Get(ctx, ctrlclient.ObjectKey{Namespace: *ns, Name: name}, &snap); err != nil {
		return err
	}
	if snap.Status.Structure == nil {
		return fmt.Errorf("snapshot %s has no structure (phase=%s)", name, snap.Status.Phase)
	}
	raw, err := yaml.Marshal(snap.Status.Structure)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(raw)
	return err
}

func sumVerify(dir string, files map[string]string) error {
	path := filepath.Join(dir, migration.SumFilename)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s not found — run `keystonectl sum %s` to create it", path, dir)
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	sum, err := migration.ParseSum(raw)
	if err != nil {
		return fmt.Errorf("%s is malformed: %w", path, err)
	}
	if err := migration.VerifySum(sum, files); err != nil {
		// The *SumMismatch error string is already operator-friendly —
		// no need to re-wrap.
		return fmt.Errorf("verify failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "%s verified (root %s, %d files)\n",
		path, sum.RootHash[:12], len(sum.Entries))
	return nil
}
