// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/dogukanturhal/keystone-sdk/go/migration"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// -- migrate bundle ---------------------------------------------------
//
// `keystonectl migrate bundle` turns an authored migration directory into
// the two cluster objects that carry it — a ConfigMap of SQL and a
// MigrationBundle that points at it — so the versioned path ends in
// something committable instead of a hand-written manifest.
//
// Without this the workflow dead-ended: `migrate diff` produced
// NNN_name.up.sql / .down.sql, and the operator consumed ConfigMaps, and
// a human bridged the two by copy-paste. The declarative path
// (SchemaDefinition → differ → generated bundle) was already closed-loop;
// this closes the versioned one.
//
// Manifests are content-addressed. The name carries a hash of the SQL, so
// changing a migration produces a *new* object rather than mutating one
// in place — the failure mode where a ConfigMap is rewritten under a
// bundle name that the tracking table already records as applied, leaving
// the runner silently stuck on SQL that no longer exists.
//
// keystone.sum ships inside the ConfigMap because the controller verifies
// the resolved source against it and downgrades to "integrity unverified"
// when it is missing.

type migrateBundleOpts struct {
	dir          string
	name         string
	namespace    string
	selector     string
	rolloutRef   string
	policyRef    string
	includeDown  bool
	autoRollback bool
	out          string
	configPath   string
	env          string

	// selectorFromConfig is the structured selector out of keystone.yaml,
	// used when --selector was not given.
	selectorFromConfig *metav1.LabelSelector
}

// versionPrefix matches the NNN_ ordering prefix authoring writes.
var versionPrefix = regexp.MustCompile(`^([0-9]+)_`)

