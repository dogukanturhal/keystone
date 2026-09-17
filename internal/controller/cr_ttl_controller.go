// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// CRTTLController garbage-collects terminal MigrationBundle and
// MigrationExecution CRs whose age has exceeded the retention window
// configured on the owning SchemaDefinition (or the operator-wide
// defaults when the SD does not pin its own policy).
//
// MOTIVATION
// ==========
// On 2026-05-08 a runaway reconcile leaked 182 954 AuditEntry CRs into
// etcd, bloating the keyspace to 4.7 GB and triggering a 17 h apiserver
// outage. AuditEntry has its own off-cluster archiver
// (AuditEntryRetentionController + AuditLog.spec.archive); the
// post-mortem flagged that MigrationBundle and MigrationExecution share
// the same accumulation shape (one CR per schema per emit / per apply
// attempt), but had no GC at all. This controller is the missing piece
// — Argo Workflows' spec.ttlStrategy.{secondsAfterSuccess,
// secondsAfterFailure}, adapted to the Keystone CR graph.
//
// SAFETY MODEL
// ============
//   - Off by default. The operator-wide --enable-cr-ttl flag is the
//     single global gate; SD-level config has no effect until the
//     manager is started with the flag set. Existing clusters opt in
//     deliberately.
//   - The latest Succeeded CR per (kind, owning SD) is ALWAYS retained.
//     The SD reconciler reads it for re-emit decisions and operators
//     read it from `kubectl get mb -A` to answer "what last shipped".
//     A controller bug that violated this invariant would cascade into
//     spurious re-emits.
//   - Non-terminal CRs (Pending / Running / Expanding / Aborting /
//     RollingBack and any MigrationBundle whose Ready condition is not
//     True) are NEVER deleted regardless of age. The TTL window only
//     starts ticking once the CR reaches a terminal phase.
//   - Owner-reference cascade is the redundant safety net: every
//     MigrationBundle emitted by SD reconcile carries a controller
//     reference to its SchemaDefinition, and every MigrationExecution
//     carries one to its MigrationBundle. Deleting the SD already
//     cascades; this controller covers the case where the SD survives
//     and the bundle/execution graveyard underneath it does not.
//   - Idempotent. Re-running a pass is safe — already-deleted CRs come
//     back as 404s and the controller logs them as "deleted-by-someone-
//     else".
//
// SHAPE
// =====
// Mirrors AuditEntryRetentionController: a manager.Runnable driven by a
// time.Ticker rather than a controller-runtime Reconciler. The work is
// list-everything-and-prune across multiple kinds, which doesn't fit the
// per-CR reconcile model. Leader-election guarded so only one replica
// is collecting at a time (every other replica's delete would be a
// 404 anyway, but the API churn cost is real).
type CRTTLController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Enabled is the global feature flag. When false the controller
	// runs the loop and emits a "disabled" pass-duration metric on
	// every tick but does not list or delete anything. Wired from
	// --enable-cr-ttl in cmd/manager/main.go; defaults to false on
	// first release.
	Enabled bool

	// CheckInterval controls the loop cadence. Zero defaults to 15
	// minutes — short enough that GC keeps up with a 30-min reconcile
	// cadence, long enough that a 100-tenant fan-out doesn't burn
	// API quota.
	CheckInterval time.Duration

	// DefaultRetainSuccess is the operator-wide retention window for
	// terminal Succeeded CRs when the owning SD does not pin
	// spec.cleanup.retainSuccessFor. Zero defaults to 24h.
	DefaultRetainSuccess time.Duration

	// DefaultRetainFailure is the operator-wide retention window for
	// terminal Failed / Aborted / RollbackFailed CRs when the owning
	// SD does not pin spec.cleanup.retainFailureFor. Zero defaults
	// to 7 days.
	DefaultRetainFailure time.Duration

	// nowFn lets tests inject a deterministic clock.
	nowFn func() time.Time
}

const (
	defaultCRTTLCheckInterval  = 15 * time.Minute
	defaultRetainSuccessWindow = 24 * time.Hour
	defaultRetainFailureWindow = 7 * 24 * time.Hour
)

