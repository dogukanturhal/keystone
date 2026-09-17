// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Audit ledger metrics surfaced by the Logger + AuditLogReconciler.
// Registered against the controller-runtime global metrics registry
// on init() so the manager's /metrics endpoint exposes them
// automatically.
//
// The retries counter is the primary alerting target — a rising
// rate on reason="alreadyexists" signals the 2026-04-21 failure
// mode (AuditLog.status drift vs true max AuditEntry.sequence).
// The healer metric signals the background reconciler is actively
// correcting that drift.
var (
	auditAppendRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_audit_append_retries_total",
			Help: "Cumulative count of AuditEntry Create retries, " +
				"partitioned by reason. reason=alreadyexists is the " +
				"2026-04-21 etcd-wedge signature — alert on " +
				"rate(...) > 1/s for 2m. reason=status_conflict is " +
				"benign AuditLog.status races under concurrent writers.",
		},
		[]string{"reason"},
	)

	auditAppendTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_audit_append_total",
			Help: "Cumulative count of AuditEntry Append calls, " +
				"partitioned by outcome (success | exhausted | error). " +
				"Use to compute a success ratio alongside the " +
				"retries counter.",
		},
		[]string{"outcome"},
	)

	auditLogDriftHealedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "keystone_audit_log_drift_healed_total",
			Help: "Cumulative count of AuditLog.status drift-heal " +
				"patches applied by the AuditLogReconciler. Any " +
				"non-zero rate means writers are hitting the " +
				"status-patch conflict path in production — " +
				"investigate concurrent Append pressure.",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(
		auditAppendRetriesTotal,
		auditAppendTotal,
		auditLogDriftHealedTotal,
	)
}

// RecordRetry bumps the retries counter. Exposed as a package
// function rather than a Logger method so tests can assert
// counter values without constructing a Logger.
func RecordRetry(reason string) {
	auditAppendRetriesTotal.WithLabelValues(reason).Inc()
}

// RecordAppend bumps the Append-outcome counter.
func RecordAppend(outcome string) {
	auditAppendTotal.WithLabelValues(outcome).Inc()
}

// RecordDriftHealed bumps the drift-healed counter. Called by
// AuditLogReconciler when it patches status.
func RecordDriftHealed() {
	auditLogDriftHealedTotal.Inc()
}
