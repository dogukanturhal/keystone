// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/conditions"
	"github.com/dogukanturhal/keystone-sdk/go/drift"
	"github.com/dogukanturhal/keystone-sdk/go/declarative"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// SchemaDefinitionReconciler is the Atlas-parity declarative engine.
// It watches SchemaDefinition CRs, inspects the live schema, diffs it
// against the desired state, and auto-generates a MigrationBundle
// (plus ConfigMap with the SQL) when the diff is non-empty.
//
// The resulting MigrationBundle flows through the normal Phase 3
// pipeline: Conftest → admission → Phase 11 analyzers → MigrationPlan
// → MigrationExecution. The declarative path is purely an "author
// experience" layer on top; the execution machinery is unchanged.
type SchemaDefinitionReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Pools    *pg.PoolCache

	SystemNamespace string
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=schemadefinitions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=schemadefinitions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=schemadefinitions/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the entry point.
func (r *SchemaDefinitionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("schemadefinition", req.NamespacedName)

	var sd keystonev1alpha1.SchemaDefinition
	if err := r.Get(ctx, req.NamespacedName, &sd); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !sd.DeletionTimestamp.IsZero() {
		// No finalizer today — owner-reference to the generated
		// ConfigMap/MigrationBundle means Argo prune handles children.
		return ctrl.Result{}, nil
	}

	// Resolve target DatabaseSchemas. SchemaRef → 1 item, SchemaSelector
	// → N items. Admission's XValidation guarantees exactly one is set.
	// A dangling schemaRef gets the precise SchemaRefNotFound reason
	// (mirrors SchemaSnapshotReconciler); everything else stays the
	// generic SchemaResolutionFailed.
	schemas, err := r.resolveTargetSchemas(ctx, &sd)
	if err != nil {
		reason := "SchemaResolutionFailed"
		if errors.Is(err, errSchemaRefNotFound) {
			reason = "SchemaRefNotFound"
		}
		return r.fail(ctx, &sd, reason, err)
	}
	if len(schemas) == 0 {
		// SchemaSelector matched nothing. Not a hard fail — operator may
		// be pre-creating the SD before any tenant lands. Re-queue and
		// surface as "not yet matched".
		return r.markNoMatches(ctx, &sd)
	}
	for i := range schemas {
		if !conditions.IsTrue(schemas[i].Status.Conditions, keystonev1alpha1.ConditionTypeReady) {
			// Wait for every matched schema to be Ready before inspecting.
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}

	// Inspect + diff each matched schema. With SchemaRef this is one
	// pass; with SchemaSelector each tenant DB is hit individually so
	// the differ sees its actual live shape.
	results, err := r.inspectAndDiffAll(ctx, &sd, schemas)
	if err != nil {
		var destructive *declarative.ErrDestructiveRefused
		if errors.As(err, &destructive) {
			// The first schema's diff contained refused destructive
			// ops. We surface it the same way as singular SchemaRef
			// because the destructive policy applies SD-wide.
			plan := destructive.Plan
			return r.markDestructiveBlocked(ctx, &sd, plan, destructive)
		}
		return r.fail(ctx, &sd, "InspectOrDiffFailed", err)
	}

	// Drift detection across fan-out. With one schema this is a no-op.
	canonicalSchema, canonicalPlan, drifted := selectCanonicalPlan(results)
	if len(drifted) > 0 && policyOf(&sd) == keystonev1alpha1.MixedVersionPolicyRefuse {
		return r.markDriftRefused(ctx, &sd, results, drifted)
	}

	// Empty diff = every matched schema is at desired state.
	if canonicalPlan.Empty() {
		return r.markReady(ctx, &sd, len(schemas), logger)
	}

	// Non-empty diff → emit one MigrationBundle.
	// Strategy selection: versioned emits SQL in a ConfigMap; pgroll
	// emits MigrationOperations directly on the bundle spec.
	var bundleName string
	if sd.Spec.ApplyStrategy == string(keystonev1alpha1.StrategyPgrollExpandContract) {
		bundleName, err = r.emitPgrollBundle(ctx, &sd, canonicalSchema, canonicalPlan)
	} else {
		bundleName, err = r.emitBundle(ctx, &sd, canonicalSchema, canonicalPlan)
	}
	if err != nil {
		return r.fail(ctx, &sd, "BundleEmissionFailed", err)
	}
	plan := canonicalPlan

	// Status update: report pending ops + current bundle ref.
	patch := client.MergeFrom(sd.DeepCopy())
	sd.Status.ObservedGeneration = sd.Generation
	sd.Status.PendingOperations = int32(len(plan.Statements))
	sd.Status.CurrentBundleRef = bundleName
	sd.Status.MatchedSchemas = int32(len(schemas))
	sd.Status.DriftedSchemas = nil
	sd.Status.Fingerprint = specFingerprint(&sd.Spec)
	now := metav1.Now()
	sd.Status.LastDiffTime = &now
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeInspected, metav1.ConditionTrue, "LiveSchemaRead",
		fmt.Sprintf("inspected %d schema(s); canonical %s carries %d ops",
			len(schemas), canonicalSchema.Name, len(plan.Statements)),
		sd.Generation)
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeDiffComputed, metav1.ConditionTrue, "DriftDetected",
		fmt.Sprintf("%d statement(s) to apply (%d destructive)",
			len(plan.Statements), plan.DestructiveOps),
		sd.Generation)
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeBundleGenerated, metav1.ConditionTrue, "BundleEmitted",
		fmt.Sprintf("MigrationBundle %q tracks apply across %d schema(s)",
			bundleName, len(schemas)),
		sd.Generation)
	// Clear any stale Ready=False from a previous reconcile (e.g.
	// DestructiveRefused that has since been resolved by a spec edit).
	// The bundle-in-flight state is "not yet at desired state" — model
	// that as Ready=False [BundleEmitted] until the matched schemas
	// report LastAppliedFingerprint == SD.Status.Fingerprint, at which
	// point markReady flips Ready=True.
	//
	// Without this update, conditions can disagree across reconciles:
	// DiffComputed[True, "0 destructive"] coexists with the stale
	// Ready[False, "DestructiveRefused: 2 destructive ops refused"]
	// from before the spec was fixed. Operators reading the SD via
	// `kubectl describe` see contradictory state and assume the gate
	// is still blocking apply (observed 2026-05-04 against
	// example-service-control-public-desired after the realms.id /
	// realm_tenants.* nullability fix).
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeReady, metav1.ConditionFalse, "BundleEmitted",
		fmt.Sprintf("%d operation(s) pending via %s; awaiting MigrationExecution apply",
			len(plan.Statements), bundleName),
		sd.Generation)
	sd.Status.Conditions = conditions.MarkProgressing(sd.Status.Conditions,
		"Applying",
		fmt.Sprintf("%d operation(s) pending via %s", len(plan.Statements), bundleName),
		sd.Generation)
	if err := r.Status().Patch(ctx, &sd, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}
	logger.Info("bundle emitted", "bundle", bundleName, "operations", len(plan.Statements),
		"destructive", plan.DestructiveOps, "matched_schemas", len(schemas))
	// Re-queue in a minute to re-diff once the bundle applies.
	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

