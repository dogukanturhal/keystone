// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// minimalBundle returns a versioned-strategy bundle referencing a
// (dummy, not-resolved) ConfigMap. The plan reconciler only needs the
// bundle's labels + PolicyRef to resolve the approval mode; the
// source does not need to actually exist.
func minimalBundle(name string, labels map[string]string, policyRef string) *keystonev1alpha1.MigrationBundle {
	return &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      name,
			Labels:    labels,
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:   "v1",
			Strategy:  keystonev1alpha1.StrategyVersioned,
			PolicyRef: policyRef,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-" + name, FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
}

// minimalPlan returns a plan referencing the given bundle name.
// spec.approved defaults to false.
func minimalPlan(name, bundleRef string) *keystonev1alpha1.MigrationPlan {
	return &keystonev1alpha1.MigrationPlan{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      name,
		},
		Spec: keystonev1alpha1.MigrationPlanSpec{
			BundleRef:     bundleRef,
			BundleVersion: "v1",
			SchemaRef:     "some-schema",
		},
	}
}

// TestMigrationPlanReconciler_AutoApprovesByDefault — when no
// SchemaPolicy declares planApproval=Manual, the reconciler flips
// spec.approved=true and sets condition Approved=True.
func TestMigrationPlanReconciler_AutoApprovesByDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bundle := minimalBundle("b-auto", map[string]string{"tier": "dev"}, "")
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	plan := minimalPlan("p-auto", "b-auto")
	if err := k8s.Create(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	r := &MigrationPlanReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: plan.Namespace, Name: plan.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Second reconcile after spec mutation settles status.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	var after keystonev1alpha1.MigrationPlan
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !after.Spec.Approved {
		t.Errorf("spec.approved should be true in Auto mode")
	}
	if after.Status.ApprovalMode != keystonev1alpha1.PlanApprovalAuto {
		t.Errorf("status.approvalMode=%q, want Auto", after.Status.ApprovalMode)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeApproved)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Approved condition should be True; got %+v", cond)
	}
}

// TestMigrationPlanReconciler_ManualPolicyBlocks — when a
// SchemaPolicy matches with planApproval=Manual, spec.approved stays
// false until flipped by an external agent.
func TestMigrationPlanReconciler_ManualPolicyBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	policy := &keystonev1alpha1.SchemaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-manual"},
		Spec: keystonev1alpha1.SchemaPolicySpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			PlanApproval:   keystonev1alpha1.PlanApprovalManual,
		},
	}
	if err := k8s.Create(ctx, policy); err != nil {
		t.Fatalf("create policy: %v", err)
	}

	bundle := minimalBundle("b-manual", map[string]string{"tier": "prod"}, "")
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	plan := minimalPlan("p-manual", "b-manual")
	if err := k8s.Create(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	r := &MigrationPlanReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: plan.Namespace, Name: plan.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var after keystonev1alpha1.MigrationPlan
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Spec.Approved {
		t.Errorf("spec.approved should stay false in Manual mode")
	}
	if after.Status.ApprovalMode != keystonev1alpha1.PlanApprovalManual {
		t.Errorf("status.approvalMode=%q, want Manual", after.Status.ApprovalMode)
	}
	if after.Status.ApprovalPolicyName != "prod-manual" {
		t.Errorf("status.approvalPolicyName=%q, want prod-manual", after.Status.ApprovalPolicyName)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeApproved)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Approved condition should be False; got %+v", cond)
	}
}

// TestMigrationPlanReconciler_ManualFlipApprovesRecognized — after
// an external agent (operator / CI) flips spec.approved=true on a
// Manual-mode plan, the next reconcile updates the Approved
// condition to True.
func TestMigrationPlanReconciler_ManualFlipApprovesRecognized(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	policy := &keystonev1alpha1.SchemaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "flip-manual"},
		Spec: keystonev1alpha1.SchemaPolicySpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			PlanApproval:   keystonev1alpha1.PlanApprovalManual,
		},
	}
	if err := k8s.Create(ctx, policy); err != nil {
		t.Fatalf("create policy: %v", err)
	}

	bundle := minimalBundle("b-flip", map[string]string{"tier": "prod"}, "")
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	plan := minimalPlan("p-flip", "b-flip")
	if err := k8s.Create(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	r := &MigrationPlanReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: plan.Namespace, Name: plan.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile pre-flip: %v", err)
	}

	// Simulate `keystonectl plan approve` — external spec.approved flip.
	var p keystonev1alpha1.MigrationPlan
	if err := k8s.Get(ctx, req.NamespacedName, &p); err != nil {
		t.Fatalf("get: %v", err)
	}
	p.Spec.Approved = true
	if err := k8s.Update(ctx, &p); err != nil {
		t.Fatalf("flip approval: %v", err)
	}

	// Reconcile picks up the flip.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile post-flip: %v", err)
	}

	var after keystonev1alpha1.MigrationPlan
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !after.Spec.Approved {
		t.Fatal("spec.approved should have been kept true")
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeApproved)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Approved condition should be True post-flip; got %+v", cond)
	}
}
