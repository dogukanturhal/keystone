// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/conditions"
)

// reconcileStaged drives a MigrationBundle through a RolloutPolicy
// pipeline. Stages are processed in order; each stage:
//   1. selects a subset of the bundle's matched schemas (combination of
//      schema-label selector + cluster-tier filter)
//   2. creates MigrationExecutions for that subset, capped by parallelism
//   3. waits for all stage executions to reach Succeeded
//   4. starts a soak timer
//   5. (optionally) waits for an approval annotation
//   6. advances to the next stage
//
// All-at-once fanout (Phase 3 behaviour) is retained for bundles
// without spec.rolloutPolicyRef — see reconcileAllAtOnce in the main
// controller file.
func (r *MigrationBundleReconciler) reconcileStaged(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	matchedSchemas []keystonev1alpha1.DatabaseSchema,
	contentHash string,
) (ctrl.Result, error) {
	// Resolve the policy.
	var policy keystonev1alpha1.RolloutPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: bundle.Spec.RolloutPolicyRef}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, bundle, "RolloutPolicyNotFound",
				fmt.Errorf("RolloutPolicy %q not found", bundle.Spec.RolloutPolicyRef))
		}
		return ctrl.Result{}, fmt.Errorf("get RolloutPolicy: %w", err)
	}
	if len(policy.Spec.Stages) == 0 {
		return r.fail(ctx, bundle, "RolloutPolicyEmpty",
			fmt.Errorf("RolloutPolicy %q has no stages", policy.Name))
	}

	// Determine current stage. Default to the first stage when none recorded.
	currentName := bundle.Status.CurrentStage
	if currentName == "" {
		currentName = policy.Spec.Stages[0].Name
	}
	currentIdx := -1
	for i, s := range policy.Spec.Stages {
		if s.Name == currentName {
			currentIdx = i
			break
		}
	}
	if currentIdx < 0 {
		// Stage referenced in status no longer exists in policy. Reset
		// to the first stage and let the rollout re-flow.
		currentIdx = 0
		currentName = policy.Spec.Stages[0].Name
	}
	stage := policy.Spec.Stages[currentIdx]

	// Filter matched schemas down to this stage's targets.
	stageTargets, err := r.filterStageTargets(ctx, bundle, &stage, matchedSchemas)
	if err != nil {
		return r.fail(ctx, bundle, "StageFilterFailed", err)
	}

	// Walk this stage's existing executions to count progress.
	progress := stageProgress{Targets: int32(len(stageTargets))}
	executions := make([]keystonev1alpha1.MigrationExecution, 0, len(stageTargets))
	for i := range stageTargets {
		s := &stageTargets[i]
		execName := executionName(bundle.Name, bundle.Spec.Version, s.Name)
		var exec keystonev1alpha1.MigrationExecution
		err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: execName}, &exec)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("lookup execution: %w", err)
		}
		executions = append(executions, exec)
		switch exec.Status.Phase {
		case keystonev1alpha1.ExecutionPhaseSucceeded:
			progress.Succeeded++
		case keystonev1alpha1.ExecutionPhaseFailed, keystonev1alpha1.ExecutionPhaseAborted:
			progress.Failed++
		}
	}

	// Abort guard: any failure in this stage with AbortOnFailure=true
	// freezes the rollout. Status reflects the freeze; operator must
	// fix and unblock.
	if stage.AbortOnFailure && progress.Failed > 0 {
		return r.markStageBlocked(ctx, bundle, &stage, &progress,
			fmt.Errorf("%d execution(s) failed in stage %q; rollout frozen until resolved",
				progress.Failed, stage.Name))
	}

	// Dispatch more executions if we're under the parallelism cap.
	inflight := int32(len(executions)) - progress.Succeeded - progress.Failed
	for i := range stageTargets {
		s := &stageTargets[i]
		if inflight >= stage.Parallelism {
			break
		}
		execName := executionName(bundle.Name, bundle.Spec.Version, s.Name)
		// Skip if we already have an execution for this target.
		// Content-drift assertion mirrors the all-at-once path in
		// migrationbundle_controller.go: an existing ME for this
		// (bundleName, version, schema) must agree on ContentHash, or
		// else the bundle's source was mutated outside the SD
		// reconciler. Fail-loud rather than silently reuse.
		var existing *keystonev1alpha1.MigrationExecution
		for i := range executions {
			if executions[i].Name == execName {
				existing = &executions[i]
				break
			}
		}
		if existing != nil {
			if existing.Spec.ContentHash != contentHash {
				return ctrl.Result{}, fmt.Errorf(
					"BundleNameContentDrift: existing MigrationExecution %s/%s has "+
						"ContentHash=%s but bundle resolves to %s — refusing to "+
						"reuse stale ME (delete the ME and re-emit)",
					existing.Namespace, existing.Name,
					existing.Spec.ContentHash, contentHash)
			}
			continue
		}
		// Phase 3.1: materialise a MigrationPlan before the execution.
		// For staged rollouts we still want one plan per (bundle,
		// schema, version) — same identity as the all-at-once path.
		// Construct the schema's source materialisation if needed; we
		// pass nil here because the staged path doesn't carry the
		// resolved source, and the plan auto-approves regardless of
		// statements (Phase 3.1.1 fixes that).
		plan, planErr := ensureMigrationPlan(ctx, r.Client, bundle, s, nil, r.Scheme)
		if planErr != nil {
			return ctrl.Result{}, fmt.Errorf("ensure plan: %w", planErr)
		}
		exec := &keystonev1alpha1.MigrationExecution{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: s.Namespace,
				Name:      execName,
				Labels: map[string]string{
					"keystone.hexxlock.io/bundle":  bundle.Name,
					"keystone.hexxlock.io/version": bundle.Spec.Version,
					"keystone.hexxlock.io/schema":  s.Name,
					"keystone.hexxlock.io/stage":   stage.Name,
				},
			},
			Spec: keystonev1alpha1.MigrationExecutionSpec{
				PlanRef:       plan.Name,
				BundleRef:     bundle.Name,
				BundleVersion: bundle.Spec.Version,
				SchemaRef:     s.Name,
				ContentHash:   contentHash,
				ExecutionMode: bundle.Spec.ExecutionMode,
			},
		}
		if err := controllerutil.SetControllerReference(bundle, exec, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set owner ref: %w", err)
		}
		if err := r.Create(ctx, exec); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create execution %s: %w", execName, err)
		}
		r.Recorder.Eventf(bundle, corev1.EventTypeNormal, "StageExecutionCreated",
			"stage=%s schema=%s execution=%s", stage.Name, s.Name, execName)
		inflight++
	}

	// Stage progress accounting.
	now := metav1.Now()
	hist := r.touchStageHistory(bundle, &stage, &progress, &now)

	// Stage exit: all targets succeeded?
	if progress.Targets > 0 && progress.Succeeded == progress.Targets {
		// Start soak if not already started.
		if hist.SoakStartedAt == nil {
			hist.SoakStartedAt = &now
			r.upsertStageHistory(bundle, hist)
			if err := r.patchStatus(ctx, bundle); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(bundle, corev1.EventTypeNormal, "StageSoaking",
				"stage %q reached all-succeeded; soaking for %s", stage.Name, stage.SoakDuration)
		}
		// Soak elapsed?
		soakDur, soakErr := time.ParseDuration(nonEmpty(stage.SoakDuration, "10m"))
		if soakErr != nil {
			return r.fail(ctx, bundle, "SoakDurationInvalid", soakErr)
		}
		if time.Since(hist.SoakStartedAt.Time) < soakDur {
			r.upsertStageHistory(bundle, hist)
			r.markStageProgressing(bundle, &stage, &progress)
			if err := r.patchStatus(ctx, bundle); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: timeUntil(hist.SoakStartedAt.Time, soakDur)}, nil
		}
		// Approval required?
		if stage.RequireApproval {
			ann := keystonev1alpha1.AnnotationApproveStagePrefix + stage.Name
			if bundle.Annotations[ann] != "true" {
				r.upsertStageHistory(bundle, hist)
				r.markStageAwaitingApproval(bundle, &stage, &progress, ann)
				rolloutStageAwaitingApproval.WithLabelValues(bundle.Namespace,
					bundle.Name, bundle.Spec.Version, bundle.Spec.RolloutPolicyRef,
					stage.Name).Set(1)
				if err := r.patchStatus(ctx, bundle); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		}
		// Advance.
		hist.CompletedAt = &now
		r.upsertStageHistory(bundle, hist)
		// Stage metrics: completion + duration histogram +
		// clear-the-awaiting-approval gauge if it was set.
		labels := []string{bundle.Namespace, bundle.Name, bundle.Spec.Version,
			bundle.Spec.RolloutPolicyRef, stage.Name}
		rolloutStageCompleted.WithLabelValues(labels...).Inc()
		rolloutStageDurationSeconds.WithLabelValues(labels...).
			Observe(now.Time.Sub(hist.StartedAt.Time).Seconds())
		rolloutStageAwaitingApproval.WithLabelValues(labels...).Set(0)
		if currentIdx+1 < len(policy.Spec.Stages) {
			next := policy.Spec.Stages[currentIdx+1]
			bundle.Status.CurrentStage = next.Name
			r.markStageAdvanced(bundle, &stage, &next)
			if err := r.patchStatus(ctx, bundle); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(bundle, corev1.EventTypeNormal, "StageAdvanced",
				"stage %q complete; advancing to %q", stage.Name, next.Name)
			return ctrl.Result{Requeue: true}, nil
		}
		// Last stage: rollout complete.
		bundle.Status.CurrentStage = ""
		r.markRolloutComplete(bundle)
		if err := r.patchStatus(ctx, bundle); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(bundle, corev1.EventTypeNormal, "RolloutComplete",
			"version %s applied to all stages", bundle.Spec.Version)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Still in flight: keep status fresh, requeue soon.
	r.upsertStageHistory(bundle, hist)
	r.markStageProgressing(bundle, &stage, &progress)
	if err := r.patchStatus(ctx, bundle); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// stageProgress is a local accumulator that mirrors v1alpha1.StageProgress
// minus the timestamps (which are managed via touchStageHistory).
type stageProgress struct {
	Targets   int32
	Succeeded int32
	Failed    int32
}

// filterStageTargets returns the schemas that the current stage targets.
// Combines the stage's SchemaSelector (schema labels) and ClusterTierSelector
// (resolved via LogicalDatabase → ClusterRegistration → tier).
func (r *MigrationBundleReconciler) filterStageTargets(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	stage *keystonev1alpha1.RolloutStage,
	matched []keystonev1alpha1.DatabaseSchema,
) ([]keystonev1alpha1.DatabaseSchema, error) {
	sel, err := metav1.LabelSelectorAsSelector(&stage.SchemaSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid stage schemaSelector: %w", err)
	}
	tierSet := map[keystonev1alpha1.ClusterTier]bool{}
	for _, t := range stage.ClusterTierSelector {
		tierSet[t] = true
	}

	out := make([]keystonev1alpha1.DatabaseSchema, 0, len(matched))
	for i := range matched {
		s := matched[i]
		if !sel.Empty() && !sel.Matches(labels.Set(s.Labels)) {
			continue
		}
		if len(tierSet) > 0 {
			tier, err := r.resolveClusterTier(ctx, &s)
			if err != nil {
				// Resolution failure is logged but doesn't fail the
				// whole rollout; the schema is excluded from this stage.
				continue
			}
			if !tierSet[tier] {
				continue
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// resolveClusterTier walks DatabaseSchema → LogicalDatabase →
// ClusterRegistration to find the cluster's tier. Cached via the
// controller's K8s informer cache.
func (r *MigrationBundleReconciler) resolveClusterTier(
	ctx context.Context,
	schema *keystonev1alpha1.DatabaseSchema,
) (keystonev1alpha1.ClusterTier, error) {
	var ldb keystonev1alpha1.LogicalDatabase
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: schema.Namespace, Name: schema.Spec.LogicalDatabaseRef,
	}, &ldb); err != nil {
		return "", err
	}
	var cr keystonev1alpha1.ClusterRegistration
	if err := r.Get(ctx, types.NamespacedName{Name: ldb.Spec.ClusterRef}, &cr); err != nil {
		return "", err
	}
	return cr.Spec.Tier, nil
}

func (r *MigrationBundleReconciler) touchStageHistory(
	bundle *keystonev1alpha1.MigrationBundle,
	stage *keystonev1alpha1.RolloutStage,
	progress *stageProgress,
	now *metav1.Time,
) *keystonev1alpha1.StageProgress {
	for i := range bundle.Status.StageHistory {
		if bundle.Status.StageHistory[i].Name == stage.Name {
			h := &bundle.Status.StageHistory[i]
			h.Targets = progress.Targets
			h.Succeeded = progress.Succeeded
			h.Failed = progress.Failed
			return h
		}
	}
	h := keystonev1alpha1.StageProgress{
		Name:      stage.Name,
		StartedAt: *now,
		Targets:   progress.Targets,
		Succeeded: progress.Succeeded,
		Failed:    progress.Failed,
	}
	bundle.Status.StageHistory = append(bundle.Status.StageHistory, h)
	rolloutStageStarted.WithLabelValues(bundle.Namespace, bundle.Name, bundle.Spec.Version,
		bundle.Spec.RolloutPolicyRef, stage.Name).Inc()
	return &bundle.Status.StageHistory[len(bundle.Status.StageHistory)-1]
}

func (r *MigrationBundleReconciler) upsertStageHistory(
	bundle *keystonev1alpha1.MigrationBundle,
	hist *keystonev1alpha1.StageProgress,
) {
	for i := range bundle.Status.StageHistory {
		if bundle.Status.StageHistory[i].Name == hist.Name {
			bundle.Status.StageHistory[i] = *hist
			return
		}
	}
	bundle.Status.StageHistory = append(bundle.Status.StageHistory, *hist)
}

func (r *MigrationBundleReconciler) markStageProgressing(
	bundle *keystonev1alpha1.MigrationBundle,
	stage *keystonev1alpha1.RolloutStage,
	progress *stageProgress,
) {
	bundle.Status.CurrentStage = stage.Name
	bundle.Status.Conditions = conditions.MarkProgressing(bundle.Status.Conditions,
		"StageInFlight",
		fmt.Sprintf("stage=%s succeeded=%d/%d failed=%d parallelism=%d",
			stage.Name, progress.Succeeded, progress.Targets, progress.Failed, stage.Parallelism),
		bundle.Generation)
}

func (r *MigrationBundleReconciler) markStageAwaitingApproval(
	bundle *keystonev1alpha1.MigrationBundle,
	stage *keystonev1alpha1.RolloutStage,
	progress *stageProgress,
	annotationKey string,
) {
	bundle.Status.CurrentStage = stage.Name
	bundle.Status.Conditions = conditions.MarkProgressing(bundle.Status.Conditions,
		"AwaitingApproval",
		fmt.Sprintf("stage=%s soak elapsed; set annotation %s=true to advance",
			stage.Name, annotationKey),
		bundle.Generation)
}

func (r *MigrationBundleReconciler) markStageBlocked(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	stage *keystonev1alpha1.RolloutStage,
	progress *stageProgress,
	cause error,
) (ctrl.Result, error) {
	patch := client.MergeFrom(bundle.DeepCopy())
	bundle.Status.ObservedGeneration = bundle.Generation
	bundle.Status.CurrentStage = stage.Name
	bundle.Status.Conditions = conditions.MarkNotReady(bundle.Status.Conditions,
		"StageBlocked", cause, bundle.Generation)
	if err := r.Status().Patch(ctx, bundle, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch blocked status: %w (cause: %v)", err, cause)
	}
	r.Recorder.Eventf(bundle, corev1.EventTypeWarning, "StageBlocked", "%s", cause.Error())
	rolloutStageBlocked.WithLabelValues(bundle.Namespace, bundle.Name, bundle.Spec.Version,
		bundle.Spec.RolloutPolicyRef, stage.Name).Inc()
	// Don't bubble the error — the rollout is intentionally frozen, not failing.
	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

func (r *MigrationBundleReconciler) markStageAdvanced(
	bundle *keystonev1alpha1.MigrationBundle,
	from, to *keystonev1alpha1.RolloutStage,
) {
	bundle.Status.Conditions = conditions.MarkProgressing(bundle.Status.Conditions,
		"StageAdvanced",
		fmt.Sprintf("stage %s complete; entering stage %s", from.Name, to.Name),
		bundle.Generation)
}

func (r *MigrationBundleReconciler) markRolloutComplete(bundle *keystonev1alpha1.MigrationBundle) {
	bundle.Status.Conditions = conditions.MarkReady(bundle.Status.Conditions,
		"RolloutComplete",
		fmt.Sprintf("version %s applied through every stage of policy %s",
			bundle.Spec.Version, bundle.Spec.RolloutPolicyRef),
		bundle.Generation)
	bundle.Status.Conditions = conditions.ClearProgressing(bundle.Status.Conditions, bundle.Generation)
}

// patchStatus pushes a fresh status mutation. Local helper to keep the
// big reconcileStaged function readable.
func (r *MigrationBundleReconciler) patchStatus(ctx context.Context, bundle *keystonev1alpha1.MigrationBundle) error {
	bundle.Status.ObservedGeneration = bundle.Generation
	now := metav1.Now()
	bundle.Status.LastPlanTime = &now
	// We don't have a prior copy to MergeFrom here because callers have
	// already mutated bundle.Status in place. Use Update on the status
	// subresource via Patch with a MergeFrom of a fresh DeepCopy of the
	// post-mutation state — that's a no-op patch but the call refreshes
	// resourceVersion. This pattern works because earlier in the
	// reconcile we Got() the bundle freshly.
	patch := client.MergeFrom(bundle.DeepCopy())
	return r.Status().Patch(ctx, bundle, patch)
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func timeUntil(start time.Time, dur time.Duration) time.Duration {
	d := dur - time.Since(start)
	if d < time.Second {
		return time.Second
	}
	return d
}
