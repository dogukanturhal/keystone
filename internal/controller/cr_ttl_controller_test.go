// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// newCRTTLFakeClient builds a fake Kubernetes client populated with
// keystone + corev1 schemes. Mirrors newRetentionFakeClient (audit
// retention test) so the two retention test suites stay
// shape-compatible.
func newCRTTLFakeClient(objs ...runtime.Object) client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(
			&keystonev1alpha1.SchemaDefinition{},
			&keystonev1alpha1.MigrationBundle{},
			&keystonev1alpha1.MigrationExecution{},
		).
		Build()
}

func mkSD(name, namespace string, success, failure *time.Duration) *keystonev1alpha1.SchemaDefinition {
	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("sd-" + name),
		},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef: "test-schema",
			Tables: []keystonev1alpha1.DesiredTable{{
				Name: "t",
				Columns: []keystonev1alpha1.DesiredColumn{
					{Name: "id", Type: "integer", Nullable: false, PrimaryKey: true},
				},
			}},
		},
	}
	if success != nil || failure != nil {
		sd.Spec.Cleanup = &keystonev1alpha1.CleanupPolicy{}
		if success != nil {
			sd.Spec.Cleanup.RetainSuccessFor = &metav1.Duration{Duration: *success}
		}
		if failure != nil {
			sd.Spec.Cleanup.RetainFailureFor = &metav1.Duration{Duration: *failure}
		}
	}
	return sd
}

func mkBundle(name, namespace, sdName string, ready metav1.ConditionStatus, completedAge time.Duration, matched, applied int32) *keystonev1alpha1.MigrationBundle {
	now := time.Now()
	transition := metav1.NewTime(now.Add(-completedAge))
	b := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: keystonev1alpha1.GroupVersion.String(),
				Kind:       "SchemaDefinition",
				Name:       sdName,
				UID:        types.UID("sd-" + sdName),
				Controller: ptrBool(true),
			}},
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:        "v1",
			Strategy:       keystonev1alpha1.StrategyVersioned,
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"k": "v"}},
		},
		Status: keystonev1alpha1.MigrationBundleStatus{
			MatchedSchemas: matched,
			AppliedSchemas: applied,
			LastPlanTime:   &transition,
			Conditions: []metav1.Condition{{
				Type:               keystonev1alpha1.ConditionTypeReady,
				Status:             ready,
				Reason:             "TestReady",
				Message:            "test",
				LastTransitionTime: transition,
			}},
		},
	}
	return b
}

func mkExec(name, namespace, bundleName, schemaRef string, phase keystonev1alpha1.ExecutionPhase, completedAge time.Duration) *keystonev1alpha1.MigrationExecution {
	now := time.Now()
	completion := metav1.NewTime(now.Add(-completedAge))
	return &keystonev1alpha1.MigrationExecution{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: keystonev1alpha1.GroupVersion.String(),
				Kind:       "MigrationBundle",
				Name:       bundleName,
				UID:        types.UID("mb-" + bundleName),
				Controller: ptrBool(true),
			}},
		},
		Spec: keystonev1alpha1.MigrationExecutionSpec{
			PlanRef:       "plan-1",
			BundleRef:     bundleName,
			BundleVersion: "v1",
			SchemaRef:     schemaRef,
			ContentHash:   "deadbeef",
		},
		Status: keystonev1alpha1.MigrationExecutionStatus{
			Phase:          phase,
			CompletionTime: &completion,
			StartTime:      &completion,
		},
	}
}

func ptrBool(v bool) *bool { return &v }
func ptrDur(d time.Duration) *time.Duration { return &d }

