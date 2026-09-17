// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

func makeProvider(name, secretName string) *keystonev1alpha1.DatabaseProvider {
	return &keystonev1alpha1.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: keystonev1alpha1.DatabaseProviderSpec{
			Engine:              keystonev1alpha1.DatabaseProviderEnginePostgreSQL,
			Host:                "example.db",
			Port:                5432,
			MaintenanceDatabase: "postgres",
			SSLMode:             keystonev1alpha1.DatabaseProviderSSLModeRequire,
			AdminCredentialsRef: keystonev1alpha1.DatabaseProviderCredentials{
				SecretName: secretName,
			},
			PoolMaxConns: 4,
		},
	}
}

func makeAdminSecret(name string, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: name},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte(password),
		},
	}
}

// TestDatabaseProviderReconciler_InitialObservation — first reconcile
// records the Secret's ResourceVersion in status and sets
// CredentialsReady=True. CredentialsRotatedAt stays empty (no rotation
// observed yet).
func TestDatabaseProviderReconciler_InitialObservation(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sec := makeAdminSecret("admin-v1", "initial")
	if err := k8s.Create(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	provider := makeProvider("prov-init", "admin-v1")
	if err := k8s.Create(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	r := &DatabaseProviderReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var after keystonev1alpha1.DatabaseProvider
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status.CredentialsObservedVersion == "" {
		t.Errorf("expected CredentialsObservedVersion to be stamped; got empty")
	}
	if after.Status.CredentialsRotatedAt != nil {
		t.Errorf("no rotation should be recorded on first observation; got %+v", after.Status.CredentialsRotatedAt)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeCredentialsReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("CredentialsReady should be True; got %+v", cond)
	}
}

// TestDatabaseProviderReconciler_RotationDetected — when the Secret
// is updated, the next reconcile records a CredentialsRotatedAt
// timestamp and evicts any cached pools matching the provider.
func TestDatabaseProviderReconciler_RotationDetected(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sec := makeAdminSecret("admin-rot", "pw-v1")
	if err := k8s.Create(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	provider := makeProvider("prov-rot", "admin-rot")
	if err := k8s.Create(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	pools := pg.NewPoolCache()
	r := &DatabaseProviderReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pools,
		SystemNamespace: "keystone-system",
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}

	// First reconcile records the baseline version.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	var initial keystonev1alpha1.DatabaseProvider
	if err := k8s.Get(ctx, req.NamespacedName, &initial); err != nil {
		t.Fatalf("get initial: %v", err)
	}
	if initial.Status.CredentialsRotatedAt != nil {
		t.Fatalf("no rotation should be recorded on first reconcile")
	}

	// Rotate: update the Secret's password.
	var fresh corev1.Secret
	if err := k8s.Get(ctx, types.NamespacedName{
		Namespace: "keystone-system", Name: "admin-rot",
	}, &fresh); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	fresh.Data["password"] = []byte("pw-v2")
	if err := k8s.Update(ctx, &fresh); err != nil {
		t.Fatalf("update secret: %v", err)
	}

	// Second reconcile detects the rotation.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("post-rotation reconcile: %v", err)
	}
	var after keystonev1alpha1.DatabaseProvider
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Status.CredentialsRotatedAt == nil {
		t.Fatal("CredentialsRotatedAt should be stamped after Secret update")
	}
	if after.Status.CredentialsObservedVersion == initial.Status.CredentialsObservedVersion {
		t.Errorf("observed version should advance; stayed %q",
			after.Status.CredentialsObservedVersion)
	}
}

// TestDatabaseProviderReconciler_MissingSecret — an absent Secret
// flips CredentialsReady=False with reason=SecretNotFound; a later
// Secret create recovers on the next reconcile.
func TestDatabaseProviderReconciler_MissingSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	provider := makeProvider("prov-missing", "admin-not-yet")
	if err := k8s.Create(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	r := &DatabaseProviderReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var after keystonev1alpha1.DatabaseProvider
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeCredentialsReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("CredentialsReady should be False when Secret absent; got %+v", cond)
	}
	if cond.Reason != "SecretNotFound" {
		t.Errorf("Reason=%q, want SecretNotFound", cond.Reason)
	}
}

// TestDatabaseProviderReconciler_ConnectionProfileChange — when
// spec.host/spec.port are repointed, the reconciler evicts cached
// pools still dialing the previous endpoint, emits a
// ConnectionProfileChanged event, and stamps the new profile into
// status.observedConnectionProfile. Regression coverage for the
// 2026-06-04 report where a host edit (cluster-DNS → 127.0.0.1:55432)
// left old-endpoint pools cached until operator restart.
func TestDatabaseProviderReconciler_ConnectionProfileChange(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sec := makeAdminSecret("admin-profile", "pw")
	if err := k8s.Create(ctx, sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	provider := makeProvider("prov-profile", "admin-profile") // example.db:5432
	if err := k8s.Create(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	pools := pg.NewPoolCache()
	rec := record.NewFakeRecorder(16)
	r := &DatabaseProviderReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        rec,
		Pools:           pools,
		SystemNamespace: "keystone-system",
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}

	// First reconcile stamps the initial profile.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	var initial keystonev1alpha1.DatabaseProvider
	if err := k8s.Get(ctx, req.NamespacedName, &initial); err != nil {
		t.Fatalf("get initial: %v", err)
	}
	if got := initial.Status.ObservedConnectionProfile; got != "example.db:5432" {
		t.Fatalf("ObservedConnectionProfile=%q, want example.db:5432", got)
	}

	// Seed a pool dialing the ORIGINAL endpoint. Acquire is lazy
	// (MinConns=0, no eager dial) so no live PostgreSQL is needed.
	if _, _, err := pools.Acquire(ctx, pg.PoolConfig{
		Host: "example.db", Port: 5432, Database: "postgres",
		Username: "admin", Password: "pw", SSLMode: "require", MaxConns: 2,
	}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	// Repoint the provider — mirrors a local port-forward verification.
	var fresh keystonev1alpha1.DatabaseProvider
	if err := k8s.Get(ctx, req.NamespacedName, &fresh); err != nil {
		t.Fatalf("get for update: %v", err)
	}
	fresh.Spec.Host = "127.0.0.1"
	fresh.Spec.Port = 55432
	if err := k8s.Update(ctx, &fresh); err != nil {
		t.Fatalf("update provider: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("post-repoint reconcile: %v", err)
	}

	var after keystonev1alpha1.DatabaseProvider
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if got := after.Status.ObservedConnectionProfile; got != "127.0.0.1:55432" {
		t.Errorf("ObservedConnectionProfile=%q, want 127.0.0.1:55432", got)
	}

	// The old-endpoint pool must be evicted; the event carries the count.
	found := false
	for drained := false; !drained; {
		select {
		case ev := <-rec.Events:
			if containsSubstring(ev, "ConnectionProfileChanged") &&
				containsSubstring(ev, "1 stale pool(s) evicted") {
				found = true
			}
		default:
			drained = true
		}
	}
	if !found {
		t.Errorf("expected ConnectionProfileChanged event with 1 evicted pool")
	}
}
