// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
)

// TestMigrationBundleReconciler_SourceResolutionFailure — when the
// ConfigMap referenced by spec.source doesn't exist, the bundle is
// marked Ready=False with reason=SourceResolutionFailed.
func TestMigrationBundleReconciler_SourceResolutionFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-missing-cm"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name:        "does-not-exist",
					FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"module": "none"},
			},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}
	// Finalizer add.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	// Source resolution step → fails.
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatalf("expected error on missing ConfigMap")
	}

	var after keystonev1alpha1.MigrationBundle
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
	if cond.Reason != "SourceResolutionFailed" {
		t.Errorf("reason=%q, want SourceResolutionFailed", cond.Reason)
	}
}

// TestMigrationBundleReconciler_LintFailsOnDestructiveSQL — SQL that
// contains a DROP TABLE (no-drop-table analyzer, severity=error) must
// populate LintFindings and block the bundle with LintFailed.
func TestMigrationBundleReconciler_LintFailsOnDestructiveSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-sql-drops"},
		Data: map[string]string{
			"001_drops.up.sql": "DROP TABLE users;",
		},
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create cm: %v", err)
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-lint-fails"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name:        "mb-sql-drops",
					FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"module": "none"},
			},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatalf("expected error from lint")
	}

	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(after.Status.LintFindings) == 0 {
		t.Errorf("expected lintFindings to be populated, got empty")
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready should be False after LintFailed; got %+v", cond)
	}
	if cond != nil && cond.Reason != "LintFailed" {
		t.Errorf("reason=%q, want LintFailed", cond.Reason)
	}
	// Sanity: at least one finding should be the no-drop-table rule.
	hasDrop := false
	for _, f := range after.Status.LintFindings {
		if f.Rule == "no-drop-table" {
			hasDrop = true
			break
		}
	}
	if !hasDrop {
		t.Errorf("expected a no-drop-table finding; got %+v", after.Status.LintFindings)
	}
}

// TestMigrationBundleReconciler_RecordOnlyLiftsLintBlock — the same
// destructive SQL that LintFailed-blocks an Apply bundle proceeds to
// fan-out when executionMode=RecordOnly (adoption baseline, ADR 0027):
// the SQL never executes, so execution-safety lint is advisory.
// Findings must still land in status.lintFindings, and the created
// MigrationExecution must carry the frozen RecordOnly mode.
func TestMigrationBundleReconciler_RecordOnlyLiftsLintBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-adopt-baseline"},
		Data: map[string]string{
			// Destructive on purpose: lints at severity=error.
			"0001_baseline.up.sql": "DROP TABLE users;",
		},
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create cm: %v", err)
	}
	schema := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "adopt-schema",
			Labels:    map[string]string{"module": "adopt"},
		},
		Spec: keystonev1alpha1.DatabaseSchemaSpec{
			Name:               "adopt_schema",
			LogicalDatabaseRef: "adopt-ldb",
			OwnerRole:          "adopt_owner",
			DeletionPolicy:     keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-adopt"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:       "0001",
			Strategy:      keystonev1alpha1.StrategyVersioned,
			ExecutionMode: keystonev1alpha1.ExecutionModeRecordOnly,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name:        "mb-adopt-baseline",
					FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"module": "adopt"},
			},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("RecordOnly bundle must not LintFail; got %v", err)
	}

	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	// Findings stay visible as advisory.
	if len(after.Status.LintFindings) == 0 {
		t.Errorf("expected advisory lintFindings to be populated, got empty")
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond != nil && cond.Reason == "LintFailed" {
		t.Errorf("RecordOnly bundle must not be LintFailed; got %+v", cond)
	}

	// Fan-out happened and the mode is frozen onto the execution.
	var exec keystonev1alpha1.MigrationExecution
	execKey := types.NamespacedName{
		Namespace: "keystone-system",
		Name:      executionName(bundle.Name, bundle.Spec.Version, schema.Name),
	}
	if err := k8s.Get(ctx, execKey, &exec); err != nil {
		t.Fatalf("expected MigrationExecution %s to exist: %v", execKey.Name, err)
	}
	if exec.Spec.ExecutionMode != keystonev1alpha1.ExecutionModeRecordOnly {
		t.Errorf("execution mode=%q, want RecordOnly", exec.Spec.ExecutionMode)
	}
}