// TestCRTTL_DisabledIsNoOp — feature flag off → no deletes, no errors,
// no list calls beyond the dormant short-circuit. Critical for the
// "default-off, opt-in per cluster" rollout property. A regression here
// would silently GC bundles on every cluster on first deploy of this
// version.
func TestCRTTL_DisabledIsNoOp(t *testing.T) {
	sd := mkSD("billing", "default", nil, nil)
	old := mkBundle("mb-old", "default", "billing", metav1.ConditionTrue, 30*24*time.Hour, 1, 1)
	c := newCRTTLFakeClient(sd, old)

	ctrl := &CRTTLController{
		Client:  c,
		Enabled: false,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	got := &keystonev1alpha1.MigrationBundle{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mb-old"}, got); err != nil {
		t.Fatalf("disabled controller deleted bundle: %v", err)
	}
}

// TestCRTTL_PreservesLatestSucceededBundle — the "always retain latest
// succeeded" invariant. Even when the only Succeeded bundle is older
// than retainSuccessFor, it MUST survive.
func TestCRTTL_PreservesLatestSucceededBundle(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Hour), ptrDur(time.Hour))
	// Single Succeeded bundle, 30 days old (way past 1h retention).
	mb := mkBundle("mb-only", "default", "billing", metav1.ConditionTrue, 30*24*time.Hour, 1, 1)
	c := newCRTTLFakeClient(sd, mb)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Hour,
		DefaultRetainFailure: time.Hour,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	got := &keystonev1alpha1.MigrationBundle{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mb-only"}, got); err != nil {
		t.Fatalf("latest succeeded bundle deleted: %v", err)
	}
}

// TestCRTTL_DeletesOldSucceededKeepsLatest — three Succeeded bundles,
// two old enough to GC, one fresh. The two old ones go; the latest
// (regardless of being old) stays.
func TestCRTTL_DeletesOldSucceededKeepsLatest(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Hour), ptrDur(time.Hour))
	old1 := mkBundle("mb-old1", "default", "billing", metav1.ConditionTrue, 48*time.Hour, 1, 1)
	old2 := mkBundle("mb-old2", "default", "billing", metav1.ConditionTrue, 24*time.Hour, 1, 1)
	latest := mkBundle("mb-latest", "default", "billing", metav1.ConditionTrue, time.Minute, 1, 1)
	c := newCRTTLFakeClient(sd, old1, old2, latest)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Hour,
		DefaultRetainFailure: time.Hour,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	remaining := &keystonev1alpha1.MigrationBundleList{}
	_ = c.List(context.Background(), remaining)
	if len(remaining.Items) != 1 {
		t.Fatalf("expected 1 bundle to remain; got %d", len(remaining.Items))
	}
	if remaining.Items[0].Name != "mb-latest" {
		t.Errorf("wrong bundle preserved: %s (expected mb-latest)", remaining.Items[0].Name)
	}
}

// TestCRTTL_FailureRetentionLongerThanSuccess — Failed bundles use the
// longer failure window. Set success=1h, failure=72h; a 24h-old Failed
// bundle MUST survive while a 24h-old Succeeded sibling (with a fresher
// Succeeded latest also present) is GC'd.
func TestCRTTL_FailureRetentionLongerThanSuccess(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Hour), ptrDur(72*time.Hour))
	failed := mkBundle("mb-failed", "default", "billing", metav1.ConditionFalse, 24*time.Hour, 1, 0)
	oldSucc := mkBundle("mb-old-succ", "default", "billing", metav1.ConditionTrue, 24*time.Hour, 1, 1)
	latest := mkBundle("mb-latest", "default", "billing", metav1.ConditionTrue, time.Minute, 1, 1)
	c := newCRTTLFakeClient(sd, failed, oldSucc, latest)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Hour,
		DefaultRetainFailure: 72 * time.Hour,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	got := &keystonev1alpha1.MigrationBundle{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mb-failed"}, got); err != nil {
		t.Fatalf("failed bundle deleted within failure window: %v", err)
	}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mb-old-succ"}, got)
	if err == nil {
		t.Fatalf("old succeeded bundle was NOT deleted (a fresh Succeeded sibling exists)")
	}
}

// TestCRTTL_NonTerminalIsSkipped — bundles whose Ready condition is
// Unknown (still planning) are skipped regardless of age.
func TestCRTTL_NonTerminalIsSkipped(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Minute), ptrDur(time.Minute))
	planning := mkBundle("mb-planning", "default", "billing", metav1.ConditionUnknown, 30*24*time.Hour, 1, 0)
	c := newCRTTLFakeClient(sd, planning)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Minute,
		DefaultRetainFailure: time.Minute,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	got := &keystonev1alpha1.MigrationBundle{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mb-planning"}, got); err != nil {
		t.Fatalf("non-terminal bundle deleted: %v", err)
	}
}

