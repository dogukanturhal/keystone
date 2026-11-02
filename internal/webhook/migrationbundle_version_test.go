// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dogukanturhal/keystone-sdk/go/analyze"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func TestMigrationBundleValidator_VersionAndContentUpdate(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "new-sql"},
		Data: map[string]string{"001.up.sql": "SELECT 2;"}}
	v := &MigrationBundleValidator{Client: newFakeClient(cm).Build(), Registry: analyze.DefaultRegistry()}
	for _, legacy := range []bool{false, true} {
		old := makeBundle(cm.Namespace, "bundle", "old-sql")
		old.Status.ContentHash = "old-hash"
		if !legacy {
			old.Status.ContentHashVersion = "v1"
		}
		updated := old.DeepCopy()
		updated.Spec.Source.ConfigMapRef.Name = cm.Name
		if _, err := v.ValidateUpdate(context.Background(), old, updated); err == nil || !strings.Contains(err.Error(), "bump spec.version") {
			t.Fatalf("same-version edit (legacy=%v): %v", legacy, err)
		}
		updated.Spec.Version = "v2"
		if _, err := v.ValidateUpdate(context.Background(), old, updated); err != nil {
			t.Fatalf("version+SQL bump (legacy=%v): %v", legacy, err)
		}
		// Another spec edit while v2 has not reconciled yet must not
		// compare v2 SQL against the v1 pin.
		pending := updated.DeepCopy()
		pending.Spec.MaxConcurrentExecutions = 1
		if !legacy {
			if _, err := v.ValidateUpdate(context.Background(), updated, pending); err != nil {
				t.Fatalf("pending bump uses old-version hash: %v", err)
			}
		}
	}
}

func TestMigrationBundleValidator_WedgedMetadataUpdates(t *testing.T) {
	// No client/registry: unchanged-spec updates must never touch the
	// external source or policy store (even if both are unavailable).
	v := &MigrationBundleValidator{}
	old := makeBundle("keystone-system", "wedged", "mutated-configmap")
	old.Finalizers = []string{keystonev1alpha1.FinalizerMigrationBundle}
	old.Status.ContentHash = "old-hash"
	old.Status.ContentHashVersion = "v1"
	for _, kind := range []string{"finalizer-removal", "annotation", "label", "dry-run-unchanged", "status"} {
		t.Run(kind, func(t *testing.T) {
			updated := old.DeepCopy()
			switch kind {
			case "finalizer-removal":
				updated.Finalizers = nil
			case "annotation":
				updated.Annotations = map[string]string{"approval": "true"}
			case "label":
				updated.Labels = map[string]string{"owner": "gitops"}
			case "status":
				updated.Status.ObservedGeneration = 2
			}
			warnings, err := v.ValidateUpdate(context.Background(), old, updated)
			if err != nil || len(warnings) != 0 {
				t.Fatalf("metadata update blocked: warnings=%v err=%v", warnings, err)
			}
		})
	}
}