func (r *SchemaDefinitionReconciler) markReady(
	ctx context.Context,
	sd *keystonev1alpha1.SchemaDefinition,
	matchedSchemas int,
	logger logr.Logger,
) (ctrl.Result, error) {
	// Change-only gate (KS#5 follow-up).
	//
	// Pre-fix this method stamped LastDiffTime=now on every tick and
	// patched the resulting status, which fired the bundle-emit lane's
	// Owns(SchemaDefinition) → MigrationBundle → SD round-trip on every
	// hourly tick (and on every MigrationBundle status churn from the
	// 30s success-path stamp pre-fix). Snapshotting status before
	// mutation and gating the patch on semantic-equality keeps drift
	// detection (still hourly via RequeueAfter) without persisting a
	// no-op write.
	originalStatus := sd.Status.DeepCopy()
	patch := client.MergeFrom(sd.DeepCopy())

	sd.Status.ObservedGeneration = sd.Generation
	sd.Status.PendingOperations = 0
	sd.Status.CurrentBundleRef = ""
	sd.Status.MatchedSchemas = int32(matchedSchemas)
	sd.Status.DriftedSchemas = nil
	sd.Status.Fingerprint = specFingerprint(&sd.Spec)
	msg := "live schema matches desired state; no operations pending"
	if matchedSchemas > 1 {
		msg = fmt.Sprintf("%d schema(s) at desired state; no operations pending", matchedSchemas)
	}
	// Restate the pipeline-stage conditions. Only the bundle-emit lane
	// used to write them, so on convergence they stayed frozen at the
	// last drifted reconcile: Ready[True, "schema matches desired"]
	// sitting next to DiffComputed[True, DriftDetected, "174
	// statement(s) to apply"] and BundleGenerated[True] naming a bundle
	// that CurrentBundleRef had already been cleared of. Reading that
	// via `kubectl describe`, an operator cannot tell whether the SD
	// converged or is 174 ops behind — and the timestamps do not
	// disambiguate, because LastTransitionTime only moves on a status
	// flip, so all four look equally current. Observed 2026-08-06 on
	// downstream-service-public-desired, where the three lane conditions were
	// still pinned to a 21:11:30 diff hours after it applied.
	//
	// This is the mirror of the bundle lane's stale-Ready clear: the
	// invariant either way is that Ready and the three stage conditions
	// all describe the same reconcile.
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeInspected, metav1.ConditionTrue, "LiveSchemaRead",
		fmt.Sprintf("inspected %d schema(s); all at desired state", matchedSchemas),
		sd.Generation)
	// True, not False: the diff ran and came back empty. Empty is a
	// result, not a missing one — False here would read as "the differ
	// never got to run", which is what the failure paths mean by it.
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeDiffComputed, metav1.ConditionTrue, "NoDrift",
		"0 statement(s) to apply (0 destructive)", sd.Generation)
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeBundleGenerated, metav1.ConditionFalse, "NoBundleNeeded",
		"no MigrationBundle required; live schema matches desired", sd.Generation)
	sd.Status.Conditions = conditions.MarkReady(sd.Status.Conditions,
		"SchemaMatchesDesired", msg, sd.Generation)
	sd.Status.Conditions = conditions.ClearProgressing(sd.Status.Conditions, sd.Generation)

	// Drift backstop is hourly. With ~50 SDs, even on a fully steady
	// cluster that's 1 reconcile/72s — the DriftController is the
	// primary detector and runs on its own schedule.
	requeue := 1 * time.Hour

	if equality.Semantic.DeepEqual(originalStatus, &sd.Status) {
		logger.V(1).Info("schema at desired state (no change)",
			"matched_schemas", matchedSchemas)
		return ctrl.Result{RequeueAfter: requeue}, nil
	}

	now := metav1.Now()
	sd.Status.LastDiffTime = &now

	if err := r.Status().Patch(ctx, sd, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch ready: %w", err)
	}
	// Demoted from info → V(1): a per-tick "still healthy" liveness
	// signal belongs in metrics, not stdout. Real transitions
	// (NotReady → Ready, drift detected, bundle emitted) still log at
	// info elsewhere. Matches cert-manager / Flux / Argo CD verbosity
	// conventions.
	logger.V(1).Info("schema at desired state", "matched_schemas", matchedSchemas)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// markNoMatches handles SchemaSelector matching zero DatabaseSchemas.
