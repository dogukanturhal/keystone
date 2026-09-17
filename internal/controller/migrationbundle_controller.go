// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/approval"
	"github.com/dogukanturhal/keystone/internal/audit"
	"github.com/dogukanturhal/keystone/internal/conditions"
	"github.com/dogukanturhal/keystone/internal/devdb"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
	"github.com/dogukanturhal/keystone-sdk/go/analyze"
)

// integrityMessageMaxLen matches MigrationBundle.status.integrity.message
// in the CRD schema. Longer diagnostics are truncated before patch so
// the apiserver doesn't reject the whole update.
const integrityMessageMaxLen = 512

// Reconcile cadence (Phase 5a follow-up to KS#5).
//
// readyRequeue is the long-period drift backstop applied when the bundle
// is Ready=True (matched==applied, every schema at the target version).
// Real spec changes flow through the watch instantaneously via the
// `For()` predicate and a new MigrationExecution succeeding fires the
// `Owns()` watch — so the periodic re-reconcile only exists to detect
// out-of-band drift (operator hand-edits, post-restore divergence,
// etc.). Five minutes matches Crossplane's circuit-breaker cooldown
// and Argo CD's reconciliation default; the prior 30-second cadence
// was a Phase-1 holdover from when fanout was bursty and the audit
// chain was tolerant of duplicate "reconcile success" entries.
//
// applyingRequeue keeps the original tighter cadence for bundles that
// are actively rolling out — a faster loop here surfaces per-execution
// progress in the bundle's status without waiting on the watch path
// (Owns(MigrationExecution) fires on terminal phase only via the
// predicate; non-terminal Phase changes don't propagate up).
//
// Both intervals are jittered ±20% in `requeueWithJitter` to spread
// 50+ bundles across the period rather than synchronising on the
// reconcile boundary.
const (
	readyRequeue     = 5 * time.Minute
	applyingRequeue  = 30 * time.Second
	requeueJitterPct = 20
)

// requeueWithJitter applies a ±jitterPct% random offset to d. Spreads
// reconcile fanout for 50+ bundles so they don't synchronise on the
// period boundary and crowd the apiserver's write-path queue.
func requeueWithJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	span := int64(d) * int64(requeueJitterPct) / 100
	if span <= 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(2*span)-span)
}

// MigrationBundleReconciler watches MigrationBundles, resolves their
// source, and creates one MigrationExecution per matched DatabaseSchema
// that doesn't already have a successful execution at the bundle's
// version. The execution itself is reconciled by
// MigrationExecutionReconciler — this controller only fans out.
type MigrationBundleReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Resolver migration.SourceResolver

	// AuditLogger appends tamper-evident entries to the audit chain on
	// every meaningful state transition. Wired from main.go; tests
	// leave it nil and the emitAudit helper no-ops gracefully.
	AuditLogger *audit.Logger

	// ManagerIdentity is the Actor stamped on reconciler-sourced
	// audit entries. Typically the manager's own ServiceAccount.
	ManagerIdentity keystonev1alpha1.AuditActor

	// DevDBPool is the optional dev database pool for semantic migration
	// analysis. When non-nil, the reconciler runs the SemanticAnalyzer
	// after the 51 static analyzers to replay migrations against an
	// ephemeral schema and catch runtime SQL errors. Configured via
	// SchemaPolicy.spec.devDatabaseRef.
	DevDBPool *devdb.Pool
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=migrationbundles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=migrationbundles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=migrationbundles/finalizers,verbs=update
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=migrationexecutions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

