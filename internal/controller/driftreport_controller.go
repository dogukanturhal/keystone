// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/dogukanturhal/keystone-sdk/go/drift"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
	"github.com/dogukanturhal/keystone/internal/conditions"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// DriftController periodically inspects every Ready DatabaseSchema,
// computes a structural fingerprint via information_schema, and
// compares it against the recorded baseline. On mismatch it creates or
// updates a DriftReport CR and flips the schema's DriftFree condition
// to False.
//
// Two responsibilities, one binary:
//   - PeriodicChecker (manager.Runnable): owns the time.Ticker that
//     drives `inspectAll(ctx)` every CheckInterval.
//   - reconcileSchema(): does the per-schema work (inspect, baseline
//     compare, CR upsert).
//
// Detection is deliberately NOT event-driven: DriftReports are written
// by this controller, and reconciling our own writes would buy nothing —
// drift is a property of the database, not of the CR, so only the clock
// can tell us it changed. The PeriodicChecker drives every detection.
//
// The one exception is the accept-drift annotation, which is an operator
// edit that the controller is supposed to act on. Waiting a full
// CheckInterval to read it left an acceptance indistinguishable from an
// ignored annotation, so that single field is watched — narrowly, by
// DriftAcceptanceReconciler, which delegates straight back into
// reconcileSchema.
type DriftController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Pools    *pg.PoolCache

	// CheckInterval is how often the periodic loop runs. Defaults to
	// 1 hour. Pin shorter for staging or chaos tests.
	CheckInterval time.Duration

	// SystemNamespace is where DatabaseProvider admin Secrets live.
	SystemNamespace string

	// AuditLogger appends tamper-evident entries on drift lifecycle
	// events (detect, resolve, accept). Routine clean checks are
	// excluded — they are already counted in driftCheckTotal.
	AuditLogger     *audit.Logger
	ManagerIdentity keystonev1alpha1.AuditActor
}

// emitAudit appends a tamper-evident entry for drift-lifecycle events.
// Errors logged at warn level rather than discarded.
func (c *DriftController) emitAudit(
	ctx context.Context,
	schema *keystonev1alpha1.DatabaseSchema,
	verb, reason, outcome string,
) {
	if c.AuditLogger == nil {
		return
	}
	if _, _, err := c.AuditLogger.Append(ctx, audit.Event{
		Verb:        verb,
		Actor:       c.ManagerIdentity,
		ResourceRef: audit.ResourceRefFromObject(schema),
		Reason:      reason,
		Outcome:     outcome,
		After:       schema,
	}); err != nil {
		log.FromContext(ctx).Error(err, "audit append failed",
			"controller", "driftreport",
			"schema", schema.Name,
			"verb", verb,
			"outcome", outcome)
	}
}

// Start implements manager.Runnable. Runs until the manager's context
// is canceled.
func (c *DriftController) Start(ctx context.Context) error {
	if c.CheckInterval <= 0 {
		c.CheckInterval = time.Hour
	}
	logger := log.FromContext(ctx).WithValues("component", "driftcontroller")
	logger.Info("starting drift periodic loop", "interval", c.CheckInterval)

	t := time.NewTicker(c.CheckInterval)
	defer t.Stop()

	// Run once immediately so the first inspection doesn't wait a full
	// interval after the manager comes up.
	c.inspectAll(ctx, logger)

	for {
		select {
		case <-ctx.Done():
			logger.Info("drift loop stopping")
			return nil
		case <-t.C:
			c.inspectAll(ctx, logger)
		}
	}
}

// NeedLeaderElection makes the periodic loop run only on the elected
// leader, matching the rest of the manager.
func (c *DriftController) NeedLeaderElection() bool { return true }

