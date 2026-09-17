// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics for the AuditEntryRetentionController. Registered against
// the controller-runtime global registry on package init() so the
// manager's /metrics endpoint exposes them automatically.
//
// Operator alerting playbook (PrometheusRule ships separately, in
// your GitOps repository):
//
//   - keystone_audit_archive_lag_seconds > 86400 for 1h
//     => archive backend down or misconfigured; expired entries
//        accumulating in etcd. Page on-call.
//
//   - rate(keystone_audit_archive_errors_total{reason="upload"}[5m]) > 0
//     => MinIO endpoint or bucket misconfiguration. Investigate
//        S3 credentials Secret.
//
//   - rate(keystone_audit_archive_entries_total{outcome="success"}[24h])
//     == 0 AND lag > 0
//     => silent failure; archiver thinks it's working but no entries
//        moving. Probably a race with the deleter or rate limiter.
var (
	auditArchiveEntriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_audit_archive_entries_total",
			Help: "Cumulative count of AuditEntry CRs successfully archived to off-cluster storage by outcome (success | error).",
		},
		[]string{"outcome"},
	)

	auditArchiveBytesTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "keystone_audit_archive_bytes_total",
			Help: "Cumulative bytes uploaded to the archive backend (post-gzip). Use to size MinIO bucket capacity planning.",
		},
	)

	auditArchiveLagSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "keystone_audit_archive_lag_seconds",
			Help: "Age of the oldest AuditEntry that has expired but not yet been archived, in seconds. Zero when no entries are past retention. Alert if > 86400 (24h).",
		},
	)

	auditArchiveErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_audit_archive_errors_total",
			Help: "Cumulative count of retention-pass errors by reason (load_auditlog | list | upload | delete | patch_status).",
		},
		[]string{"reason"},
	)

	auditArchivePassDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "keystone_audit_archive_pass_duration_seconds",
			Help:    "Wall-clock duration of one retention-controller pass by outcome (dormant | no_expired | archived | error).",
			Buckets: []float64{0.01, 0.1, 1, 5, 10, 30, 60, 300, 600},
		},
		[]string{"outcome"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		auditArchiveEntriesTotal,
		auditArchiveBytesTotal,
		auditArchiveLagSeconds,
		auditArchiveErrorsTotal,
		auditArchivePassDurationSeconds,
	)
}
