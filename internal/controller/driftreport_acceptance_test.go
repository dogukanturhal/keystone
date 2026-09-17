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
	"sigs.k8s.io/controller-runtime/pkg/event"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/conditions"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// The accept-drift annotation is the one DriftReport field an operator is
// expected to edit, and until now editing it produced no response of any
// kind until the next hourly sweep. The contract these tests pin is that
// every request now gets an answer: acceptance deletes the report, and
// anything else says on the report why it did not happen.

// TestAcceptDriftPredicate — the sweep re-patches DriftReport status on
// every pass that re-observes drift. Those writes must not wake the
// acceptance controller, or each one costs an admin inspection per
// report per sweep. Only the annotation appearing or changing counts.
func TestAcceptDriftPredicate(t *testing.T) {
	p := acceptDriftPredicate()

	report := func(annotations map[string]string, observedHash string) *keystonev1alpha1.DriftReport {
		return &keystonev1alpha1.DriftReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "keystone-system", Name: "s-drift", Annotations: annotations,
			},
			Status: keystonev1alpha1.DriftReportStatus{ObservedHash: observedHash},
		}
	}
	accepted := map[string]string{AnnotationAcceptDrift: "true"}

	t.Run("create without the annotation is dropped", func(t *testing.T) {
		if p.Create(event.CreateEvent{Object: report(nil, "aaa")}) {
			t.Error("unannotated create admitted")
		}
	})
	t.Run("create with the annotation is admitted", func(t *testing.T) {
		if !p.Create(event.CreateEvent{Object: report(accepted, "aaa")}) {
			t.Error("annotated create dropped")
		}
	})
	t.Run("sweep status re-patch is dropped", func(t *testing.T) {
		// Same annotations (none), different status — exactly what
		// upsertDriftReport writes on every pass.
		if p.Update(event.UpdateEvent{
			ObjectOld: report(nil, "aaa"),
			ObjectNew: report(nil, "bbb"),
		}) {
			t.Error("sweep status re-patch admitted; this wakes an admin inspection per sweep")
		}
	})
	t.Run("sweep re-patch of an annotated report is dropped", func(t *testing.T) {
		// The annotated-but-not-yet-processed case: acceptance already
		// ran, the annotation is unchanged, only status moved.
		if p.Update(event.UpdateEvent{
			ObjectOld: report(accepted, "aaa"),
			ObjectNew: report(accepted, "bbb"),
		}) {
			t.Error("status-only re-patch of an annotated report admitted")
		}
	})
	t.Run("annotation appearing is admitted", func(t *testing.T) {
		if !p.Update(event.UpdateEvent{
			ObjectOld: report(nil, "aaa"),
			ObjectNew: report(accepted, "aaa"),
		}) {
			t.Error("annotation appearing dropped — this is the whole point of the watch")
		}
	})
	t.Run("annotation value changing is admitted", func(t *testing.T) {
		if !p.Update(event.UpdateEvent{
			ObjectOld: report(map[string]string{AnnotationAcceptDrift: "False"}, "aaa"),
			ObjectNew: report(accepted, "aaa"),
		}) {
			t.Error("annotation correction dropped")
		}
	})
	t.Run("delete is dropped", func(t *testing.T) {
		if p.Delete(event.DeleteEvent{Object: report(accepted, "aaa")}) {
			t.Error("delete admitted; acceptance deletes the report, so this chases its own tail")
		}
	})
}