func runMigrateBundle(args []string) error {
	var o migrateBundleOpts

	fs := flag.NewFlagSet("migrate bundle", flag.ContinueOnError)
	fs.StringVar(&o.dir, "dir", "migrations", "migration bundle directory to package")
	fs.StringVar(&o.name, "name", "", "base name for the emitted objects (default: the migration directory's parent name)")
	fs.StringVar(&o.namespace, "namespace", "", "namespace to emit the manifests into")
	fs.StringVar(&o.selector, "selector", "", "label selector picking target DatabaseSchemas, e.g. app=crm,tier=prod")
	fs.StringVar(&o.rolloutRef, "rollout-policy", "", "RolloutPolicy to stage the fan-out through (default: all matched schemas at once)")
	fs.StringVar(&o.policyRef, "policy", "", "SchemaPolicy to validate the bundle against")
	fs.BoolVar(&o.includeDown, "include-down", true, "package the .down.sql files and wire them as the bundle's downSource")
	fs.BoolVar(&o.autoRollback, "auto-rollback", false, "let a failed execution apply the down source automatically")
	fs.StringVar(&o.out, "out", "", "directory to write the manifests into (default: stdout)")
	fs.StringVar(&o.configPath, "config", "", "path to keystone.yaml (default: nearest one walking up from the working directory)")
	fs.StringVar(&o.env, "env", "", "environment to select from the config file's env: block")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `keystonectl migrate bundle — package an authored migration directory as cluster manifests

Usage:
  keystonectl migrate bundle [--dir migrations] --namespace <ns> --selector <sel> [flags]

Emits a ConfigMap holding the SQL (plus keystone.sum, which the controller
verifies) and a MigrationBundle referencing it. Both names carry a content
hash, so editing a migration yields new objects instead of rewriting
applied ones in place.

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

	if o.namespace == "" {
		return errors.New("--namespace is required (or set bundle.namespace in keystone.yaml)")
	}
	selector, err := o.resolveSelector()
	if err != nil {
		return err
	}
	if o.name == "" {
		abs, err := filepath.Abs(o.dir)
		if err != nil {
			return err
		}
		o.name = sanitizeObjectName(filepath.Base(filepath.Dir(abs)))
	}

	// Collect the SQL. Down files are optional but the *set* is what gets
	// hashed, so include them before hashing or the same SQL would
	// address differently depending on a flag.
	files, err := loadSumInputs(o.dir, "*.sql")
	if err != nil {
		return err
	}
	ups := filterSuffix(files, ".up.sql")
	if len(ups) == 0 {
		return fmt.Errorf("no *.up.sql files in %s — run `keystonectl migrate diff` first", o.dir)
	}
	payload := map[string]string{}
	for k, v := range ups {
		payload[k] = v
	}
	downs := filterSuffix(files, ".down.sql")
	if o.includeDown {
		for k, v := range downs {
			payload[k] = v
		}
	}

	// keystone.sum is always computed over the *.up.sql set, matching
	// what `keystonectl sum` writes and therefore what the controller
	// recomputes when it verifies integrity.
	sum := migration.BuildSum(ups)
	payload[migration.SumFilename] = string(migration.MarshalSum(sum))

	hash := sum.RootHash[:12]
	cmName := fmt.Sprintf("%s-migrations-%s", o.name, hash)
	bundleName := fmt.Sprintf("%s-%s", o.name, hash)

	version, err := latestVersion(ups)
	if err != nil {
		return err
	}

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "keystonectl",
		"keystone.hexxlock.io/bundle":  o.name,
	}

	cm := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: o.namespace,
			Labels:    labels,
		},
		Data: payload,
	}

	bundle := keystonev1alpha1.MigrationBundle{
		TypeMeta: metav1.TypeMeta{
			APIVersion: keystonev1alpha1.GroupVersion.String(),
			Kind:       "MigrationBundle",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      bundleName,
			Namespace: o.namespace,
			Labels:    labels,
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  version,
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name:        cmName,
					FilePattern: "*.up.sql",
				},
			},
			SchemaSelector:   *selector,
			RolloutPolicyRef: o.rolloutRef,
			PolicyRef:        o.policyRef,
			AutoRollback:     o.autoRollback,
		},
	}
	if o.includeDown && len(downs) > 0 {
		bundle.Spec.DownSource = &keystonev1alpha1.MigrationSource{
			Type: keystonev1alpha1.SourceConfigMap,
			ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
				Name:        cmName,
				FilePattern: "*.down.sql",
			},
		}
	}

	cmYAML, err := yaml.Marshal(cm)
	if err != nil {
		return fmt.Errorf("render ConfigMap: %w", err)
	}
	bundleYAML, err := yaml.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("render MigrationBundle: %w", err)
	}

	if o.out == "" {
		fmt.Printf("---\n%s---\n%s", cmYAML, bundleYAML)
	} else {
		if err := os.MkdirAll(o.out, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", o.out, err)
		}
		for name, body := range map[string][]byte{
			cmName + ".configmap.yaml":  cmYAML,
			bundleName + ".bundle.yaml": bundleYAML,
		} {
			p := filepath.Join(o.out, name)
			if err := os.WriteFile(p, body, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", p, err)
			}
			fmt.Fprintf(os.Stderr, "wrote %s\n", p)
		}
	}

	fmt.Fprintf(os.Stderr, "bundle %s version=%s (%d up, %d down) → ConfigMap %s\n",
		bundleName, version, len(ups), len(downs), cmName)
	if o.autoRollback && (!o.includeDown || len(downs) == 0) {
		fmt.Fprintln(os.Stderr,
			"warning: --auto-rollback set but no down source is packaged; a failed execution will have nothing to roll back to")
	}
	return nil
}

// applyConfig folds keystone.yaml's bundle: block into the parsed flags,
// with the same flags-beat-config precedence `migrate diff` uses.
func (o *migrateBundleOpts) applyConfig(fs *flag.FlagSet) error {
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

	if !explicit["dir"] && env.Dir != "" {
		o.dir = env.Dir
	}
	b := env.Bundle
	if b == nil {
		return nil
	}
	if !explicit["name"] && b.Name != "" {
		o.name = b.Name
	}
	if !explicit["namespace"] && b.Namespace != "" {
		o.namespace = b.Namespace
	}
	if !explicit["rollout-policy"] && b.RolloutPolicyRef != "" {
		o.rolloutRef = b.RolloutPolicyRef
	}
	if !explicit["policy"] && b.PolicyRef != "" {
		o.policyRef = b.PolicyRef
	}
	if !explicit["include-down"] && b.IncludeDown != nil {
		o.includeDown = *b.IncludeDown
	}
	if !explicit["auto-rollback"] && b.AutoRollback {
		o.autoRollback = true
	}
	if !explicit["selector"] && b.SchemaSelector != nil {
		o.selectorFromConfig = b.SchemaSelector
	}
	return nil
}

// resolveSelector turns either the --selector string or the config file's
// structured selector into a LabelSelector. An empty selector is refused
// here rather than at admission: a bundle matching nothing is a no-op
// that looks like success until someone notices the schema never changed.
func (o *migrateBundleOpts) resolveSelector() (*metav1.LabelSelector, error) {
	if o.selector != "" {
		sel, err := metav1.ParseToLabelSelector(o.selector)
		if err != nil {
			return nil, fmt.Errorf("--selector: %w", err)
		}
		return sel, nil
	}
	if o.selectorFromConfig != nil {
		return o.selectorFromConfig, nil
	}
	return nil, errors.New("--selector is required (or set bundle.schemaSelector in keystone.yaml); " +
		"a bundle with an empty selector applies to nothing and is refused at admission")
}

// filterSuffix picks the entries whose key ends with suffix.
func filterSuffix(files map[string]string, suffix string) map[string]string {
	out := make(map[string]string, len(files))
	for k, v := range files {
		if strings.HasSuffix(k, suffix) {
			out[k] = v
		}
	}
	return out
}

// latestVersion returns the highest NNN prefix across the up files — the
// bundle's version. Ordering is lexicographic on zero-padded prefixes,
// which is what authoring.NextVersion produces.
func latestVersion(ups map[string]string) (string, error) {
	versions := make([]string, 0, len(ups))
	for name := range ups {
		m := versionPrefix.FindStringSubmatch(name)
		if m == nil {
			return "", fmt.Errorf("migration %q has no NNN_ version prefix; "+
				"`migrate bundle` packages directories authored by `migrate diff`", name)
		}
		versions = append(versions, m[1])
	}
	sort.Strings(versions)
	return versions[len(versions)-1], nil
}

// sanitizeObjectName coerces a directory name into an RFC 1123 label so
// it can seed a Kubernetes object name.
//
// Deliberately not scaffold.go's sanitizeName: that one maps separators
// to underscores because it produces a PostgreSQL identifier, and an
// underscore is illegal in a Kubernetes object name. Same intent, two
// different alphabets.
func sanitizeObjectName(in string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(in) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '.' || r == '_' || r == ' ':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "migrations"
	}
	return out
}