// TestCRTTL_PerSDOverrideTakesPrecedence — when an SD pins
// retainSuccessFor=1m and the operator-default is 30 days, the SD's
// 1m wins and the bundle is GC'd.
func TestCRTTL_PerSDOverrideTakesPrecedence(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Minute), ptrDur(time.Minute))
	old := mkBundle("mb-old", "default", "billing", metav1.ConditionTrue, time.Hour, 1, 1)
	latest := mkBundle("mb-latest", "default", "billing", metav1.ConditionTrue, 0, 1, 1)
	c := newCRTTLFakeClient(sd, old, latest)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: 30 * 24 * time.Hour, // 30 days, SD's 1m must override
		DefaultRetainFailure: 30 * 24 * time.Hour,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	got := &keystonev1alpha1.MigrationBundle{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mb-old"}, got)
	if err == nil {
		t.Fatalf("per-SD retainSuccessFor=1m did NOT override 30d operator default")
	}
}

// TestCRTTL_ExecutionPhasesGCedCorrectly — Succeeded execution past
// retention window goes; Failed execution within failure window stays;
// in-flight (Running) is never touched.
func TestCRTTL_ExecutionPhasesGCedCorrectly(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Hour), ptrDur(72*time.Hour))
	mb := mkBundle("mb-1", "default", "billing", metav1.ConditionTrue, time.Minute, 1, 1)
	// One latest Succeeded execution (kept).
	latestSucc := mkExec("e-latest", "default", "mb-1", "schema-A", keystonev1alpha1.ExecutionPhaseSucceeded, time.Minute)
	// One old Succeeded execution on same schema (GC'd).
	oldSucc := mkExec("e-old-succ", "default", "mb-1", "schema-A", keystonev1alpha1.ExecutionPhaseSucceeded, 24*time.Hour)
	// One old Failed execution (kept — within 72h failure window).
	oldFail := mkExec("e-old-fail", "default", "mb-1", "schema-B", keystonev1alpha1.ExecutionPhaseFailed, 24*time.Hour)
	// One ancient Failed (GC'd — past 72h failure window).
	ancientFail := mkExec("e-ancient-fail", "default", "mb-1", "schema-C", keystonev1alpha1.ExecutionPhaseFailed, 96*time.Hour)
	// One running (always kept).
	running := mkExec("e-running", "default", "mb-1", "schema-D", keystonev1alpha1.ExecutionPhaseRunning, 30*24*time.Hour)
	c := newCRTTLFakeClient(sd, mb, latestSucc, oldSucc, oldFail, ancientFail, running)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Hour,
		DefaultRetainFailure: 72 * time.Hour,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	cases := []struct {
		name      string
		shouldExist bool
	}{
		{"e-latest", true},      // latest Succeeded — kept
		{"e-old-succ", false},   // old Succeeded sibling — GC'd
		{"e-old-fail", true},    // within failure window
		{"e-ancient-fail", false}, // past failure window
		{"e-running", true},     // non-terminal — kept
	}
	for _, tc := range cases {
		got := &keystonev1alpha1.MigrationExecution{}
		err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: tc.name}, got)
		exists := err == nil
		if exists != tc.shouldExist {
			t.Errorf("%s: existence got %v, want %v (err=%v)", tc.name, exists, tc.shouldExist, err)
		}
	}
}

// TestCRTTL_IdempotentReruns — running runOnce twice on the same
// state must produce the same result on pass 2 (no double-deletes,
// no errors). Critical because a leader-election flap could cause two
// passes in close succession.
func TestCRTTL_IdempotentReruns(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Hour), ptrDur(time.Hour))
	old := mkBundle("mb-old", "default", "billing", metav1.ConditionTrue, 48*time.Hour, 1, 1)
	latest := mkBundle("mb-latest", "default", "billing", metav1.ConditionTrue, time.Minute, 1, 1)
	c := newCRTTLFakeClient(sd, old, latest)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Hour,
		DefaultRetainFailure: time.Hour,
	}
	// Pass 1
	ctrl.runOnce(context.Background(), &discardLogger{})
	// Pass 2 — should be a no-op (the only candidate was the latest, which we keep).
	ctrl.runOnce(context.Background(), &discardLogger{})
	// Pass 3 — still no-op.
	ctrl.runOnce(context.Background(), &discardLogger{})

	remaining := &keystonev1alpha1.MigrationBundleList{}
	_ = c.List(context.Background(), remaining)
	if len(remaining.Items) != 1 {
		t.Fatalf("after 3 idempotent passes expected 1 bundle, got %d", len(remaining.Items))
	}
}