// TestDriftAcceptance_RefusalsAreVisible — every way an accept-drift
// request can fail short of the re-baseline itself must leave a reason on
// the report. The regression is silence: before this controller existed,
// a wrong value or a dangling schemaRef looked exactly like a correct
// annotation waiting on the sweep, and the only way to tell them apart
// was to wait an hour and look again.
//
// The paths below are the ones reachable without a live PostgreSQL. The
// re-baseline itself is the pre-existing acceptDrift path, unchanged
// here and driven by reconcileSchema.
func TestDriftAcceptance_RefusalsAreVisible(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &DriftAcceptanceReconciler{
		Client: k8s,
		Drift: &DriftController{
			Client:          k8s,
			Scheme:          clientgoscheme.Scheme,
			Recorder:        record.NewFakeRecorder(32),
			Pools:           pg.NewPoolCache(),
			SystemNamespace: "keystone-system",
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	newReport := func(t *testing.T, name, schemaRef string, annotations map[string]string) ctrl.Request {
		t.Helper()
		rep := &keystonev1alpha1.DriftReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "keystone-system", Name: name, Annotations: annotations,
			},
			Spec: keystonev1alpha1.DriftReportSpec{
				DatabaseSchemaRef:  schemaRef,
				LogicalDatabaseRef: "ldb",
				ProviderRef:        "provider",
			},
		}
		if err := k8s.Create(ctx, rep); err != nil {
			t.Fatalf("create report: %v", err)
		}
		return ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: rep.Namespace, Name: rep.Name,
		}}
	}

	acceptedCond := func(t *testing.T, req ctrl.Request) *metav1.Condition {
		t.Helper()
		var after keystonev1alpha1.DriftReport
		if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
			t.Fatalf("get report: %v", err)
		}
		return findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeAccepted)
	}

	t.Run("no annotation is left entirely alone", func(t *testing.T) {
		req := newReport(t, "unannotated-drift", "schema-a", nil)
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if c := acceptedCond(t, req); c != nil {
			t.Errorf("unannotated report gained an Accepted condition: %+v", c)
		}
	})

	t.Run("wrong value is named, not ignored", func(t *testing.T) {
		// "True" is the mistake an operator actually makes. Silently
		// doing nothing is what made this feature feel broken.
		req := newReport(t, "miscased-drift", "schema-a",
			map[string]string{AnnotationAcceptDrift: "True"})
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		c := acceptedCond(t, req)
		if c == nil {
			t.Fatal("no Accepted condition; the operator is back to guessing")
		}
		if c.Status != metav1.ConditionFalse || c.Reason != "MalformedAnnotation" {
			t.Errorf("Accepted = %s/%s, want False/MalformedAnnotation", c.Status, c.Reason)
		}
	})

	t.Run("dangling schemaRef is named", func(t *testing.T) {
		req := newReport(t, "dangling-drift", "no-such-schema",
			map[string]string{AnnotationAcceptDrift: "true"})
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		c := acceptedCond(t, req)
		if c == nil {
			t.Fatal("no Accepted condition")
		}
		if c.Status != metav1.ConditionFalse || c.Reason != "SchemaNotFound" {
			t.Errorf("Accepted = %s/%s, want False/SchemaNotFound", c.Status, c.Reason)
		}
	})

	t.Run("not-Ready schema requeues rather than reporting a rebaseline failure", func(t *testing.T) {
		schema := &keystonev1alpha1.DatabaseSchema{
			ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "schema-pending"},
			Spec: keystonev1alpha1.DatabaseSchemaSpec{
				Name:               "public",
				LogicalDatabaseRef: "ldb",
				OwnerRole:          "keystone_admin",
			},
		}
		if err := k8s.Create(ctx, schema); err != nil {
			t.Fatalf("create schema: %v", err)
		}
		schema.Status.Conditions = conditions.Set(nil,
			keystonev1alpha1.ConditionTypeReady, metav1.ConditionFalse,
			"Provisioning", "not yet", schema.Generation)
		if err := k8s.Status().Update(ctx, schema); err != nil {
			t.Fatalf("seed schema status: %v", err)
		}

		req := newReport(t, "pending-drift", "schema-pending",
			map[string]string{AnnotationAcceptDrift: "true"})
		res, err := r.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Error("expected a requeue; a schema that is not Ready yet is a wait, not a failure")
		}
		c := acceptedCond(t, req)
		if c == nil {
			t.Fatal("no Accepted condition")
		}
		// Distinct from RebaselineFailed on purpose: one is "come back
		// later", the other is "something is wrong".
		if c.Status != metav1.ConditionFalse || c.Reason != "SchemaNotReady" {
			t.Errorf("Accepted = %s/%s, want False/SchemaNotReady", c.Status, c.Reason)
		}
	})

	t.Run("deleted report is not an error", func(t *testing.T) {
		// The success path: acceptance re-baselines and deletes, so the
		// next event finds nothing. That must not surface as a failure.
		req := ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: "keystone-system", Name: "already-accepted-drift",
		}}
		res, err := r.Reconcile(ctx, req)
		if err != nil {
			t.Errorf("reconcile of a deleted report errored: %v", err)
		}
		if res.RequeueAfter != 0 || res.Requeue {
			t.Errorf("expected a clean no-op; got %+v", res)
		}
	})
}

// TestDriftAcceptance_RequiresDriftController — the reconciler delegates
// every re-check to DriftController.reconcileSchema. Wired without one it
// would nil-panic on the first annotated report rather than at startup,
// which is the wrong place to find out.
func TestDriftAcceptance_RequiresDriftController(t *testing.T) {
	r := &DriftAcceptanceReconciler{}
	if err := r.SetupWithManager(nil); err == nil {
		t.Error("expected SetupWithManager to refuse a nil DriftController")
	}
}