// TestMigrationBundleReconciler_EmptySelectorRefused — the reconciler
// refuses to fan out when SchemaSelector resolves to the empty
// everything-matcher. Avoids accidental scan of every DatabaseSchema.
func TestMigrationBundleReconciler_EmptySelectorRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-sql-empty-sel"},
		Data: map[string]string{
			"001_init.up.sql": "SELECT 1;",
		},
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create cm: %v", err)
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-empty-sel"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name:        "mb-sql-empty-sel",
					FilePattern: "*.up.sql",
				},
			},
			// Empty selector — the reconciler should refuse this.
			SchemaSelector: metav1.LabelSelector{},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatalf("expected error on empty selector")
	}
	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready should be False; got %+v", cond)
	}
	if cond != nil && cond.Reason != "EmptySelector" {
		t.Errorf("reason=%q, want EmptySelector", cond.Reason)
	}
}

// TestMigrationBundleReconciler_FinalizerAddedAndCleared — cover the
// add-finalizer path and the deletion path (finalizer removed without
// cascading to executions).
func TestMigrationBundleReconciler_FinalizerAddedAndCleared(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-fin"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name:        "whatever",
					FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"module": "noop"},
			},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&after, keystonev1alpha1.FinalizerMigrationBundle) {
		t.Fatalf("finalizer not added")
	}
	// Delete — finalizer holds it.
	if err := k8s.Delete(ctx, &after); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Reconcile during deletion clears the finalizer.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile during deletion: %v", err)
	}
	if err := k8s.Get(ctx, req.NamespacedName, &after); err == nil {
		t.Errorf("expected NotFound after finalizer cleanup; got %+v", after)
	}
}

// TestMigrationBundleReconciler_IntegrityValid — a bundle whose source
// carries a correct keystone.sum flips ConditionTypeIntegrityVerified
// to True with reason=SumValid and populates status.integrity.files.
func TestMigrationBundleReconciler_IntegrityValid(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sqlFiles := map[string]string{
		"001_init.up.sql": "-- clean migration\nCREATE TABLE demo_ok (id uuid PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now());",
	}
	sum := string(migration.MarshalSum(migration.BuildSum(sqlFiles)))
	data := make(map[string]string, len(sqlFiles)+1)
	for k, v := range sqlFiles {
		data[k] = v
	}
	data[migration.SumFilename] = sum

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-integrity-ok"},
		Data:       data,
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create CM: %v", err)
	}

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-integrity-ok"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-integrity-ok", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"module": "nomatch"}},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}

	// Finalizer, then main reconcile. Controller exits on the empty-
	// selector guard for this bundle; the integrity condition is set
	// before that guard, so we get the signal we want.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	_, _ = r.Reconcile(ctx, req)

	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}

	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeIntegrityVerified)
	if cond == nil {
		t.Fatal("IntegrityVerified condition not set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("IntegrityVerified=%s, want True", cond.Status)
	}
	if cond.Reason != keystonev1alpha1.ReasonSumValid {
		t.Errorf("Reason=%q, want %q", cond.Reason, keystonev1alpha1.ReasonSumValid)
	}
	if after.Status.Integrity == nil {
		t.Fatal("status.integrity not populated")
	}
	if after.Status.Integrity.Status != string(migration.SumStatusValid) {
		t.Errorf("status.integrity.status=%q, want Valid", after.Status.Integrity.Status)
	}
	if len(after.Status.Integrity.Files) != 1 {
		t.Errorf("expected 1 integrity file entry, got %d", len(after.Status.Integrity.Files))
	}
}

