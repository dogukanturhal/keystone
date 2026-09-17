// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/dogukanturhal/keystone-sdk/go/declarative"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// TestSchemaDefinitionReconciler_SchemaRefNotFound — a SchemaDefinition
// whose spec.schemaRef points at a non-existent DatabaseSchema is
// marked Ready=False with reason SchemaRefNotFound.
func TestSchemaDefinitionReconciler_SchemaRefNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &SchemaDefinitionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "sd-missing"},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef:        "no-such-schema",
			ApplyStrategy:    "versioned",
			AllowDestructive: false,
			Tables: []keystonev1alpha1.DesiredTable{
				{
					Name: "placeholder",
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "id", Type: "uuid", PrimaryKey: true, Nullable: false},
					},
				},
			},
		},
	}
	if err := k8s.Create(ctx, sd); err != nil {
		t.Fatalf("create: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: sd.Namespace, Name: sd.Name}}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatalf("expected SchemaRefNotFound error")
	}
	var after keystonev1alpha1.SchemaDefinition
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready=False; got %+v", cond)
	}
	if cond != nil && cond.Reason != "SchemaRefNotFound" {
		t.Errorf("reason=%q, want SchemaRefNotFound", cond.Reason)
	}
}

// TestSchemaDefinitionReconciler_WaitsForSchemaReady — when the parent
// DatabaseSchema exists but isn't Ready, reconciler requeues without
// touching the apiserver beyond the wait.
func TestSchemaDefinitionReconciler_WaitsForSchemaReady(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &SchemaDefinitionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	schema := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "sch-pending"},
		Spec: keystonev1alpha1.DatabaseSchemaSpec{
			Name: "s_pending", LogicalDatabaseRef: "ldb-any",
			OwnerRole:      "s_pending_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "sd-wait-schema"},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef:        "sch-pending",
			ApplyStrategy:    "versioned",
			AllowDestructive: false,
			Tables: []keystonev1alpha1.DesiredTable{
				{
					Name: "placeholder",
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "id", Type: "uuid", PrimaryKey: true, Nullable: false},
					},
				},
			},
		},
	}
	if err := k8s.Create(ctx, sd); err != nil {
		t.Fatalf("create sd: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: sd.Namespace, Name: sd.Name}}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter when schema not Ready; got %+v", res)
	}
}

