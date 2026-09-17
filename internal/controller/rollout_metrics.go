// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Stage-level metrics emitted by the staged rollout reconciler. Pair
// with the burn-rate alerts shipped in your GitOps repository.
var (
	rolloutStageStarted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_rollout_stage_started_total",
			Help: "Cumulative count of rollout stages entered, partitioned by " +
				"bundle, version, policy, and stage. Increments when a stage's " +
				"first MigrationExecution is dispatched.",
		},
		[]string{"namespace", "bundle", "version", "policy", "stage"},
	)

	rolloutStageCompleted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_rollout_stage_completed_total",
			Help: "Cumulative count of rollout stages successfully completed " +
				"(soak elapsed + approval received).",
		},
		[]string{"namespace", "bundle", "version", "policy", "stage"},
	)

	rolloutStageBlocked = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "keystone_rollout_stage_blocked_total",
			Help: "Cumulative count of rollout stages frozen by an execution " +
				"failure (when AbortOnFailure=true). High value means the " +
				"rollout policy is firing as designed; use it to detect " +
				"systemic regressions.",
		},
		[]string{"namespace", "bundle", "version", "policy", "stage"},
	)

	rolloutStageDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "keystone_rollout_stage_duration_seconds",
			Help: "Wall-clock duration of a stage from StartedAt to " +
				"CompletedAt. Buckets cover 1m to 6h — typical canary stages " +
				"are minutes, paid stages can be hours.",
			Buckets: []float64{60, 300, 600, 1800, 3600, 10800, 21600},
		},
		[]string{"namespace", "bundle", "version", "policy", "stage"},
	)

	rolloutStageAwaitingApproval = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "keystone_rollout_stage_awaiting_approval",
			Help: "1 when a stage is sitting at soak-elapsed waiting for the " +
				"operator approval annotation; 0 otherwise. Page on this " +
				"being 1 longer than the SLO for human response (e.g. 4h).",
		},
		[]string{"namespace", "bundle", "version", "policy", "stage"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		rolloutStageStarted,
		rolloutStageCompleted,
		rolloutStageBlocked,
		rolloutStageDurationSeconds,
		rolloutStageAwaitingApproval,
	)
}