// Not a hard failure: an operator pre-creating a tenant-template SD
// before the first tenant DB exists is a legitimate use case. We
// surface it on status and re-queue.
func (r *SchemaDefinitionReconciler) markNoMatches(
	ctx context.Context,
	sd *keystonev1alpha1.SchemaDefinition,
) (ctrl.Result, error) {
	patch := client.MergeFrom(sd.DeepCopy())
	sd.Status.ObservedGeneration = sd.Generation
	sd.Status.MatchedSchemas = 0
	sd.Status.PendingOperations = 0
	sd.Status.CurrentBundleRef = ""
	sd.Status.DriftedSchemas = nil
	now := metav1.Now()
	sd.Status.LastDiffTime = &now
	// Nothing was inspected, diffed, or emitted this reconcile — say so
	// rather than leaving the previous reconcile's stages standing. An
	// SD whose selector stops matching (a tenant DB deleted, a label
	// edited) would otherwise keep reporting a live inspection and an
	// emitted bundle for schemas that are no longer in scope.
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeInspected, metav1.ConditionFalse, "NoMatchingSchemas",
		"no schemas matched; nothing to inspect", sd.Generation)
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeDiffComputed, metav1.ConditionFalse, "NoMatchingSchemas",
		"no schemas matched; no diff attempted", sd.Generation)
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeBundleGenerated, metav1.ConditionFalse, "NoMatchingSchemas",
		"no schemas matched; no MigrationBundle emitted", sd.Generation)
	sd.Status.Conditions = conditions.MarkNotReady(sd.Status.Conditions,
		"NoMatchingSchemas",
		fmt.Errorf("schemaSelector matched 0 DatabaseSchemas in namespace %s; create at least one matching schema",
			sd.Namespace),
		sd.Generation)
	sd.Status.Conditions = conditions.ClearProgressing(sd.Status.Conditions, sd.Generation)
	if err := r.Status().Patch(ctx, sd, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch no-matches: %w", err)
	}
	return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}

// markDriftRefused records the divergent fan-out and refuses to emit
// a bundle when MixedVersionPolicy=Refuse. Operators see the drifted
// schemas in Status.DriftedSchemas and either reconcile manually or
// flip MixedVersionPolicy=MostBehindWins after audit review.
func (r *SchemaDefinitionReconciler) markDriftRefused(
	ctx context.Context,
	sd *keystonev1alpha1.SchemaDefinition,
	results []schemaDiff,
	drifted []keystonev1alpha1.DriftedSchema,
) (ctrl.Result, error) {
	patch := client.MergeFrom(sd.DeepCopy())
	sd.Status.ObservedGeneration = sd.Generation
	sd.Status.MatchedSchemas = int32(len(results))
	sd.Status.DriftedSchemas = drifted
	// Pending ops reflect the most-behind schema's diff so operators
	// see the impact even though no bundle is being emitted.
	mostBehindOps := 0
	for _, d := range drifted {
		if int(d.PendingOperations) > mostBehindOps {
			mostBehindOps = int(d.PendingOperations)
		}
	}
	sd.Status.PendingOperations = int32(mostBehindOps)
	sd.Status.CurrentBundleRef = ""
	now := metav1.Now()
	sd.Status.LastDiffTime = &now
	// Inspect and diff both ran here — the policy gate is what stopped
	// the emit. BundleGenerated must follow CurrentBundleRef down to
	// False, or status claims a bundle it just cleared the ref to.
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeInspected, metav1.ConditionTrue, "LiveSchemaRead",
		fmt.Sprintf("inspected %d schema(s); %d diverge", len(results), len(drifted)),
		sd.Generation)
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeDiffComputed, metav1.ConditionTrue, "MixedVersionsDetected",
		fmt.Sprintf("most-behind schema carries %d statement(s)", mostBehindOps),
		sd.Generation)
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeBundleGenerated, metav1.ConditionFalse, "MixedVersionsDetected",
		"no MigrationBundle emitted under mixedVersionPolicy=Refuse", sd.Generation)
	sd.Status.Conditions = conditions.MarkNotReady(sd.Status.Conditions,
		"MixedVersionsDetected",
		fmt.Errorf("%d schema(s) diverge under mixedVersionPolicy=Refuse; reconcile drift or set policy=MostBehindWins",
			len(drifted)),
		sd.Generation)
	sd.Status.Conditions = conditions.ClearProgressing(sd.Status.Conditions, sd.Generation)
	if err := r.Status().Patch(ctx, sd, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch drift-refused: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(sd, corev1.EventTypeWarning, "MixedVersionsDetected",
			"%d schema(s) diverge under mixedVersionPolicy=Refuse; review Status.DriftedSchemas",
			len(drifted))
	}
	// Slow re-queue — drift won't self-heal without operator action.
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// schemaDiff bundles a fanned-out schema with its computed diff so the
// reconciler can pick the canonical (most-behind) plan and surface
// drift state.
type schemaDiff struct {
	schema *keystonev1alpha1.DatabaseSchema
	plan   *declarative.Plan
	// inspectedAt records when the live shape was read for this
	// individual schema. Surfaced into DriftedSchema entries.
	inspectedAt metav1.Time
}