// TestMigrationBundleReconciler_IntegrityMismatch — a tampered bundle
// flips ConditionTypeIntegrityVerified to False with reason=SumMismatch
// and makes the reconcile Ready=False.
func TestMigrationBundleReconciler_IntegrityMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Stage 1: build sum against the clean content.
	files := map[string]string{
		"001_init.up.sql": "-- clean migration\nCREATE TABLE demo_mm (id uuid PRIMARY KEY);",
	}
	sum := string(migration.MarshalSum(migration.BuildSum(files)))
	// Stage 2: swap the SQL. ConfigMap now ships a stale sum.
	files["001_init.up.sql"] = "DROP TABLE demo_mm;"
	data := map[string]string{migration.SumFilename: sum}
	for k, v := range files {
		data[k] = v
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-integrity-mm"},
		Data:       data,
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create CM: %v", err)
	}

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-integrity-mm"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-integrity-mm", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"module": "nomatch"}},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	// Main reconcile returns an error via r.fail — expected.
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("expected mismatch reconcile to return error")
	}

	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}

	iv := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeIntegrityVerified)
	if iv == nil {
		t.Fatal("IntegrityVerified condition missing")
	}
	if iv.Status != metav1.ConditionFalse || iv.Reason != keystonev1alpha1.ReasonSumMismatch {
		t.Errorf("IntegrityVerified=%+v, want False/SumMismatch", iv)
	}

	ready := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready should be False on mismatch; got %+v", ready)
	}

	if after.Status.Integrity == nil ||
		after.Status.Integrity.Status != string(migration.SumStatusMismatch) {
		t.Errorf("status.integrity should report Mismatch; got %+v", after.Status.Integrity)
	}
	if len(after.Status.Integrity.Files) != 0 {
		t.Errorf("Mismatch must not publish per-file list (stale data); got %d entries",
			len(after.Status.Integrity.Files))
	}
}

// TestMigrationBundleReconciler_ApprovalMissing — a bundle matched
// by a SchemaPolicy that requires approvals, with no approver
// annotations, must flip ConditionTypeApproved to False and the
// reconcile must fail fast without fanning out executions.
func TestMigrationBundleReconciler_ApprovalMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// SchemaPolicy: tier=prod needs 1 DBA approval.
	policy := &keystonev1alpha1.SchemaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-dba"},
		Spec: keystonev1alpha1.SchemaPolicySpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			ApprovalPolicies: []keystonev1alpha1.ApprovalPolicy{{
				Name:              "dba",
				RequiredApprovers: 1,
				FromGroups:        []string{"dba"},
			}},
		},
	}
	if err := k8s.Create(ctx, policy); err != nil {
		t.Fatalf("create policy: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-appr-missing"},
		Data: map[string]string{
			"001_init.up.sql": "-- clean\nCREATE TABLE demo_appr (id uuid PRIMARY KEY);",
		},
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create CM: %v", err)
	}

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "mb-appr-missing",
			Labels:    map[string]string{"tier": "prod"},
			// No approval annotations → under-quorum.
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-appr-missing", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"module": "nomatch"}},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	// Main reconcile returns an error via r.fail — expected.
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("expected reconcile error on under-quorum approvals")
	}

	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	ca := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeApproved)
	if ca == nil {
		t.Fatal("Approved condition not set")
	}
	if ca.Status != metav1.ConditionFalse {
		t.Errorf("Approved=%s, want False", ca.Status)
	}
	if after.Status.Approval == nil {
		t.Fatal("status.approval not populated")
	}
	if after.Status.Approval.Satisfied {
		t.Error("status.approval.satisfied should be false")
	}
	if len(after.Status.Approval.Policies) != 1 {
		t.Errorf("expected 1 policy entry, got %d", len(after.Status.Approval.Policies))
	}
}

