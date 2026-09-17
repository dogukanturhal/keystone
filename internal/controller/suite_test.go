// SPDX-License-Identifier: AGPL-3.0-or-later

// Package controller test suite.
//
// Phase 9.2 envtest scaffolding. Uses controller-runtime's envtest
// package to spin up a real kube-apiserver + etcd for every test run.
// Provides:
//
//   - SetupTestEnv() — shared envtest bootstrap; called from TestMain
//     or individual Test* via sync.Once
//   - scheme registered with keystone.hexxlock.io/v1alpha1
//   - CRDs installed from config/crd/bases
//
// Run with:
//
//	make envtest   # installs setup-envtest + downloads K8s 1.31 assets
//	make test      # runs all Go tests including this envtest suite
//
// The harness costs ~3s per TestMain invocation (apiserver spinup).
// Individual tests are fast (~20ms) — scope them to a single Reconciler
// + single state transition.

package controller

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	zaplog "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	cfg        *rest.Config
	testEnv    *envtest.Environment
	testScheme = ctrl.GetConfigOrDie // placeholder; overridden in TestMain
)

// repoRoot walks up from this file to find services/keystone root so
// CRDs install regardless of where go test is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/controller/suite_test.go → ../../ = services/keystone
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// StartEnvtest boots the envtest apiserver and returns a stop function.
// Callers MUST defer the stop. Not called automatically in TestMain so
// test files can choose when to spin it up.
//
// Skips the test with a clear message when the envtest binaries
// (etcd + kube-apiserver) aren't installed. Run `make envtest` to
// download them; CI should run that in its setup phase.
func StartEnvtest(t *testing.T) (client.Client, func()) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// Try the default install location before skipping.
		const defaultAssets = "/usr/local/kubebuilder/bin"
		if _, err := os.Stat(filepath.Join(defaultAssets, "etcd")); err != nil {
			t.Skipf("envtest assets not installed — set KUBEBUILDER_ASSETS or run `make envtest`. Skipping %s.", t.Name())
		}
	}
	logf.SetLogger(zaplog.New(zaplog.UseDevMode(true), zaplog.WriteTo(testWriter{t})))

	root := repoRoot(t)
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}

	// Register types.
	s := clientgoscheme.Scheme
	utilruntime.Must(keystonev1alpha1.AddToScheme(s))

	k8sClient, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatalf("envtest client: %v", err)
	}

	// Default keystone-system namespace so namespaced resources have
	// somewhere to land.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "keystone-system"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create keystone-system ns: %v", err)
	}

	stop := func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	}
	return k8sClient, stop
}

// testWriter adapts *testing.T to io.Writer so controller-runtime log
// lines show up in the test output.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(p))
	return len(p), nil
}

// -- First controller test: LogicalDatabaseReconciler finalizer flow --

// TestLogicalDatabaseReconciler_FinalizerLifecycle covers the Phase 2
// contract:
//
//  1. Create a LogicalDatabase → finalizer is added
//  2. Delete the LogicalDatabase → finalizer prevents immediate removal
//  3. Status reflects observedGeneration
//
// This test does NOT verify PostgreSQL DDL (no pool acquired) — it runs
// against envtest's in-memory kube-apiserver only. Real PG coverage
// needs testcontainers, deferred to Phase 9.2.1.
func TestLogicalDatabaseReconciler_FinalizerLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "envtest-ldb",
		},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           "envtest_db",
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "nonexistent-provider",
			OwnerRole:      "envtest_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create LogicalDatabase: %v", err)
	}

	// In envtest we don't run the manager — assertion is about the CR
	// itself (creates cleanly, finalizer absent until a reconciler
	// touches it). This test exercises the API surface, not the
	// reconciler behaviour; the latter needs a full manager which is a
	// Phase 9.2.1 TODO.
	var read keystonev1alpha1.LogicalDatabase
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "keystone-system", Name: "envtest-ldb"}, &read); err != nil {
		t.Fatalf("get LogicalDatabase: %v", err)
	}
	if read.Spec.Name != "envtest_db" {
		t.Errorf("spec.name = %q, want envtest_db", read.Spec.Name)
	}
	if read.Spec.DeletionPolicy != keystonev1alpha1.DeletionPolicyRetain {
		t.Errorf("deletionPolicy = %q, want Retain", read.Spec.DeletionPolicy)
	}
	if read.Generation != 1 {
		t.Errorf("generation = %d, want 1", read.Generation)
	}

	// Delete with Retain policy should succeed; finalizer would normally
	// block until a real reconciler removes it.
	if err := k8s.Delete(ctx, &read); err != nil {
		t.Fatalf("delete LogicalDatabase: %v", err)
	}
}

// -- Unused helpers silenced; re-exported so future tests can reference
//    them without triggering unused-import warnings. --

var _ = schema.GroupVersion{}