// TestSchemaDefinitionReconciler_DeletionIsNoop — SchemaDefinition
// deletion is a no-op: owner refs on generated children handle cleanup.
func TestSchemaDefinitionReconciler_DeletionIsNoop(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &SchemaDefinitionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "keystone-system",
			Name:              "sd-delete",
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef: "any", ApplyStrategy: "versioned",
			Tables: []keystonev1alpha1.DesiredTable{
				{
					Name: "placeholder",
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "id", Type: "uuid", PrimaryKey: true, Nullable: false},
					},
				},
			},
		},
	}
	// DeletionTimestamp cannot be set directly on Create — simulate the
	// deletion path by creating then deleting; finalizer is absent so
	// the delete is immediate but we can still reconcile a fetched copy.
	sd.DeletionTimestamp = nil
	if err := k8s.Create(ctx, sd); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := k8s.Delete(ctx, sd); err != nil {
		t.Fatalf("delete: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "keystone-system", Name: "sd-delete"}}
	// Reconcile after deletion — the Get returns NotFound, reconcile is a no-op.
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Errorf("reconcile after deletion should not error; got %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("deletion reconcile should be no-op; got %+v", res)
	}
}

// TestSchemaDefinitionReconciler_LaneConditionsFollowTerminalPath —
// Inspected / DiffComputed / BundleGenerated are pipeline stages, not
// latches. Every terminal path must restate all three so that they and
// Ready always describe the same reconcile.
//
// Regression: only the bundle-emit lane wrote them, so any path that
// finished without emitting left the last drifted reconcile's values
// frozen on status. Observed 2026-08-06 on downstream-service-public-desired,
// which carried Ready=True [SchemaMatchesDesired] beside
// DiffComputed=True [DriftDetected] "174 statement(s) to apply" and a
// BundleGenerated=True naming a bundle CurrentBundleRef had already
// been cleared of. LastTransitionTime does not disambiguate — it only
// moves on a status flip — so all four conditions read as equally
// current and an operator cannot tell converged from 174 ops behind.
func TestSchemaDefinitionReconciler_LaneConditionsFollowTerminalPath(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &SchemaDefinitionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The exact shape the bundle-emit lane leaves behind, and that each
	// terminal path below has to overwrite rather than inherit.
	staleFromEmitLane := func() []metav1.Condition {
		return []metav1.Condition{
			{
				Type: keystonev1alpha1.ConditionTypeInspected, Status: metav1.ConditionTrue,
				Reason:             "LiveSchemaRead",
				Message:            "inspected 1 schema(s); canonical downstream-service-public carries 174 ops",
				LastTransitionTime: metav1.Now(),
			},
			{
				Type: keystonev1alpha1.ConditionTypeDiffComputed, Status: metav1.ConditionTrue,
				Reason:             "DriftDetected",
				Message:            "174 statement(s) to apply (0 destructive)",
				LastTransitionTime: metav1.Now(),
			},
			{
				Type: keystonev1alpha1.ConditionTypeBundleGenerated, Status: metav1.ConditionTrue,
				Reason:             "BundleEmitted",
				Message:            `MigrationBundle "downstream-service-public-desired-9f2c1a" tracks apply across 1 schema(s)`,
				LastTransitionTime: metav1.Now(),
			},
		}
	}

	// seed creates an SD already carrying the stale emit-lane status.
	seed := func(t *testing.T, name string) *keystonev1alpha1.SchemaDefinition {
		t.Helper()
		sd := &keystonev1alpha1.SchemaDefinition{
			ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: name},
			Spec: keystonev1alpha1.SchemaDefinitionSpec{
				SchemaRef: "downstream-service-public", ApplyStrategy: "versioned",
				Tables: []keystonev1alpha1.DesiredTable{
					{
						Name: "placeholder",
						Columns: []keystonev1alpha1.DesiredColumn{
							{Name: "id", Type: "uuid", PrimaryKey: true, Nullable: false},
						},
					},
				},
			},
		}
		if err := k8s.Create(ctx, sd); err != nil {
			t.Fatalf("create: %v", err)
		}
		sd.Status.Conditions = staleFromEmitLane()
		sd.Status.CurrentBundleRef = "downstream-service-public-desired-9f2c1a"
		sd.Status.PendingOperations = 174
		sd.Status.MatchedSchemas = 1
		if err := k8s.Status().Update(ctx, sd); err != nil {
			t.Fatalf("seed status: %v", err)
		}
		return sd
	}

	// wantCond asserts one condition's status and reason on the
	// persisted object, so the test covers the patch as well as the
	// in-memory mutation.
	wantCond := func(t *testing.T, name, condType string, status metav1.ConditionStatus, reason string) {
		t.Helper()
		var after keystonev1alpha1.SchemaDefinition
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: "keystone-system", Name: name}, &after); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		c := findCondition(after.Status.Conditions, condType)
		if c == nil {
			t.Errorf("%s: condition %s absent", name, condType)
			return
		}
		if c.Status != status || c.Reason != reason {
			t.Errorf("%s: %s = %s/%s, want %s/%s",
				name, condType, c.Status, c.Reason, status, reason)
		}
	}

	t.Run("converged", func(t *testing.T) {
		sd := seed(t, "sd-lane-ready")
		if _, err := r.markReady(ctx, sd, 1, logf.Log); err != nil {
			t.Fatalf("markReady: %v", err)
		}
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeReady, metav1.ConditionTrue, "SchemaMatchesDesired")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeInspected, metav1.ConditionTrue, "LiveSchemaRead")
		// True, not False: the differ ran and returned an empty plan.
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeDiffComputed, metav1.ConditionTrue, "NoDrift")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeBundleGenerated, metav1.ConditionFalse, "NoBundleNeeded")

		// The message is what an operator actually reads; the stale op
		// count must be gone from it, not merely contradicted by Ready.
		var after keystonev1alpha1.SchemaDefinition
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: sd.Namespace, Name: sd.Name}, &after); err != nil {
			t.Fatalf("get: %v", err)
		}
		if c := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeDiffComputed); c != nil {
			if strings.Contains(c.Message, "174") {
				t.Errorf("DiffComputed still reports the stale plan: %q", c.Message)
			}
		}
	})

	t.Run("no matching schemas", func(t *testing.T) {
		sd := seed(t, "sd-lane-nomatch")
		if _, err := r.markNoMatches(ctx, sd); err != nil {
			t.Fatalf("markNoMatches: %v", err)
		}
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeReady, metav1.ConditionFalse, "NoMatchingSchemas")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeInspected, metav1.ConditionFalse, "NoMatchingSchemas")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeDiffComputed, metav1.ConditionFalse, "NoMatchingSchemas")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeBundleGenerated, metav1.ConditionFalse, "NoMatchingSchemas")
	})

	t.Run("destructive refused", func(t *testing.T) {
		sd := seed(t, "sd-lane-destructive")
		plan := &declarative.Plan{
			Statements:     []string{"DROP TABLE public.legacy", "DROP COLUMN"},
			DestructiveOps: 2,
		}
		cause := &declarative.ErrDestructiveRefused{Count: 2, Plan: plan}
		if _, err := r.markDestructiveBlocked(ctx, sd, plan, cause); err != nil {
			t.Fatalf("markDestructiveBlocked: %v", err)
		}
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeReady, metav1.ConditionFalse, "DestructiveRefused")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeInspected, metav1.ConditionTrue, "LiveSchemaRead")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeDiffComputed, metav1.ConditionTrue, "DestructiveRefused")
		// CurrentBundleRef is cleared on this path; BundleGenerated has
		// to follow it down or status claims a bundle that is not there.
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeBundleGenerated, metav1.ConditionFalse, "DestructiveRefused")
	})

	t.Run("mixed versions refused", func(t *testing.T) {
		sd := seed(t, "sd-lane-mixed")
		results := []schemaDiff{
			{schema: &keystonev1alpha1.DatabaseSchema{
				ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "tenant-a"},
			}, plan: &declarative.Plan{}},
			{schema: &keystonev1alpha1.DatabaseSchema{
				ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "tenant-b"},
			}, plan: &declarative.Plan{Statements: []string{"ALTER TABLE t ADD COLUMN c text"}}},
		}
		drifted := []keystonev1alpha1.DriftedSchema{{SchemaRef: "tenant-b", PendingOperations: 1}}
		if _, err := r.markDriftRefused(ctx, sd, results, drifted); err != nil {
			t.Fatalf("markDriftRefused: %v", err)
		}
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeReady, metav1.ConditionFalse, "MixedVersionsDetected")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeInspected, metav1.ConditionTrue, "LiveSchemaRead")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeDiffComputed, metav1.ConditionTrue, "MixedVersionsDetected")
		wantCond(t, sd.Name, keystonev1alpha1.ConditionTypeBundleGenerated, metav1.ConditionFalse, "MixedVersionsDetected")
	})

	// The steady-state no-op gate in markReady compares the whole status
	// before and after. Restating four conditions every tick must not
	// defeat it, or a converged SD writes to the apiserver hourly for
	// nothing — the exact regression the change-only gate was added for.
	t.Run("converged twice does not rewrite", func(t *testing.T) {
		sd := seed(t, "sd-lane-noop")
		if _, err := r.markReady(ctx, sd, 1, logf.Log); err != nil {
			t.Fatalf("first markReady: %v", err)
		}
		var first keystonev1alpha1.SchemaDefinition
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: sd.Namespace, Name: sd.Name}, &first); err != nil {
			t.Fatalf("get: %v", err)
		}
		second := first.DeepCopy()
		if _, err := r.markReady(ctx, second, 1, logf.Log); err != nil {
			t.Fatalf("second markReady: %v", err)
		}
		var after keystonev1alpha1.SchemaDefinition
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: sd.Namespace, Name: sd.Name}, &after); err != nil {
			t.Fatalf("get: %v", err)
		}
		if after.ResourceVersion != first.ResourceVersion {
			t.Errorf("second markReady wrote (resourceVersion %s → %s); expected the no-op gate to hold",
				first.ResourceVersion, after.ResourceVersion)
		}
	})
}