// errSchemaRefNotFound distinguishes a dangling spec.schemaRef from
// transient resolution failures so Reconcile can surface the precise
// SchemaRefNotFound condition reason instead of the generic
// SchemaResolutionFailed.
var errSchemaRefNotFound = errors.New("schemaRef not found")

// resolveTargetSchemas returns the DatabaseSchemas this SD applies to.
// For SchemaRef, exactly one (or NotFound). For SchemaSelector, every
// in-namespace schema whose labels satisfy the selector.
func (r *SchemaDefinitionReconciler) resolveTargetSchemas(
	ctx context.Context,
	sd *keystonev1alpha1.SchemaDefinition,
) ([]keystonev1alpha1.DatabaseSchema, error) {
	if sd.Spec.SchemaRef != "" {
		var s keystonev1alpha1.DatabaseSchema
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: sd.Namespace, Name: sd.Spec.SchemaRef,
		}, &s); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("%w: DatabaseSchema %s/%s",
					errSchemaRefNotFound, sd.Namespace, sd.Spec.SchemaRef)
			}
			return nil, fmt.Errorf("get DatabaseSchema: %w", err)
		}
		return []keystonev1alpha1.DatabaseSchema{s}, nil
	}
	if sd.Spec.SchemaSelector == nil {
		// Defensive — admission's XValidation makes this unreachable in
		// practice but the controller should not panic on a hand-crafted
		// CR that bypassed admission (e.g. via raw etcd write during DR).
		return nil, fmt.Errorf("neither schemaRef nor schemaSelector set")
	}
	sel, err := metav1.LabelSelectorAsSelector(sd.Spec.SchemaSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid schemaSelector: %w", err)
	}
	if sel.Empty() {
		return nil, fmt.Errorf("schemaSelector matches everything; refusing — declare at least one matchLabels entry")
	}
	var list keystonev1alpha1.DatabaseSchemaList
	if err := r.List(ctx, &list,
		client.InNamespace(sd.Namespace),
		client.MatchingLabelsSelector{Selector: sel},
	); err != nil {
		return nil, fmt.Errorf("list DatabaseSchemas: %w", err)
	}
	return list.Items, nil
}

// inspectAndDiffAll opens an admin pool to each matched schema's
// LogicalDatabase + Provider, reads the live shape, and computes the
// per-schema diff. Pool acquisition is cached in r.Pools so repeated
// reconciles against the same provider reuse connections.
//
// On the first ErrDestructiveRefused encountered, the function returns
// the wrapped error so the caller can surface it consistently with the
// singular-SchemaRef path. (Destructive policy is SD-wide, so refusing
// once is enough — the bundle wouldn't be emittable anyway.)
func (r *SchemaDefinitionReconciler) inspectAndDiffAll(
	ctx context.Context,
	sd *keystonev1alpha1.SchemaDefinition,
	schemas []keystonev1alpha1.DatabaseSchema,
) ([]schemaDiff, error) {
	out := make([]schemaDiff, 0, len(schemas))
	for i := range schemas {
		s := &schemas[i]
		var ldb keystonev1alpha1.LogicalDatabase
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: s.Namespace, Name: s.Spec.LogicalDatabaseRef,
		}, &ldb); err != nil {
			return nil, fmt.Errorf("get LogicalDatabase for schema %s: %w", s.Name, err)
		}
		var provider keystonev1alpha1.DatabaseProvider
		if err := r.Get(ctx, types.NamespacedName{Name: ldb.Spec.ProviderRef}, &provider); err != nil {
			return nil, fmt.Errorf("get Provider for schema %s: %w", s.Name, err)
		}
		creds, err := r.resolveCredentials(ctx, &provider)
		if err != nil {
			return nil, fmt.Errorf("credentials for schema %s: %w", s.Name, err)
		}
		cfg := pg.PoolConfig{
			Host:     provider.Spec.Host,
			Port:     int(provider.Spec.Port),
			Database: ldb.Spec.Name,
			Username: creds.Username,
			Password: creds.Password,
			SSLMode:  string(provider.Spec.SSLMode),
			MaxConns: provider.Spec.PoolMaxConns,
		}
		pool, release, err := r.Pools.Acquire(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("pool acquire for schema %s: %w", s.Name, err)
		}
		inspector := drift.NewInspector(pool)
		observed, err := inspector.Inspect(ctx, s.Spec.Name)
		release()
		if err != nil {
			return nil, fmt.Errorf("inspect schema %s: %w", s.Name, err)
		}
		plan, diffErr := declarative.Diff(observed, &sd.Spec)
		if diffErr != nil {
			// Propagate the destructive-refused error verbatim so the
			// caller can route it to markDestructiveBlocked. Other diff
			// errors are wrapped with the schema name for triage.
			var destructive *declarative.ErrDestructiveRefused
			if errors.As(diffErr, &destructive) {
				return nil, diffErr
			}
			return nil, fmt.Errorf("diff schema %s: %w", s.Name, diffErr)
		}
		out = append(out, schemaDiff{
			schema:      s,
			plan:        plan,
			inspectedAt: metav1.Now(),
		})
	}
	return out, nil
}

