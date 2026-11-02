// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// Ownership drift check for a LogicalDatabase.
//
// Report-only by default and never part of Ready: DatabaseSchema waits for
// its LogicalDatabase to be Ready, so gating on drift would stop the very
// migrations that could fix it. Every error here becomes an Unknown
// condition; nothing in this file may call r.fail.

const (
	// DefaultOwnershipCheckInterval is the minimum time between checks of one
	// LogicalDatabase (in-memory throttle; the reconcile itself runs more often).
	DefaultOwnershipCheckInterval = 30 * time.Minute

	ownershipSampleSize       = 5
	ownershipMaxStatusGroups  = 32
	ownershipMaxMessageBytes  = 1024
	ownershipMaxSampleLen     = 256
	ownershipMessageMaxGroups = 5
	ownershipRunbook          = "docs/runbooks/ownership-drift.md"

	// ownershipInspectTimeoutDefault bounds one InspectOwnership call, including
	// the wait for a free connection when the target pool is exhausted.
	ownershipInspectTimeoutDefault = 15 * time.Second
	// ownershipRemediationSlackDefault is added to the remediation budget to
	// bound candidate listing plus the reassign pass.
	ownershipRemediationSlackDefault = 15 * time.Second
	// ownershipRetryDelay is the short backoff after a lock timeout, an
	// exhausted budget or a capped pass (more candidates remain).
	ownershipRetryDelay = 5 * time.Minute
)

// Package vars so tests can shrink the pool-use bounds.
var (
	ownershipInspectTimeout   = ownershipInspectTimeoutDefault
	ownershipRemediationSlack = ownershipRemediationSlackDefault
)

// ownershipOutcome is what checkOwnership hands to success(). The zero value
// (Ran=false) leaves the existing condition and status untouched.
type ownershipOutcome struct {
	Ran bool
	// Status/Reason/Message become the OwnershipDrift condition.
	Status  metav1.ConditionStatus
	Reason  string
	Message string
	// Summary replaces status.ownershipDrift; nil clears it.
	Summary *keystonev1alpha1.OwnershipDriftStatus
}

// ownershipThrottle remembers when each LogicalDatabase was last checked.
// In memory on purpose: a status-stored throttle would never engage if the
// manager rolls out before the CRD (unknown status fields are dropped).
type ownershipThrottle struct {
	m sync.Map // types.UID -> ownershipThrottleEntry
}

type ownershipThrottleEntry struct {
	nextDue    time.Time
	generation int64
	policy     string
}

// due reports whether a check should run now: never checked, generation or
// effective policy changed, or the next-due time has passed.
func (t *ownershipThrottle) due(uid types.UID, gen int64, policy string, now time.Time) bool {
	v, ok := t.m.Load(uid)
	if !ok {
		return true
	}
	e := v.(ownershipThrottleEntry)
	return e.generation != gen || e.policy != policy || !now.Before(e.nextDue)
}

// mark records that the check ran and is next due at nextDue.
func (t *ownershipThrottle) mark(uid types.UID, gen int64, policy string, nextDue time.Time) {
	t.m.Store(uid, ownershipThrottleEntry{nextDue: nextDue, generation: gen, policy: policy})
}

// ownershipNextDue picks the next check time. A lock timeout or an exhausted
// budget retries soon (the pass made partial progress and the database is
// busy); non-lock failures wait the full interval so a persistently failing
// object cannot hot-loop; a capped pass (more candidates remain) retries soon.
func ownershipNextDue(now time.Time, interval time.Duration, res pg.ReassignResult, capped bool) time.Time {
	short := ownershipRetryDelay
	if interval < short {
		short = interval
	}
	switch {
	case res.StoppedOnLock || res.BudgetExhausted:
		return now.Add(short)
	case len(res.Failed) > 0:
		return now.Add(interval)
	case capped:
		return now.Add(short)
	}
	return now.Add(interval)
}

func (t *ownershipThrottle) forget(uid types.UID) { t.m.Delete(uid) }

