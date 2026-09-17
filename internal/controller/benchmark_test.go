//go:build benchmark

// SPDX-License-Identifier: AGPL-3.0-or-later

// Phase S11 — integration benchmarks.
//
// Build tag `benchmark` keeps these out of the default `go test` path;
// they need envtest's kube-apiserver + etcd and take ~30s+ per
// iteration. Run via:
//
//	make bench-integration
//
// or directly:
//
//	GOWORK=off go test -tags=benchmark -bench=. -benchmem \
//	    -count=3 -run=^$ ./internal/controller/...
//
// What's measured
//
//  1. LogicalDatabaseReconciler reconcile latency at N=1, 10, 100, 500
//     concurrent CRs. No real PostgreSQL — we exercise the
//     pre-pool-acquire paths (provider lookup, credentials resolution,
//     status patch). Same path the 9.2 envtest suite already hits.
//  2. MigrationBundleReconciler fan-out when SchemaSelector matches 100
//     DatabaseSchemas. Measures list + per-schema execution create.
//
// Output is benchstat-friendly. Use:
//
//	go install golang.org/x/perf/cmd/benchstat@latest
//	./bin/benchstat before.txt after.txt
//
// to compare runs across a change.

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// -----------------------------------------------------------------------------
// LogicalDatabase reconcile latency
// -----------------------------------------------------------------------------

// BenchmarkLogicalDatabaseReconcile measures the wall-clock latency of
// a single LogicalDatabase reconcile against an envtest kube-apiserver.
// The provider reference is intentionally dangling so the reconciler
// reaches the ProviderNotFound error path — same code path the 9.2
// test suite exercises, which is what we want for a deterministic
// microbenchmark (no real PG → no network jitter).
func BenchmarkLogicalDatabaseReconcile(b *testing.B) {
	for _, n := range []int{1, 10, 100, 500} {
		n := n
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			benchReconcileLogicalDatabases(b, n)
		})
	}
}

func benchReconcileLogicalDatabases(b *testing.B, n int) {
	b.Helper()
	k8s, stop := startEnvtestBench(b)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	r := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(4096),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}

	prefix := fmt.Sprintf("bench-ldb-n%d-", n)
	reqs := make([]ctrl.Request, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s%04d", prefix, i)
		ldb := &keystonev1alpha1.LogicalDatabase{
			ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: name},
			Spec: keystonev1alpha1.LogicalDatabaseSpec{
				Name:           fmt.Sprintf("bench_db_%04d", i),
				ClusterRef:     "bench-cluster",
				ProviderRef:    "nonexistent-provider",
				OwnerRole:      fmt.Sprintf("bench_owner_%04d", i),
				DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
			},
		}
		if err := k8s.Create(ctx, ldb); err != nil {
			b.Fatalf("create[%d]: %v", i, err)
		}
		reqs[i] = ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: "keystone-system", Name: name,
		}}
	}

	// First reconcile adds the finalizer + requeues. Do that warm-up
	// outside the measured loop so each timed iteration hits the
	// "provider lookup" path uniformly.
	for _, req := range reqs {
		if _, err := r.Reconcile(ctx, req); err != nil {
			b.Fatalf("warmup: %v", err)
		}
	}

	var totalReconciles atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		wg.Add(n)
		for _, req := range reqs {
			req := req
			go func() {
				defer wg.Done()
				_, _ = r.Reconcile(ctx, req)
				totalReconciles.Add(1)
			}()
		}
		wg.Wait()
	}
	b.StopTimer()
	b.ReportMetric(float64(totalReconciles.Load())/b.Elapsed().Seconds(), "reconciles/s")
	b.ReportMetric(float64(n), "CRs")
}

// -----------------------------------------------------------------------------
// MigrationBundle fan-out
// -----------------------------------------------------------------------------