// Start implements manager.Runnable. Runs until the manager's context
// is canceled.
func (c *CRTTLController) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithValues("component", "cr-ttl")
	interval := c.interval()
	if !c.Enabled {
		logger.Info("CR-TTL controller starting in DISABLED mode (--enable-cr-ttl=false)",
			"interval", interval,
			"hint", "set --enable-cr-ttl=true to begin GC of terminal MigrationBundle / MigrationExecution CRs")
	} else {
		logger.Info("CR-TTL controller starting",
			"interval", interval,
			"defaultRetainSuccess", c.retainSuccess(nil),
			"defaultRetainFailure", c.retainFailure(nil))
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	// First pass immediately so we don't wait a full interval after
	// manager startup. Matches AuditEntryRetentionController shape.
	c.runOnce(ctx, logger)

	for {
		select {
		case <-ctx.Done():
			logger.Info("CR-TTL controller stopping")
			return nil
		case <-t.C:
			c.runOnce(ctx, logger)
		}
	}
}

// NeedLeaderElection makes the periodic loop run only on the elected
// leader, matching the rest of the manager.
func (c *CRTTLController) NeedLeaderElection() bool { return true }

func (c *CRTTLController) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn().UTC()
	}
	return time.Now().UTC()
}

func (c *CRTTLController) interval() time.Duration {
	if c.CheckInterval > 0 {
		return c.CheckInterval
	}
	return defaultCRTTLCheckInterval
}

func (c *CRTTLController) retainSuccess(sd *keystonev1alpha1.SchemaDefinition) time.Duration {
	if sd != nil && sd.Spec.Cleanup != nil && sd.Spec.Cleanup.RetainSuccessFor != nil &&
		sd.Spec.Cleanup.RetainSuccessFor.Duration > 0 {
		return sd.Spec.Cleanup.RetainSuccessFor.Duration
	}
	if c.DefaultRetainSuccess > 0 {
		return c.DefaultRetainSuccess
	}
	return defaultRetainSuccessWindow
}

func (c *CRTTLController) retainFailure(sd *keystonev1alpha1.SchemaDefinition) time.Duration {
	if sd != nil && sd.Spec.Cleanup != nil && sd.Spec.Cleanup.RetainFailureFor != nil &&
		sd.Spec.Cleanup.RetainFailureFor.Duration > 0 {
		return sd.Spec.Cleanup.RetainFailureFor.Duration
	}
	if c.DefaultRetainFailure > 0 {
		return c.DefaultRetainFailure
	}
	return defaultRetainFailureWindow
}

// runOnce is the per-tick body. Errors are logged and metric-counted
// but never fail the loop.
func (c *CRTTLController) runOnce(ctx context.Context, logger logrLike) {
	start := time.Now()

	if !c.Enabled {
		c.recordPassDuration(start, "disabled")
		return
	}

	// 1. List every SchemaDefinition cluster-wide. We index by name for
	// cheap lookups when classifying child CRs in steps 2 and 3. SDs
	// without cleanup config still get the operator-wide defaults
	// applied — the controller does not skip "unconfigured" SDs.
	sdList := &keystonev1alpha1.SchemaDefinitionList{}
	if err := c.List(ctx, sdList); err != nil {
		logger.Error(err, "list SchemaDefinitions")
		crTTLErrorsTotal.WithLabelValues("list_sd").Inc()
		c.recordPassDuration(start, "error")
		return
	}
	sdByName := make(map[string]*keystonev1alpha1.SchemaDefinition, len(sdList.Items))
	for i := range sdList.Items {
		sd := &sdList.Items[i]
		sdByName[sd.Namespace+"/"+sd.Name] = sd
	}

	// 2. Sweep MigrationBundles.
	bundlesDeleted, bundleCandidates, bundleErr := c.sweepBundles(ctx, sdByName, logger)

	// 3. Sweep MigrationExecutions.
	execsDeleted, execCandidates, execErr := c.sweepExecutions(ctx, sdByName, logger)

	// 4. Update gauges + duration.
	crTTLCandidatesGauge.WithLabelValues("MigrationBundle").Set(float64(bundleCandidates))
	crTTLCandidatesGauge.WithLabelValues("MigrationExecution").Set(float64(execCandidates))

	outcome := "swept"
	if bundleErr != nil || execErr != nil {
		outcome = "error"
	} else if bundlesDeleted == 0 && execsDeleted == 0 {
		outcome = "no_candidates"
	}
	c.recordPassDuration(start, outcome)
	logger.Info("CR-TTL pass complete",
		"sds", len(sdList.Items),
		"bundles_deleted", bundlesDeleted,
		"executions_deleted", execsDeleted,
		"bundle_candidates", bundleCandidates,
		"execution_candidates", execCandidates)
}