// selectCanonicalPlan picks the canonical schema+plan to emit a single
// MigrationBundle for and identifies any schemas that diverge from
// that plan. With one schema in `results` (SchemaRef path), there is
// no divergence and the only schema is canonical. With N schemas:
//
//   - If every schema has the same number of ops AND each plan's
//     statement set is identical, the schemas are in lockstep —
//     return the first as canonical with no drift.
//
//   - Otherwise, the schema with the largest plan is canonical (most-
//     behind); every other schema is recorded as drifted. Under
//     MixedVersionPolicy=MostBehindWins the caller proceeds with this
//     canonical plan; under Refuse the caller halts.
func selectCanonicalPlan(results []schemaDiff) (
	*keystonev1alpha1.DatabaseSchema,
	*declarative.Plan,
	[]keystonev1alpha1.DriftedSchema,
) {
	if len(results) == 1 {
		return results[0].schema, results[0].plan, nil
	}
	// Find the largest plan (most-behind candidate).
	canonicalIdx := 0
	for i := 1; i < len(results); i++ {
		if len(results[i].plan.Statements) > len(results[canonicalIdx].plan.Statements) {
			canonicalIdx = i
		}
	}
	canonicalPlan := results[canonicalIdx].plan
	// Lockstep check: same statement count AND same set of statements.
	lockstep := true
	for i := range results {
		if len(results[i].plan.Statements) != len(canonicalPlan.Statements) {
			lockstep = false
			break
		}
		if !sameStatementSet(results[i].plan.Statements, canonicalPlan.Statements) {
			lockstep = false
			break
		}
	}
	if lockstep {
		return results[canonicalIdx].schema, canonicalPlan, nil
	}
	// Drift — record everything but the canonical schema.
	drifted := make([]keystonev1alpha1.DriftedSchema, 0, len(results)-1)
	for i := range results {
		if i == canonicalIdx {
			continue
		}
		insp := results[i].inspectedAt
		drifted = append(drifted, keystonev1alpha1.DriftedSchema{
			SchemaRef:         results[i].schema.Name,
			PendingOperations: int32(len(results[i].plan.Statements)),
			LastInspectedAt:   &insp,
		})
	}
	return results[canonicalIdx].schema, canonicalPlan, drifted
}

// sameStatementSet reports whether two statement slices contain the
// same statements (order-insensitive). Differ output is ordered, so
// equal-ordered slices will satisfy this; the set comparison handles
// the rare case where two tenants produce equal-length but reordered
// plans (e.g. independent CREATE INDEX statements).
func sameStatementSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		if seen[s] == 0 {
			return false
		}
		seen[s]--
	}
	return true
}

// policyOf returns the SD's MixedVersionPolicy with the default
// applied. Default is Refuse (audit-grade); the CRD's kubebuilder
// default handles freshly-created CRs but a CR persisted before the
// field existed would carry an empty string — handle that here.
func policyOf(sd *keystonev1alpha1.SchemaDefinition) keystonev1alpha1.MixedVersionPolicy {
	if sd.Spec.MixedVersionPolicy == "" {
		return keystonev1alpha1.MixedVersionPolicyRefuse
	}
	return sd.Spec.MixedVersionPolicy
}

