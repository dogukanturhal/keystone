// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ignoreStatusOnlyUpdates returns a predicate that filters Update events
// where only .status changed — i.e. spec, metadata.generation, labels,
// annotations, finalizers, deletionTimestamp are all unchanged.
//
// # Why this exists
//
// Without this filter the controller-runtime default For(...) watch
// fires on ANY change including status patches. Reconcilers that
// patch their own .status (LastDiffTime, conditions, etc.) on every
// reconcile pass create a self-sustaining hot-loop:
//
//	reconcile → status patch → watch event → workqueue add →
//	reconcile → status patch → watch event → ...
//
// At the rate-limited workqueue floor (1Hz default) this drives
// continuous reconciliation that:
//
//   - amplifies AuditEntry writes per reconcile (every reconcile
//     emits a "reconcile success" entry via audit.Logger.Append)
//   - drives the AuditLog reconciler's drift-healing loop into the
//     same hot-loop
//   - causes List(AuditEntry) to scan an ever-growing entry set
//     each pass, blowing through the operator's memory limit
//
// The 2026-05-04 OOM cascade (35 restarts in 169min on a
// 512Mi-limited pod) traced back to exactly this pattern in
// SchemaDefinitionReconciler.markReady writing
// Status.LastDiffTime = now on every reconcile.
//
// # What it filters
//
// Update events where:
//
//   - generation unchanged (spec didn't change)
//   - labels unchanged
//   - annotations unchanged
//   - finalizers unchanged
//   - deletionTimestamp unchanged
//
// Status-only updates fall through this gate. Create / Delete /
// Generic events are always passed (controllers must see CR
// lifecycle).
//
// # When NOT to use it
//
// This predicate goes on the primary For(...) watch only. Owns(...)
// watches usually DO want to fire on child status changes — e.g. a
// MigrationBundle's status update is exactly how the SchemaDefinition
// reconciler learns the bundle finished applying. Apply this predicate
// to Owns() only when the parent doesn't need child-status visibility.
func ignoreStatusOnlyUpdates() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return true },
		DeleteFunc:  func(e event.DeleteEvent) bool { return true },
		GenericFunc: func(e event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			oldM, newM := e.ObjectOld, e.ObjectNew
			if oldM.GetGeneration() != newM.GetGeneration() {
				return true
			}
			if !mapsEqualStr(oldM.GetLabels(), newM.GetLabels()) {
				return true
			}
			if !mapsEqualStr(oldM.GetAnnotations(), newM.GetAnnotations()) {
				return true
			}
			if !slicesEqualStr(oldM.GetFinalizers(), newM.GetFinalizers()) {
				return true
			}
			oldDel, newDel := oldM.GetDeletionTimestamp(), newM.GetDeletionTimestamp()
			if (oldDel == nil) != (newDel == nil) {
				return true
			}
			if oldDel != nil && newDel != nil && !oldDel.Equal(newDel) {
				return true
			}
			return false
		},
	}
}

func mapsEqualStr(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func slicesEqualStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