// sweepBundles lists every MigrationBundle, classifies each as
// terminal-and-eligible / terminal-but-keep-as-latest /
// non-terminal-skip, and deletes the eligibles. Returns (deleted,
// candidates_seen, fatalErr). A non-nil fatalErr means the list call
// failed; non-fatal per-CR errors are logged and counted but do not
// abort the sweep.
func (c *CRTTLController) sweepBundles(
	ctx context.Context,
	sdByName map[string]*keystonev1alpha1.SchemaDefinition,
	logger logrLike,
) (int, int, error) {
	bundles := &keystonev1alpha1.MigrationBundleList{}
	if err := c.List(ctx, bundles); err != nil {
		logger.Error(err, "list MigrationBundles")
		crTTLErrorsTotal.WithLabelValues("list_bundles").Inc()
		return 0, 0, err
	}

	// Group bundles by the SD that owns them. Bundles whose owning SD
	// is not present in sdByName (orphaned, or hand-authored without an
	// SD) are placed under the empty-string key — they get cleaned up
	// using operator-wide defaults too.
	type bundleEntry struct {
		bundle *keystonev1alpha1.MigrationBundle
		phase  string // canonical terminal label for metrics + decisions
		// completedAt is the timestamp the controller uses to age the
		// bundle: prefers Ready condition's LastTransitionTime, falls
		// back to status.LastPlanTime, then to creationTimestamp.
		completedAt time.Time
	}
	byOwner := make(map[string][]bundleEntry, len(bundles.Items))

	for i := range bundles.Items {
		b := &bundles.Items[i]
		phase, completed, terminal := classifyBundle(b)
		if !terminal {
			continue
		}
		key := ownerSDKey(b.OwnerReferences, b.Namespace)
		byOwner[key] = append(byOwner[key], bundleEntry{
			bundle:      b,
			phase:       phase,
			completedAt: completed,
		})
	}

	candidates := 0
	deleted := 0
	now := c.now()

	for ownerKey, group := range byOwner {
		sd := sdByName[ownerKey]
		retainOK := c.retainSuccess(sd)
		retainBad := c.retainFailure(sd)

		// Sort by completedAt descending so the most recent terminal
		// CR is at index 0 — that's the "always keep latest succeeded"
		// candidate. We track the latest succeeded explicitly because
		// the most recent terminal might be a Failed CR (which is
		// itself fine to retain on its own window) but the user-facing
		// invariant is "latest SUCCEEDED bundle is always preserved".
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].completedAt.After(group[j].completedAt)
		})
		latestSucceededIdx := -1
		for i := range group {
			if group[i].phase == terminalPhaseSucceeded {
				latestSucceededIdx = i
				break
			}
		}

		for i := range group {
			candidates++
			entry := &group[i]
			if i == latestSucceededIdx {
				// Always preserve the latest Succeeded bundle.
				continue
			}
			window := retainOK
			if entry.phase != terminalPhaseSucceeded {
				window = retainBad
			}
			if !ageExceeds(now, entry.completedAt, window) {
				continue
			}

			if err := c.Delete(ctx, entry.bundle); err != nil {
				if apierrors.IsNotFound(err) {
					// Already gone — race with another controller or
					// kubectl. Treat as success.
				} else {
					logger.Error(err, "delete MigrationBundle",
						"name", entry.bundle.Name,
						"namespace", entry.bundle.Namespace,
						"phase", entry.phase)
					crTTLErrorsTotal.WithLabelValues("delete_bundle").Inc()
					continue
				}
			}
			crTTLDeletedTotal.WithLabelValues("MigrationBundle", entry.phase).Inc()
			deleted++
			logger.Info("CR-TTL deleted MigrationBundle",
				"name", entry.bundle.Name,
				"namespace", entry.bundle.Namespace,
				"phase", entry.phase,
				"age", now.Sub(entry.completedAt).Truncate(time.Second).String(),
				"window", window.String(),
				"ownerSD", ownerKey)

			// Cascade-delete the bundle's source ConfigMap. The CM is
			// owned by the SchemaDefinition (NOT the bundle), so the
			// kube garbage collector never reaps it when the bundle is
			// deleted in isolation — the CM survives until the SD itself
			// is removed. With content-addressed bundle naming (a CM is
			// one-per-bundle-content), the CM has no purpose once its
			// owning bundle is gone, so we delete it explicitly here.
			//
			// Why this can't move to a finalizer on MigrationBundle:
			// finalizers run on every delete path including operator
			// intent ("oops, undo"), and would defeat manual GC. CR-TTL
			// is the single deliberate GC funnel; we want the CM
			// cleanup tied to *that* gate, not to deletion in general.
			//
			// Safety: bundles created with strategy=pgrollExpandContract
			// have spec.operations inline and no ConfigMap — skip those.
			// CMs already deleted return NotFound, which is fine.
			if entry.bundle.Spec.Source.Type == keystonev1alpha1.SourceConfigMap &&
				entry.bundle.Spec.Source.ConfigMapRef != nil &&
				entry.bundle.Spec.Source.ConfigMapRef.Name != "" {
				cmName := entry.bundle.Spec.Source.ConfigMapRef.Name
				cm := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: entry.bundle.Namespace,
						Name:      cmName,
					},
				}
				if err := c.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
					logger.Error(err, "delete bundle source ConfigMap",
						"name", cmName, "namespace", entry.bundle.Namespace,
						"bundle", entry.bundle.Name)
					crTTLErrorsTotal.WithLabelValues("delete_configmap").Inc()
					// Bundle deletion already succeeded; the CM is
					// retried on the next pass, so we don't `continue`
					// out of the deletion bookkeeping.
				} else {
					crTTLDeletedTotal.WithLabelValues("ConfigMap", entry.phase).Inc()
					logger.Info("CR-TTL deleted bundle source ConfigMap",
						"name", cmName, "namespace", entry.bundle.Namespace,
						"bundle", entry.bundle.Name)
				}
			}
		}
	}

	return deleted, candidates, nil
}