func (r *MigrationBundleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("migrationbundle", req.NamespacedName)

	var bundle keystonev1alpha1.MigrationBundle
	if err := r.Get(ctx, req.NamespacedName, &bundle); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !bundle.DeletionTimestamp.IsZero() {
		// Bundle deletion does NOT cascade to executions — they are kept
		// for the audit window. Just remove the finalizer.
		if controllerutil.RemoveFinalizer(&bundle, keystonev1alpha1.FinalizerMigrationBundle) {
			if err := r.Update(ctx, &bundle); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&bundle, keystonev1alpha1.FinalizerMigrationBundle) {
		if err := r.Update(ctx, &bundle); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Resolve source.
	src, err := r.Resolver.Resolve(ctx, bundle.Namespace, bundle.Spec.Source)
	if err != nil {
		return r.fail(ctx, &bundle, "SourceResolutionFailed", err)
	}
	if len(src.Names) == 0 {
		return r.fail(ctx, &bundle, "SourceEmpty",
			fmt.Errorf("source resolved to zero files; check filePattern"))
	}

	// Anti-tamper check: if status.contentHash is set and differs from
	// the freshly-resolved hash, an operator changed the SQL in place
	// at the same Version. Refuse.
	if bundle.Status.ContentHash != "" && bundle.Status.ContentHash != src.ContentHash {
		return r.fail(ctx, &bundle, "ContentHashChanged",
			fmt.Errorf(
				"version %q has prior contentHash %q but source now hashes to %q; "+
					"bundles are immutable per-version — bump spec.version",
				bundle.Spec.Version, bundle.Status.ContentHash, src.ContentHash,
			))
	}

	// A1 — keystone.sum integrity. Always publish the observation so
	// `kubectl describe` / dashboards see the outcome even when the
	// sum is absent (advisory mode). Malformed and Mismatch also fail
	// the reconcile so downstream fanout doesn't run on suspect SQL.
	if err := r.patchIntegrity(ctx, &bundle, src); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch integrity: %w", err)
	}
	switch src.IntegrityStatus {
	case migration.SumStatusMalformed:
		return r.fail(ctx, &bundle, keystonev1alpha1.ReasonSumMalformed, src.IntegrityError)
	case migration.SumStatusMismatch:
		return r.fail(ctx, &bundle, keystonev1alpha1.ReasonSumMismatch, src.IntegrityError)
	}

	// Phase 11 — lint the resolved source before fanout. Populates
	// status.lintFindings; blocks Ready=True when any finding is at
	// severity=error. Registry is the default 20-rule pack; future
	// SchemaPolicy.spec.disabledAnalyzers can trim it.
	//
	// Findings are patched to the apiserver in a standalone call so that
	// subsequent success/fail patches don't have a stale baseline (the
	// merge-patch diff uses the in-memory DeepCopy as baseline, which
	// would otherwise treat findings as "unchanged" and drop them).
	lintFindings, lintBlock := r.runAnalyzers(ctx, &bundle, src)
	if err := r.patchLintFindings(ctx, &bundle, lintFindings); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch lintFindings: %w", err)
	}

	// A3 — N-of-M approval gate. Run BEFORE the lint-error refusal
	// so an applied + satisfied ApprovalPolicy with a lint-severity
	// gate can lift the LintFailed block (mirror of the admission
	// webhook's same fix). Without this re-order, the
	// AppliesWhen.MinLintSeverity feature in the SchemaPolicy CRD
	// never fires for lint-error bundles at the reconciler tier
	// either — so even a fully-approved bundle that slipped past
	// admission would still be rejected by the reconciler.
	if err := r.patchApproval(ctx, &bundle, lintFindings); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch approval: %w", err)
	}
	if bundle.Status.Approval != nil && !bundle.Status.Approval.Satisfied {
		return r.fail(ctx, &bundle, "ApprovalsMissing",
			fmt.Errorf("bundle is not approved by one or more matched SchemaPolicies; "+
				"see status.approval.policies for detail"))
	}

	// RecordOnly bundles (adoption baselines, ADR 0027) are never
	// executed, so execution-safety lint cannot block them — the same
	// semantics as `flyway baseline` / Liquibase `changelog-sync` /
	// `atlas migrate apply --baseline`, none of which run execution
	// analysis on adoption baselines. The findings were patched into
	// status.lintFindings above, so they stay visible as advisory.
	recordOnly := bundle.Spec.ExecutionMode == keystonev1alpha1.ExecutionModeRecordOnly
	if lintBlock != nil && !recordOnly {
		// LintFailed is suppressed when an applied+satisfied
		// ApprovalPolicy has a non-empty AppliesWhen.MinLintSeverity
		// (which by definition covers severity=error). The
		// LiftsLintErrors helper centralises this check.
		matched, _ := r.matchedSchemaPolicies(ctx, &bundle)
		if !approval.Evaluate(&bundle, matched, lintFindings).LiftsLintErrors() {
			return r.fail(ctx, &bundle, "LintFailed", lintBlock)
		}
		// Approved — proceed past the block. The findings are still
		// patched into status.lintFindings so they remain visible.
	}

	// Match schemas.
	selector, err := metav1.LabelSelectorAsSelector(&bundle.Spec.SchemaSelector)
	if err != nil {
		return r.fail(ctx, &bundle, "InvalidSelector", err)
	}
	if selector.Empty() {
		return r.fail(ctx, &bundle, "EmptySelector",
			fmt.Errorf("schemaSelector matches everything; refusing — be explicit"))
	}

	var schemas keystonev1alpha1.DatabaseSchemaList
	if err := r.List(ctx, &schemas, &client.ListOptions{LabelSelector: selector}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list schemas: %w", err)
	}

	// Staged rollout path: when spec.rolloutPolicyRef is set, hand off
	// to reconcileStaged. Bundles without a policy keep the original
	// all-at-once fanout (Phase 3 behaviour).
	if bundle.Spec.RolloutPolicyRef != "" {
		bundle.Status.MatchedSchemas = int32(len(schemas.Items))
		bundle.Status.ContentHash = src.ContentHash
		return r.reconcileStaged(ctx, &bundle, schemas.Items, src.ContentHash)
	}

	// Fan out: one MigrationExecution per (bundle, schema, version) that
	// doesn't already exist. B2 — cap concurrent non-terminal
	// Executions at spec.maxConcurrentExecutions; schemas beyond the
	// cap surface as Phase="Throttled" in status.schemaProgress so
	// operators see where they stand.
	var (
		matched  int32
		applied  int32
		inFlight int32
	)
	cap := bundle.Spec.MaxConcurrentExecutions
	progress := make([]keystonev1alpha1.SchemaProgress, 0, len(schemas.Items))
	for i := range schemas.Items {
		s := &schemas.Items[i]
		matched++

		execName := executionName(bundle.Name, bundle.Spec.Version, s.Name)
		var existing keystonev1alpha1.MigrationExecution
		err := r.Get(ctx, types.NamespacedName{
			Namespace: s.Namespace, Name: execName,
		}, &existing)
		switch {
		case err == nil:
			// Content-drift assertion: with content-addressed bundle
			// naming, an existing ME for (bundleName, version, schema)
			// must carry the same ContentHash we're about to write. If
			// it doesn't, the bundle's underlying source was mutated
			// outside the SD reconciler (or the operator was downgraded
			// from a content-addressed build to a 12-char-prefix build
			// and back). Fail loud rather than silently reusing — the
			// runner would then short-circuit "already applied" while
			// the live schema diverges from desired.
			if existing.Spec.ContentHash != src.ContentHash {
				return ctrl.Result{}, fmt.Errorf(
					"BundleNameContentDrift: existing MigrationExecution %s/%s has "+
						"ContentHash=%s but bundle resolves to %s — refusing to "+
						"reuse stale ME (delete the ME and re-emit)",
					existing.Namespace, existing.Name,
					existing.Spec.ContentHash, src.ContentHash)
			}
			if existing.Status.Phase == keystonev1alpha1.ExecutionPhaseSucceeded {
				applied++
			}
			if isExecutionActive(existing.Status.Phase) {
				inFlight++
			}
			progress = append(progress, schemaProgressFromExecution(s, &existing))
			continue
		case !apierrors.IsNotFound(err):
			return ctrl.Result{}, fmt.Errorf("lookup execution %s: %w", execName, err)
		}

		// Not yet created — throttle check. cap=0 means unlimited
		// (pre-B2 default); anything else caps parallelism.
		if cap > 0 && inFlight >= cap {
			progress = append(progress, keystonev1alpha1.SchemaProgress{
				SchemaRef: s.Name,
				TenantID:  s.Labels[keystonev1alpha1.LabelTenantID],
				Phase:     "Throttled",
				Message: fmt.Sprintf(
					"deferred: %d concurrent execution(s) in flight (spec.maxConcurrentExecutions=%d)",
					inFlight, cap),
			})
			continue
		}

		// Phase 3.1: materialise a MigrationPlan before the execution.
		// The plan is auto-approved today; Phase 3.1.1 will gate it on
		// SchemaPolicy lint findings.
		plan, planErr := ensureMigrationPlan(ctx, r.Client, &bundle, s, src, r.Scheme)
		if planErr != nil {
			return ctrl.Result{}, fmt.Errorf("ensure plan: %w", planErr)
		}

		// Create the execution referencing the plan.
		// `bundle` label intentionally carries bundle.Spec.Version (the
		// short content-addressed identifier `<sd>-decl-<16-hex>` or a
		// hand-written semver like `v0.232.250`), NOT bundle.Name —
		// metadata.name for declarative-source bundles embeds the full
		// 64-hex SHA-256, well past the 63-char Kubernetes label-value
		// limit. The PrimaryRef for "which bundle did this come from"
		// is still bundle.Spec.BundleRef on MigrationExecutionSpec (full
		// name, stored as a spec field where MaxLength=253 is permitted).
		// Use the annotation below for the full bundleName when needed.
		exec := &keystonev1alpha1.MigrationExecution{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: s.Namespace,
				Name:      execName,
				Labels: map[string]string{
					"keystone.hexxlock.io/bundle":  bundle.Spec.Version,
					"keystone.hexxlock.io/version": bundle.Spec.Version,
					"keystone.hexxlock.io/schema":  s.Name,
				},
				Annotations: map[string]string{
					"keystone.hexxlock.io/bundle-name": bundle.Name,
				},
			},
			Spec: keystonev1alpha1.MigrationExecutionSpec{
				PlanRef:       plan.Name,
				BundleRef:     bundle.Name,
				BundleVersion: bundle.Spec.Version,
				SchemaRef:     s.Name,
				ContentHash:   src.ContentHash,
				ExecutionMode: bundle.Spec.ExecutionMode,
			},
		}
		if err := controllerutil.SetControllerReference(&bundle, exec, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("set owner ref: %w", err)
		}
		if err := r.Create(ctx, exec); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create execution %s: %w", execName, err)
		}
		inFlight++
		progress = append(progress, keystonev1alpha1.SchemaProgress{
			SchemaRef:    s.Name,
			TenantID:     s.Labels[keystonev1alpha1.LabelTenantID],
			ExecutionRef: execName,
			Phase:        string(keystonev1alpha1.ExecutionPhasePending),
		})
		r.Recorder.Eventf(&bundle, corev1.EventTypeNormal, "ExecutionCreated",
			"created MigrationExecution %s for schema %s (tenant=%s)",
			execName, s.Name, s.Labels[keystonev1alpha1.LabelTenantID])
	}

	// Persist per-schema progress in its own merge patch so the final
	// success/fail patch below doesn't diff it out against a stale
	// baseline (same rationale as patchLintFindings / patchIntegrity /
	// patchApproval).
	if err := r.patchSchemaProgress(ctx, &bundle, progress); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch schemaProgress: %w", err)
	}

	return r.success(ctx, &bundle, src.ContentHash, matched, applied, logger)
}