// resolveOwnershipPolicy: LogicalDatabase override, else provider, else Report.
func resolveOwnershipPolicy(ldb *keystonev1alpha1.LogicalDatabase, provider *keystonev1alpha1.DatabaseProvider) string {
	if v := ldb.Spec.OwnershipRemediation; v != "" {
		return v
	}
	if provider != nil && provider.Spec.OwnershipRemediation != "" {
		return provider.Spec.OwnershipRemediation
	}
	return keystonev1alpha1.OwnershipRemediationReport
}

// ownershipQueryFor builds the catalog query from the DatabaseSchemas that
// reference ldb in its namespace.
func (r *LogicalDatabaseReconciler) ownershipQueryFor(ctx context.Context, ldb *keystonev1alpha1.LogicalDatabase) (pg.OwnershipQuery, error) {
	var schemas keystonev1alpha1.DatabaseSchemaList
	if err := r.List(ctx, &schemas, client.InNamespace(ldb.Namespace)); err != nil {
		return pg.OwnershipQuery{}, fmt.Errorf("list DatabaseSchemas: %w", err)
	}
	q := pg.OwnershipQuery{
		ExpectedOwner: ldb.Spec.OwnerRole,
		SampleSize:    ownershipSampleSize,
		ExcludedRelations: []string{
			"schema_migrations", "keystone_schema_migrations", "keystone_baselines",
		},
	}
	if t := ldb.Spec.TrackingTableName; t != "" {
		q.ExcludedRelations = append(q.ExcludedRelations, t)
	}
	for i := range schemas.Items {
		s := &schemas.Items[i]
		if s.Spec.LogicalDatabaseRef != ldb.Name || !s.DeletionTimestamp.IsZero() {
			continue
		}
		q.Schemas = append(q.Schemas, pg.SchemaOwner{Name: s.Spec.Name, Owner: s.Spec.OwnerRole})
	}
	sort.Slice(q.Schemas, func(i, j int) bool { return q.Schemas[i].Name < q.Schemas[j].Name })
	return q, nil
}

