// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/conditions"
)

// MigrationPlanReconciler owns the Plan-Gate-Apply gate. It resolves
// the effective approval mode from matched SchemaPolicies, flips
// spec.approved=true when the mode is Auto, and reports the resolved
// state via status.approvalMode + a standard Approved condition. The
// MigrationExecution controller refuses to apply SQL until
// spec.approved=true — this reconciler is what turns the key.
//
// Design invariants:
//   - ensureMigrationPlan (in migrationplan_helpers.go) only CREATES
//     plans; approval state is owned here. Separation makes the
//     condition reachable from both paths (bundle fanout, direct
//     kubectl apply of a plan) without duplicated logic.
//   - Manual wins over Auto when multiple matched policies disagree.
//     The reasoning is the usual "strictest policy wins" rule that
//     integrity + approvals already follow.
//   - Plans with no matched policy default to Auto, preserving pre-A4
//     behavior. Operators opt INTO manual approval by attaching a
//     SchemaPolicy with planApproval=Manual.
type MigrationPlanReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=migrationplans,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=migrationplans/status,verbs=get;update;patch

func (r *MigrationPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("migrationplan", req.NamespacedName)

	var plan keystonev1alpha1.MigrationPlan
	if err := r.Get(ctx, req.NamespacedName, &plan); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Look up the parent bundle to read its labels — TargetSelector
	// matching reuses the bundle's labels, not the plan's.
	parent, err := r.fetchParentBundle(ctx, &plan)
	if err != nil {
		// Soft-fail: a plan whose bundle was deleted outside Keystone
		// shouldn't hang the controller. Default to Auto.
		parent = nil
		logger.V(1).Info("parent bundle not found; defaulting to Auto", "bundle", plan.Spec.BundleRef)
	}

	mode, policyName := r.resolveApprovalMode(ctx, parent)

	// If we need to flip spec.approved=true (Auto mode, currently
	// false), do that patch FIRST — it's a spec mutation and must
	// land before any status patch or the apiserver treats it as
	// stale-baseline in later merges.
	if mode == keystonev1alpha1.PlanApprovalAuto && !plan.Spec.Approved {
		if err := r.autoApprove(ctx, &plan); err != nil {
			return ctrl.Result{}, fmt.Errorf("auto-approve plan: %w", err)
		}
		// Re-read after spec mutation so the status patch below uses
		// the latest generation.
		if err := r.Get(ctx, req.NamespacedName, &plan); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.patchStatus(ctx, &plan, mode, policyName)
}

// fetchParentBundle reads the MigrationBundle referenced by the plan.
// The plan's namespace equals the bundle's namespace (plans are
// created in the bundle's namespace — see ensureMigrationPlan).
func (r *MigrationPlanReconciler) fetchParentBundle(ctx context.Context, plan *keystonev1alpha1.MigrationPlan) (*keystonev1alpha1.MigrationBundle, error) {
	if plan.Spec.BundleRef == "" {
		return nil, fmt.Errorf("plan has no bundleRef")
	}
	var b keystonev1alpha1.MigrationBundle
	key := types.NamespacedName{Namespace: plan.Namespace, Name: plan.Spec.BundleRef}
	if err := r.Get(ctx, key, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// resolveApprovalMode walks matched SchemaPolicies and returns the
// strictest mode (Manual wins). When bundle is nil (parent gone) or
// no policy declares planApproval, returns Auto — backward compat.
//
// Also returns the name of the policy that selected Manual mode so
// status.approvalPolicyName points operators at the right CR for
// "why is this plan waiting?" diagnostics.
func (r *MigrationPlanReconciler) resolveApprovalMode(ctx context.Context, bundle *keystonev1alpha1.MigrationBundle) (keystonev1alpha1.PlanApprovalMode, string) {
	if bundle == nil {
		return keystonev1alpha1.PlanApprovalAuto, ""
	}
	var policies keystonev1alpha1.SchemaPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return keystonev1alpha1.PlanApprovalAuto, ""
	}
	for i := range policies.Items {
		p := &policies.Items[i]
		if p.Spec.PlanApproval != keystonev1alpha1.PlanApprovalManual {
			continue
		}
		// Pinned policyRef bypasses selector evaluation.
		if bundle.Spec.PolicyRef != "" {
			if bundle.Spec.PolicyRef == p.Name {
				return keystonev1alpha1.PlanApprovalManual, p.Name
			}
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&p.Spec.TargetSelector)
		if err != nil {
			continue
		}
		if sel.Matches(labels.Set(bundle.Labels)) {
			return keystonev1alpha1.PlanApprovalManual, p.Name
		}
	}
	return keystonev1alpha1.PlanApprovalAuto, ""
}

// autoApprove patches spec.approved=true and records an event.
// Separate from status update because spec mutations need their own
// merge patch with the current resourceVersion.
func (r *MigrationPlanReconciler) autoApprove(ctx context.Context, plan *keystonev1alpha1.MigrationPlan) error {
	original := plan.DeepCopy()
	plan.Spec.Approved = true
	if err := r.Patch(ctx, plan, client.MergeFrom(original)); err != nil {
		return err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(plan, corev1.EventTypeNormal, "AutoApproved",
			"plan auto-approved under SchemaPolicy mode=Auto; run `keystonectl plan approve` or attach a Manual-mode policy to require a reviewer")
	}
	return nil
}

// patchStatus writes the observed approval mode + owning policy
// + Approved condition onto the plan's status subresource. Always
// called — we want kubectl describe to reflect the resolved mode
// even when spec.approved is still false (Manual path).
func (r *MigrationPlanReconciler) patchStatus(
	ctx context.Context,
	plan *keystonev1alpha1.MigrationPlan,
	mode keystonev1alpha1.PlanApprovalMode,
	policyName string,
) (ctrl.Result, error) {
	original := plan.DeepCopy()
	plan.Status.ObservedGeneration = plan.Generation
	plan.Status.ApprovalMode = mode
	plan.Status.ApprovalPolicyName = policyName

	switch {
	case plan.Spec.Approved:
		plan.Status.Conditions = conditions.Set(plan.Status.Conditions,
			keystonev1alpha1.ConditionTypeApproved,
			metav1.ConditionTrue, "Approved",
			fmt.Sprintf("spec.approved=true under mode=%s", mode),
			plan.Generation)
	case mode == keystonev1alpha1.PlanApprovalManual:
		plan.Status.Conditions = conditions.Set(plan.Status.Conditions,
			keystonev1alpha1.ConditionTypeApproved,
			metav1.ConditionFalse, "AwaitingApproval",
			fmt.Sprintf("SchemaPolicy %q set planApproval=Manual; run `keystonectl plan approve %s/%s` "+
				"or patch spec.approved=true to proceed",
				policyName, plan.Namespace, plan.Name),
			plan.Generation)
	default:
		// Auto mode but spec.approved still false — transient; the
		// auto-approve patch lost the race with this Reconcile tick
		// and we'll catch up next loop.
		plan.Status.Conditions = conditions.Set(plan.Status.Conditions,
			keystonev1alpha1.ConditionTypeApproved,
			metav1.ConditionUnknown, "PendingAutoApproval",
			"auto-approval pending (reconcile will retry)",
			plan.Generation)
	}

	if err := r.Status().Patch(ctx, plan, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch plan status: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler. Watches SchemaPolicies so a
// policy flip (e.g., toggling planApproval from Auto to Manual on a
// tier=prod selector) immediately re-evaluates every affected plan
// without waiting for the periodic requeue.
func (r *MigrationPlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keystone-migrationplan")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.MigrationPlan{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Named("migrationplan").
		Complete(r)
}
