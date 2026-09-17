// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/conditions"
)

// DriftAcceptanceReconciler acts on the one DriftReport field an
// operator is expected to edit: the accept-drift annotation.
//
// Before this existed, acceptance was only ever read by the periodic
// sweep. Annotating a report did nothing until the next tick — up to
// driftCheckInterval, an hour by default — and nothing was written back
// in the meantime. From the operator's side the two outcomes are
// indistinguishable: a correct annotation waiting on the sweep and an
// annotation that will never be read (wrong key, wrong value, wrong
// object) both look like "I edited it and nothing happened". The only
// way to tell them apart was to wait an hour and look again, and the
// documented workaround was to restart the operator to force a sweep.
//
// So: watch for the annotation and re-check that one schema
// immediately, then make sure every outcome is visible.
//
//	accepted        → re-baselined, report deleted, DriftAccepted event
//	                  and audit entry (all pre-existing, in acceptDrift)
//	not accepted    → Accepted=False on the report with a reason that
//	                  says which of the four ways it failed, plus a
//	                  Warning event
//
// There is deliberately no Accepted=True: acceptance deletes the report,
// so a True would have nowhere to live and, worse, a lingering one would
// mean acceptance had *not* completed. Absence of the report is the
// success signal.
//
// This does not replace the periodic sweep, which remains the drift
// detector. It only shortens the acceptance path from "up to an hour,
// silently" to "now, with an answer either way".
type DriftAcceptanceReconciler struct {
	client.Client

	// Drift is the periodic checker. Acceptance re-uses its
	// reconcileSchema so that the annotated path and the swept path
	// cannot diverge — reconcileSchema is what calls acceptDrift.
	Drift *DriftController
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=driftreports,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=driftreports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=databaseschemas,verbs=get;list;watch

// Reconcile handles one annotated DriftReport.
func (r *DriftAcceptanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("component", "driftacceptance")

	var report keystonev1alpha1.DriftReport
	if err := r.Get(ctx, req.NamespacedName, &report); err != nil {
		// Gone almost always means a previous pass accepted it and
		// deleted it. That is the success path, not an error.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	raw, present := report.Annotations[AnnotationAcceptDrift]
	if !present {
		return ctrl.Result{}, nil
	}
	// Strict on the value, loud about it. Accepting "True"/"yes"/"1"
	// would make the contract mushy; silently ignoring them is what
	// produced the ambiguity in the first place. An operator who typed
	// "True" gets told so instead of waiting out an hour and concluding
	// the feature is broken.
	if raw != "true" {
		r.refuse(ctx, &report, "MalformedAnnotation",
			fmt.Sprintf("annotation %s=%q is not the literal string \"true\"; drift was not accepted",
				AnnotationAcceptDrift, raw))
		return ctrl.Result{}, nil
	}

	var schema keystonev1alpha1.DatabaseSchema
	schemaKey := types.NamespacedName{
		Namespace: report.Namespace, Name: report.Spec.DatabaseSchemaRef,
	}
	if err := r.Get(ctx, schemaKey, &schema); err != nil {
		if apierrors.IsNotFound(err) {
			r.refuse(ctx, &report, "SchemaNotFound",
				fmt.Sprintf("DatabaseSchema %s does not exist; nothing to re-baseline", schemaKey))
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get DatabaseSchema %s: %w", schemaKey, err)
	}

	// Mirror the sweep's gate. reconcileSchema opens an admin pool to
	// the schema's provider; a schema that is not Ready has no usable
	// connection details yet, and the resulting failure would read as a
	// rebaseline error rather than the "not yet" it actually is.
	if !conditions.IsTrue(schema.Status.Conditions, keystonev1alpha1.ConditionTypeReady) {
		r.refuse(ctx, &report, "SchemaNotReady",
			fmt.Sprintf("DatabaseSchema %s is not Ready; acceptance will be retried when it is",
				schema.Name))
		// Not an error — requeue on the schema's own timescale rather
		// than burning controller-runtime backoff on a wait.
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// reconcileSchema re-inspects, sees the drift, and reaches
	// acceptDrift — which reads this same annotation, re-baselines, and
	// deletes the report. Going through the full path rather than
	// calling acceptDrift directly keeps the annotated and swept routes
	// on identical logic, including the audit entry and the
	// snapshot-model-upgrade check that runs ahead of acceptance.
	if err := r.Drift.reconcileSchema(ctx, &schema, logger); err != nil {
		r.refuse(ctx, &report, "RebaselineFailed", err.Error())
		// Return the error so controller-runtime applies its backoff —
		// a provider that is down should not be retried in a tight loop.
		return ctrl.Result{}, fmt.Errorf("re-check schema %s for acceptance: %w", schema.Name, err)
	}

	// If acceptance succeeded the report is now deleted; a NotFound here
	// is the confirmation. If it still exists, the re-check found the
	// schema clean by other means (drift resolved on its own, or a
	// snapshot-model restatement) and resolveExistingReport pruned it.
	// Either way there is nothing left to annotate.
	if err := r.Get(ctx, req.NamespacedName, &report); err == nil {
		// Still here: drift was re-observed and acceptDrift did not
		// take it. That should not happen — acceptDrift is checked
		// before anything is recorded — so say so rather than leaving
		// the operator with a silently unchanged report again.
		r.refuse(ctx, &report, "RebaselineFailed",
			"schema was re-checked but the report was not resolved; see controller logs")
		return ctrl.Result{}, nil
	}

	logger.Info("accept-drift applied", "report", req.NamespacedName, "schema", schema.Name)
	return ctrl.Result{}, nil
}

// refuse records why an accept-drift request did not take effect, on the
// report itself and as an event. Best-effort: the operator-visible
// signal must not be able to fail the reconcile that produced it.
func (r *DriftAcceptanceReconciler) refuse(
	ctx context.Context,
	report *keystonev1alpha1.DriftReport,
	reason, message string,
) {
	logger := log.FromContext(ctx)
	patch := client.MergeFrom(report.DeepCopy())
	report.Status.Conditions = conditions.Set(report.Status.Conditions,
		keystonev1alpha1.ConditionTypeAccepted, metav1.ConditionFalse,
		reason, message, report.Generation)
	if err := r.Status().Patch(ctx, report, patch); err != nil && !apierrors.IsNotFound(err) {
		logger.Error(err, "patch Accepted condition", "report", report.Name, "reason", reason)
	}
	if r.Drift != nil && r.Drift.Recorder != nil {
		r.Drift.Recorder.Eventf(report, corev1.EventTypeWarning, reason, "%s", message)
	}
	logger.Info("accept-drift refused", "report", report.Name, "reason", reason, "message", message)
}

// SetupWithManager registers the watch.
//
// The filter matters as much as the watch. DriftReports are written by
// the periodic sweep on every pass that re-observes drift, and those
// writes must not wake this controller — it would re-run an admin
// inspection per report per sweep for nothing. Only a report that
// actually carries the annotation is enqueued, and updates only when the
// annotation itself changed.
func (r *DriftAcceptanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Drift == nil {
		return fmt.Errorf("DriftAcceptanceReconciler requires a DriftController")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.DriftReport{}).
		Named("driftacceptance").
		WithEventFilter(acceptDriftPredicate()).
		Complete(r)
}

// acceptDriftPredicate admits only events that could represent an
// operator asking for acceptance.
func acceptDriftPredicate() predicate.Funcs {
	annotated := func(o client.Object) bool {
		_, ok := o.GetAnnotations()[AnnotationAcceptDrift]
		return ok
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return annotated(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Only when the annotation itself appears or changes. The
			// sweep re-patches status on every pass that re-observes
			// drift and leaves the annotation alone, so those writes —
			// the overwhelming majority — are dropped here rather than
			// costing an admin inspection per report per sweep.
			return e.ObjectOld.GetAnnotations()[AnnotationAcceptDrift] !=
				e.ObjectNew.GetAnnotations()[AnnotationAcceptDrift]
		},
		// Acceptance deletes the report; reconciling that would be
		// chasing our own tail.
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return annotated(e.Object) },
	}
}