func (r *SchemaDefinitionReconciler) markDestructiveBlocked(
	ctx context.Context,
	sd *keystonev1alpha1.SchemaDefinition,
	plan *declarative.Plan,
	cause *declarative.ErrDestructiveRefused,
) (ctrl.Result, error) {
	patch := client.MergeFrom(sd.DeepCopy())
	sd.Status.ObservedGeneration = sd.Generation
	sd.Status.PendingOperations = int32(len(plan.Statements))
	sd.Status.CurrentBundleRef = "" // Nothing emitted
	// Same shape as the mixed-version refusal: the pipeline got all the
	// way through the diff and the gate refused the emit. Leaving
	// BundleGenerated latched True from an earlier reconcile is what
	// makes an operator think apply is already in flight and wait for a
	// bundle that will never appear.
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeInspected, metav1.ConditionTrue, "LiveSchemaRead",
		"live schema read; diff refused by the destructive gate", sd.Generation)
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeDiffComputed, metav1.ConditionTrue, "DestructiveRefused",
		fmt.Sprintf("%d statement(s) to apply (%d destructive)",
			len(plan.Statements), plan.DestructiveOps),
		sd.Generation)
	sd.Status.Conditions = conditions.Set(sd.Status.Conditions,
		keystonev1alpha1.ConditionTypeBundleGenerated, metav1.ConditionFalse, "DestructiveRefused",
		fmt.Sprintf("no MigrationBundle emitted; %d destructive op(s) refused", cause.Count),
		sd.Generation)
	sd.Status.Conditions = conditions.MarkNotReady(sd.Status.Conditions,
		"DestructiveRefused", cause, sd.Generation)
	if err := r.Status().Patch(ctx, sd, patch); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(sd, corev1.EventTypeWarning, "DestructiveRefused",
			"%d destructive op(s) refused; set spec.allowDestructive=true to permit",
			cause.Count)
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// specFingerprint computes a deterministic SHA-256 hex (truncated to
// 16 chars) of the SchemaDefinition's spec. Stamped on
// SD.Status.Fingerprint and propagated to every emitted MigrationBundle
// as the annotation `keystone.hexxlock.io/sd-fingerprint`. The
// MigrationExecution reconciler reads the annotation on Phase=Succeeded
// and stamps DatabaseSchema.Status.LastAppliedFingerprint = annotation
// value, enabling identity-based per-schema convergence checks
// (keystone-sdk/go/keystone.WaitSchemaFingerprint).
//
// Marshaling via encoding/json with sorted map keys (Go's default for
// struct fields is declaration order; for any map[string]X fields
// inside the spec, Go's json.Marshal sorts keys deterministically).
// The SD spec is a closed schema (no map[string]X with operator-
// controlled values), so the result is stable across reconciles.
func specFingerprint(spec *keystonev1alpha1.SchemaDefinitionSpec) string {
	raw, err := json.Marshal(spec)
	if err != nil {
		// json.Marshal of a generated CRD struct should never fail;
		// returning empty makes the SD reconcile observable as
		// "Fingerprint not yet computed" rather than crashing.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// emitBundle creates (or updates) the ConfigMap holding the SQL
// statements and the MigrationBundle that references it. Both are
// owned by the SchemaDefinition so Argo prune sweeps them on delete.
//
// Naming: deterministic — same spec content produces same names, so
// repeated reconciles don't spawn duplicates. Hash includes the plan
// content (statements + version) so a spec change produces a new
// bundle version (bundles are immutable per-version).
func (r *SchemaDefinitionReconciler) emitBundle(
	ctx context.Context,
	sd *keystonev1alpha1.SchemaDefinition,
	schema *keystonev1alpha1.DatabaseSchema,
	plan *declarative.Plan,
) (string, error) {
	// Content hash = stable identity for this plan.
	//
	// Two derived identifiers with DIFFERENT length requirements:
	//
	//   bundleName / cmName  — metadata.Name slot, capped at 253
	//     chars (DNS-1123 subdomain). Uses the FULL SHA-256 hex (64
	//     chars). This is the field that prevents in-place mutation:
	//     SQL change → new full-hash → new ConfigMap name → bundle
	//     controller resolves fresh source → MigrationExecution with
	//     a distinct spec.ContentHash → runner re-applies, by
	//     construction.
	//
	//   bundleVersion — bundle.Spec.Version, capped at 64 chars by
	//     CRD MaxLength AND used as a Kubernetes label value
	//     (`keystone.hexxlock.io/version`) which is capped at 63 chars
	//     by the apiserver. Uses a SHORT 16-hex prefix of the same
	//     hash (64 bits of entropy = ~18 quintillion distinct values
	//     per SD before any prefix collision). Short-prefix safety is
	//     defended by the runtime ContentHash gate in
	//     migrationexecution_controller.go: a hypothetical prefix
	//     collision still fails `prior.ContentHash != exec.Spec.ContentHash`
	//     and surfaces as `PriorVersionContentMismatch` rather than
	//     silent skip.
	//
	// Previously bundleName was truncated to 12 chars (collision-safe
	// for content-addressing, but a different DOMAIN of hash than the
	// runner used → silent CM mutation across reconciles). The
	// architectural fix is to make bundleName content-addressed at the
	// full hash; bundleVersion can remain short because it's never the
	// PRIMARY content-address — that role belongs to bundleName.
	h := sha256.New()
	for _, s := range plan.Statements {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	contentHash := hex.EncodeToString(h.Sum(nil))
	shortHash := contentHash[:16]

	cmName := fmt.Sprintf("%s-sql-%s", sd.Name, contentHash)
	bundleName := fmt.Sprintf("%s-%s", sd.Name, contentHash)
	bundleVersion := fmt.Sprintf("%s-decl-%s", sd.Name, shortHash)

	// Pack statements into a single .up.sql file. The runner's
	// per-file transaction semantics are good for declarative output
	// too — either the whole diff applies or nothing.
	body := strings.Join(plan.Statements, ";\n\n") + ";\n"

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sd.Namespace,
			Name:      cmName,
			Labels: map[string]string{
				keystonev1alpha1.LabelSchemaDefinition: sd.Name,
				keystonev1alpha1.LabelSchemaRef:        schema.Name,
				keystonev1alpha1.LabelSource:           "declarative-diff",
			},
			Annotations: map[string]string{
				keystonev1alpha1.AnnotationSDFingerprint: specFingerprint(&sd.Spec),
			},
		},
		Data: map[string]string{
			"001_declarative_diff.up.sql": body,
		},
	}
	if err := controllerutil.SetControllerReference(sd, cm, r.Scheme); err != nil {
		return "", fmt.Errorf("set CM owner ref: %w", err)
	}
	// Create-only: with content-addressed naming, an AlreadyExists
	// collision means the existing CM holds the exact same SQL body
	// (deterministic identity), so the no-op skip is safe. Updating
	// in place is forbidden — it would re-introduce the silent-mutate
	// bug that motivated content-addressed naming.
	if err := r.Client.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create ConfigMap: %w", err)
	}

	// Propagate select labels + annotations from the SchemaDefinition
	// to the emitted MigrationBundle. Two purposes:
	//
	//   1. SchemaPolicy.TargetSelector picks bundles by label. The
	//      SD's labels (e.g., `keystone.hexxlock.io/scope: example-service`)
	//      decide which approval / lint policies apply. Without
	//      propagating these, the auto-emitted bundle escapes every
	//      policy that an operator wrote against the SD's scope —
	//      destructive ops fall through to the default-deny webhook
	//      with no path to admission.
	//
	//   2. Approval evidence annotations
	//      (`keystone.hexxlock.io/approval.<policy>.<approver>=<group>`)
	//      let an operator pre-approve a SchemaDefinition edit at PR
	//      time. The SD reconciler copies that evidence onto every
	//      bundle it emits for the SD until the operator removes the
	//      annotation. Mirrors the hand-authored bundle workflow.
	//
	// Filtered to a known-safe set: scope label + approval-prefix
	// annotations. Arbitrary user labels/annotations don't auto-leak
	// to bundles (avoid surprising downstream consumers).
	propagatedLabels := map[string]string{
		keystonev1alpha1.LabelSchemaDefinition: sd.Name,
		keystonev1alpha1.LabelSchemaRef:        schema.Name,
		keystonev1alpha1.LabelSource:           "declarative-diff",
		// Target the parent DatabaseSchema by label so the
		// bundle's schemaSelector matches. See DatabaseSchema
		// sample for the label convention.
		"module": schema.Name,
	}
	if scope, ok := sd.Labels[keystonev1alpha1.LabelScope]; ok && scope != "" {
		propagatedLabels[keystonev1alpha1.LabelScope] = scope
	}
	propagatedAnnotations := map[string]string{}
	for k, v := range sd.Annotations {
		if strings.HasPrefix(k, keystonev1alpha1.AnnotationApprovalPrefix) {
			propagatedAnnotations[k] = v
		}
	}
	// Fingerprint stamp — the MigrationExecution reconciler copies this
	// value onto DatabaseSchema.Status.LastAppliedFingerprint on
	// Phase=Succeeded, enabling identity-based convergence checks.
	// Compute it here rather than reading sd.Status.Fingerprint directly
	// because the status patch above runs AFTER emitBundle returns —
	// reading SD.Status here would observe the previous reconcile's
	// fingerprint (or empty on the first pass).
	propagatedAnnotations[keystonev1alpha1.AnnotationSDFingerprint] = specFingerprint(&sd.Spec)
	if len(propagatedAnnotations) == 0 {
		propagatedAnnotations = nil
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sd.Namespace,
			Name:      bundleName,
		},
	}
	// PolicyRef defaulting — without a policy set on the bundle, the
	// MigrationBundle controller has nothing to look up and the pipeline
	// stalls before MigrationPlan emission. Default to production-safety
	// (the policy every hand-authored bundle uses) so declarative SDs
	// behave the same out of the box.
	policyRef := sd.Spec.PolicyRef
	if policyRef == "" {
		policyRef = "production-safety"
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, bundle, func() error {
		// Labels + annotations MUST be set inside the mutate fn (not on
		// the struct passed to CreateOrUpdate) — otherwise on UPDATE the
		// fetched existing object's metadata wins and propagation is a
		// no-op for already-existing bundles. v0.1.35 had this bug;
		// fixed here.
		if bundle.Labels == nil {
			bundle.Labels = map[string]string{}
		}
		for k, v := range propagatedLabels {
			bundle.Labels[k] = v
		}
		if len(propagatedAnnotations) > 0 {
			if bundle.Annotations == nil {
				bundle.Annotations = map[string]string{}
			}
			for k, v := range propagatedAnnotations {
				bundle.Annotations[k] = v
			}
		}
		bundle.Spec = keystonev1alpha1.MigrationBundleSpec{
			Version:  bundleVersion,
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name:        cmName,
					FilePattern: "*.up.sql",
				},
			},
			// Bundle selector mirrors the SD: with SchemaRef this targets
			// the single schema by name; with SchemaSelector the bundle
			// fans out at apply-time across the same set the SD evaluated.
			SchemaSelector: bundleSchemaSelector(sd, schema),
			PolicyRef:      policyRef,
		}
		return controllerutil.SetControllerReference(sd, bundle, r.Scheme)
	}); err != nil {
		return "", fmt.Errorf("upsert MigrationBundle: %w", err)
	}
	return bundleName, nil
}