// isExecutionActive reports whether an execution is in a phase that
// occupies a throttle slot — i.e. running, expanding, or waiting on
// the Contract annotation. Terminal phases (Succeeded / Failed /
// Aborted) free the slot.
func isExecutionActive(phase keystonev1alpha1.ExecutionPhase) bool {
	switch phase {
	case keystonev1alpha1.ExecutionPhaseSucceeded,
		keystonev1alpha1.ExecutionPhaseFailed,
		keystonev1alpha1.ExecutionPhaseAborted:
		return false
	}
	return true
}

// schemaProgressFromExecution translates a MigrationExecution into
// the CRD status shape for a single SchemaProgress entry.
func schemaProgressFromExecution(
	s *keystonev1alpha1.DatabaseSchema,
	exec *keystonev1alpha1.MigrationExecution,
) keystonev1alpha1.SchemaProgress {
	sp := keystonev1alpha1.SchemaProgress{
		SchemaRef:    s.Name,
		TenantID:     s.Labels[keystonev1alpha1.LabelTenantID],
		ExecutionRef: exec.Name,
		Phase:        string(exec.Status.Phase),
		StartedAt:    exec.Status.StartTime,
		CompletedAt:  exec.Status.CompletionTime,
	}
	// Pull the first non-nominal Ready message into the summary so
	// operators see failure cause in one place.
	for i := range exec.Status.Conditions {
		c := &exec.Status.Conditions[i]
		if c.Type == keystonev1alpha1.ConditionTypeReady &&
			c.Status != metav1.ConditionTrue && c.Message != "" {
			sp.Message = truncateIntegrityMessage(c.Message)
			break
		}
	}
	return sp
}