// checkOwnership inspects (and, when opted in, remediates) ownership drift.
// pool is the target-database pool, or nil when it could not be acquired.
// It never fails the reconcile.
func (r *LogicalDatabaseReconciler) checkOwnership(
	ctx context.Context,
	ldb *keystonev1alpha1.LogicalDatabase,
	provider *keystonev1alpha1.DatabaseProvider,
	pool *pgxpool.Pool,
	poolErr error,
) ownershipOutcome {
	interval := r.OwnershipCheckInterval
	if interval <= 0 {
		return ownershipOutcome{}
	}
	logger := log.FromContext(ctx).WithValues("logicaldatabase", client.ObjectKeyFromObject(ldb))
	policy := resolveOwnershipPolicy(ldb, provider)
	now := time.Now()
	if !r.ownership.due(ldb.UID, ldb.Generation, policy, now) {
		return ownershipOutcome{}
	}

	unknown := func(reason string, err error) ownershipOutcome {
		ownershipDriftChecks.WithLabelValues(ldb.Namespace, ldb.Name, "error").Inc()
		logger.Info("ownership check did not complete", "reason", reason, "error", err.Error())
		return ownershipOutcome{
			Ran: true, Status: metav1.ConditionUnknown, Reason: reason,
			Message: truncateBytes(fmt.Sprintf("ownership check did not complete: %v", err), ownershipMaxMessageBytes),
			Summary: previousSummary(ldb),
		}
	}

	if pool == nil {
		if poolErr == nil {
			poolErr = fmt.Errorf("no target pool")
		}
		return unknown(keystonev1alpha1.ReasonOwnershipCheckFailed, poolErr)
	}
	q, err := r.ownershipQueryFor(ctx, ldb)
	if err != nil {
		return unknown(keystonev1alpha1.ReasonOwnershipCheckFailed, err)
	}
	if len(q.Schemas) == 0 {
		forgetOwnershipGauges(ldb.Namespace, ldb.Name)
		r.ownership.mark(ldb.UID, ldb.Generation, policy, now.Add(interval))
		ownershipDriftChecks.WithLabelValues(ldb.Namespace, ldb.Name, "clean").Inc()
		return ownershipOutcome{
			Ran: true, Status: metav1.ConditionFalse, Reason: keystonev1alpha1.ReasonOwnershipNoManagedSchemas,
			Message: "no DatabaseSchema references this LogicalDatabase",
		}
	}

	start := time.Now()
	inspectCtx, cancelInspect := context.WithTimeout(ctx, ownershipInspectTimeout)
	rep, err := pg.InspectOwnership(inspectCtx, pool, q)
	cancelInspect()
	ownershipDriftCheckDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		// Back off: a hung or exhausted target pool must not cost a full
		// timeout on every reconcile.
		r.ownership.mark(ldb.UID, ldb.Generation, policy, now.Add(min(ownershipRetryDelay, interval)))
		return unknown(keystonev1alpha1.ReasonOwnershipCheckFailed, err)
	}
	if rep.OwnerRoleMissing {
		resetOwnershipSeries(ldb.Namespace, ldb.Name)
		return unknown(keystonev1alpha1.ReasonOwnershipOwnerRoleMissing,
			fmt.Errorf("owner role %q does not exist", ldb.Spec.OwnerRole))
	}

	nextDue := now.Add(interval)
	note := ""
	remediationRan := false
	var res pg.ReassignResult
	if policy == keystonev1alpha1.OwnershipRemediationReassignFromConnectionRole {
		switch {
		case rep.ConnRoleSuper:
			note = "remediation refused: connection role is a superuser"
		case rep.RemediableCount() > 0:
			var capped bool
			res, capped = r.remediateOwnership(ctx, ldb, pool, q)
			nextDue = ownershipNextDue(now, interval, res, capped)
			remediationRan = true
			// Re-inspect so condition, status and metrics describe the
			// post-remediation state. A failed re-inspect keeps the first report.
			reCtx, cancelRe := context.WithTimeout(ctx, ownershipInspectTimeout)
			if again, rerr := pg.InspectOwnership(reCtx, pool, q); rerr == nil && !again.OwnerRoleMissing {
				rep = again
			}
			cancelRe()
		}
	}

	outcome := buildOwnershipOutcome(rep, ldb.Spec.OwnerRole, policy, note, now)
	publishOwnershipMetrics(ldb, rep, now)
	if outcome.Status == metav1.ConditionTrue {
		ownershipDriftChecks.WithLabelValues(ldb.Namespace, ldb.Name, "drift").Inc()
	} else {
		ownershipDriftChecks.WithLabelValues(ldb.Namespace, ldb.Name, "clean").Inc()
	}
	// Always throttle: a busy database or failing object must not be retried
	// on every reconcile. nextDue distinguishes short retry from full interval.
	r.ownership.mark(ldb.UID, ldb.Generation, policy, nextDue)

	prev := meta.FindStatusCondition(ldb.Status.Conditions, keystonev1alpha1.ConditionTypeOwnershipDrift)
	newlyDrifted := outcome.Status == metav1.ConditionTrue && (prev == nil || prev.Status != metav1.ConditionTrue)
	if newlyDrifted && r.Recorder != nil {
		r.Recorder.Eventf(ldb, corev1.EventTypeWarning, "OwnershipDriftDetected", "%s", outcome.Message)
	}
	if newlyDrifted && !remediationRan {
		r.emitAudit(ctx, ldb, "ownership-drift", outcome.Message, "success")
	}
	if remediationRan {
		r.reportRemediation(ctx, ldb, res)
	}
	return outcome
}

// remediateOwnership runs one budgeted reassign pass and records per-object
// metrics. capped=true when the candidate cap was hit and more may remain.
func (r *LogicalDatabaseReconciler) remediateOwnership(
	ctx context.Context,
	ldb *keystonev1alpha1.LogicalDatabase,
	pool *pgxpool.Pool,
	q pg.OwnershipQuery,
) (res pg.ReassignResult, capped bool) {
	budget := r.OwnershipRemediationBudget
	if budget <= 0 {
		budget = pg.DefaultReassignBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget+ownershipRemediationSlack)
	defer cancel()
	cands, err := pg.ListRemediationCandidates(ctx, pool, q, pg.MaxRemediationCandidates)
	if err != nil {
		log.FromContext(ctx).Info("ownership remediation: candidate query failed", "error", err.Error())
		return pg.ReassignResult{FirstError: err, Failed: map[string]int{"unknown": 1}}, false
	}
	res = pg.ReassignCandidates(ctx, pool, cands, ldb.Spec.OwnerRole, budget)
	for kind, n := range res.Reassigned {
		ownershipRemediations.WithLabelValues(ldb.Namespace, ldb.Name, kind, "reassigned").Add(float64(n))
	}
	for kind, n := range res.Failed {
		ownershipRemediations.WithLabelValues(ldb.Namespace, ldb.Name, kind, "failed").Add(float64(n))
	}
	for reason, n := range res.Skipped {
		ownershipRemediations.WithLabelValues(ldb.Namespace, ldb.Name, "", reason).Add(float64(n))
	}
	return res, len(cands) >= pg.MaxRemediationCandidates
}

