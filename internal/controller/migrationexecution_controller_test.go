// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// TestMigrationExecutionReconciler_FinalizerAdded — first reconcile of
// a Pending-phase execution adds the finalizer.
func TestMigrationExecutionReconciler_FinalizerAdded(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &MigrationExecutionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		Resolver:        migration.NewConfigMapResolver(k8s),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exec := &keystonev1alpha1.MigrationExecution{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mx-fin"},
		Spec: keystonev1alpha1.MigrationExecutionSpec{
			PlanRef:       "plan-none",
			BundleRef:     "bundle-none",
			BundleVersion: "v1",
			SchemaRef:     "schema-none",
			ContentHash:   "deadbeef",
		},
	}
	if err := k8s.Create(ctx, exec); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: exec.Namespace, Name: exec.Name}}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Requeue {
		t.Errorf("expected Requeue after finalizer add; got %+v", res)
	}
	var after keystonev1alpha1.MigrationExecution
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&after, keystonev1alpha1.FinalizerMigrationExecution) {
		t.Errorf("finalizer not added; finalizers=%v", after.Finalizers)
	}
}

// TestMigrationExecutionReconciler_BundleNotFound — a freshly-created
// execution whose bundle no longer exists is marked Failed with reason
// BundleNotFound. Covers the case where the bundle was deleted after
// the execution was fanned out.
func TestMigrationExecutionReconciler_BundleNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &MigrationExecutionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		Resolver:        migration.NewConfigMapResolver(k8s),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exec := &keystonev1alpha1.MigrationExecution{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mx-no-bundle"},
		Spec: keystonev1alpha1.MigrationExecutionSpec{
			PlanRef:       "plan-x",
			BundleRef:     "vanished-bundle",
			BundleVersion: "v1",
			SchemaRef:     "schema-x",
			ContentHash:   "deadbeef",
		},
	}
	if err := k8s.Create(ctx, exec); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: exec.Namespace, Name: exec.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatalf("expected bundle-not-found error")
	}
	var after keystonev1alpha1.MigrationExecution
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Pre-flight failures (bundle/schema/provider lookups) surface via
	// Conditions, not Phase. Phase only transitions to Failed once SQL
	// apply itself has been attempted and errored.
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond == nil {
		t.Fatalf("Ready condition not set")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready=%s, want False", cond.Status)
	}
	if cond.Reason != "BundleNotFound" {
		t.Errorf("reason=%q, want BundleNotFound", cond.Reason)
	}
}

// TestMigrationExecutionReconciler_TerminalPhaseSkipsReconcile — an
// execution already at Succeeded/Failed/Aborted must not be reconciled
// further. Important to prevent re-applying SQL against a row already
// recorded in schema_migrations.
func TestMigrationExecutionReconciler_TerminalPhaseSkipsReconcile(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &MigrationExecutionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		Resolver:        migration.NewConfigMapResolver(k8s),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exec := &keystonev1alpha1.MigrationExecution{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mx-terminal"},
		Spec: keystonev1alpha1.MigrationExecutionSpec{
			PlanRef:       "plan-terminal",
			BundleRef:     "bundle-terminal",
			BundleVersion: "v1",
			SchemaRef:     "schema-terminal",
			ContentHash:   "hash1",
		},
	}
	if err := k8s.Create(ctx, exec); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Manually set phase to Succeeded via status subresource.
	var fetched keystonev1alpha1.MigrationExecution
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(exec), &fetched); err != nil {
		t.Fatalf("get: %v", err)
	}
	fetched.Status.Phase = keystonev1alpha1.ExecutionPhaseSucceeded
	if err := k8s.Status().Update(ctx, &fetched); err != nil {
		t.Fatalf("status update: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: exec.Namespace, Name: exec.Name}}
	// Reconcile — must short-circuit with no-op since terminal.
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Errorf("reconcile should no-op on terminal phase; got err=%v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("reconcile should return zero Result on terminal phase; got %+v", res)
	}
	// Phase stays Succeeded.
	var after keystonev1alpha1.MigrationExecution
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status.Phase != keystonev1alpha1.ExecutionPhaseSucceeded {
		t.Errorf("phase changed from Succeeded to %q", after.Status.Phase)
	}
}

// TestMigrationExecutionReconciler_DeletionClearsFinalizer — deletion
// of an execution always clears the finalizer without external cleanup.
// Executions are audit records, never rolled back.
func TestMigrationExecutionReconciler_DeletionClearsFinalizer(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &MigrationExecutionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		Resolver:        migration.NewConfigMapResolver(k8s),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exec := &keystonev1alpha1.MigrationExecution{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "keystone-system",
			Name:       "mx-del",
			Finalizers: []string{keystonev1alpha1.FinalizerMigrationExecution},
		},
		Spec: keystonev1alpha1.MigrationExecutionSpec{
			PlanRef:       "plan-del",
			BundleRef:     "bundle-del",
			BundleVersion: "v1",
			SchemaRef:     "schema-del",
			ContentHash:   "h",
		},
	}
	if err := k8s.Create(ctx, exec); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := k8s.Delete(ctx, exec); err != nil {
		t.Fatalf("delete: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: exec.Namespace, Name: exec.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile during deletion: %v", err)
	}
	var fetched keystonev1alpha1.MigrationExecution
	if err := k8s.Get(ctx, req.NamespacedName, &fetched); !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound after cleanup; got %v", err)
	}
}