// TestMigrationBundleReconciler_ApprovalSatisfied — a bundle with
// sufficient approvers flips ConditionTypeApproved to True and the
// reconcile proceeds past the approval gate.
func TestMigrationBundleReconciler_ApprovalSatisfied(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	policy := &keystonev1alpha1.SchemaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-dba-sat"},
		Spec: keystonev1alpha1.SchemaPolicySpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			ApprovalPolicies: []keystonev1alpha1.ApprovalPolicy{{
				Name:                 "dba",
				RequiredApprovers:    1,
				FromGroups:           []string{"dba"},
				DisallowSelfApproval: true,
			}},
		},
	}
	if err := k8s.Create(ctx, policy); err != nil {
		t.Fatalf("create policy: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-appr-sat"},
		Data: map[string]string{
			"001_init.up.sql": "-- clean\nCREATE TABLE demo_appr_sat (id uuid PRIMARY KEY);",
		},
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create CM: %v", err)
	}

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "mb-appr-sat",
			Labels:    map[string]string{"tier": "prod"},
			Annotations: map[string]string{
				keystonev1alpha1.AnnotationAuthor:                     "alice",
				keystonev1alpha1.AnnotationApprovalPrefix + "dba.bob": "dba",
			},
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-appr-sat", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"module": "nomatch"}},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	_, _ = r.Reconcile(ctx, req) // main reconcile; empty-selector stop after approval passes.

	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	ca := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeApproved)
	if ca == nil {
		t.Fatal("Approved condition not set")
	}
	if ca.Status != metav1.ConditionTrue {
		t.Errorf("Approved=%s, want True (approval should pass)", ca.Status)
	}
	if after.Status.Approval == nil || !after.Status.Approval.Satisfied {
		t.Errorf("status.approval should be Satisfied=true; got %+v", after.Status.Approval)
	}
}

// TestMigrationBundleReconciler_SchemaProgressPopulated — a non-
// staged bundle matching multiple tenant schemas populates the new
// B2 Status.SchemaProgress slice with per-schema entries, each
// carrying the tenant-id label value.
func TestMigrationBundleReconciler_SchemaProgressPopulated(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Clean ConfigMap + two tenant schemas, each labelled.
	sqlFiles := map[string]string{
		"001_init.up.sql": "-- clean\nCREATE TABLE demo_tenants (id uuid PRIMARY KEY);",
	}
	sum := string(migration.MarshalSum(migration.BuildSum(sqlFiles)))
	cmData := map[string]string{migration.SumFilename: sum}
	for k, v := range sqlFiles {
		cmData[k] = v
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-tenants"},
		Data:       cmData,
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create CM: %v", err)
	}
	// Two schemas; module=tenants, tenant-id unique per schema.
	// Spec.Name follows PG identifier rules (underscore-only); the CR
	// metadata.name can differ and may contain hyphens.
	for _, tc := range []struct{ metaName, sqlName, tenant string }{
		{"tenants-acme", "tenants_acme", "acme"},
		{"tenants-globex", "tenants_globex", "globex"},
	} {
		sch := &keystonev1alpha1.DatabaseSchema{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "keystone-system",
				Name:      tc.metaName,
				Labels: map[string]string{
					"module":                       "tenants",
					keystonev1alpha1.LabelTenantID: tc.tenant,
				},
			},
			Spec: keystonev1alpha1.DatabaseSchemaSpec{
				LogicalDatabaseRef: "ldb-none",
				Name:               tc.sqlName,
				OwnerRole:          "tenants_owner",
			},
		}
		if err := k8s.Create(ctx, sch); err != nil {
			t.Fatalf("create schema %s: %v", tc.metaName, err)
		}
	}

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-tenants"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-tenants", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"module": "tenants"},
			},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("main reconcile: %v", err)
	}

	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(after.Status.SchemaProgress) != 2 {
		t.Fatalf("expected 2 SchemaProgress entries; got %d: %+v",
			len(after.Status.SchemaProgress), after.Status.SchemaProgress)
	}
	tenants := map[string]bool{}
	for _, sp := range after.Status.SchemaProgress {
		if sp.TenantID == "" {
			t.Errorf("SchemaProgress for %q missing tenantID label", sp.SchemaRef)
		}
		tenants[sp.TenantID] = true
		if sp.ExecutionRef == "" {
			t.Errorf("SchemaProgress for %q missing executionRef", sp.SchemaRef)
		}
	}
	if !tenants["acme"] || !tenants["globex"] {
		t.Errorf("expected both tenants represented; got %v", tenants)
	}
}

