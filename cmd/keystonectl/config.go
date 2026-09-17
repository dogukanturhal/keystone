// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// -- keystone.yaml ----------------------------------------------------
//
// A project-level config file, so authoring a migration is
//
//	keystonectl migrate diff add_customer
//
// instead of four flags repeated in every invocation and every CI job.
// This is Keystone's answer to `atlas.hcl`, in the format the rest of the
// ecosystem already reads.
//
// Discovery walks up from the working directory, so the file can sit at
// the repository root and still be found from any subdirectory — the same
// rule `dotnet ef` uses for .config/dotnet-ef.json. Every relative path
// inside resolves against the file's own directory rather than the
// process's, so `keystonectl` behaves identically wherever it is invoked
// from; a CI job that cd's into a subdirectory must not silently author
// migrations into a different tree.
//
// Precedence is flags > config > defaults. "Explicit" means the flag was
// actually written on the command line, not that it holds a non-zero
// value — otherwise a config file could never override a flag that has a
// default, which is most of them.

// ConfigFileNames are the accepted file names, in search order.
var ConfigFileNames = []string{"keystone.yaml", "keystone.yml"}

// Config is a parsed keystone.yaml.
//
// A project with one setup writes the settings at the top level. A
// project that needs several (local dev vs CI vs an adoption baseline)
// adds an `env:` map and selects with --env. The flat form is the same
// shape as one env entry, so growing from one to many is additive.
type Config struct {
	// Version pins the config schema. Absent or 1 for now; a future
	// incompatible change bumps it rather than guessing.
	Version int `json:"version,omitempty"`

	// Env holds named environments. When non-empty, --env selects one
	// and the top-level fields are ignored.
	Env map[string]EnvConfig `json:"env,omitempty"`

	// EnvConfig embedded: the single-environment (flat) form.
	EnvConfig `json:",omitempty"`

	// dir is the directory the file was loaded from. Not serialised —
	// it anchors every relative path in the file.
	dir string
}

// EnvConfig is one environment's settings — the flag set of
// `keystonectl migrate diff`, plus the manifest-emission settings used by
// `keystonectl migrate bundle`.
type EnvConfig struct {
	// Desired is the desired-state source.
	Desired *DesiredConfig `json:"desired,omitempty"`

	// Dir is the migration bundle directory. Default "migrations".
	Dir string `json:"dir,omitempty"`

	// DevURL is the dev database used to normalise DDL and replay the
	// migration directory. Supports ${ENV_VAR} expansion so a config
	// file can be committed without a credential in it.
	DevURL string `json:"devURL,omitempty"`

	// From overrides the current state with a live database instead of
	// dev-replaying Dir.
	From string `json:"from,omitempty"`

	// Schema is the target schema. Default "public".
	Schema string `json:"schema,omitempty"`

	// SchemaRef stamps a DatabaseSchema name onto a derived spec.
	SchemaRef string `json:"schemaRef,omitempty"`

	// Lint is the lint gate policy: error | warn | off.
	Lint string `json:"lint,omitempty"`

	// AllowDestructive permits DROP/ALTER TYPE in authored migrations.
	AllowDestructive bool `json:"allowDestructive,omitempty"`

	// Bundle configures `keystonectl migrate bundle` manifest emission.
	Bundle *BundleConfig `json:"bundle,omitempty"`
}

// DesiredConfig names the desired schema. Exactly one form is set.
type DesiredConfig struct {
	// Ref is a source reference string: yaml:// , sql:// , db:// or
	// exec:// .
	Ref string `json:"ref,omitempty"`

	// Program is an ORM provider invocation as an argv array — the
	// structured equivalent of exec://. Preferred over squeezing a
	// command line into Ref: no quoting rules apply, so an argument
	// containing spaces is unambiguous.
	Program []string `json:"program,omitempty"`
}

