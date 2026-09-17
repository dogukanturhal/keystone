// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dogukanturhal/keystone-sdk/go/analyze"
	"github.com/dogukanturhal/keystone-sdk/go/authoring"
	"github.com/dogukanturhal/keystone-sdk/go/declarative"
	"github.com/dogukanturhal/keystone-sdk/go/drift"
	"github.com/dogukanturhal/keystone-sdk/go/schemasource"
	"github.com/dogukanturhal/keystone-sdk/go/schemaspec"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// -- migrate ----------------------------------------------------------
//
// `keystonectl migrate diff <name>` authors a new versioned, reversible
// migration bundle from a desired schema expressed in code — the
// `atlas migrate diff` of Keystone.
//
// The desired state (--desired) is a SchemaDefinition YAML, a raw SQL DDL
// file, or a live database. The current state is established either by
// replaying the existing migration directory onto a dev database
// (--dev-url, canonical — no live drift leaks into the authored
// migration) or by inspecting a live database directly (--from db://...,
// right for adoption / greenfield). The diff between them is rendered
// into <dir>/<NNN>_<name>.up.sql + .down.sql and keystone.sum is
// regenerated.

func runMigrate(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: keystonectl migrate <diff|bundle> [flags]")
	}
	switch args[0] {
	case "diff":
		return runMigrateDiff(args[1:])
	case "bundle":
		return runMigrateBundle(args[1:])
	default:
		return fmt.Errorf("unknown migrate subcommand %q (want: diff, bundle)", args[0])
	}
}

type migrateDiffOpts struct {
	name        string
	desired     string
	dir         string
	devURL      string
	from        string
	schema      string
	allowDestr  bool
	lint        string
	schemaRef   string
	generatedBy string

	// configPath / env select a keystone.yaml environment; execDir is
	// the directory exec:// providers run in (the config file's dir, so
	// an ORM project resolves relative to the config rather than to the
	// caller's working directory).
	configPath string
	env        string
	execDir    string
	// program is a desired-state provider given as an argv array in the
	// config file — the structured form of exec://.
	program []string
}