// TestMigrationBundleReconciler_MaxConcurrentExecutionsThrottles —
// when spec.maxConcurrentExecutions=1 and two schemas match, the
// first reconcile creates one Execution and marks the second as
// "Throttled" in SchemaProgress.
func TestMigrationBundleReconciler_MaxConcurrentExecutionsThrottles(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sqlFiles := map[string]string{
		"001_init.up.sql": "-- clean\nCREATE TABLE demo_throttle (id uuid PRIMARY KEY);",
	}
	sum := string(migration.MarshalSum(migration.BuildSum(sqlFiles)))
	cmData := map[string]string{migration.SumFilename: sum}
	for k, v := range sqlFiles {
		cmData[k] = v
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-throttle"},
		Data:       cmData,
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create CM: %v", err)
	}
	for _, tc := range []struct{ metaName, sqlName string }{
		{"throttle-a", "throttle_a"},
		{"throttle-b", "throttle_b"},
	} {
		sch := &keystonev1alpha1.DatabaseSchema{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "keystone-system",
				Name:      tc.metaName,
				Labels:    map[string]string{"module": "throttle"},
			},
			Spec: keystonev1alpha1.DatabaseSchemaSpec{
				LogicalDatabaseRef: "ldb-none",
				Name:               tc.sqlName,
				OwnerRole:          "throttle_owner",
			},
		}
		if err := k8s.Create(ctx, sch); err != nil {
			t.Fatalf("create schema %s: %v", tc.metaName, err)
		}
	}

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-throttle"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:                 "v1",
			Strategy:                keystonev1alpha1.StrategyVersioned,
			MaxConcurrentExecutions: 1,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-throttle", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"module": "throttle"},
			},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("main reconcile: %v", err)
	}

	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	var pending, throttled int
	for _, sp := range after.Status.SchemaProgress {
		switch sp.Phase {
		case string(keystonev1alpha1.ExecutionPhasePending):
			pending++
		case "Throttled":
			throttled++
		}
	}
	if pending != 1 || throttled != 1 {
		t.Errorf("expected 1 Pending + 1 Throttled; got pending=%d throttled=%d (entries: %+v)",
			pending, throttled, after.Status.SchemaProgress)
	}
}

// TestMigrationBundleReconciler_IntegrityMissingAdvisory — a bundle
// with no keystone.sum still reconciles (Unknown condition), so
// advisory-mode adoption rollout does not regress existing bundles.
func TestMigrationBundleReconciler_IntegrityMissingAdvisory(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-integrity-missing"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_miss (id uuid PRIMARY KEY);",
		},
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create CM: %v", err)
	}

	r := &MigrationBundleReconciler{
		Client:   k8s,
		Scheme:   clientgoscheme.Scheme,
		Recorder: record.NewFakeRecorder(16),
		Resolver: migration.NewConfigMapResolver(k8s),
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "mb-integrity-missing"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-integrity-missing", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"module": "nomatch"}},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer: %v", err)
	}
	_, _ = r.Reconcile(ctx, req)

	var after keystonev1alpha1.MigrationBundle
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	iv := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeIntegrityVerified)
	if iv == nil {
		t.Fatal("IntegrityVerified condition missing")
	}
	if iv.Status != metav1.ConditionUnknown || iv.Reason != keystonev1alpha1.ReasonSumMissing {
		t.Errorf("expected Unknown/SumMissing, got %s/%s", iv.Status, iv.Reason)
	}
}