// patchSchemaProgress commits Status.SchemaProgress in its own merge
// patch. Like every other standalone patch helper in this controller,
// the reason is baseline drift — the final success patch captures its
// baseline AFTER this call, so progress entries are treated as
// "already present" and survive the downstream diff.
func (r *MigrationBundleReconciler) patchSchemaProgress(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	progress []keystonev1alpha1.SchemaProgress,
) error {
	original := bundle.DeepCopy()
	bundle.Status.SchemaProgress = progress
	return r.Status().Patch(ctx, bundle, client.MergeFrom(original))
}

// executionName composes a stable name from the bundle, version, and
// schema. Stable so retries find the same Execution rather than spawning
// duplicates. Trimmed to the K8s 253-char limit.
func executionName(bundle, version, schema string) string {
	name := fmt.Sprintf("%s-%s-%s", bundle, version, schema)
	if len(name) > 253 {
		// Fall back to a hash of the long suffix so we stay deterministic.
		name = name[:253]
	}
	return name
}

func (r *MigrationBundleReconciler) success(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	contentHash string,
	matched, applied int32,
	logger logr.Logger,
) (ctrl.Result, error) {
	// Change-only gate (KS#5 follow-up).
	//
	// Snapshot the persisted status BEFORE any mutation so we can
	// compare semantic equality after applying the planned mutations.
	// On a steady-state bundle the only field that would have changed
	// pre-fix was LastPlanTime (stamped unconditionally). With that
	// stamp moved into the change-only branch below, a no-op tick now
	// produces an identical status, and we skip the patch + the audit
	// emit + return a long requeue.
	//
	// Why this matters: the `For()` watch already filters status-only
	// updates via ignoreStatusOnlyUpdates, but SchemaDefinition
	// `Owns(MigrationBundle)` does NOT — and intentionally so, because
	// SD relies on bundle status transitions to learn that fanout
	// finished. Persisting an identical status every 30 seconds was
	// firing SD's Owns watch, which then re-ran markReady, which
	// stamped LastDiffTime and emitted "schema at desired state",
	// driving the same loop in the SD layer. Two layers of churn from
	// one root cause.
	//
	// Compliance impact (audit emit): a no-op reconcile carries no
	// state-change signal. SOC2/HIPAA require an unbroken trail of
	// meaningful events, not proof-of-liveness — that lives in
	// metrics + the apiserver audit log. Emitting one AuditEntry per
	// 30s × 50 bundles = 432K entries/day was directly responsible
	// for the etcd backlogs drained twice in May 2026 (50K and 20K).
	originalStatus := bundle.Status.DeepCopy()
	patch := client.MergeFrom(bundle.DeepCopy())

	bundle.Status.ObservedGeneration = bundle.Generation
	bundle.Status.ContentHash = contentHash
	bundle.Status.MatchedSchemas = matched
	bundle.Status.AppliedSchemas = applied

	bundle.Status.Conditions = conditions.Set(bundle.Status.Conditions,
		keystonev1alpha1.ConditionTypePlanned, metav1.ConditionTrue,
		"Reconciled",
		fmt.Sprintf("dispatched to %d schema(s); %d already at version %s",
			matched, applied, bundle.Spec.Version),
		bundle.Generation)

	ready := matched == applied && matched > 0
	if ready {
		bundle.Status.Conditions = conditions.MarkReady(bundle.Status.Conditions,
			"AllSchemasApplied",
			fmt.Sprintf("version %s applied to %d schema(s)", bundle.Spec.Version, matched),
			bundle.Generation)
		bundle.Status.Conditions = conditions.ClearProgressing(bundle.Status.Conditions, bundle.Generation)
	} else {
		bundle.Status.Conditions = conditions.MarkProgressing(bundle.Status.Conditions,
			"ApplyingToSchemas",
			fmt.Sprintf("%d/%d schemas at version %s", applied, matched, bundle.Spec.Version),
			bundle.Generation)
	}

	// Long requeue when Ready=True (drift backstop only); tighter when
	// applying so per-execution progress surfaces fast.
	requeue := readyRequeue
	if !ready {
		requeue = applyingRequeue
	}
	requeue = requeueWithJitter(requeue)

	// Equality check is on Status only — apimeta.SetStatusCondition
	// (wrapped by conditions.Set / MarkReady / etc.) preserves
	// LastTransitionTime when status doesn't transition, so a true
	// no-op pass produces a fully-equal status.
	if equality.Semantic.DeepEqual(originalStatus, &bundle.Status) {
		logger.V(1).Info("reconciled (no change)",
			"matched", matched, "applied", applied, "ready", ready)
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	// Real change. Stamp LastPlanTime now (and only now) so operators
	// reading `kubectl get migrationbundle -o yaml` see the wallclock
	// of the most recent state mutation, not of the most recent
	// reconcile-tick.
	now := metav1.Now()
	bundle.Status.LastPlanTime = &now

	if err := r.Status().Patch(ctx, bundle, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}
	logger.V(1).Info("reconciled", "matched", matched, "applied", applied, "ready", ready)

	// Audit chain — one entry per actual reconcile-driven state
	// transition. Non-blocking.
	r.emitAudit(ctx, bundle, "reconcile",
		fmt.Sprintf("fanned out to %d schema(s); %d at target version %s",
			matched, applied, bundle.Spec.Version),
		"success")

	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *MigrationBundleReconciler) fail(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	reason string, cause error,
) (ctrl.Result, error) {
	patch := client.MergeFrom(bundle.DeepCopy())
	bundle.Status.ObservedGeneration = bundle.Generation
	bundle.Status.Conditions = conditions.MarkNotReady(bundle.Status.Conditions, reason, cause, bundle.Generation)
	if perr := r.Status().Patch(ctx, bundle, patch); perr != nil {
		return ctrl.Result{}, fmt.Errorf("patch status (after %s): %w; underlying: %v", reason, perr, cause)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(bundle, corev1.EventTypeWarning, reason, "%s", cause.Error())
	}
	r.emitAudit(ctx, bundle, "reconcile", reason+": "+cause.Error(), "error")
	return ctrl.Result{}, cause
}

// emitAudit is a convenience wrapper that feeds the AuditLogger when
// wired. The reconciler stays usable in tests (fake clients, no
// AuditLogger plumbed) — missing logger is a no-op, not an error, so
// test-only flows don't have to reconstruct the audit chain.
func (r *MigrationBundleReconciler) emitAudit(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	verb, reason, outcome string,
) {
	if r.AuditLogger == nil {
		return
	}
	if _, _, err := r.AuditLogger.Append(ctx, audit.Event{
		Verb:        verb,
		Actor:       r.ManagerIdentity,
		ResourceRef: audit.ResourceRefFromObject(bundle),
		Reason:      reason,
		Outcome:     outcome,
		After:       bundle,
	}); err != nil {
		log.FromContext(ctx).Error(err, "audit append failed",
			"controller", "migrationbundle",
			"name", bundle.Name,
			"verb", verb,
			"outcome", outcome)
	}
}

// patchIntegrity commits the bundle's keystone.sum observation to the
// apiserver in its own merge patch. Same rationale as
// patchLintFindings: success/fail patches downstream capture their
// baseline AFTER this call, so any status fields mutated here are
// already persisted and will survive the next patch diff. Without the
// standalone patch, the integrity block would appear in the DeepCopy
// baseline the success path uses and be diffed out to nothing.
func (r *MigrationBundleReconciler) patchIntegrity(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	src *migration.ResolvedSource,
) error {
	original := bundle.DeepCopy()
	bundle.Status.Integrity = integrityFromSource(src)

	cs, reason, message := integrityCondition(src)
	bundle.Status.Conditions = conditions.Set(bundle.Status.Conditions,
		keystonev1alpha1.ConditionTypeIntegrityVerified,
		cs, reason, message, bundle.Generation)

	return r.Status().Patch(ctx, bundle, client.MergeFrom(original))
}

// integrityFromSource marshals the resolver's integrity observation
// onto the CRD status shape. Returns nil only in defensive cases; the
// resolver always stamps a status.
func integrityFromSource(src *migration.ResolvedSource) *keystonev1alpha1.IntegrityStatus {
	if src == nil {
		return nil
	}
	out := &keystonev1alpha1.IntegrityStatus{
		Status:   string(src.IntegrityStatus),
		RootHash: src.IntegritySum.RootHash,
	}
	if src.IntegrityError != nil {
		out.Message = truncateIntegrityMessage(src.IntegrityError.Error())
	}
	// Per-file listing is published only for Valid — we do not surface
	// partial sums for Malformed/Mismatch because clients may act on
	// stale or ambiguous data.
	if src.IntegrityStatus == migration.SumStatusValid {
		out.Files = make([]keystonev1alpha1.FileIntegrity, 0, len(src.IntegritySum.Entries))
		for _, e := range src.IntegritySum.Entries {
			out.Files = append(out.Files, keystonev1alpha1.FileIntegrity{
				Name: e.Name,
				Hash: e.Hash,
			})
		}
	}
	return out
}

// integrityCondition maps the resolver's SumStatus onto the standard
// Kubernetes condition tuple (status, reason, message).
func integrityCondition(src *migration.ResolvedSource) (metav1.ConditionStatus, string, string) {
	switch src.IntegrityStatus {
	case migration.SumStatusValid:
		return metav1.ConditionTrue,
			keystonev1alpha1.ReasonSumValid,
			fmt.Sprintf("resolved source matches keystone.sum (%d files, root %s)",
				len(src.IntegritySum.Entries), shortHash(src.IntegritySum.RootHash))
	case migration.SumStatusMissing:
		return metav1.ConditionUnknown,
			keystonev1alpha1.ReasonSumMissing,
			"no keystone.sum in resolved source; integrity unverified (advisory mode). " +
				"Run `keystonectl sum <dir>` and commit the file to opt in."
	case migration.SumStatusMalformed:
		msg := "keystone.sum failed to parse"
		if src.IntegrityError != nil {
			msg = "keystone.sum failed to parse: " + src.IntegrityError.Error()
		}
		return metav1.ConditionFalse,
			keystonev1alpha1.ReasonSumMalformed,
			truncateIntegrityMessage(msg)
	case migration.SumStatusMismatch:
		msg := "resolved source diverges from keystone.sum"
		if src.IntegrityError != nil {
			msg = src.IntegrityError.Error()
		}
		return metav1.ConditionFalse,
			keystonev1alpha1.ReasonSumMismatch,
			truncateIntegrityMessage(msg)
	default:
		return metav1.ConditionUnknown, "UnknownIntegrity",
			fmt.Sprintf("unknown integrity status %q", src.IntegrityStatus)
	}
}

// shortHash trims a hex digest to 12 chars for condition messages. The
// full hash lives in status.integrity.rootHash; operators don't need
// all 64 characters on their terminal.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

func truncateIntegrityMessage(s string) string {
	if len(s) <= integrityMessageMaxLen {
		return s
	}
	return s[:integrityMessageMaxLen-1] + "…"
}

// patchApproval commits the bundle's N-of-M approval observation to
// the apiserver in its own merge patch. Same rationale as
// patchIntegrity and patchLintFindings: later success/fail patches
// capture their baseline AFTER this call, so any status fields
// mutated here are persisted before a downstream merge-patch could
// diff them out.
//
// Also responsible for setting ConditionTypeApproved — we keep
// condition + summary together so they never drift.
func (r *MigrationBundleReconciler) patchApproval(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	findings []keystonev1alpha1.LintFinding,
) error {
	matched, err := r.matchedSchemaPolicies(ctx, bundle)
	if err != nil {
		// Don't stall reconcile on policy-list errors — the controller
		// will retry. Skip the patch this cycle.
		return nil
	}
	res := approval.Evaluate(bundle, matched, findings)

	original := bundle.DeepCopy()
	bundle.Status.Approval = approvalSummaryFromResult(res)

	cs, reason, message := approvalCondition(res, bundle.Status.Approval)
	bundle.Status.Conditions = conditions.Set(bundle.Status.Conditions,
		keystonev1alpha1.ConditionTypeApproved,
		cs, reason, message, bundle.Generation)

	return r.Status().Patch(ctx, bundle, client.MergeFrom(original))
}

// matchedSchemaPolicies returns the SchemaPolicies whose target
// selector matches the bundle. Mirrors the webhook's helper so
// admission-time and reconcile-time evaluators agree.
// matchedSchemaPolicies returns SchemaPolicies that govern this bundle.
// Pinned PolicyRef + label-matched TargetSelectors are UNION (additive),
// not exclusive — mirrors the webhook's matchedPolicies fix in v0.1.38.
// See that helper's comment for the rationale.
func (r *MigrationBundleReconciler) matchedSchemaPolicies(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
) ([]keystonev1alpha1.SchemaPolicy, error) {
	var policies keystonev1alpha1.SchemaPolicyList
	if err := r.List(ctx, &policies); err != nil {
		return nil, fmt.Errorf("list SchemaPolicies: %w", err)
	}
	seen := make(map[string]bool, len(policies.Items))
	var out []keystonev1alpha1.SchemaPolicy
	for i := range policies.Items {
		p := &policies.Items[i]
		// Pinned policyRef matches additively.
		if bundle.Spec.PolicyRef != "" && bundle.Spec.PolicyRef == p.Name {
			if !seen[p.Name] {
				out = append(out, *p)
				seen[p.Name] = true
			}
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&p.Spec.TargetSelector)
		if err != nil {
			continue
		}
		if sel.Matches(labels.Set(bundle.Labels)) && !seen[p.Name] {
			out = append(out, *p)
			seen[p.Name] = true
		}
	}
	return out, nil
}

// approvalSummaryFromResult translates the evaluator output onto the
// CRD status shape. When no policies applied, returns nil so the
// status field round-trips cleanly (instead of an empty but non-nil
// pointer).
func approvalSummaryFromResult(res approval.Result) *keystonev1alpha1.ApprovalSummary {
	if len(res.Policies) == 0 {
		return nil
	}
	out := &keystonev1alpha1.ApprovalSummary{Satisfied: res.Satisfied}
	for _, pr := range res.Policies {
		out.Policies = append(out.Policies, keystonev1alpha1.PolicyApprovalState{
			SchemaPolicy:   pr.SchemaPolicy,
			ApprovalPolicy: pr.ApprovalPolicy,
			Applied:        pr.Applied,
			Required:       pr.Required,
			Received:       pr.Received,
			Approvers:      pr.Approvers,
			Satisfied:      pr.Satisfied,
			Violation:      truncateIntegrityMessage(pr.Violation),
		})
	}
	return out
}

// approvalCondition maps the evaluator result onto the Kubernetes
// condition tuple (status, reason, message).
func approvalCondition(res approval.Result, summary *keystonev1alpha1.ApprovalSummary) (metav1.ConditionStatus, string, string) {
	if summary == nil {
		// No policies applied — the approval gate doesn't block. Mark
		// Unknown so operators can distinguish "no policy matched" from
		// "policy matched and approved" without parsing the summary.
		return metav1.ConditionUnknown, "NoApprovalPolicyApplies",
			"no matched SchemaPolicy declares approvalPolicies"
	}
	if res.Satisfied {
		return metav1.ConditionTrue, "AllApprovalsReceived",
			fmt.Sprintf("%d approval policy(ies) satisfied", len(summary.Policies))
	}
	// Build a compact reason-ful message listing the first few
	// violations; full list lives in status.approval.policies.
	unsat := res.Unsatisfied()
	parts := make([]string, 0, len(unsat))
	for i, pr := range unsat {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(unsat)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("%s/%s: %d/%d",
			pr.SchemaPolicy, pr.ApprovalPolicy, pr.Received, pr.Required))
	}
	return metav1.ConditionFalse, "ApprovalsMissing",
		truncateIntegrityMessage("unmet approvals — " + strings.Join(parts, "; "))
}