func (r *LogicalDatabaseReconciler) reportRemediation(ctx context.Context, ldb *keystonev1alpha1.LogicalDatabase, res pg.ReassignResult) {
	if n := res.Total(); n > 0 {
		msg := fmt.Sprintf("reassigned %d object(s) to %s (%s)", n, ldb.Spec.OwnerRole, formatCounts(res.Reassigned))
		if r.Recorder != nil {
			r.Recorder.Eventf(ldb, corev1.EventTypeNormal, "OwnershipRemediated", "%s", msg)
		}
		r.emitAudit(ctx, ldb, "reassign-ownership", msg, "success")
	}
	if res.FirstError != nil || res.StoppedOnLock || res.BudgetExhausted {
		why := "remediation incomplete"
		switch {
		case res.StoppedOnLock:
			why = "stopped at first lock timeout"
		case res.BudgetExhausted:
			why = "time budget exhausted"
		}
		msg := why
		if res.FirstError != nil {
			msg = fmt.Sprintf("%s: %v", why, res.FirstError)
		}
		msg = truncateBytes(msg, ownershipMaxMessageBytes)
		if r.Recorder != nil {
			r.Recorder.Eventf(ldb, corev1.EventTypeWarning, "OwnershipRemediationFailed", "%s", msg)
		}
		r.emitAudit(ctx, ldb, "reassign-ownership", msg, "error")
	}
}

func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

// previousSummary keeps the last good summary across an Unknown result so a
// transient failure does not blank it.
func previousSummary(ldb *keystonev1alpha1.LogicalDatabase) *keystonev1alpha1.OwnershipDriftStatus {
	return ldb.Status.OwnershipDrift
}

// publishOwnershipMetrics rewrites the gauge series of one LogicalDatabase.
func publishOwnershipMetrics(ldb *keystonev1alpha1.LogicalDatabase, rep pg.OwnershipReport, now time.Time) {
	resetOwnershipSeries(ldb.Namespace, ldb.Name)
	remediableCap := !rep.ConnRoleSuper
	for _, g := range rep.Groups {
		rem := 0
		if remediableCap {
			rem = g.Remediable()
		}
		if rem > 0 {
			ownershipDriftObjects.WithLabelValues(ldb.Namespace, ldb.Name, ldb.Spec.Name, g.Kind, g.Owner, "true").Set(float64(rem))
		}
		if rest := g.Count - rem; rest > 0 {
			ownershipDriftObjects.WithLabelValues(ldb.Namespace, ldb.Name, ldb.Spec.Name, g.Kind, g.Owner, "false").Set(float64(rest))
		}
	}
	ownershipDriftLastCheck.WithLabelValues(ldb.Namespace, ldb.Name).Set(float64(now.Unix()))
}

