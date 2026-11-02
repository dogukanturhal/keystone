// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Ownership-drift metrics surfaced by the LogicalDatabase controller.
// Registered against the controller-runtime registry on init(), same
// pattern as drift_metrics.go.
var (
	ownershipDriftObjects = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "keystone_ownership_drift_objects",
			Help: "Objects in a LogicalDatabase's managed schemas not owned by " +
				"spec.ownerRole, by kind and current owner. remediable=true means " +
				"the operator could reassign them when remediation is enabled. " +
				"Only non-zero groups are exported.",
		},
		[]string{"namespace", "logical_database", "database", "kind", "owner", "remediable"},
	)

	ownershipDriftLastCheck = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "keystone_ownership_drift_last_check_timestamp_seconds",
			Help: "Unix time of the last successful ownership check per LogicalDatabase.",
		},
		[]string{"namespace", "logical_database"},
	)

	ownershipDriftChecks = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_ownership_drift_checks_total",
			Help: "Ownership checks by outcome (clean | drift | error).",
		},
		[]string{"namespace", "logical_database", "outcome"},
	)

	// No per-database labels: realm databases would blow up cardinality.
	ownershipDriftCheckDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "keystone_ownership_drift_check_duration_seconds",
			Help:    "Wall-clock duration of one ownership catalog check.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		},
	)

	ownershipRemediations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_ownership_remediations_total",
			Help: "Per-object remediation results (reassigned | failed | skipped_*).",
		},
		[]string{"namespace", "logical_database", "kind", "outcome"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		ownershipDriftObjects,
		ownershipDriftLastCheck,
		ownershipDriftChecks,
		ownershipDriftCheckDuration,
		ownershipRemediations,
	)
}

// resetOwnershipSeries drops the per-object gauges of one LogicalDatabase so
// groups that disappeared do not linger.
func resetOwnershipSeries(namespace, name string) {
	ownershipDriftObjects.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace, "logical_database": name,
	})
}

// forgetOwnershipGauges removes the per-database gauge series (object counts
// and last-check timestamp) so a stale timestamp cannot fire
// KeystoneOwnershipCheckStale for a database that is no longer checked.
func forgetOwnershipGauges(namespace, name string) {
	resetOwnershipSeries(namespace, name)
	ownershipDriftLastCheck.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace, "logical_database": name,
	})
}

// forgetOwnershipMetrics removes every series (gauges and counters) of a
// deleted LogicalDatabase; Prometheus never expires a counter series on its
// own, so they are deleted explicitly.
func forgetOwnershipMetrics(namespace, name string) {
	forgetOwnershipGauges(namespace, name)
	l := prometheus.Labels{"namespace": namespace, "logical_database": name}
	ownershipDriftChecks.DeletePartialMatch(l)
	ownershipRemediations.DeletePartialMatch(l)
}