func runMigrateDiff(args []string) error {
	var o migrateDiffOpts

	// Pull the migration <name> off the front so it can precede the flags
	// (`migrate diff add_users_index --desired …`), matching `atlas
	// migrate diff <name>` ergonomics.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		o.name = args[0]
		args = args[1:]
	}

	fs := flag.NewFlagSet("migrate diff", flag.ContinueOnError)
	fs.StringVar(&o.desired, "desired", "", "desired-state source: yaml://schema.yaml | sql://schema.sql | db://<dsn>?schema=<s> | exec://<program> <args…>")
	fs.StringVar(&o.configPath, "config", "", "path to keystone.yaml (default: nearest one walking up from the working directory)")
	fs.StringVar(&o.env, "env", "", "environment to select from the config file's env: block")
	fs.StringVar(&o.dir, "dir", "migrations", "migration bundle directory (created if absent)")
	fs.StringVar(&o.devURL, "dev-url", os.Getenv("KEYSTONECTL_DEV_URL"), "dev database for replaying the dir + normalising sql:// desired (or env KEYSTONECTL_DEV_URL)")
	fs.StringVar(&o.from, "from", "", "current-state override (db://<dsn>?schema=<s>); default: dev-replay the --dir on --dev-url")
	fs.StringVar(&o.schema, "schema", "public", "target schema (drives qualification; generated SQL is rewritten schema-relative)")
	fs.BoolVar(&o.allowDestr, "allow-destructive", false, "permit DROP TABLE/COLUMN and ALTER COLUMN TYPE in the generated migration")
	fs.StringVar(&o.lint, "lint", "error", "lint gate over the generated up.sql: error (block on error-severity) | warn (print only) | off")
	fs.StringVar(&o.schemaRef, "schema-ref", "", "schemaRef to stamp on a spec derived from sql://|db:// desired (cosmetic; affects nothing in the diff)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `keystonectl migrate diff <name> — author a versioned migration from a desired schema

Usage:
  keystonectl migrate diff <name> --desired <src> --dev-url <dsn> [--dir migrations] [flags]
  keystonectl migrate diff <name> --desired <src> --from db://<dsn> [--dir migrations] [flags]

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := o.applyConfig(fs); err != nil {
		return err
	}
	if o.name == "" {
		fs.Usage()
		return errors.New("migration <name> is required (e.g. keystonectl migrate diff add_users_email_index --desired …)")
	}
	if o.desired == "" && len(o.program) == 0 {
		return errors.New("--desired is required (or set desired: in keystone.yaml)")
	}
	if o.from == "" && o.devURL == "" {
		return errors.New("provide --dev-url (to dev-replay the migration dir) or --from db://<dsn> (to diff against a live database)")
	}
	o.generatedBy = "keystonectl " + version

	ctx := context.Background()

	// Optional dev pool — needed for sql:// desired and for dev-replay.
	var devPool *pgxpool.Pool
	if o.devURL != "" {
		p, err := pgxpool.New(ctx, o.devURL)
		if err != nil {
			return fmt.Errorf("connect --dev-url: %w", err)
		}
		defer p.Close()
		devPool = p
	}

	// 1. Desired state.
	var desiredRef schemasource.Ref
	if len(o.program) > 0 {
		// The config file's argv form — no quoting rules to get wrong.
		desiredRef = schemasource.Ref{Scheme: schemasource.SchemeExec, Program: o.program}
	} else {
		r, err := schemasource.ParseRef(o.desired)
		if err != nil {
			return fmt.Errorf("--desired: %w", err)
		}
		desiredRef = r
	}
	// keystonectl is developer-driven, so it may run provider programs —
	// unlike the operator, which resolves references that arrive in CR
	// specs and must never grant this.
	resolver := schemasource.Resolver{AllowExec: true, ExecDir: o.execDir}
	desired, err := resolver.ResolveDesired(ctx, desiredRef, devPool, o.schema,
		schemaspecOptions(o.schemaRef))
	if err != nil {
		return fmt.Errorf("resolve desired: %w", err)
	}
	if o.allowDestr {
		desired.AllowDestructive = true
	}

	// 2. Current state.
	current, err := o.resolveCurrent(ctx, devPool)
	if err != nil {
		return err
	}
	// The differ qualifies emitted DDL with current.Schema; pin it to the
	// target so the authored SQL (after schema-relative stripping) is
	// consistent regardless of where the current state was read from.
	current.Schema = o.schema

	// 3. Diff.
	plan, err := declarative.Diff(current, desired)
	if err != nil {
		var dr *declarative.ErrDestructiveRefused
		if errors.As(err, &dr) {
			return fmt.Errorf("diff is destructive (%d op(s)); re-run with --allow-destructive to author it", dr.Count)
		}
		return fmt.Errorf("diff: %w", err)
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(os.Stderr, "warn: %s\n", w)
	}
	if plan.Empty() {
		fmt.Fprintln(os.Stderr, "schema already matches desired state; no migration authored")
		return nil
	}

	// 4. Render the bundle.
	existing, err := existingUpFiles(o.dir)
	if err != nil {
		return err
	}
	bundle := authoring.RenderUpDown(plan, authoring.Options{
		Name:        o.name,
		Version:     authoring.NextVersion(existing),
		Schema:      o.schema,
		StripSchema: o.schema,
		GeneratedBy: o.generatedBy,
	})

	// 5. Lint gate over the generated up.sql.
	if err := o.lintGate(ctx, bundle); err != nil {
		return err
	}

	// 6. Write files + regenerate keystone.sum.
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", o.dir, err)
	}
	for name, body := range bundle.Files() {
		if err := os.WriteFile(filepath.Join(o.dir, name), []byte(body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	sumFiles, err := loadSumInputs(o.dir, "*.up.sql")
	if err != nil {
		return err
	}
	if err := sumWrite(o.dir, sumFiles); err != nil {
		return err
	}

	fmt.Printf("authored %s\n", filepath.Join(o.dir, bundle.UpFile))
	fmt.Printf("         %s\n", filepath.Join(o.dir, bundle.DownFile))
	fmt.Printf("%d statement(s), %d destructive\n", len(plan.Statements), plan.DestructiveOps)
	return nil
}

// applyConfig folds keystone.yaml into the parsed flags.
//
// Precedence is flags > environment variable > config > flag defaults.
// "Explicit" is decided by FlagSet.Visit, which reports only the flags
// actually written on the command line — testing for a non-zero value
// instead would make it impossible for a config file to override any
// flag that carries a default, which is nearly all of them.
//
// Running with no config file at all is normal, not an error: a one-off
// invocation should keep working with flags alone.
func (o *migrateDiffOpts) applyConfig(fs *flag.FlagSet) error {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	path := o.configPath
	if path == "" {
		found, err := FindConfig(".")
		if err != nil {
			return fmt.Errorf("locate keystone.yaml: %w", err)
		}
		if found == "" {
			return nil
		}
		path = found
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	env, err := cfg.Resolve(o.env)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	o.execDir = cfg.ConfigDir()

	if !explicit["dir"] && env.Dir != "" {
		o.dir = env.Dir
	}
	// The dev-url flag's default is already KEYSTONECTL_DEV_URL, so an
	// operator who exported it has expressed an intent the config file
	// should not quietly overrule.
	if !explicit["dev-url"] && os.Getenv("KEYSTONECTL_DEV_URL") == "" && env.DevURL != "" {
		o.devURL = env.DevURL
	}
	if !explicit["from"] && env.From != "" {
		o.from = env.From
	}
	if !explicit["schema"] && env.Schema != "" {
		o.schema = env.Schema
	}
	if !explicit["schema-ref"] && env.SchemaRef != "" {
		o.schemaRef = env.SchemaRef
	}
	if !explicit["lint"] && env.Lint != "" {
		o.lint = env.Lint
	}
	if !explicit["allow-destructive"] && env.AllowDestructive {
		o.allowDestr = true
	}
	if !explicit["desired"] && env.Desired != nil {
		o.desired = env.Desired.Ref
		o.program = env.Desired.Program
	}
	return nil
}

// resolveCurrent builds the current-state Snapshot, either by inspecting
// a live database (--from db://) or by replaying the migration directory
// onto the dev database.
func (o *migrateDiffOpts) resolveCurrent(ctx context.Context, devPool *pgxpool.Pool) (*drift.Snapshot, error) {
	if o.from != "" {
		ref, err := schemasource.ParseRef(o.from)
		if err != nil {
			return nil, fmt.Errorf("--from: %w", err)
		}
		if ref.Scheme != schemasource.SchemeDB {
			return nil, fmt.Errorf("--from must be a db:// reference, got %q", o.from)
		}
		schema := ref.Schema
		if schema == "" {
			schema = o.schema
		}
		pool, err := pgxpool.New(ctx, ref.DSN)
		if err != nil {
			return nil, fmt.Errorf("connect --from: %w", err)
		}
		defer pool.Close()
		snap, err := drift.NewInspector(pool).Inspect(ctx, schema)
		if err != nil {
			return nil, fmt.Errorf("inspect --from: %w", err)
		}
		return snap, nil
	}

	// Dev-replay: materialise the existing *.up.sql onto a scratch schema.
	if devPool == nil {
		return nil, errors.New("dev-replay requires --dev-url")
	}
	files, err := loadSumInputs(o.dir, "*.up.sql")
	if err != nil {
		// A missing directory means an empty baseline (first migration).
		if errors.Is(err, os.ErrNotExist) {
			return &drift.Snapshot{Schema: o.schema}, nil
		}
		return nil, err
	}
	if len(files) == 0 {
		// No prior migrations: current state is the empty schema.
		return &drift.Snapshot{Schema: o.schema}, nil
	}
	return schemasource.SnapshotFromFiles(ctx, devPool, files, o.schema)
}

// lintGate runs the analyzer over the generated up.sql and enforces the
// configured policy. Only the forward file is linted (down files
// legitimately contain DROPs); destructive-class findings are exempted
// when --allow-destructive was passed, since the author has accepted them.
func (o *migrateDiffOpts) lintGate(ctx context.Context, bundle authoring.Bundle) error {
	if o.lint == "off" {
		return nil
	}
	m := &analyze.Migration{
		BundleName:   o.name,
		Version:      bundle.Version,
		TargetSchema: o.schema,
		Files:        []analyze.FileBody{{Name: bundle.UpFile, Body: bundle.Up}},
	}
	findings, err := analyze.DefaultRegistry().Run(ctx, m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: analyzer error: %v\n", err)
	}

	var blocking []analyze.Finding
	for _, f := range findings {
		fmt.Fprintf(os.Stderr, "lint: %s [%s] %s\n", f.Severity, f.Rule, f.Message)
		if f.Severity == keystonev1alpha1.LintLevelError {
			if o.allowDestr && destructiveLintRules[f.Rule] {
				continue // intentional destructive op
			}
			blocking = append(blocking, f)
		}
	}
	if o.lint == "error" && len(blocking) > 0 {
		return fmt.Errorf("refusing to author: %d error-severity lint finding(s) in the generated migration "+
			"(fix the desired schema, or lower --lint=warn to author anyway)", len(blocking))
	}
	return nil
}

// destructiveLintRules are the error-severity analyzer rules whose
// findings are the direct, intended consequence of --allow-destructive.
var destructiveLintRules = map[string]bool{
	"no-drop-table":        true,
	"no-drop-column":       true,
	"no-alter-column-type": true,
	"no-rename-column":     true,
	"no-rename-table":      true,
}

// existingUpFiles returns the basenames of *.up.sql files already in dir
// (used to compute the next version). A missing dir yields no files.
func existingUpFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// schemaspecOptions builds the conversion options for a desired spec
// derived from a sql://|db:// source.
func schemaspecOptions(schemaRef string) schemaspec.Options {
	return schemaspec.Options{SchemaRef: schemaRef}
}