// lintFindingsStatusCap is the upper bound on findings persisted to
// status.lintFindings. Mirrors the kubebuilder MaxItems annotation on
// the CRD type — exceeding it causes the apiserver to reject the
// status patch with "Too many: N: must have at most 512 items",
// which kills the entire reconcile loop and blocks downstream
// MigrationPlan creation. A 187-table tenant SD diff trips this
// trivially: a single lint rule like no-varchar-without-limit fires
// once per VARCHAR column and a realistic schema can have 500+
// such columns.
//
// We truncate at the cap and stamp a synthetic last finding that
// records how many were elided, so the operator sees the elision
// in `kubectl get migrationbundle … -o yaml` rather than silently
// missing diagnostics.
const lintFindingsStatusCap = 512

// patchLintFindings commits lintFindings to the apiserver in its own
// merge patch so that later success/fail patches — whose baselines are
// captured AFTER this call — don't inadvertently treat findings as
// "unchanged from baseline" and drop them from the diff.
//
// Caps the slice at lintFindingsStatusCap to satisfy the CRD's
// MaxItems schema; preserves the original count via a trailing
// synthetic "findings-truncated" entry when truncation occurred.
func (r *MigrationBundleReconciler) patchLintFindings(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	findings []keystonev1alpha1.LintFinding,
) error {
	original := bundle.DeepCopy()
	bundle.Status.LintFindings = capLintFindings(findings)
	return r.Status().Patch(ctx, bundle, client.MergeFrom(original))
}