// TestCapLintFindings_NoTruncationBelowCap — input under cap is returned
// verbatim, including the original ordering and content.
func TestCapLintFindings_NoTruncationBelowCap(t *testing.T) {
	in := make([]keystonev1alpha1.LintFinding, lintFindingsStatusCap)
	for i := range in {
		in[i] = keystonev1alpha1.LintFinding{
			Rule:     "test-rule",
			Severity: keystonev1alpha1.LintLevelWarning,
			File:     "001.sql",
			Line:     int32(i + 1),
			Message:  "ok",
		}
	}
	out := capLintFindings(in)
	if len(out) != lintFindingsStatusCap {
		t.Fatalf("len(out)=%d, want %d (no truncation)", len(out), lintFindingsStatusCap)
	}
	for i := range out {
		if out[i].Line != int32(i+1) {
			t.Errorf("ordering broken at %d: line=%d", i, out[i].Line)
		}
	}
}

// TestCapLintFindings_TruncatesAboveCap — when input exceeds the cap,
// the slice is shortened and the LAST entry becomes a synthetic
// "findings-truncated" warning that records the elided count.
//
// Regression test for the 2026-05-04 OOM-aftermath bug where the
// MigrationBundle reconciler tried to patch 529 lintFindings into
// status.lintFindings (CRD MaxItems=512), got rejected by the
// apiserver, and looped forever on the patch error — blocking
// MigrationPlan creation for the e2e tenant SD with 187 tables / 446
// ops / 529 unbounded-VARCHAR findings.
func TestCapLintFindings_TruncatesAboveCap(t *testing.T) {
	in := make([]keystonev1alpha1.LintFinding, lintFindingsStatusCap+17)
	for i := range in {
		in[i] = keystonev1alpha1.LintFinding{
			Rule:     "no-varchar-without-limit",
			Severity: keystonev1alpha1.LintLevelWarning,
			File:     "001_declarative_diff.up.sql",
			Line:     int32(i + 1),
			Message:  "unbounded VARCHAR",
		}
	}
	out := capLintFindings(in)
	if len(out) != lintFindingsStatusCap {
		t.Fatalf("len(out)=%d, want %d (truncated to cap)", len(out), lintFindingsStatusCap)
	}
	// First N-1 entries preserve original content + ordering.
	for i := 0; i < lintFindingsStatusCap-1; i++ {
		if out[i].Rule != "no-varchar-without-limit" {
			t.Errorf("entry %d Rule=%q, want original", i, out[i].Rule)
		}
		if out[i].Line != int32(i+1) {
			t.Errorf("entry %d Line=%d, want %d", i, out[i].Line, i+1)
		}
	}
	// Last entry is the synthetic truncation marker.
	last := out[lintFindingsStatusCap-1]
	if last.Rule != "findings-truncated" {
		t.Errorf("last.Rule=%q, want findings-truncated", last.Rule)
	}
	if last.Severity != keystonev1alpha1.LintLevelWarning {
		t.Errorf("last.Severity=%q, want warning", last.Severity)
	}
	// 17 over the cap → 17 elided + 1 marker takes the (cap-1)th slot →
	// dropped count published in the marker is (input - (cap - 1)) = 18.
	wantDropped := len(in) - (lintFindingsStatusCap - 1)
	if !strings.Contains(last.Message, "18 further finding(s) elided") {
		t.Errorf("last.Message=%q, want to contain '%d further finding(s) elided'", last.Message, wantDropped)
	}
}

// TestCapLintFindings_NilInput — defensive: nil/empty input must not
// panic and must return a non-truncated empty result.
func TestCapLintFindings_NilInput(t *testing.T) {
	if out := capLintFindings(nil); len(out) != 0 {
		t.Fatalf("len(capLintFindings(nil))=%d, want 0", len(out))
	}
	if out := capLintFindings([]keystonev1alpha1.LintFinding{}); len(out) != 0 {
		t.Fatalf("len(capLintFindings(empty))=%d, want 0", len(out))
	}
}