// BundleConfig describes the cluster manifests `migrate bundle` emits.
type BundleConfig struct {
	// Name is the MigrationBundle / ConfigMap base name. Defaults to
	// the config file's directory name.
	Name string `json:"name,omitempty"`

	// Namespace the manifests are emitted into.
	Namespace string `json:"namespace,omitempty"`

	// SchemaSelector picks the DatabaseSchemas the bundle targets.
	// Required — the admission webhook refuses an empty selector,
	// because a bundle that matches nothing is a silent no-op.
	SchemaSelector *metav1.LabelSelector `json:"schemaSelector,omitempty"`

	// RolloutPolicyRef pins a staged-rollout policy. Empty fans out to
	// every matched schema at once.
	RolloutPolicyRef string `json:"rolloutPolicyRef,omitempty"`

	// PolicyRef pins the SchemaPolicy to validate against.
	PolicyRef string `json:"policyRef,omitempty"`

	// IncludeDown also emits the .down.sql files and wires them as the
	// bundle's downSource. Default true — authoring produces reversible
	// pairs, and dropping half of that on the floor at manifest time
	// would silently lose the rollback path.
	IncludeDown *bool `json:"includeDown,omitempty"`

	// AutoRollback lets a failed execution apply the down source
	// automatically. Default false: forward-fix remains the norm.
	AutoRollback bool `json:"autoRollback,omitempty"`
}

// FindConfig walks up from start looking for a config file. Returns an
// empty path (no error) when none exists — running without a config file
// is the normal case for a one-off invocation.
func FindConfig(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		for _, name := range ConfigFileNames {
			candidate := filepath.Join(dir, name)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root.
			return "", nil
		}
		dir = parent
	}
}

// LoadConfig reads and validates a config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Version != 0 && cfg.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d (this keystonectl understands 1)", path, cfg.Version)
	}
	cfg.dir = filepath.Dir(path)
	return &cfg, nil
}

// Resolve returns the settings for env, with paths made absolute and
// ${VAR} references expanded.
//
// env is the --env value; empty selects the flat form, or the sole entry
// when exactly one is defined (a file with one env should not force
// everyone to pass --env).
func (c *Config) Resolve(env string) (EnvConfig, error) {
	var out EnvConfig

	switch {
	case len(c.Env) == 0:
		if env != "" && env != "default" {
			return out, fmt.Errorf("--env %q requested but the config file defines no env: block", env)
		}
		out = c.EnvConfig

	case env != "":
		e, ok := c.Env[env]
		if !ok {
			return out, fmt.Errorf("--env %q not found in the config file (have: %s)", env, joinKeys(c.Env))
		}
		out = e

	case len(c.Env) == 1:
		for _, e := range c.Env {
			out = e
		}

	default:
		if e, ok := c.Env["default"]; ok {
			out = e
			break
		}
		return out, fmt.Errorf("the config file defines %d environments and no `default`; pass --env (have: %s)",
			len(c.Env), joinKeys(c.Env))
	}

	// Credentials belong in the environment, not in a committed file.
	out.DevURL = os.ExpandEnv(out.DevURL)
	out.From = os.ExpandEnv(out.From)

	// Relative paths anchor to the config file, not the process CWD.
	out.Dir = c.abs(out.Dir)
	if out.Desired != nil && out.Desired.Ref != "" {
		out.Desired.Ref = c.absRef(out.Desired.Ref)
	}

	if out.Desired != nil && out.Desired.Ref != "" && len(out.Desired.Program) > 0 {
		return out, errors.New("desired: set either `ref` or `program`, not both")
	}
	return out, nil
}

// ConfigDir is the directory the config file was loaded from. Provider
// programs run there so an ORM project resolves relative to the config
// rather than to wherever the CLI happened to be invoked.
func (c *Config) ConfigDir() string { return c.dir }

// abs resolves a possibly-relative path against the config file's dir.
func (c *Config) abs(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}

// absRef anchors the path inside a file-bearing source reference. db://
// and exec:// carry no path, so they pass through untouched.
func (c *Config) absRef(ref string) string {
	for _, scheme := range []string{"yaml://", "sql://", "file://"} {
		if len(ref) > len(scheme) && ref[:len(scheme)] == scheme {
			return scheme + c.abs(ref[len(scheme):])
		}
	}
	if _, err := os.Stat(ref); err == nil {
		return ref
	}
	// A bare relative path (classified by extension downstream).
	if !filepath.IsAbs(ref) && (filepath.Ext(ref) == ".sql" ||
		filepath.Ext(ref) == ".yaml" || filepath.Ext(ref) == ".yml") {
		return c.abs(ref)
	}
	return ref
}

func joinKeys(m map[string]EnvConfig) string {
	out := ""
	for k := range m {
		if out != "" {
			out += ", "
		}
		out += k
	}
	return out
}