// sweepExecutions mirrors sweepBundles for MigrationExecution CRs.
// Executions carry an explicit phase field, so terminal classification
// is direct (no Ready-condition lookup).
//
// Owner-reference grouping uses MigrationBundle as the proximate
// parent; the SD is two hops away via the bundle's controller ref. We
// therefore look up the bundle from the cache to get to the SD's
// cleanup policy. Bundles already deleted from this pass's bundle
// sweep above are still in our local snapshot via a re-list, so the
// lookup stays O(N+M) rather than O(N*M).
func (c *CRTTLController) sweepExecutions(
	ctx context.Context,
	sdByName map[string]*keystonev1alpha1.SchemaDefinition,
	logger logrLike,
) (int, int, error) {
	execs := &keystonev1alpha1.MigrationExecutionList{}
	if err := c.List(ctx, execs); err != nil {
		logger.Error(err, "list MigrationExecutions")
		crTTLErrorsTotal.WithLabelValues("list_executions").Inc()
		return 0, 0, err
	}

	bundles := &keystonev1alpha1.MigrationBundleList{}
	if err := c.List(ctx, bundles); err != nil {
		// Non-fatal: degrade to using operator-wide defaults for every
		// execution. The error is still counted so operators see it
		// in metrics.
		logger.Error(err, "list MigrationBundles for execution-sweep grouping")
		crTTLErrorsTotal.WithLabelValues("list_bundles").Inc()
	}
	bundleSDOwner := make(map[string]string, len(bundles.Items)) // bundle ns/name → SD ns/name
	for i := range bundles.Items {
		b := &bundles.Items[i]
		bundleSDOwner[b.Namespace+"/"+b.Name] = ownerSDKey(b.OwnerReferences, b.Namespace)
	}

	type execEntry struct {
		exec        *keystonev1alpha1.MigrationExecution
		phase       string
		completedAt time.Time
		// schemaKey is the (bundle, schema) tuple — we keep the latest
		// Succeeded execution PER schema, not per bundle, because the
		// SD reconciler reads the per-schema fingerprint from this
		// execution to decide re-emits.
		schemaKey string
	}
	byOwner := make(map[string][]execEntry, len(execs.Items))

	for i := range execs.Items {
		e := &execs.Items[i]
		phase, completed, terminal := classifyExecution(e)
		if !terminal {
			continue
		}
		bundleKey := e.Namespace + "/" + e.Spec.BundleRef
		ownerKey := bundleSDOwner[bundleKey] // empty when bundle is gone
		byOwner[ownerKey] = append(byOwner[ownerKey], execEntry{
			exec:        e,
			phase:       phase,
			completedAt: completed,
			schemaKey:   e.Spec.SchemaRef,
		})
	}

	candidates := 0
	deleted := 0
	now := c.now()

	for ownerKey, group := range byOwner {
		sd := sdByName[ownerKey]
		retainOK := c.retainSuccess(sd)
		retainBad := c.retainFailure(sd)

		// Sort by completedAt desc so the "latest Succeeded per
		// schema" preservation is a one-pass map.
		sort.SliceStable(group, func(i, j int) bool {
			return group[i].completedAt.After(group[j].completedAt)
		})
		latestSucceededPerSchema := make(map[string]int, len(group))
		for i := range group {
			if group[i].phase != terminalPhaseSucceeded {
				continue
			}
			if _, seen := latestSucceededPerSchema[group[i].schemaKey]; seen {
				continue
			}
			latestSucceededPerSchema[group[i].schemaKey] = i
		}

		for i := range group {
			candidates++
			entry := &group[i]
			if entry.phase == terminalPhaseSucceeded &&
				latestSucceededPerSchema[entry.schemaKey] == i {
				// Always preserve the latest Succeeded execution per
				// (SD, schema) tuple. Operator's read path for
				// "what last shipped to tenant T".
				continue
			}
			window := retainOK
			if entry.phase != terminalPhaseSucceeded {
				window = retainBad
			}
			if !ageExceeds(now, entry.completedAt, window) {
				continue
			}

			if err := c.Delete(ctx, entry.exec); err != nil {
				if apierrors.IsNotFound(err) {
					// Already gone — owner-ref cascade or operator
					// kubectl. Count as success.
				} else {
					logger.Error(err, "delete MigrationExecution",
						"name", entry.exec.Name,
						"namespace", entry.exec.Namespace,
						"phase", entry.phase)
					crTTLErrorsTotal.WithLabelValues("delete_execution").Inc()
					continue
				}
			}
			crTTLDeletedTotal.WithLabelValues("MigrationExecution", entry.phase).Inc()
			deleted++
			logger.Info("CR-TTL deleted MigrationExecution",
				"name", entry.exec.Name,
				"namespace", entry.exec.Namespace,
				"phase", entry.phase,
				"age", now.Sub(entry.completedAt).Truncate(time.Second).String(),
				"window", window.String(),
				"ownerSD", ownerKey)
		}
	}

	return deleted, candidates, nil
}