// TestMigrationBundleReconciler_SuccessNoOpSkipsPatch (KS#5 follow-up) —
// the change-only gate in success() must:
//
//   - On first call (transition into Ready=True): persist a status patch
//     that bumps resourceVersion AND stamps LastPlanTime.
//   - On second call with identical inputs (no-op): NOT bump
//     resourceVersion, NOT change LastPlanTime, AND return the long
//     readyRequeue.
//
// Rationale: a status patch on every tick fires
// SchemaDefinition.Owns(MigrationBundle) and re-runs that controller's
// markReady, which used to stamp LastDiffTime + emit "schema at
// desired state" + create an AuditEntry CR. Two-layer churn from one
// root cause. Documented in keystone#5.
//
// We test success() directly with a fake client rather than the full
// Reconcile() flow because the change-only gate is the load-bearing
// invariant — exercising it without the analyzer/approval/fanout
// machinery isolates the regression surface.
func TestMigrationBundleReconciler_SuccessNoOpSkipsPatch(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := keystonev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add keystone scheme: %v", err)
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "keystone-system",
			Name:       "mb-noop",
			Generation: 1,
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keystonev1alpha1.MigrationBundle{}).
		WithObjects(bundle).
		Build()

	r := &MigrationBundleReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(16),
	}
	ctx := context.Background()
	logger := logr.Discard()

	// Re-fetch: WithObjects assigns initial resourceVersion=999 by default.
	var got keystonev1alpha1.MigrationBundle
	if err := c.Get(ctx, types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}, &got); err != nil {
		t.Fatalf("initial get: %v", err)
	}
	rvBefore := got.ResourceVersion

	// First call — Ready transitions from absent → True. Status MUST patch.
	res, err := r.success(ctx, &got, "hash-1", 2, 2, logger)
	if err != nil {
		t.Fatalf("first success(): %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("expected non-zero RequeueAfter on Ready path; got %v", res.RequeueAfter)
	}
	// Bound by readyRequeue ± jitter.
	maxReady := readyRequeue + readyRequeue*requeueJitterPct/100
	if res.RequeueAfter > maxReady {
		t.Errorf("first RequeueAfter %v exceeds readyRequeue+jitter ceiling %v", res.RequeueAfter, maxReady)
	}

	var afterFirst keystonev1alpha1.MigrationBundle
	if err := c.Get(ctx, types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}, &afterFirst); err != nil {
		t.Fatalf("get after first: %v", err)
	}
	if afterFirst.ResourceVersion == rvBefore {
		t.Errorf("first success() did not bump resourceVersion (was %s, still %s) — change should have persisted",
			rvBefore, afterFirst.ResourceVersion)
	}
	if afterFirst.Status.LastPlanTime == nil {
		t.Fatalf("first success() did not stamp LastPlanTime")
	}
	if afterFirst.Status.AppliedSchemas != 2 || afterFirst.Status.MatchedSchemas != 2 {
		t.Errorf("first success() did not persist matched/applied counts: status=%+v", afterFirst.Status)
	}
	rvAfterFirst := afterFirst.ResourceVersion
	stampAfterFirst := afterFirst.Status.LastPlanTime.DeepCopy()

	// Second call with identical inputs — MUST be a no-op against the apiserver.
	res2, err := r.success(ctx, &afterFirst, "hash-1", 2, 2, logger)
	if err != nil {
		t.Fatalf("second success(): %v", err)
	}
	if res2.RequeueAfter <= 0 {
		t.Errorf("expected non-zero RequeueAfter on second call; got %v", res2.RequeueAfter)
	}
	if res2.RequeueAfter > maxReady {
		t.Errorf("second RequeueAfter %v exceeds readyRequeue+jitter ceiling %v", res2.RequeueAfter, maxReady)
	}

	var afterSecond keystonev1alpha1.MigrationBundle
	if err := c.Get(ctx, types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}, &afterSecond); err != nil {
		t.Fatalf("get after second: %v", err)
	}
	if afterSecond.ResourceVersion != rvAfterFirst {
		t.Errorf("second success() bumped resourceVersion (%s → %s) — no-op gate failed",
			rvAfterFirst, afterSecond.ResourceVersion)
	}
	if !afterSecond.Status.LastPlanTime.Equal(stampAfterFirst) {
		t.Errorf("second success() rewrote LastPlanTime (was %v, now %v) — should only stamp on real change",
			stampAfterFirst, afterSecond.Status.LastPlanTime)
	}
}