// inspectAll lists every DatabaseSchema across the cluster and
// inspects each one. Errors per-schema do NOT fail the whole pass —
// we want partial visibility better than none.
func (c *DriftController) inspectAll(ctx context.Context, logger logr.Logger) {
	var schemas keystonev1alpha1.DatabaseSchemaList
	if err := c.List(ctx, &schemas); err != nil {
		logger.Error(err, "list DatabaseSchemas for drift inspection")
		return
	}
	for i := range schemas.Items {
		s := &schemas.Items[i]
		if !conditions.IsTrue(s.Status.Conditions, keystonev1alpha1.ConditionTypeReady) {
			// Skip schemas that aren't Ready — there's nothing to
			// inspect against and a baseline would be premature.
			continue
		}
		if err := c.reconcileSchema(ctx, s, logger); err != nil {
			logger.Error(err, "drift inspection failed", "schema", s.Name, "namespace", s.Namespace)
			driftCheckTotal.WithLabelValues(s.Namespace, s.Spec.LogicalDatabaseRef, s.Spec.Name, "error").Inc()
		}
	}
}

func (c *DriftController) reconcileSchema(ctx context.Context, schema *keystonev1alpha1.DatabaseSchema, logger logr.Logger) error {
	start := time.Now()

	// Resolve the LogicalDatabase + DatabaseProvider for connectivity.
	var ldb keystonev1alpha1.LogicalDatabase
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: schema.Namespace, Name: schema.Spec.LogicalDatabaseRef,
	}, &ldb); err != nil {
		return fmt.Errorf("resolve LogicalDatabase: %w", err)
	}
	var provider keystonev1alpha1.DatabaseProvider
	if err := c.Get(ctx, types.NamespacedName{Name: ldb.Spec.ProviderRef}, &provider); err != nil {
		return fmt.Errorf("resolve DatabaseProvider: %w", err)
	}
	creds, err := c.resolveCredentials(ctx, &provider)
	if err != nil {
		return fmt.Errorf("resolve admin Secret: %w", err)
	}

	// Open pool against the target database.
	cfg := pg.PoolConfig{
		Host:     provider.Spec.Host,
		Port:     int(provider.Spec.Port),
		Database: ldb.Spec.Name,
		Username: creds.Username,
		Password: creds.Password,
		SSLMode:  string(provider.Spec.SSLMode),
		MaxConns: provider.Spec.PoolMaxConns,
	}
	pool, release, err := c.Pools.Acquire(ctx, cfg)
	if err != nil {
		return fmt.Errorf("acquire pool: %w", err)
	}
	defer release()

	// Ensure the baseline table + snapshot column exist. Cheap on every
	// call; the IF NOT EXISTS clauses make them no-ops after first run.
	if err := drift.EnsureBaselineTable(ctx, pool, schema.Spec.Name); err != nil {
		return fmt.Errorf("ensure baseline table: %w", err)
	}
	if err := drift.EnsureSnapshotColumn(ctx, pool, schema.Spec.Name); err != nil {
		return fmt.Errorf("ensure snapshot column: %w", err)
	}

	// Inspect the live schema.
	snap, err := drift.NewInspector(pool).Inspect(ctx, schema.Spec.Name)
	if err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	observedHash, err := drift.Hash(snap)
	if err != nil {
		return fmt.Errorf("hash snapshot: %w", err)
	}

	// Read the recorded baseline.
	baseline, err := drift.ReadBaseline(ctx, pool, schema.Spec.Name, drift.BaselineKindStructure)
	if err != nil {
		return fmt.Errorf("read baseline: %w", err)
	}

	// First inspection of a schema we've never baselined: record the
	// current state as the baseline and treat as clean. This avoids
	// flooding alerts when the controller is first deployed.
	if baseline == nil {
		if err := drift.WriteBaseline(ctx, pool, schema.Spec.Name,
			drift.BaselineKindStructure, observedHash, "drift-first-observation"); err != nil {
			return fmt.Errorf("write initial baseline: %w", err)
		}
		if err := drift.WriteSnapshot(ctx, pool, schema.Spec.Name, snap); err != nil {
			return fmt.Errorf("write initial snapshot: %w", err)
		}
		c.markClean(ctx, schema, observedHash, observedHash)
		c.recordOutcome(schema, "clean", start)
		return nil
	}

	// Compare.
	if baseline.Hash == observedHash {
		c.markClean(ctx, schema, baseline.Hash, observedHash)
		c.recordOutcome(schema, "clean", start)
		// Clean up any prior DriftReport for this schema.
		c.resolveExistingReport(ctx, schema, logger)
		return nil
	}

	// Read the prior snapshot once: the model-upgrade check below and the
	// structured findings further down both need it.
	priorSnap, sErr := drift.ReadSnapshot(ctx, pool, schema.Spec.Name)
	if sErr != nil {
		// Don't fail the whole reconcile — fall back to hash-only diff.
		logger.Error(sErr, "read prior snapshot; falling back to hash-only DriftReport",
			"schema", schema.Name)
		priorSnap = nil
	}

	modelOnly, mErr := snapshotModelOnlyChange(priorSnap, snap, baseline.Hash)
	if mErr != nil {
		logger.Error(mErr, "hash legacy projection; treating as ordinary drift",
			"schema", schema.Name)
	}
	if modelOnly {
		if err := drift.WriteBaseline(ctx, pool, schema.Spec.Name,
			drift.BaselineKindStructure, observedHash, "snapshot-model-upgrade:rls_forced"); err != nil {
			return fmt.Errorf("restate baseline for snapshot model upgrade: %w", err)
		}
		if err := drift.WriteSnapshot(ctx, pool, schema.Spec.Name, snap); err != nil {
			return fmt.Errorf("restate snapshot for snapshot model upgrade: %w", err)
		}
		logger.Info("baseline restated in the new snapshot model; no schema change",
			"schema", schema.Name, "was", baseline.Hash, "now", observedHash)
		c.markClean(ctx, schema, observedHash, observedHash)
		c.recordOutcome(schema, "clean", start)
		c.resolveExistingReport(ctx, schema, logger)
		return nil
	}

	// Drift detected — but an operator may have already reviewed it and
	// asked for the observed state to become the new baseline. That has
	// to be checked BEFORE anything is recorded, or accepting drift would
	// still increment the drift counter, re-mark the schema drifted and
	// re-upsert the report it is supposed to resolve.
	if accepted, aErr := c.acceptDrift(ctx, schema, pool, snap, observedHash, logger); aErr != nil {
		// Fall through and report the drift normally: failing to
		// re-baseline must not lose the drift signal.
		logger.Error(aErr, "accept-drift requested but re-baselining failed",
			"schema", schema.Name)
	} else if accepted {
		c.markClean(ctx, schema, observedHash, observedHash)
		c.recordOutcome(schema, "clean", start)
		return nil
	}

	driftDetectedTotal.WithLabelValues(schema.Namespace, schema.Spec.LogicalDatabaseRef, schema.Spec.Name).Inc()
	c.recordOutcome(schema, "drift", start)
	c.markDrifted(ctx, schema, baseline.Hash, observedHash)

	findings := drift.Diff(priorSnap, snap)
	severity := drift.Severity(findings)

	return c.upsertDriftReport(ctx, schema, &ldb, &provider, baseline.Hash, observedHash, findings, severity, logger)
}

