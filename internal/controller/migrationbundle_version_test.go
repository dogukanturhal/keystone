// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/dogukanturhal/keystone-sdk/go/migration"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// The API server exercises CRD persistence and merge-patch baselines, not
// just in-memory status. In particular, fail() advances ObservedGeneration
// on a wedged legacy bump and must not destroy its version evidence.
func TestMigrationBundleReconciler_VersionedHashPin(t *testing.T) {
	k8s, stop := StartEnvtest(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r := &MigrationBundleReconciler{
		Client: k8s, Scheme: clientgoscheme.Scheme, Recorder: record.NewFakeRecorder(100),
		Resolver: migration.NewConfigMapResolver(k8s),
	}
	for i, tc := range []struct {
		name, legacy string
		bump, staged bool
	}{
		{name: "version-and-sql-bump", bump: true},
		{name: "same-version-refused"},
		{name: "legacy-unchanged", legacy: "unchanged"},
		{name: "legacy-wedged-execution", legacy: "execution", bump: true},
		{name: "legacy-wedged-zero-schemas", legacy: "planned", bump: true},
		{name: "staged-pin-and-reset", bump: true, staged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("version-pin-%d", i)
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: name},
				Data: map[string]string{"001.up.sql": "SELECT 1;"}}
			if err := k8s.Create(ctx, cm); err != nil {
				t.Fatal(err)
			}
			bundle := &keystonev1alpha1.MigrationBundle{
				ObjectMeta: metav1.ObjectMeta{Namespace: cm.Namespace, Name: name},
				Spec: keystonev1alpha1.MigrationBundleSpec{
					Version: "v1", Strategy: keystonev1alpha1.StrategyVersioned,
					Source: keystonev1alpha1.MigrationSource{Type: keystonev1alpha1.SourceConfigMap,
						ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{Name: name, FilePattern: "*.up.sql"}},
					SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"module": name}},
				},
			}
			if err := k8s.Create(ctx, bundle); err != nil {
				t.Fatal(err)
			}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(bundle)}
			for j := 0; j < 2; j++ {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
			}
			if err := k8s.Get(ctx, req.NamespacedName, bundle); err != nil {
				t.Fatal(err)
			}
			oldHash := bundle.Status.ContentHash
			if oldHash == "" || bundle.Status.ContentHashVersion != "v1" {
				t.Fatalf("initial pin not persisted: %+v", bundle.Status)
			}
			if tc.legacy != "" {
				bundle.Status.ContentHashVersion = ""
				if tc.legacy == "execution" {
					exec := &keystonev1alpha1.MigrationExecution{
						ObjectMeta: metav1.ObjectMeta{Namespace: cm.Namespace, Name: name + "-old"},
						Spec:       keystonev1alpha1.MigrationExecutionSpec{BundleRef: name, BundleVersion: "v1", SchemaRef: "old-schema", ContentHash: oldHash},
					}
					if err := controllerutil.SetControllerReference(bundle, exec, r.Scheme); err != nil {
						t.Fatal(err)
					}
					if err := k8s.Create(ctx, exec); err != nil {
						t.Fatal(err)
					}
					// No Planned evidence: recovery must use the owned ME.
					bundle.Status.Conditions = nil
				}
			}
			if tc.staged {
				bundle.Status.CurrentStage = "old-last-stage"
				bundle.Status.StageHistory = []keystonev1alpha1.StageProgress{{Name: "old-last-stage", StartedAt: metav1.Now()}}
			}
			if err := k8s.Status().Update(ctx, bundle); err != nil {
				t.Fatal(err)
			}
			if tc.legacy != "unchanged" {
				cm.Data["001.up.sql"] = "SELECT 2;"
				if err := k8s.Update(ctx, cm); err != nil {
					t.Fatal(err)
				}
			}
			if tc.bump {
				bundle.Spec.Version = "v2"
				if tc.staged {
					bundle.Spec.RolloutPolicyRef = "missing-policy"
				}
				if err := k8s.Update(ctx, bundle); err != nil {
					t.Fatal(err)
				}
				if tc.legacy == "execution" || tc.legacy == "planned" {
					if _, err := r.fail(ctx, bundle, "ContentHashChanged", fmt.Errorf("old manager wedged this bump")); err == nil {
						t.Fatal("expected old failure")
					}
				}
			}
			_, err := r.Reconcile(ctx, req)
			if !tc.bump && tc.legacy == "" {
				if err == nil {
					t.Fatal("same-version SQL edit must fail")
				}
			} else if tc.staged {
				if err == nil {
					t.Fatal("missing rollout policy must still fail")
				}
			} else if err != nil {
				t.Fatalf("version/legacy reconciliation: %v", err)
			}
			if err := k8s.Get(ctx, req.NamespacedName, bundle); err != nil {
				t.Fatal(err)
			}
			if tc.bump || tc.legacy == "unchanged" {
				wantHash := migration.HashFiles(cm.Data, []string{"001.up.sql"})
				if bundle.Status.ContentHashVersion != bundle.Spec.Version || bundle.Status.ContentHash != wantHash {
					t.Fatalf("new pair not persisted: %+v", bundle.Status)
				}
			} else if bundle.Status.ContentHash != oldHash {
				t.Fatal("tamper refusal must retain original pin")
			}
			if tc.staged && (bundle.Status.CurrentStage != "" || len(bundle.Status.StageHistory) != 0) {
				t.Fatalf("old stage/soak state survived bump: %+v", bundle.Status)
			}
		})
	}
}
