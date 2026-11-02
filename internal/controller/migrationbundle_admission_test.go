// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	keystonewebhook "github.com/dogukanturhal/keystone/internal/webhook"
)

// Boot a real manager/webhook against envtest, then submit actual UPDATE
// admission requests. This catches finalizer/dry-run failures that direct
// validator calls alone cannot prove through the API server.
func TestMigrationBundle_AdmissionAndManagerBoot(t *testing.T) {
	k8s, stop := startEnvtest(t, true)
	defer stop()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	probeAddress := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	options := testEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: k8s.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: probeAddress,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host: options.LocalServingHost, Port: options.LocalServingPort, CertDir: options.LocalServingCertDir,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		t.Fatal(err)
	}
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		t.Fatal(err)
	}
	if err := (&MigrationBundleReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	for _, setup := range []func(ctrl.Manager) error{
		(&keystonewebhook.MigrationBundleValidator{}).SetupWithManager,
		(&keystonewebhook.SchemaDefinitionValidator{}).SetupWithManager,
		(&keystonewebhook.AuditEntryValidator{}).SetupWithManager,
	} {
		if err := setup(mgr); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	exited := make(chan error, 1)
	go func() { exited <- mgr.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("manager exit: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("manager did not stop")
		}
	}()
	for _, path := range []string{"/healthz", "/readyz"} {
		waitBundleAdmission(t, ctx, func() error {
			response, err := (&http.Client{Timeout: time.Second}).Get("http://" + probeAddress + path)
			if err != nil {
				return err
			}
			if err := response.Body.Close(); err != nil {
				return err
			}
			if response.StatusCode != http.StatusOK {
				return fmt.Errorf("%s returned %d", path, response.StatusCode)
			}
			return nil
		})
		t.Logf("manager boot: %s HTTP 200", path)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "admission-source"},
		Data: map[string]string{"001.up.sql": "SELECT 1;"}}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatal(err)
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: cm.Namespace, Name: "admission-bundle"},
		Spec: keystonev1alpha1.MigrationBundleSpec{Version: "v1", Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{Name: cm.Name, FilePattern: "*.up.sql"}},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"module": "admission"}}},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(bundle)
	waitBundleAdmission(t, ctx, func() error {
		if err := k8s.Get(ctx, key, bundle); err != nil {
			return err
		}
		if bundle.Status.ContentHashVersion != "v1" || bundle.Status.ObservedGeneration != bundle.Generation {
			return fmt.Errorf("v1 not pinned")
		}
		return nil
	})
	cm.Data["001.up.sql"] = "SELECT 2;"
	if err := k8s.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	// Unchanged-spec dry-run must admit despite external source drift.
	if err := k8s.Update(ctx, bundle.DeepCopy(), client.DryRunAll); err != nil {
		t.Fatalf("server-side unchanged-spec dry-run blocked: %v", err)
	}
	edit := bundle.DeepCopy()
	edit.Spec.MaxConcurrentExecutions = 1
	if err := k8s.Update(ctx, edit); err == nil {
		t.Fatal("same-version spec edit with changed SQL admitted")
	}
	edit.Spec.Version = "v2"
	if err := k8s.Update(ctx, edit); err != nil {
		t.Fatalf("version+SQL bump admission: %v", err)
	}
	waitBundleAdmission(t, ctx, func() error {
		if err := k8s.Get(ctx, key, bundle); err != nil {
			return err
		}
		if bundle.Status.ContentHashVersion != "v2" || bundle.Status.ObservedGeneration != bundle.Generation {
			return fmt.Errorf("v2 not pinned")
		}
		return nil
	})
	cm.Data["001.up.sql"] = "DROP TABLE unsafe;" // lint + hash wedge
	if err := k8s.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	bundle.Annotations = map[string]string{"test.keystone/reconcile": "wedged"}
	if err := k8s.Update(ctx, bundle); err != nil {
		t.Fatalf("metadata update of drifted bundle: %v", err)
	}
	waitBundleAdmission(t, ctx, func() error {
		if err := k8s.Get(ctx, key, bundle); err != nil {
			return err
		}
		ready := findCondition(bundle.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
		if ready == nil || ready.Reason != "ContentHashChanged" {
			return fmt.Errorf("bundle not yet wedged")
		}
		return nil
	})
	if err := k8s.Delete(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	waitBundleAdmission(t, ctx, func() error {
		var after keystonev1alpha1.MigrationBundle
		err := k8s.Get(ctx, key, &after)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("wedged bundle still exists: %v", err)
	})
}

func waitBundleAdmission(t *testing.T, ctx context.Context, check func() error) {
	t.Helper()
	for {
		err := check()
		if err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("admission/boot did not converge: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