// snapshotModelOnlyChange reports whether the observed hash differs from
// the recorded baseline solely because the snapshot model grew a field,
// rather than because anything about the database moved.
//
// Growing the model changes every stored hash, and that is not drift.
// Left alone, the first reconcile after such an upgrade would mark every
// schema in the fleet drifted and keep it that way: the stored baseline
// is only rewritten when an operator accepts drift, so the report would
// return on every pass until each schema was hand-accepted. That is
// precisely the state in which a real finding gets lost in the noise.
//
// Hashing the observed snapshot projected back onto the old model answers
// the question exactly, with no version counter and no heuristic. A match
// means the model is the only thing that changed and the caller may
// restate the baseline; anything else means something really did change
// and the ordinary drift path must run. The upgrade therefore cannot
// swallow a genuine finding.
//
// The projection is only meaningful while a legacy baseline is still on
// record, so this reports false once the prior snapshot carries the field
// — restating is idempotent and happens at most once per schema.
//
// An error means the projection could not be hashed; the boolean is then
// false, so the caller falls through to ordinary drift handling.
func snapshotModelOnlyChange(prior, observed *drift.Snapshot, baselineHash string) (bool, error) {
	if prior == nil || !prior.PredatesRLSForce() {
		return false, nil
	}
	legacyHash, err := drift.Hash(observed.WithoutRLSForce())
	if err != nil {
		return false, fmt.Errorf("hash legacy projection of observed snapshot: %w", err)
	}
	return legacyHash == baselineHash, nil
}