// capLintFindings truncates findings to lintFindingsStatusCap and
// appends a synthetic "findings-truncated" entry recording how many
// were elided. Returns input unchanged when len <= cap.
func capLintFindings(findings []keystonev1alpha1.LintFinding) []keystonev1alpha1.LintFinding {
	if len(findings) <= lintFindingsStatusCap {
		return findings
	}
	dropped := len(findings) - (lintFindingsStatusCap - 1)
	out := make([]keystonev1alpha1.LintFinding, lintFindingsStatusCap)
	copy(out, findings[:lintFindingsStatusCap-1])
	out[lintFindingsStatusCap-1] = keystonev1alpha1.LintFinding{
		Rule:     "findings-truncated",
		Severity: keystonev1alpha1.LintLevelWarning,
		File:     "",
		Line:     0,
		Message:  fmt.Sprintf("status.lintFindings truncated; %d further finding(s) elided to fit CRD MaxItems=%d cap. Run analyzers locally for the full set.", dropped, lintFindingsStatusCap),
	}
	return out
}

// runAnalyzers walks the default analyzer registry over the resolved
// source. Returns the findings slice (for status.lintFindings) and a
// non-nil error when any finding is at severity=error — reconcile
// refuses to fan out MigrationExecutions when that happens.
//
// Analyzers run against SQL content (versioned strategy) and
// operations (pgroll strategy). The migration.Migration shape
// normalises both.
func (r *MigrationBundleReconciler) runAnalyzers(
	_ context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	src *migration.ResolvedSource,
) ([]keystonev1alpha1.LintFinding, error) {
	reg := analyze.DefaultRegistry()

	// Phase 9.1: if a dev database pool is available, register the
	// SemanticAnalyzer to replay migrations against an ephemeral
	// schema after the static analyzers run. The semantic analyzer
	// catches runtime SQL errors, type mismatches, and FK reference
	// issues that static analysis cannot detect.
	if r.DevDBPool != nil {
		reg.Register(devdb.NewSemanticAnalyzer(r.DevDBPool))
	}

	m := &analyze.Migration{
		BundleName:    bundle.Name,
		Version:       bundle.Spec.Version,
		Operations:    bundle.Spec.Operations,
		HasDownSource: bundle.Spec.DownSource != nil,
		Strategy:      string(bundle.Spec.Strategy),
	}
	if src != nil {
		for _, name := range src.Names {
			m.Files = append(m.Files, analyze.FileBody{Name: name, Body: src.Files[name]})
		}
	}
	raw, _ := reg.Run(context.Background(), m)
	out := make([]keystonev1alpha1.LintFinding, 0, len(raw))
	var firstError string
	for _, f := range raw {
		out = append(out, keystonev1alpha1.LintFinding{
			Severity: f.Severity,
			Rule:     f.Rule,
			File:     f.File,
			Line:     f.Line,
			Message:  f.Message,
		})
		if f.Severity == keystonev1alpha1.LintLevelError && firstError == "" {
			firstError = fmt.Sprintf("[%s] %s:%d — %s", f.Rule, f.File, f.Line, f.Message)
		}
	}
	if firstError != "" {
		return out, fmt.Errorf("lint error: %s (and %d total findings)", firstError, len(out))
	}
	return out, nil
}

// SetupWithManager wires the reconciler. Watches DatabaseSchemas so a
// new schema with matching labels triggers a re-fanout without waiting
// for the periodic requeue.
func (r *MigrationBundleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keystone-migrationbundle")
	}
	if r.Resolver == nil {
		// APIReader (uncached) — the operator's ConfigMap cache is
		// label-scoped to SD-emitted bundles; user-authored
		// ConfigMap-sourced bundles fall outside that scope and must
		// be read direct from the apiserver. See cmd/manager/main.go
		// cache.Options.ByObject.
		r.Resolver = migration.NewConfigMapResolver(mgr.GetAPIReader())
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.MigrationBundle{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Owns(&keystonev1alpha1.MigrationExecution{}).
		Named("migrationbundle").
		Complete(r)
}

// labelSelectorString is a thin helper kept for symmetry with future
// admission-webhook code that needs to log selectors. Currently unused
// outside its return value.
func labelSelectorString(s metav1.LabelSelector) string {
	sel, err := metav1.LabelSelectorAsSelector(&s)
	if err != nil {
		return "<invalid>"
	}
	return labels.FormatLabels(map[string]string{"selector": sel.String()})
}

var _ = labelSelectorString // referenced indirectly when admission webhook is wired
