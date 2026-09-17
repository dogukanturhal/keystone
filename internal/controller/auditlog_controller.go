// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
)

// AuditLogReconciler heals AuditLog.status when it drifts below the
// true max AuditEntry sequence. This is the defence-in-depth partner
// of the audit.Logger.Append 409 recovery path: Append re-scans
// max(AuditEntry.spec.sequence) on AlreadyExists, and this reconciler
// independently tracks the invariant on a 30s tick so a stuck
// AuditLog.status eventually self-corrects even if no writer is
// currently looping.
//
// Why this exists
// ===============
// The 2026-04-21 incident (150-180 etcd writes/sec for 50 minutes on
// entry-00000000000000114604) was caused by the status.nextSequence
// counter lagging behind the real max across existing AuditEntries,
// so every Append attempted to create the same doomed Name. The
// Logger fix handles the hot path; this reconciler keeps the
// invariant true even when no Append is active (operator restart,
// partition after a status-patch conflict, etc.).
type AuditLogReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Interval controls how often we re-scan even if no event fired.
	// Zero = default 30s.
	Interval time.Duration
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=auditlogs,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=auditlogs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=auditentries,verbs=get;list;watch;create
// ^ create is required by audit.Logger.Append (called from
// MigrationBundle / MigrationExecution / SchemaSnapshot reconcilers).
// AuditLogReconciler itself only reads — but controller-gen unions
// verbs across markers cluster-wide, so the create permission lives
// here as the canonical audit-subsystem RBAC declaration. AuditEntry
// CRs are append-only by design (no update/patch/delete in the chain
// invariants), so create is the only mutating verb needed.

func (r *AuditLogReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("auditlog", req.Name)

	alog := &keystonev1alpha1.AuditLog{}
	if err := r.Get(ctx, types.NamespacedName{Name: keystonev1alpha1.AuditLogName}, alog); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: r.interval()}, nil
		}
		return ctrl.Result{}, err
	}

	entries := &keystonev1alpha1.AuditEntryList{}
	if err := r.List(ctx, entries); err != nil {
		return ctrl.Result{}, err
	}

	var maxSeq int64
	var latestHash string
	var latestTime = alog.Status.LastEntryTime
	for i := range entries.Items {
		e := &entries.Items[i]
		if e.Spec.Sequence > maxSeq {
			maxSeq = e.Spec.Sequence
			latestHash = e.Spec.SelfHash
			t := e.Spec.Timestamp
			latestTime = &t
		}
	}

	expectedNext := maxSeq + 1
	if alog.Status.NextSequence >= expectedNext && alog.Status.LastSequence >= maxSeq {
		return ctrl.Result{RequeueAfter: r.interval()}, nil
	}

	logger.Info("healing AuditLog.status drift",
		"from.nextSequence", alog.Status.NextSequence,
		"to.nextSequence", expectedNext,
		"from.lastSequence", alog.Status.LastSequence,
		"to.lastSequence", maxSeq)

	updated := alog.DeepCopy()
	updated.Status.NextSequence = expectedNext
	updated.Status.LastSequence = maxSeq
	if latestHash != "" {
		updated.Status.LastEntryHash = latestHash
	}
	updated.Status.LastEntryTime = latestTime
	if err := r.Status().Patch(ctx, updated, client.MergeFrom(alog)); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	audit.RecordDriftHealed()
	return ctrl.Result{RequeueAfter: r.interval()}, nil
}

func (r *AuditLogReconciler) interval() time.Duration {
	if r.Interval <= 0 {
		return 30 * time.Second
	}
	return r.Interval
}

func (r *AuditLogReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watches AuditLog only — the periodic RequeueAfter tick covers
	// AuditEntry-level drift without firing on every entry create
	// (which would reintroduce the exact write-amplification pattern
	// this controller is meant to dampen).
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.AuditLog{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Complete(r)
}
