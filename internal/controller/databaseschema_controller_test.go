// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// TestDatabaseSchemaReconciler_AddsFinalizer — first reconcile adds the
// finalizer before any external state is touched.
func TestDatabaseSchemaReconciler_AddsFinalizer(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &DatabaseSchemaReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ds := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "dsf"},
		Spec: keystonev1alpha1.DatabaseSchemaSpec{
			Name:               "tenant_xyz",
			LogicalDatabaseRef: "nonexistent",
			OwnerRole:          "tenant_xyz_owner",
			DeletionPolicy:     keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ds); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Requeue {
		t.Errorf("expected Requeue after finalizer add")
	}
	var after keystonev1alpha1.DatabaseSchema
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&after, keystonev1alpha1.FinalizerDatabaseSchema) {
		t.Errorf("finalizer not added; finalizers=%v", after.Finalizers)
	}
}

// TestDatabaseSchemaReconciler_LogicalDatabaseNotFound surfaces
// LogicalDatabaseNotFound when the parent CR doesn't exist.
func TestDatabaseSchemaReconciler_LogicalDatabaseNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &DatabaseSchemaReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ds := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "ds-no-parent"},
		Spec: keystonev1alpha1.DatabaseSchemaSpec{
			Name:               "tenant_missing_parent",
			LogicalDatabaseRef: "definitely-not-there",
			OwnerRole:          "tmp_owner",
			DeletionPolicy:     keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ds); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}}
	// Reconcile #1 adds the finalizer; #2 self-heals the
	// keystone.hexxlock.io/name label (both return Requeue before the
	// LogicalDatabase lookup runs).
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("settle reconcile %d: %v", i, err)
		}
	}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatalf("expected error when parent LogicalDatabase is missing")
	}

	var after keystonev1alpha1.DatabaseSchema
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond == nil {
		t.Fatalf("Ready condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready=%s, want False", cond.Status)
	}
	if cond.Reason != "LogicalDatabaseNotFound" {
		t.Errorf("reason=%q, want LogicalDatabaseNotFound", cond.Reason)
	}
}

// TestDatabaseSchemaReconciler_WaitsForParentReady — when the parent
// LogicalDatabase exists but isn't Ready, the reconciler MUST surface
// Progressing=True (not Ready=False) and requeue.
func TestDatabaseSchemaReconciler_WaitsForParentReady(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &DatabaseSchemaReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Parent LogicalDatabase exists but has no conditions → not Ready.
	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "pending-parent"},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name: "pending_parent", ClusterRef: "prod-eu-west-1-hub",
			ProviderRef: "noop", OwnerRole: "pending_parent_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create ldb: %v", err)
	}

	ds := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "ds-wait-parent"},
		Spec: keystonev1alpha1.DatabaseSchemaSpec{
			Name:               "tenant_wait",
			LogicalDatabaseRef: "pending-parent",
			OwnerRole:          "tenant_wait_owner",
			DeletionPolicy:     keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ds); err != nil {
		t.Fatalf("create ds: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}}
	// Reconcile #1 adds the finalizer; #2 self-heals the
	// keystone.hexxlock.io/name label (both return Requeue before the
	// wait-for-parent path runs).
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("settle reconcile %d: %v", i, err)
		}
	}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter when parent not Ready; got %+v", res)
	}

	var after keystonev1alpha1.DatabaseSchema
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Progressing should be True, Ready should not be True.
	prog := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeProgressing)
	if prog == nil || prog.Status != metav1.ConditionTrue {
		t.Errorf("Progressing condition not True; got %+v", prog)
	}
	if ready := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady); ready != nil && ready.Status == metav1.ConditionTrue {
		t.Errorf("Ready=True while parent not ready; got %+v", ready)
	}
}

// TestDatabaseSchemaReconciler_DeletionRetainPolicy — Retain policy
// deletion doesn't need any external connectivity to clear finalizer.
func TestDatabaseSchemaReconciler_DeletionRetainPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &DatabaseSchemaReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ds := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "ds-del"},
		Spec: keystonev1alpha1.DatabaseSchemaSpec{
			Name: "tenant_del", LogicalDatabaseRef: "anything",
			OwnerRole:      "tenant_del_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ds); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}

	var fetched keystonev1alpha1.DatabaseSchema
	if err := k8s.Get(ctx, req.NamespacedName, &fetched); err != nil {
		t.Fatalf("pre-delete get: %v", err)
	}
	if err := k8s.Delete(ctx, &fetched); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile during deletion: %v", err)
	}
	if err := k8s.Get(ctx, req.NamespacedName, &fetched); !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound after cleanup; got %v", err)
	}
}

// TestSchemasForProvider_FanoutMapping — the DatabaseProvider watch
// map function resolves the transitive chain (schema →
// spec.logicalDatabaseRef → LogicalDatabase → spec.providerRef) and
// returns exactly the schemas reachable from the changed provider.
func TestSchemasForProvider_FanoutMapping(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	prov := makeProvider("prov-schema-fanout", "fanout-secret")
	if err := k8s.Create(ctx, prov); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "schema-fanout-ldb"},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           "schema_fanout_db",
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "prov-schema-fanout",
			OwnerRole:      "schema_fanout_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create ldb: %v", err)
	}
	makeDS := func(name, ldbRef string) *keystonev1alpha1.DatabaseSchema {
		return &keystonev1alpha1.DatabaseSchema{
			ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: name},
			Spec: keystonev1alpha1.DatabaseSchemaSpec{
				Name:               "fanout_schema",
				LogicalDatabaseRef: ldbRef,
				OwnerRole:          "fanout_schema_owner",
				DeletionPolicy:     keystonev1alpha1.DeletionPolicyRetain,
			},
		}
	}
	for _, ds := range []*keystonev1alpha1.DatabaseSchema{
		makeDS("schema-fanout-hit", "schema-fanout-ldb"),
		makeDS("schema-fanout-miss", "some-other-ldb"),
	} {
		if err := k8s.Create(ctx, ds); err != nil {
			t.Fatalf("create schema %s: %v", ds.Name, err)
		}
	}

	r := &DatabaseSchemaReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	reqs := r.schemasForProvider(ctx, prov)
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 request; got %d (%v)", len(reqs), reqs)
	}
	if reqs[0].Namespace != "keystone-system" || reqs[0].Name != "schema-fanout-hit" {
		t.Errorf("mapped to %s/%s; want keystone-system/schema-fanout-hit",
			reqs[0].Namespace, reqs[0].Name)
	}
}