// TestMigrationExecutionReconciler_AwaitsPlanApproval — when the
// referenced MigrationPlan has spec.approved=false, the execution
// stays pending with an AwaitingPlanApproval condition rather than
// racing to apply SQL against a disapproved plan.
func TestMigrationExecutionReconciler_AwaitsPlanApproval(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create bundle + unapproved plan. No ConfigMap needed — the
	// gate fires BEFORE the source resolver runs.
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "bundle-gated",
			Labels:    map[string]string{"tier": "prod"},
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-ignored", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}

	plan := &keystonev1alpha1.MigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "plan-gated"},
		Spec: keystonev1alpha1.MigrationPlanSpec{
			BundleRef:     "bundle-gated",
			BundleVersion: "v1",
			SchemaRef:     "schema-gated",
			Approved:      false, // gate CLOSED
		},
	}
	if err := k8s.Create(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	r := &MigrationExecutionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		Resolver:        migration.NewConfigMapResolver(k8s),
		SystemNamespace: "keystone-system",
	}
	exec := &keystonev1alpha1.MigrationExecution{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mx-gated"},
		Spec: keystonev1alpha1.MigrationExecutionSpec{
			PlanRef:       "plan-gated",
			BundleRef:     "bundle-gated",
			BundleVersion: "v1",
			SchemaRef:     "schema-gated",
			ContentHash:   "h",
		},
	}
	if err := k8s.Create(ctx, exec); err != nil {
		t.Fatalf("create exec: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: exec.Namespace, Name: exec.Name}}

	// Finalizer, then gate check.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("gate reconcile: %v (should not error, just requeue)", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected non-zero RequeueAfter to poll for plan approval; got %+v", res)
	}

	var after keystonev1alpha1.MigrationExecution
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status.Phase == keystonev1alpha1.ExecutionPhaseSucceeded ||
		after.Status.Phase == keystonev1alpha1.ExecutionPhaseRunning {
		t.Errorf("execution must not advance past gate; got phase=%s", after.Status.Phase)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Reason != "AwaitingPlanApproval" {
		t.Errorf("Reason=%q, want AwaitingPlanApproval", cond.Reason)
	}
}

// TestMigrationExecutionReconciler_AutoSnapshotOnSuccess — B4: a
// successful markSucceeded creates a SchemaSnapshot CR with the
// deterministic auto-<bundle>-<version>-<schema> name.
func TestMigrationExecutionReconciler_AutoSnapshotOnSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r := &MigrationExecutionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		Resolver:        migration.NewConfigMapResolver(k8s),
		SystemNamespace: "keystone-system",
	}

	exec := &keystonev1alpha1.MigrationExecution{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "mx-snap",
		},
		Spec: keystonev1alpha1.MigrationExecutionSpec{
			PlanRef:       "plan-snap",
			BundleRef:     "bundle-snap",
			BundleVersion: "v42",
			SchemaRef:     "schema-snap",
			ContentHash:   "h",
		},
	}
	if err := k8s.Create(ctx, exec); err != nil {
		t.Fatalf("create exec: %v", err)
	}

	// Drive the execution straight through markSucceeded — bypass
	// Reconcile so we don't need a real database.
	if _, err := r.markSucceeded(ctx, exec, nil, 0, "TestApplied", "test"); err != nil {
		t.Fatalf("markSucceeded: %v", err)
	}

	expected := autoSnapshotName("bundle-snap", "v42", "schema-snap")
	var snap keystonev1alpha1.SchemaSnapshot
	if err := k8s.Get(ctx, types.NamespacedName{
		Namespace: "keystone-system", Name: expected,
	}, &snap); err != nil {
		t.Fatalf("expected auto-snapshot %q: %v", expected, err)
	}
	if snap.Spec.SchemaRef != "schema-snap" {
		t.Errorf("schemaRef=%q, want schema-snap", snap.Spec.SchemaRef)
	}
	if !strings.Contains(snap.Spec.Reason, "bundle-snap@v42") {
		t.Errorf("reason=%q should name the source bundle@version", snap.Spec.Reason)
	}
	// Second call must be idempotent — AlreadyExists is swallowed.
	if _, err := r.markSucceeded(ctx, exec, nil, 0, "TestApplied", "retry"); err != nil {
		t.Fatalf("second markSucceeded must not error on existing snapshot: %v", err)
	}
}
