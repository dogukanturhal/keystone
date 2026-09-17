// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Drift metrics surfaced by the DriftController. Registered against the
// controller-runtime global metrics registry on init() so the manager's
// /metrics endpoint exposes them automatically.
var (
	driftDetectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_drift_detected_total",
			Help: "Cumulative count of drift detections, partitioned by schema. " +
				"A counter — increments on every reconcile that observes drift, " +
				"so rate() over a window approximates 'how often this schema is " +
				"drifting'.",
		},
		[]string{"namespace", "logical_database", "schema"},
	)

	driftCheckTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_drift_checks_total",
			Help: "Cumulative count of drift inspection runs, partitioned by " +
				"schema and outcome (clean | drift | error). Use to compute " +
				"check coverage and inspection error rates.",
		},
		[]string{"namespace", "logical_database", "schema", "outcome"},
	)

	driftCheckDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "keystone_drift_check_duration_seconds",
			Help: "Wall-clock duration of a single drift inspection. " +
				"Helps tune the periodic interval — if p99 approaches the " +
				"interval, queue contention is imminent.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		},
		[]string{"namespace", "logical_database", "schema"},
	)

	driftStalenessSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "keystone_drift_staleness_seconds",
			Help: "Seconds since the last successful drift inspection per " +
				"schema. Alerts on staleness > 2× the configured interval " +
				"catch a stuck reconciler before users notice.",
		},
		[]string{"namespace", "logical_database", "schema"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		driftDetectedTotal,
		driftCheckTotal,
		driftCheckDurationSeconds,
		driftStalenessSeconds,
	)
}