// TestCRTTL_OrphanBundlesUseDefaults — bundles with no SD owner ref
// (legacy, hand-authored, or SD already deleted) fall back to the
// operator-wide defaults.
func TestCRTTL_OrphanBundlesUseDefaults(t *testing.T) {
	// No SD owner ref — orphan bundle.
	orphan := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mb-orphan",
			Namespace: "default",
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:        "v1",
			Strategy:       keystonev1alpha1.StrategyVersioned,
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"k": "v"}},
		},
		Status: keystonev1alpha1.MigrationBundleStatus{
			MatchedSchemas: 1,
			AppliedSchemas: 1,
			LastPlanTime:   &metav1.Time{Time: time.Now().Add(-48 * time.Hour)},
			Conditions: []metav1.Condition{{
				Type:               keystonev1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "AllSchemasApplied",
				Message:            "ok",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
			}},
		},
	}
	// Plus a latest sibling so the orphan isn't preserved as
	// "latest succeeded".
	latest := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mb-latest-orphan",
			Namespace: "default",
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:        "v2",
			Strategy:       keystonev1alpha1.StrategyVersioned,
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"k": "v"}},
		},
		Status: keystonev1alpha1.MigrationBundleStatus{
			MatchedSchemas: 1,
			AppliedSchemas: 1,
			LastPlanTime:   &metav1.Time{Time: time.Now().Add(-time.Minute)},
			Conditions: []metav1.Condition{{
				Type:               keystonev1alpha1.ConditionTypeReady,
				Status:             metav1.ConditionTrue,
				Reason:             "AllSchemasApplied",
				Message:            "ok",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute)),
			}},
		},
	}
	c := newCRTTLFakeClient(orphan, latest)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Hour,
		DefaultRetainFailure: time.Hour,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	got := &keystonev1alpha1.MigrationBundle{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mb-orphan"}, got); err == nil {
		t.Fatalf("orphan bundle past operator-default retention NOT deleted")
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mb-latest-orphan"}, got); err != nil {
		t.Fatalf("latest orphan bundle was deleted (always-keep-latest invariant violated): %v", err)
	}
}

// TestCRTTL_PerSchemaPreservation — when a bundle fans out to N
// schemas and each has its own MigrationExecution, the latest
// Succeeded MUST be retained PER SCHEMA, not globally. Otherwise a
// 4-tenant fanout would lose 3 of 4 last-success records on the first
// pass.
func TestCRTTL_PerSchemaPreservation(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Minute), ptrDur(time.Minute))
	mb := mkBundle("mb-1", "default", "billing", metav1.ConditionTrue, time.Minute, 4, 4)
	// Four schemas, each with one Succeeded execution at varying ages.
	// All are older than retainSuccessFor=1m. Without per-schema
	// preservation, only the absolute newest would survive.
	execs := []*keystonev1alpha1.MigrationExecution{
		mkExec("e-A", "default", "mb-1", "schema-A", keystonev1alpha1.ExecutionPhaseSucceeded, time.Hour),
		mkExec("e-B", "default", "mb-1", "schema-B", keystonev1alpha1.ExecutionPhaseSucceeded, 2*time.Hour),
		mkExec("e-C", "default", "mb-1", "schema-C", keystonev1alpha1.ExecutionPhaseSucceeded, 3*time.Hour),
		mkExec("e-D", "default", "mb-1", "schema-D", keystonev1alpha1.ExecutionPhaseSucceeded, 4*time.Hour),
	}
	objs := []runtime.Object{sd, mb}
	for _, e := range execs {
		objs = append(objs, e)
	}
	c := newCRTTLFakeClient(objs...)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Minute,
		DefaultRetainFailure: time.Minute,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	remaining := &keystonev1alpha1.MigrationExecutionList{}
	_ = c.List(context.Background(), remaining)
	if len(remaining.Items) != 4 {
		names := make([]string, len(remaining.Items))
		for i, e := range remaining.Items {
			names[i] = fmt.Sprintf("%s(%s)", e.Name, e.Spec.SchemaRef)
		}
		t.Fatalf("per-schema preservation violated: expected 4 executions (1 per schema), got %d: %v", len(remaining.Items), names)
	}
}