// bundleSchemaSelector renders the LabelSelector the emitted bundle
// uses to find DatabaseSchemas at apply time.
//
//   - SchemaRef path: target the single canonical schema by its
//     `keystone.hexxlock.io/name` label (DatabaseSchema convention).
//   - SchemaSelector path: pass the SD's selector through unchanged
//     so the bundle's own fan-out resolves the same set.
func bundleSchemaSelector(
	sd *keystonev1alpha1.SchemaDefinition,
	canonical *keystonev1alpha1.DatabaseSchema,
) metav1.LabelSelector {
	if sd.Spec.SchemaSelector != nil {
		return *sd.Spec.SchemaSelector.DeepCopy()
	}
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			"keystone.hexxlock.io/name": canonical.Name,
		},
	}
}

// emitPgrollBundle converts the Plan's SQL statements to pgroll
// MigrationOperations and creates a MigrationBundle with strategy
// pgroll-expand-contract. No ConfigMap is needed — operations live
// directly on the bundle spec.
func (r *SchemaDefinitionReconciler) emitPgrollBundle(
	ctx context.Context,
	sd *keystonev1alpha1.SchemaDefinition,
	schema *keystonev1alpha1.DatabaseSchema,
	plan *declarative.Plan,
) (string, error) {
	ops, unconvertible := declarative.PlanToOperations(plan)

	if len(ops) == 0 && len(unconvertible) > 0 {
		return "", fmt.Errorf(
			"pgroll-expand-contract strategy selected but no statements could be "+
				"converted to operations; %d statement(s) require versioned strategy",
			len(unconvertible))
	}

	// Log unconvertible as warnings — they'll need a separate versioned bundle.
	if len(unconvertible) > 0 && r.Recorder != nil {
		r.Recorder.Eventf(sd, corev1.EventTypeWarning, "PartialConversion",
			"%d of %d statement(s) could not be converted to pgroll operations; "+
				"remaining statements require a versioned MigrationBundle",
			len(unconvertible), len(plan.Statements))
	}

	// Full SHA-256 hex for bundleName (content-addressed identity,
	// 253-char metadata.Name slot), 16-hex prefix for bundleVersion
	// (fits in the 63-char k8s label-value limit, since
	// bundle.Spec.Version is also propagated as a label value). The
	// runtime ContentHash gate in migrationexecution_controller.go
	// catches any hypothetical short-prefix collision. Mirrors the
	// declarative emitBundle path; see comments there.
	h := sha256.New()
	for _, op := range ops {
		h.Write([]byte(string(op.Kind)))
		h.Write([]byte(op.Table))
		h.Write([]byte{0})
	}
	contentHash := hex.EncodeToString(h.Sum(nil))
	shortHash := contentHash[:16]
	bundleName := fmt.Sprintf("%s-pgroll-%s", sd.Name, contentHash)
	bundleVersion := fmt.Sprintf("%s-pgroll-%s", sd.Name, shortHash)

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sd.Namespace,
			Name:      bundleName,
			Labels: map[string]string{
				"keystone.hexxlock.io/schemadefinition": sd.Name,
				"keystone.hexxlock.io/schemaref":        schema.Name,
				"keystone.hexxlock.io/source":           "declarative-diff",
				"module":                                schema.Name,
			},
		},
	}
	policyRef := sd.Spec.PolicyRef
	if policyRef == "" {
		policyRef = "production-safety"
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, bundle, func() error {
		bundle.Spec = keystonev1alpha1.MigrationBundleSpec{
			Version:    bundleVersion,
			Strategy:   keystonev1alpha1.StrategyPgrollExpandContract,
			Operations: ops,
			SchemaSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"keystone.hexxlock.io/name": schema.Name,
				},
			},
			PolicyRef: policyRef,
		}
		return controllerutil.SetControllerReference(sd, bundle, r.Scheme)
	}); err != nil {
		return "", fmt.Errorf("upsert pgroll MigrationBundle: %w", err)
	}
	return bundleName, nil
}

