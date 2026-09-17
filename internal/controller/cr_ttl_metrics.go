// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics for the CRTTLController. Registered against the
// controller-runtime global registry on package init() so the manager's
// /metrics endpoint exposes them automatically.
//
// Operator alerting playbook (PrometheusRule ships separately, in
// your GitOps repository):
//
//   - rate(keystone_cr_ttl_deleted_total[1h]) == 0 AND
//     keystone_cr_ttl_candidates > 0
//     => GC stuck; investigate. Probably a permission / owner-ref bug.
//
//   - keystone_cr_ttl_candidates{kind="MigrationBundle"} > 1000
//     => fan-out spew; either retainSuccessFor is too long for this
//        cluster's reconcile cadence or the controller is back-pressured.
//
//   - rate(keystone_cr_ttl_errors_total[5m]) > 0
//     => API errors during list / delete; check apiserver health and
//        operator RBAC.
var (
	crTTLDeletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_cr_ttl_deleted_total",
			Help: "Cumulative count of resources garbage-collected by the CR-TTL controller, labelled by kind (MigrationBundle | MigrationExecution | ConfigMap) and the terminal phase the parent CR held when it was reaped (Succeeded | Failed | Aborted | RolledBack | RollbackFailed). ConfigMap rows account for declarative-diff bundle-source CMs that cascade with their owning MigrationBundle.",
		},
		[]string{"kind", "phase"},
	)

	crTTLCandidatesGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "keystone_cr_ttl_candidates",
			Help: "Count of CRs in a terminal phase past their retention window observed in the last pass, labelled by kind. Should trend toward zero on a healthy cluster; non-zero values mean the controller is behind on its work.",
		},
		[]string{"kind"},
	)

	crTTLErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_cr_ttl_errors_total",
			Help: "Cumulative count of CR-TTL controller errors by reason (list_sd | list_bundles | list_executions | delete_bundle | delete_execution | delete_configmap).",
		},
		[]string{"reason"},
	)

	crTTLPassDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "keystone_cr_ttl_pass_duration_seconds",
			Help:    "Wall-clock duration of one CR-TTL controller pass by outcome (disabled | no_candidates | swept | error).",
			Buckets: []float64{0.01, 0.1, 1, 5, 10, 30, 60, 300, 600},
		},
		[]string{"outcome"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		crTTLDeletedTotal,
		crTTLCandidatesGauge,
		crTTLErrorsTotal,
		crTTLPassDurationSeconds,
	)
}