// TestMigrationBundleReconciler_SuccessChangePatches — counterpart to
// the no-op test: a real input change (matched 1→2, applied 1→2) must
// persist + bump the LastPlanTime stamp.
func TestMigrationBundleReconciler_SuccessChangePatches(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	if err := keystonev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add keystone scheme: %v", err)
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "keystone-system",
			Name:       "mb-change",
			Generation: 1,
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keystonev1alpha1.MigrationBundle{}).
		WithObjects(bundle).
		Build()
	r := &MigrationBundleReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(16),
	}
	ctx := context.Background()
	logger := logr.Discard()

	var got keystonev1alpha1.MigrationBundle
	if err := c.Get(ctx, types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}, &got); err != nil {
		t.Fatalf("initial get: %v", err)
	}
	// First pass: still applying (matched=2, applied=1). RequeueAfter
	// must bound at applyingRequeue + jitter (NOT readyRequeue).
	if _, err := r.success(ctx, &got, "hash-A", 2, 1, logger); err != nil {
		t.Fatalf("first success(): %v", err)
	}

	var midway keystonev1alpha1.MigrationBundle
	if err := c.Get(ctx, types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}, &midway); err != nil {
		t.Fatalf("get midway: %v", err)
	}
	stampA := midway.Status.LastPlanTime.DeepCopy()

	// Second pass with state change (applied 1→2). MUST patch + stamp.
	// Tiny sleep so metav1.Now() advances past the prior stamp's
	// second-resolution truncation.
	time.Sleep(1100 * time.Millisecond)
	res, err := r.success(ctx, &midway, "hash-A", 2, 2, logger)
	if err != nil {
		t.Fatalf("second success(): %v", err)
	}
	maxReady := readyRequeue + readyRequeue*requeueJitterPct/100
	if res.RequeueAfter > maxReady {
		t.Errorf("Ready-path RequeueAfter %v exceeds ceiling %v", res.RequeueAfter, maxReady)
	}
	// Should be at least 1.5× applyingRequeue → ensures we're in the
	// readyRequeue band, not still in applyingRequeue.
	minReady := readyRequeue - readyRequeue*requeueJitterPct/100
	if res.RequeueAfter < minReady {
		t.Errorf("Ready-path RequeueAfter %v below readyRequeue-jitter floor %v — wrong cadence",
			res.RequeueAfter, minReady)
	}

	var afterSecond keystonev1alpha1.MigrationBundle
	if err := c.Get(ctx, types.NamespacedName{Namespace: bundle.Namespace, Name: bundle.Name}, &afterSecond); err != nil {
		t.Fatalf("get after second: %v", err)
	}
	if afterSecond.Status.LastPlanTime.Equal(stampA) {
		t.Errorf("real state change did not bump LastPlanTime (still %v)", stampA)
	}
	if afterSecond.Status.AppliedSchemas != 2 {
		t.Errorf("expected AppliedSchemas=2 after change; got %d", afterSecond.Status.AppliedSchemas)
	}
}

// TestRequeueWithJitter_BoundedDeviation — sanity-bound the jitter
// helper at ±requeueJitterPct% of the input. Shields against an
// accidental rewrite that drifts the ceiling.
func TestRequeueWithJitter_BoundedDeviation(t *testing.T) {
	d := 5 * time.Minute
	min := d - d*requeueJitterPct/100 - 1
	max := d + d*requeueJitterPct/100 + 1
	for i := 0; i < 200; i++ {
		got := requeueWithJitter(d)
		if got < min || got > max {
			t.Errorf("iter %d: requeueWithJitter(%v)=%v outside [%v,%v]", i, d, got, min, max)
		}
	}
	if got := requeueWithJitter(0); got != 0 {
		t.Errorf("requeueWithJitter(0)=%v, want 0", got)
	}
}
