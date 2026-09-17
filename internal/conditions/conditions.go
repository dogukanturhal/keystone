// SPDX-License-Identifier: AGPL-3.0-or-later

// Package conditions wraps apimachinery's meta.SetStatusCondition with
// Keystone-specific defaults: every condition update sets ObservedGeneration
// from the resource's metadata.generation, so dependents (Argo CD, kubectl
// wait, OpenTelemetry collectors) can tell whether the controller has seen
// the latest spec.
//
// The package is deliberately tiny — Keystone follows the metav1.Condition
// convention exactly. Anything more elaborate is a smell.
package conditions

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// Set updates the named condition on the given slice. Returns the new
// slice (the apimachinery helper mutates the underlying array but returns
// nothing, so callers must rebind explicitly).
//
// The caller is responsible for assigning the result back to the resource's
// Status.Conditions field. ObservedGeneration is the resource's
// metadata.generation at the time of the update, so a stale Reconcile
// reading an old status doesn't accidentally roll the LastTransitionTime
// back.
func Set(
	existing []metav1.Condition,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
	observedGen int64,
) []metav1.Condition {
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            truncate(message, 32_768),
		ObservedGeneration: observedGen,
		LastTransitionTime: metav1.Time{Time: time.Now().UTC()},
	}
	meta.SetStatusCondition(&existing, cond)
	return existing
}

// MarkReady is the canonical "everything reconciled successfully" helper.
// Idempotent — repeated calls do not bump LastTransitionTime if the
// condition is already True with the same reason.
func MarkReady(existing []metav1.Condition, reason, message string, observedGen int64) []metav1.Condition {
	return Set(existing, "Ready", metav1.ConditionTrue, reason, message, observedGen)
}

// MarkNotReady marks Ready=False with a structured reason. Use a single
// CamelCase token for reason; long context goes in message.
func MarkNotReady(existing []metav1.Condition, reason string, err error, observedGen int64) []metav1.Condition {
	msg := "reconciliation failed"
	if err != nil {
		msg = err.Error()
	}
	return Set(existing, "Ready", metav1.ConditionFalse, reason, msg, observedGen)
}

// MarkProgressing reports the resource is mid-flight. Set Available
// separately if the resource is *currently* serving traffic — Progressing
// and Available are orthogonal (a database can be Available=True and
// Progressing=True simultaneously while a non-blocking migration runs).
func MarkProgressing(existing []metav1.Condition, reason, message string, observedGen int64) []metav1.Condition {
	return Set(existing, "Progressing", metav1.ConditionTrue, reason, message, observedGen)
}

// ClearProgressing flips Progressing to False. Call this from the success
// path so dashboards know the long-running operation has settled.
func ClearProgressing(existing []metav1.Condition, observedGen int64) []metav1.Condition {
	return Set(existing, "Progressing", metav1.ConditionFalse, "Reconciled",
		"resource is at desired state", observedGen)
}

// Get returns the named condition or nil if absent.
func Get(existing []metav1.Condition, condType string) *metav1.Condition {
	return meta.FindStatusCondition(existing, condType)
}

// IsTrue reports whether the named condition exists and is True.
func IsTrue(existing []metav1.Condition, condType string) bool {
	c := Get(existing, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s… [truncated %d bytes]", s[:n-32], len(s)-n)
}
