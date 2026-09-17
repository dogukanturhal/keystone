// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Command lockstep is a CI-time guard that ensures the helm chart and
// kustomize bundle stay in lockstep on objects whose names the SDK
// (or external consumers) hardcode. The two install paths SHOULD ship
// identical functional behaviour; divergence is silently invisible to
// chart consumers until a missing object materialises as a runtime
// "NotFound" error in the consumer's namespace.
//
// History: keystone v0.1.43 added pkg/sdk/keystone/enroll.go +
// config/rbac/tenant_enroller_clusterrole.yaml but did NOT add the
// equivalent helm template. Every chart-based install (every Argo
// Application in the org) silently shipped the SDK code without its
// supporting RBAC. Caught when example-service phase-2b MR3 tried to bind
// the missing ClusterRole and got "NotFound" at apply time. v0.1.44
// shipped the missing template; this lockstep check exists to make
// the same class of gap impossible to merge again.
//
// Strategy:
//
//   - Render helm: `helm template lockstep config/helm/keystone-operator`.
//   - Render kustomize: `kubectl kustomize config/default`.
//   - Parse both into a set of (Kind, Name) tuples.
//   - Read hack/lockstep/locked.yaml — a curated list of (apiVersion,
//     kind, name) tuples whose names are hardcoded in the SDK or in
//     external consumer manifests. For each locked tuple, BOTH
//     renderings must contain a resource whose Kind matches and
//     whose name equals the locked Name.
//   - Exit 1 with a clear message on the first miss; exit 0 when
//     every locked tuple is present in both renderings.
//
// This is intentionally narrow — it does NOT enforce "every
// kustomize resource appears in helm" because helm legitimately ships
// extras (Service for webhook, ValidatingWebhookConfiguration,
// Certificate) and kustomize legitimately ships some that helm
// expects the operator to supply (Namespace via `--create-namespace`).
// The locked list grows ONE hardcoded-name at a time as the SDK's
// surface grows. Lower false-positive rate; the gaps it catches are
// the high-leverage ones.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

const (
	lockedManifestRel = "hack/lockstep/locked.yaml"
	chartRel          = "config/helm/keystone-operator"
	kustomizeRel      = "config/default"
)

// LockedResource identifies a resource that must appear in BOTH
// install paths verbatim. apiVersion is informational (helps reviewers
// recognise the kind); the check is by Kind+Name only — apiVersion
// drift between paths is a separate concern.
type LockedResource struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "lockstep:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}

	locked, err := loadLocked(filepath.Join(root, lockedManifestRel))
	if err != nil {
		return fmt.Errorf("load locked manifest: %w", err)
	}
	if len(locked) == 0 {
		return errors.New("locked.yaml is empty — at least one entry required (the lockstep gate has no use without a guarded resource)")
	}

	helmObjs, err := renderHelm(filepath.Join(root, chartRel))
	if err != nil {
		return fmt.Errorf("render helm: %w", err)
	}
	kustObjs, err := renderKustomize(filepath.Join(root, kustomizeRel))
	if err != nil {
		return fmt.Errorf("render kustomize: %w", err)
	}

	var failures []string
	for _, l := range locked {
		if !contains(helmObjs, l.Kind, l.Name) {
			failures = append(failures, fmt.Sprintf(
				"helm chart at %s does NOT render %s/%s — locked.yaml requires both install paths ship it. "+
					"Add the missing template under config/helm/keystone-operator/templates/ (mirror config/rbac/ when the kustomize counterpart is the source).",
				chartRel, l.Kind, l.Name,
			))
		}
		if !contains(kustObjs, l.Kind, l.Name) {
			failures = append(failures, fmt.Sprintf(
				"kustomize bundle at %s does NOT render %s/%s — locked.yaml requires both install paths ship it. "+
					"Add the missing manifest to config/rbac/ (or wherever the kustomize bundle's kustomization.yaml resources list points).",
				kustomizeRel, l.Kind, l.Name,
			))
		}
	}

	if len(failures) > 0 {
		fmt.Fprintln(os.Stderr, "lockstep: helm and kustomize diverged on SDK-hardcoded names:")
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		return fmt.Errorf("%d lockstep violation(s)", len(failures))
	}

	fmt.Printf("lockstep: %d locked resource(s) present in both helm and kustomize renderings.\n", len(locked))
	return nil
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return string(bytes.TrimSpace(out)), nil
}

func loadLocked(path string) ([]LockedResource, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var locked []LockedResource
	if err := yaml.Unmarshal(raw, &locked); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return locked, nil
}

func renderHelm(chartPath string) ([]renderedObject, error) {
	// `helm template <release> <chart>` is hermetic — no cluster touch.
	// Release name "lockstep" is arbitrary; locked-name resources
	// (the ones we care about) bypass the chart fullname prefix.
	cmd := exec.Command("helm", "template", "lockstep", chartPath)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("helm template: %w (stderr=%s)", err, stderr.String())
	}
	return parseManifests(stdout)
}

func renderKustomize(kustPath string) ([]renderedObject, error) {
	// `kubectl kustomize` ships with kubectl; avoids requiring a
	// separate kustomize binary in CI images.
	cmd := exec.Command("kubectl", "kustomize", kustPath)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubectl kustomize: %w (stderr=%s)", err, stderr.String())
	}
	return parseManifests(stdout)
}

type renderedObject struct {
	APIVersion string
	Kind       string
	Name       string
}

// parseManifests splits a multi-doc YAML stream by `---` and extracts
// (apiVersion, kind, metadata.name) from each non-empty document.
// Tolerates documents without these fields (e.g. NOTES.txt content
// helm template emits in some chart styles) — they're skipped.
func parseManifests(r io.Reader) ([]renderedObject, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	// Split on YAML document separator. Use the conservative split
	// (line-anchored "---") rather than the YAML library's stream
	// reader so we don't accidentally split on `---` inside string
	// literals — the latter never occurs in K8s manifests at the
	// document boundary, but defensive.
	docs := bytes.Split(raw, []byte("\n---\n"))
	out := make([]renderedObject, 0, len(docs))
	for _, d := range docs {
		d = bytes.TrimSpace(d)
		if len(d) == 0 {
			continue
		}
		var meta struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(d, &meta); err != nil {
			// Skip non-YAML or non-K8s documents (helm NOTES.txt, etc.).
			continue
		}
		if meta.Kind == "" || meta.Metadata.Name == "" {
			continue
		}
		out = append(out, renderedObject{
			APIVersion: meta.APIVersion,
			Kind:       meta.Kind,
			Name:       meta.Metadata.Name,
		})
	}
	return out, nil
}

func contains(objs []renderedObject, kind, name string) bool {
	for _, o := range objs {
		if o.Kind == kind && o.Name == name {
			return true
		}
	}
	return false
}