// BenchmarkMigrationBundleFanout measures the cost of matching a
// SchemaSelector against 100 DatabaseSchemas. The bundle's source
// ConfigMap is intentionally missing so the reconciler fails on
// SourceResolutionFailed — we measure list+selector+patch cost, not
// apply cost.
func BenchmarkMigrationBundleFanout(b *testing.B) {
	k8s, stop := startEnvtestBench(b)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const schemaCount = 100
	for i := 0; i < schemaCount; i++ {
		ldb := &keystonev1alpha1.LogicalDatabase{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "keystone-system",
				Name:      fmt.Sprintf("bench-fanout-ldb-%03d", i),
			},
			Spec: keystonev1alpha1.LogicalDatabaseSpec{
				Name:           fmt.Sprintf("fanout_db_%03d", i),
				ClusterRef:     "bench-cluster",
				ProviderRef:    "nonexistent-provider",
				OwnerRole:      fmt.Sprintf("fanout_owner_%03d", i),
				DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
			},
		}
		if err := k8s.Create(ctx, ldb); err != nil {
			b.Fatalf("create ldb %d: %v", i, err)
		}

		ds := &keystonev1alpha1.DatabaseSchema{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "keystone-system",
				Name:      fmt.Sprintf("bench-fanout-ds-%03d", i),
				Labels:    map[string]string{"module": "fanout-bench"},
			},
			Spec: keystonev1alpha1.DatabaseSchemaSpec{
				Name:               fmt.Sprintf("schema_%03d", i),
				LogicalDatabaseRef: ldb.Name,
				OwnerRole:          fmt.Sprintf("schema_owner_%03d", i),
			},
		}
		if err := k8s.Create(ctx, ds); err != nil {
			b.Fatalf("create ds %d: %v", i, err)
		}
	}

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(4096),
		Resolver: migration.NewConfigMapResolver(k8s),
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bundleName := fmt.Sprintf("bench-fanout-bundle-%04d", i)
		bundle := &keystonev1alpha1.MigrationBundle{
			ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: bundleName},
			Spec: keystonev1alpha1.MigrationBundleSpec{
				Version:  "v1",
				Strategy: keystonev1alpha1.StrategyVersioned,
				Source: keystonev1alpha1.MigrationSource{
					Type: keystonev1alpha1.SourceConfigMap,
					ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
						Name:        "bench-missing-cm",
						FilePattern: "*.up.sql",
					},
				},
				SchemaSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{"module": "fanout-bench"},
				},
			},
		}
		if err := k8s.Create(ctx, bundle); err != nil {
			b.Fatalf("create bundle: %v", err)
		}
		req := ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: "keystone-system", Name: bundleName,
		}}
		if _, err := r.Reconcile(ctx, req); err != nil {
			b.Fatalf("warmup reconcile: %v", err)
		}
		_, _ = r.Reconcile(ctx, req)
	}
	b.StopTimer()
	b.ReportMetric(float64(schemaCount), "schemas/match")
}

// -----------------------------------------------------------------------------
// envtest bootstrap (bench flavour)
// -----------------------------------------------------------------------------

// startEnvtestBench mirrors StartEnvtest(*testing.T) for *testing.B. We
// re-implement it here instead of adapting a *testing.B→*testing.T bridge
// so the skip-when-missing contract is explicit and stable.
// Keep this in sync with suite_test.go::StartEnvtest if that function
// changes.
func startEnvtestBench(b *testing.B) (client.Client, func()) {
	b.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		const defaultAssets = "/usr/local/kubebuilder/bin"
		if _, err := os.Stat(filepath.Join(defaultAssets, "etcd")); err != nil {
			b.Skipf("envtest assets not installed — set KUBEBUILDER_ASSETS or run `make envtest`. Skipping %s.", b.Name())
		}
	}

	_, thisFile, _, _ := runtime.Caller(0)
	// internal/controller/benchmark_test.go → ../../ = services/keystone
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		b.Fatalf("envtest start: %v", err)
	}

	s := clientgoscheme.Scheme
	utilruntime.Must(keystonev1alpha1.AddToScheme(s))

	k8sClient, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		b.Fatalf("envtest client: %v", err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "keystone-system"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		b.Fatalf("create namespace: %v", err)
	}

	stop := func() {
		if err := env.Stop(); err != nil {
			b.Logf("envtest stop: %v", err)
		}
	}
	return k8sClient, stop
}