// acceptDrift implements the accept-as-baseline flow. When an operator
// sets `keystone.hexxlock.io/accept-drift=true` on a schema's
// DriftReport, the observed state becomes the new baseline and the
// report is resolved.
//
// Reports false when there is no report, or it does not carry the
// annotation — the ordinary path.
//
// This used to exist as acceptDriftIfRequested with a doc comment saying
// it was "invoked from reconcileSchema", and nothing invoked it: the
// annotation was documented, could be set, and did nothing. An operator
// annotating a report would watch it be re-reported on the next pass
// with no indication why. It is called from the drift branch now, and
// the whole point is that a schema whose drift has been accepted must
// come out of this function clean, so the caller marks it clean rather
// than recording drift.
func (c *DriftController) acceptDrift(ctx context.Context, schema *keystonev1alpha1.DatabaseSchema, pool *pgxpool.Pool, snap *drift.Snapshot, observedHash string, logger logr.Logger) (bool, error) {
	name := schema.Name + "-drift"
	var report keystonev1alpha1.DriftReport
	err := c.Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: name}, &report)
	if apierrors.IsNotFound(err) {
		// No report yet: this is the first pass that saw the drift, so
		// there was nothing for anyone to accept.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup DriftReport %s: %w", name, err)
	}
	if report.Annotations[AnnotationAcceptDrift] != "true" {
		return false, nil
	}

	source := fmt.Sprintf("accept-drift:%s/%s", report.Namespace, report.Name)
	if err := drift.WriteBaseline(ctx, pool, schema.Spec.Name,
		drift.BaselineKindStructure, observedHash, source); err != nil {
		return false, fmt.Errorf("rebaseline hash: %w", err)
	}
	if err := drift.WriteSnapshot(ctx, pool, schema.Spec.Name, snap); err != nil {
		return false, fmt.Errorf("rebaseline snapshot: %w", err)
	}
	if err := c.Delete(ctx, &report); err != nil && !apierrors.IsNotFound(err) {
		// The baseline is already written, so the schema is clean; a
		// lingering report is cosmetic and the next pass prunes it.
		logger.Error(err, "accepted drift but could not delete the DriftReport", "name", name)
	}

	c.Recorder.Eventf(schema, corev1.EventTypeNormal, "DriftAccepted",
		"operator accepted drift on schema %q; baseline is now %s",
		schema.Spec.Name, observedHash[:8])
	c.emitAudit(ctx, schema, "accept-drift",
		fmt.Sprintf("baseline re-recorded as %s via %s", observedHash[:8], source),
		"success")
	logger.Info("drift accepted; baseline re-recorded",
		"schema", schema.Name, "baseline", observedHash[:8])
	return true, nil
}

// AnnotationAcceptDrift on a DriftReport tells the controller to
// re-baseline against the current observed state and delete the report.
const AnnotationAcceptDrift = "keystone.hexxlock.io/accept-drift"