// buildOwnershipOutcome turns a report into condition + bounded status.
func buildOwnershipOutcome(rep pg.OwnershipReport, expected, policy, note string, now time.Time) ownershipOutcome {
	t := metav1.NewTime(now)
	total := rep.Total()
	summary := &keystonev1alpha1.OwnershipDriftStatus{
		LastCheckTime:  &t,
		ExpectedOwner:  expected,
		DriftedObjects: int32(total),
		Remediable:     int32(rep.RemediableCount()),
	}
	for i, g := range rep.Groups {
		if i >= ownershipMaxStatusGroups {
			break
		}
		sample := make([]string, 0, len(g.Sample))
		for _, s := range g.Sample {
			sample = append(sample, truncateBytes(s, ownershipMaxSampleLen))
		}
		rem := g.Remediable()
		if rep.ConnRoleSuper {
			rem = 0
		}
		summary.Groups = append(summary.Groups, keystonev1alpha1.OwnershipDriftGroup{
			Kind: truncateBytes(g.Kind, ownershipMaxSampleLen), Owner: truncateBytes(g.Owner, ownershipMaxSampleLen),
			Count: int32(g.Count), Remediable: int32(rem), Sample: sample,
		})
	}
	if total == 0 {
		return ownershipOutcome{
			Ran: true, Status: metav1.ConditionFalse, Reason: keystonev1alpha1.ReasonOwnershipNoDrift,
			Message: fmt.Sprintf("all objects in managed schemas are owned by %s", expected), Summary: summary,
		}
	}
	return ownershipOutcome{
		Ran: true, Status: metav1.ConditionTrue, Reason: keystonev1alpha1.ReasonOwnershipDriftDetected,
		Message: buildOwnershipMessage(rep, expected, policy, note), Summary: summary,
	}
}

// buildOwnershipMessage renders a deterministic message of at most
// ownershipMaxMessageBytes. Groups arrive ordered (count desc, kind, owner).
func buildOwnershipMessage(rep pg.OwnershipReport, expected, policy, note string) string {
	var b strings.Builder
	total := rep.Total()
	fmt.Fprintf(&b, "%d objects not owned by %s: ", total, expected)
	for i, g := range rep.Groups {
		if i == ownershipMessageMaxGroups {
			fmt.Fprintf(&b, ", +%d more groups", len(rep.Groups)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s/%s=%d", g.Kind, g.Owner, g.Count)
	}
	b.WriteString(". ")

	rem := rep.RemediableCount()
	conn := rep.ConnRole
	if conn == "" {
		conn = "connection role"
	}
	fmt.Fprintf(&b, "Remediable (%s): %d [policy=%s", conn, rem, policy)
	if note != "" {
		b.WriteString("; " + note)
	}
	b.WriteString("]. ")

	// Everything the operator will not move, summed per owner.
	other := map[string]int{}
	for _, g := range rep.Groups {
		n := g.Count
		if !rep.ConnRoleSuper {
			n -= g.Remediable()
		}
		if n > 0 {
			other[g.Owner] += n
		}
	}
	if len(other) > 0 {
		owners := make([]string, 0, len(other))
		for o := range other {
			owners = append(owners, o)
		}
		sort.Slice(owners, func(i, j int) bool {
			if other[owners[i]] != other[owners[j]] {
				return other[owners[i]] > other[owners[j]]
			}
			return owners[i] < owners[j]
		})
		parts := make([]string, 0, len(owners))
		for i, o := range owners {
			if i == 3 {
				parts = append(parts, fmt.Sprintf("+%d more", len(owners)-i))
				break
			}
			parts = append(parts, fmt.Sprintf("%s=%d", o, other[o]))
		}
		fmt.Fprintf(&b, "Not remediable by keystone (%s): a superuser must ALTER … OWNER TO %s, see %s (never REASSIGN OWNED BY postgres). ",
			strings.Join(parts, ", "), expected, ownershipRunbook)
	}

	var sample []string
	for _, g := range rep.Groups {
		for _, s := range g.Sample {
			if len(sample) < 3 {
				sample = append(sample, s)
			}
		}
		if len(sample) >= 3 {
			break
		}
	}
	if len(sample) > 0 {
		b.WriteString("Sample: " + strings.Join(sample, ", "))
	}
	return truncateBytes(strings.TrimSpace(b.String()), ownershipMaxMessageBytes)
}

// truncateBytes cuts s to at most n bytes on a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// applyOwnership writes the outcome onto the object. Called by success()
// after the status-patch DeepCopy, otherwise the merge patch would drop it.
func applyOwnership(ldb *keystonev1alpha1.LogicalDatabase, o ownershipOutcome) {
	if !o.Ran {
		return
	}
	meta.SetStatusCondition(&ldb.Status.Conditions, metav1.Condition{
		Type:               keystonev1alpha1.ConditionTypeOwnershipDrift,
		Status:             o.Status,
		Reason:             o.Reason,
		Message:            o.Message,
		ObservedGeneration: ldb.Generation,
	})
	ldb.Status.OwnershipDrift = o.Summary
}