func (r *SchemaDefinitionReconciler) resolveCredentials(ctx context.Context, p *keystonev1alpha1.DatabaseProvider) (resolvedCreds, error) {
	ref := p.Spec.AdminCredentialsRef
	userKey := ref.UsernameKey
	if userKey == "" {
		userKey = "username"
	}
	passKey := ref.PasswordKey
	if passKey == "" {
		passKey = "password"
	}
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: r.SystemNamespace, Name: ref.SecretName,
	}, &sec); err != nil {
		return resolvedCreds{}, fmt.Errorf("read admin Secret %s/%s: %w",
			r.SystemNamespace, ref.SecretName, err)
	}
	u, ok := sec.Data[userKey]
	if !ok || len(u) == 0 {
		return resolvedCreds{}, fmt.Errorf("Secret %s/%s missing key %q",
			r.SystemNamespace, ref.SecretName, userKey)
	}
	pw, ok := sec.Data[passKey]
	if !ok || len(pw) == 0 {
		return resolvedCreds{}, fmt.Errorf("Secret %s/%s missing key %q",
			r.SystemNamespace, ref.SecretName, passKey)
	}
	return resolvedCreds{Username: string(u), Password: string(pw)}, nil
}

func (r *SchemaDefinitionReconciler) fail(
	ctx context.Context, sd *keystonev1alpha1.SchemaDefinition,
	reason string, cause error,
) (ctrl.Result, error) {
	patch := client.MergeFrom(sd.DeepCopy())
	sd.Status.ObservedGeneration = sd.Generation
	sd.Status.Conditions = conditions.MarkNotReady(sd.Status.Conditions, reason, cause, sd.Generation)
	if perr := r.Status().Patch(ctx, sd, patch); perr != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w (cause: %v)", perr, cause)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(sd, corev1.EventTypeWarning, reason, "%s", cause.Error())
	}
	return ctrl.Result{}, cause
}

// SetupWithManager wires the reconciler.
func (r *SchemaDefinitionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.SystemNamespace == "" {
		r.SystemNamespace = "keystone-system"
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keystone-schemadefinition")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.SchemaDefinition{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Owns(&corev1.ConfigMap{}).
		Owns(&keystonev1alpha1.MigrationBundle{}).
		Named("schemadefinition").
		Complete(r)
}