// TestCRTTL_DeletesSourceConfigMapWithBundle — when sweepBundles
// GC's a MigrationBundle, the associated ConfigMap referenced by
// spec.source.configMapRef must also be deleted. The CM otherwise
// lingers indefinitely because its OwnerReference is the SchemaDefinition
// (not the bundle), so k8s native GC never cascades.
//
// Two bundles, latest is preserved (its CM too); the older bundle is
// GC'd along with its CM.
func TestCRTTL_DeletesSourceConfigMapWithBundle(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Hour), ptrDur(time.Hour))
	old := mkBundleWithCM("mb-old", "default", "billing", "mb-old-sql", metav1.ConditionTrue, 24*time.Hour, 1, 1)
	latest := mkBundleWithCM("mb-latest", "default", "billing", "mb-latest-sql", metav1.ConditionTrue, time.Minute, 1, 1)
	oldCM := mkBundleCM("mb-old-sql", "default")
	latestCM := mkBundleCM("mb-latest-sql", "default")
	c := newCRTTLFakeClient(sd, old, latest, oldCM, latestCM)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Hour,
		DefaultRetainFailure: time.Hour,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	// Old bundle and its CM should be gone.
	gotBundle := &keystonev1alpha1.MigrationBundle{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "mb-old"}, gotBundle); !apierrors.IsNotFound(err) {
		t.Fatalf("old bundle not deleted: %v", err)
	}
	gotCM := &corev1.ConfigMap{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "mb-old-sql"}, gotCM); !apierrors.IsNotFound(err) {
		t.Fatalf("old bundle's source ConfigMap was NOT cascade-deleted: %v", err)
	}

	// Latest bundle and its CM must survive.
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "mb-latest"}, gotBundle); err != nil {
		t.Fatalf("latest bundle was incorrectly deleted: %v", err)
	}
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "mb-latest-sql"}, gotCM); err != nil {
		t.Fatalf("latest bundle's source CM was incorrectly deleted: %v", err)
	}
}

// TestCRTTL_PgrollBundleHasNoConfigMapToDelete — bundles with
// strategy=pgrollExpandContract carry inline Operations and have no
// source ConfigMap; the CM-cleanup must skip gracefully without
// erroring or incrementing the error counter.
func TestCRTTL_PgrollBundleHasNoConfigMapToDelete(t *testing.T) {
	sd := mkSD("billing", "default", ptrDur(time.Hour), ptrDur(time.Hour))
	old := mkBundle("mb-pgroll-old", "default", "billing", metav1.ConditionTrue, 24*time.Hour, 1, 1)
	old.Spec.Strategy = keystonev1alpha1.StrategyPgrollExpandContract
	old.Spec.Source = keystonev1alpha1.MigrationSource{} // no ConfigMapRef
	latest := mkBundleWithCM("mb-latest", "default", "billing", "mb-latest-sql", metav1.ConditionTrue, time.Minute, 1, 1)
	latestCM := mkBundleCM("mb-latest-sql", "default")
	c := newCRTTLFakeClient(sd, old, latest, latestCM)

	ctrl := &CRTTLController{
		Client:               c,
		Enabled:              true,
		DefaultRetainSuccess: time.Hour,
		DefaultRetainFailure: time.Hour,
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	gotBundle := &keystonev1alpha1.MigrationBundle{}
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "mb-pgroll-old"}, gotBundle); !apierrors.IsNotFound(err) {
		t.Fatalf("pgroll bundle not deleted: %v", err)
	}
	// Sanity: latest still alive.
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "mb-latest"}, gotBundle); err != nil {
		t.Fatalf("latest bundle incorrectly deleted: %v", err)
	}
}

// mkBundleWithCM is mkBundle plus a configured spec.source.configMapRef
// so CR-TTL knows which CM to cascade-delete.
func mkBundleWithCM(name, namespace, sdName, cmName string, ready metav1.ConditionStatus, completedAge time.Duration, matched, applied int32) *keystonev1alpha1.MigrationBundle {
	b := mkBundle(name, namespace, sdName, ready, completedAge, matched, applied)
	b.Spec.Source = keystonev1alpha1.MigrationSource{
		Type: keystonev1alpha1.SourceConfigMap,
		ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
			Name:        cmName,
			FilePattern: "*.up.sql",
		},
	}
	return b
}

func mkBundleCM(name, namespace string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				keystonev1alpha1.LabelSource: "declarative-diff",
			},
		},
		Data: map[string]string{"001_declarative_diff.up.sql": "-- test"},
	}
}