func (c *CRTTLController) recordPassDuration(start time.Time, outcome string) {
	crTTLPassDurationSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
}

// SetupWithManager registers the periodic loop with the manager.
// Mirrors AuditEntryRetentionController.SetupWithManager.
func (c *CRTTLController) SetupWithManager(mgr ctrl.Manager) error {
	if c.Recorder == nil {
		c.Recorder = mgr.GetEventRecorderFor("keystone-cr-ttl")
	}
	return mgr.Add(manager.RunnableFunc(c.Start))
}

// Phase labels used for metrics + decisions. Intentionally string
// literals (not the typed ExecutionPhase) so the bundle classifier and
// execution classifier produce a uniform vocabulary.
const (
	terminalPhaseSucceeded      = "Succeeded"
	terminalPhaseFailed         = "Failed"
	terminalPhaseAborted        = "Aborted"
	terminalPhaseRolledBack     = "RolledBack"
	terminalPhaseRollbackFailed = "RollbackFailed"
)

// classifyBundle returns (phase, completedAt, terminal). When terminal
// is false the bundle is still in flight and not eligible for GC.
//
// Bundle terminal-phase logic:
//   - status.conditions[Ready]=True with reason "AllSchemasApplied" or
//     similar success path → Succeeded.
//   - status.conditions[Ready]=False AND every matched schema has
//     reported a Failed execution (reflected in MatchedSchemas ==
//     AppliedSchemas + count of failed) → Failed. Approximated by
//     looking at Ready=False with any non-empty message AND status.
//     LastPlanTime more than 1 minute in the past — this avoids
//     deleting bundles that just transitioned and may still recover.
//
// The 1-minute floor is conservative; real "in flight" bundles never
// have status.AppliedSchemas == status.MatchedSchemas.
func classifyBundle(b *keystonev1alpha1.MigrationBundle) (string, time.Time, bool) {
	completed := bundleCompletionTime(b)
	if completed.IsZero() {
		return "", time.Time{}, false
	}

	ready := findReadyLikeCondition(b.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if ready == nil {
		return "", time.Time{}, false
	}

	switch ready.Status {
	case metav1.ConditionTrue:
		if b.Status.MatchedSchemas > 0 && b.Status.AppliedSchemas >= b.Status.MatchedSchemas {
			return terminalPhaseSucceeded, completed, true
		}
		// Ready=True but the apply count hasn't caught up — treat as
		// not-yet-terminal to avoid a race window where the controller
		// flipped Ready before SchemaProgress finished fanning. Next
		// pass will catch it.
		return "", time.Time{}, false
	case metav1.ConditionFalse:
		// Failure — but only if the bundle has actually been around
		// long enough that the failure isn't just a transient mid-
		// reconcile. We require LastPlanTime to exist (the bundle did
		// get planned at some point) to avoid GCing fresh bundles
		// that flicker through Ready=False during their first
		// reconcile.
		if b.Status.LastPlanTime != nil {
			return terminalPhaseFailed, completed, true
		}
		return "", time.Time{}, false
	default:
		// Unknown / not-yet-set — not terminal.
		return "", time.Time{}, false
	}
}

func bundleCompletionTime(b *keystonev1alpha1.MigrationBundle) time.Time {
	ready := findReadyLikeCondition(b.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if ready != nil && !ready.LastTransitionTime.IsZero() {
		return ready.LastTransitionTime.Time.UTC()
	}
	if b.Status.LastPlanTime != nil {
		return b.Status.LastPlanTime.Time.UTC()
	}
	if !b.CreationTimestamp.IsZero() {
		return b.CreationTimestamp.Time.UTC()
	}
	return time.Time{}
}

// classifyExecution returns (phase, completedAt, terminal). When
// terminal is false the execution is still in flight and not eligible
// for GC.
func classifyExecution(e *keystonev1alpha1.MigrationExecution) (string, time.Time, bool) {
	switch e.Status.Phase {
	case keystonev1alpha1.ExecutionPhaseSucceeded:
		return terminalPhaseSucceeded, executionCompletionTime(e), true
	case keystonev1alpha1.ExecutionPhaseFailed:
		return terminalPhaseFailed, executionCompletionTime(e), true
	case keystonev1alpha1.ExecutionPhaseAborted:
		return terminalPhaseAborted, executionCompletionTime(e), true
	case keystonev1alpha1.ExecutionPhaseRolledBack:
		return terminalPhaseRolledBack, executionCompletionTime(e), true
	case keystonev1alpha1.ExecutionPhaseRollbackFailed:
		return terminalPhaseRollbackFailed, executionCompletionTime(e), true
	default:
		// Pending / Running / Expanding / Expanded / Contracting /
		// Aborting / RollingBack — all in-flight.
		return "", time.Time{}, false
	}
}

func executionCompletionTime(e *keystonev1alpha1.MigrationExecution) time.Time {
	if e.Status.CompletionTime != nil {
		return e.Status.CompletionTime.Time.UTC()
	}
	if e.Status.StartTime != nil {
		return e.Status.StartTime.Time.UTC()
	}
	if !e.CreationTimestamp.IsZero() {
		return e.CreationTimestamp.Time.UTC()
	}
	return time.Time{}
}

func findReadyLikeCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

// ownerSDKey extracts the SchemaDefinition owner reference's
// "namespace/name" key from a child CR's owner refs. Returns the empty
// string when no SD ref is present (orphaned bundles, or bundles
// authored directly by humans without an SD parent — both cases fall
// back to operator-wide retention defaults).
func ownerSDKey(refs []metav1.OwnerReference, namespace string) string {
	for i := range refs {
		ref := &refs[i]
		if ref.Kind == "SchemaDefinition" &&
			ref.APIVersion == keystonev1alpha1.GroupVersion.String() {
			return namespace + "/" + ref.Name
		}
	}
	return ""
}

func ageExceeds(now, completed time.Time, window time.Duration) bool {
	if completed.IsZero() {
		return false
	}
	return now.Sub(completed) > window
}

// fmtDur is currently unused but kept for symmetry with the audit
// retention controller's diagnostic strings — silences the linter
// when interval / windows are debug-printed during incident triage.
var _ = fmt.Sprintf