// markClean sets the DriftFree condition True and refreshes the staleness gauge.
func (c *DriftController) markClean(ctx context.Context, schema *keystonev1alpha1.DatabaseSchema, expected, observed string) {
	patch := client.MergeFrom(schema.DeepCopy())
	schema.Status.Conditions = conditions.Set(schema.Status.Conditions,
		keystonev1alpha1.ConditionTypeDriftFree, metav1.ConditionTrue,
		"BaselineMatches",
		fmt.Sprintf("live schema hash %s matches recorded baseline", observed[:8]),
		schema.Generation)
	_ = c.Status().Patch(ctx, schema, patch)
	driftStalenessSeconds.WithLabelValues(schema.Namespace, schema.Spec.LogicalDatabaseRef, schema.Spec.Name).Set(0)
}

// markDrifted sets DriftFree=False with a structured reason.
func (c *DriftController) markDrifted(ctx context.Context, schema *keystonev1alpha1.DatabaseSchema, expected, observed string) {
	patch := client.MergeFrom(schema.DeepCopy())
	schema.Status.Conditions = conditions.Set(schema.Status.Conditions,
		keystonev1alpha1.ConditionTypeDriftFree, metav1.ConditionFalse,
		"BaselineMismatch",
		fmt.Sprintf("expected hash %s but observed %s", expected[:8], observed[:8]),
		schema.Generation)
	_ = c.Status().Patch(ctx, schema, patch)
}

func (c *DriftController) recordOutcome(schema *keystonev1alpha1.DatabaseSchema, outcome string, started time.Time) {
	driftCheckTotal.WithLabelValues(schema.Namespace, schema.Spec.LogicalDatabaseRef, schema.Spec.Name, outcome).Inc()
	driftCheckDurationSeconds.WithLabelValues(schema.Namespace, schema.Spec.LogicalDatabaseRef, schema.Spec.Name).
		Observe(time.Since(started).Seconds())
}

// upsertDriftReport creates or updates the DriftReport CR for this
// schema. Naming: one DriftReport per schema, name "<schema>-drift",
// so subsequent observations update the same CR rather than spawning
// new ones.
func (c *DriftController) upsertDriftReport(
	ctx context.Context,
	schema *keystonev1alpha1.DatabaseSchema,
	ldb *keystonev1alpha1.LogicalDatabase,
	provider *keystonev1alpha1.DatabaseProvider,
	expected, observed string,
	findings []keystonev1alpha1.DriftFinding,
	severity keystonev1alpha1.DriftSeverity,
	logger logr.Logger,
) error {
	name := schema.Name + "-drift"

	var report keystonev1alpha1.DriftReport
	err := c.Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: name}, &report)
	now := metav1.Now()

	if apierrors.IsNotFound(err) {
		report = keystonev1alpha1.DriftReport{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: schema.Namespace,
				Name:      name,
				Labels: map[string]string{
					"keystone.hexxlock.io/schema":           schema.Name,
					"keystone.hexxlock.io/logical-database": ldb.Name,
					"keystone.hexxlock.io/provider":         provider.Name,
				},
			},
			Spec: keystonev1alpha1.DriftReportSpec{
				DatabaseSchemaRef:  schema.Name,
				LogicalDatabaseRef: ldb.Name,
				ProviderRef:        provider.Name,
			},
		}
		if err := c.Create(ctx, &report); err != nil {
			return fmt.Errorf("create DriftReport: %w", err)
		}
		c.Recorder.Eventf(schema, corev1.EventTypeWarning, "DriftDetected",
			"schema %q diverged from recorded baseline; see DriftReport %s",
			schema.Spec.Name, name)
		c.emitAudit(ctx, schema, "detect-drift",
			fmt.Sprintf("DriftReport %s opened: expected %s observed %s severity=%s findings=%d",
				name, expected[:8], observed[:8], severity, len(findings)),
			"error")
	} else if err != nil {
		return fmt.Errorf("get DriftReport: %w", err)
	}

	patch := client.MergeFrom(report.DeepCopy())
	report.Status.ObservedGeneration = report.Generation
	report.Status.Severity = severity
	report.Status.ExpectedHash = expected
	report.Status.ObservedHash = observed
	report.Status.Findings = findings
	report.Status.LastObservedAt = &now
	if report.Status.DetectedAt == nil {
		report.Status.DetectedAt = &now
	}
	report.Status.Conditions = conditions.Set(report.Status.Conditions,
		keystonev1alpha1.ConditionTypeDetected, metav1.ConditionTrue, "BaselineMismatch",
		fmt.Sprintf("expected %s vs observed %s; %d finding(s)",
			expected[:8], observed[:8], len(findings)),
		report.Generation)
	if err := c.Status().Patch(ctx, &report, patch); err != nil {
		return fmt.Errorf("patch DriftReport status: %w", err)
	}
	logger.Info("drift recorded", "schema", schema.Name, "report", name,
		"expected", expected[:8], "observed", observed[:8],
		"findings", len(findings), "severity", severity)
	return nil
}

// resolveExistingReport deletes any open DriftReport for this schema
// once drift clears. We do not "Resolved" them — that's noise for the
// audit trail; instead we flip the schema condition and prune the CR.
func (c *DriftController) resolveExistingReport(ctx context.Context, schema *keystonev1alpha1.DatabaseSchema, logger logr.Logger) {
	name := schema.Name + "-drift"
	var report keystonev1alpha1.DriftReport
	err := c.Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: name}, &report)
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		logger.Error(err, "lookup existing DriftReport", "name", name)
		return
	}
	if err := c.Delete(ctx, &report); err != nil && !apierrors.IsNotFound(err) {
		logger.Error(err, "delete resolved DriftReport", "name", name)
		return
	}
	c.Recorder.Eventf(schema, corev1.EventTypeNormal, "DriftResolved",
		"schema %q now matches recorded baseline; DriftReport %s pruned",
		schema.Spec.Name, name)
	c.emitAudit(ctx, schema, "resolve-drift",
		fmt.Sprintf("DriftReport %s pruned; baseline now matches", name),
		"success")
}

func (c *DriftController) resolveCredentials(ctx context.Context, p *keystonev1alpha1.DatabaseProvider) (resolvedCreds, error) {
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
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: c.SystemNamespace, Name: ref.SecretName,
	}, &sec); err != nil {
		return resolvedCreds{}, fmt.Errorf("read admin Secret %s/%s: %w",
			c.SystemNamespace, ref.SecretName, err)
	}
	u, ok := sec.Data[userKey]
	if !ok || len(u) == 0 {
		return resolvedCreds{}, fmt.Errorf("Secret %s/%s missing key %q",
			c.SystemNamespace, ref.SecretName, userKey)
	}
	pw, ok := sec.Data[passKey]
	if !ok || len(pw) == 0 {
		return resolvedCreds{}, fmt.Errorf("Secret %s/%s missing key %q",
			c.SystemNamespace, ref.SecretName, passKey)
	}
	return resolvedCreds{Username: string(u), Password: string(pw)}, nil
}

// SetupWithManager registers the periodic checker with the manager.
// Note: this is NOT a standard controller — it runs as a manager.Runnable
// and drives reconciliation off a time.Ticker, not the workqueue.
func (c *DriftController) SetupWithManager(mgr ctrl.Manager) error {
	if c.SystemNamespace == "" {
		c.SystemNamespace = "keystone-system"
	}
	if c.Recorder == nil {
		c.Recorder = mgr.GetEventRecorderFor("keystone-drift")
	}
	return mgr.Add(manager.RunnableFunc(c.Start))
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=driftreports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=driftreports/status,verbs=get;update;patch
