// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
	"github.com/dogukanturhal/keystone/internal/conditions"
	"github.com/dogukanturhal/keystone-sdk/go/drift"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
	"github.com/dogukanturhal/keystone/internal/viz"
)

// SchemaSnapshotReconciler captures the applied-migration state of a
// DatabaseSchema at a point in time. Snapshots are immutable — once
// phase=Captured, the reconciler never mutates the status again.
//
// See ADR 0016 (pending) for the design rationale. Bottom line:
// Operator Capability Level 3 requires "create backups of the
// Operand"; for a schema-management product, the load-bearing piece
// of state is the applied-migration history, not the data itself
// (data is PG's business). SchemaSnapshot + future restore gives us
// the capability claim.
type SchemaSnapshotReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Pools    *pg.PoolCache

	SystemNamespace string

	AuditLogger     *audit.Logger
	ManagerIdentity keystonev1alpha1.AuditActor
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=schemasnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=schemasnapshots/status,verbs=get;update;patch

// Reconcile is the entry point.
func (r *SchemaSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("schemasnapshot", req.NamespacedName)

	var snap keystonev1alpha1.SchemaSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Snapshots are immutable. Once captured, never re-reconcile.
	if snap.Status.Phase == "Captured" || snap.Status.Phase == "Failed" {
		return ctrl.Result{}, nil
	}

	// Resolve DatabaseSchema → LogicalDatabase → DatabaseProvider chain.
	var schema keystonev1alpha1.DatabaseSchema
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: snap.Namespace, Name: snap.Spec.SchemaRef,
	}, &schema); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, &snap, "SchemaRefNotFound",
				fmt.Errorf("DatabaseSchema %s/%s not found", snap.Namespace, snap.Spec.SchemaRef))
		}
		return ctrl.Result{}, fmt.Errorf("get DatabaseSchema: %w", err)
	}
	if !conditions.IsTrue(schema.Status.Conditions, keystonev1alpha1.ConditionTypeReady) {
		// Wait — we don't snapshot transient schemas.
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	var ldb keystonev1alpha1.LogicalDatabase
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: schema.Namespace, Name: schema.Spec.LogicalDatabaseRef,
	}, &ldb); err != nil {
		return r.fail(ctx, &snap, "LogicalDatabaseNotFound", err)
	}
	var provider keystonev1alpha1.DatabaseProvider
	if err := r.Get(ctx, types.NamespacedName{Name: ldb.Spec.ProviderRef}, &provider); err != nil {
		return r.fail(ctx, &snap, "ProviderNotFound", err)
	}

	creds, err := r.resolveCredentials(ctx, &provider)
	if err != nil {
		return r.fail(ctx, &snap, "CredentialsResolutionFailed", err)
	}

	cfg := pg.PoolConfig{
		Host:     provider.Spec.Host,
		Port:     int(provider.Spec.Port),
		Database: ldb.Spec.Name,
		Username: creds.Username,
		Password: creds.Password,
		SSLMode:  string(provider.Spec.SSLMode),
		MaxConns: provider.Spec.PoolMaxConns,
	}
	pool, release, err := r.Pools.Acquire(ctx, cfg)
	if err != nil {
		return r.fail(ctx, &snap, "PoolAcquireFailed", err)
	}
	defer release()

	// Mark Capturing so concurrent reconciles see progress.
	if snap.Status.Phase == "" {
		if err := r.markPhase(ctx, &snap, "Capturing"); err != nil {
			return ctrl.Result{}, err
		}
	}

	// ownerRole left empty for SchemaSnapshot — this controller READs
	// the tracking table to capture state; it never CREATES new objects
	// in the target schema, so role attribution doesn't matter.
	runner, err := migration.NewRunner(pool, schema.Spec.Name, ldb.Spec.TrackingTableName, "")
	if err != nil {
		return r.fail(ctx, &snap, "RunnerInitFailed", err)
	}
	if err := runner.EnsureBookkeeping(ctx); err != nil {
		return r.fail(ctx, &snap, "BookkeepingFailed", err)
	}

	applied, err := runner.ListApplied(ctx)
	if err != nil {
		return r.fail(ctx, &snap, "ListAppliedFailed", err)
	}

	// Fingerprint the schema shape via the drift inspector.
	ins := drift.NewInspector(pool)
	snapshot, err := ins.Inspect(ctx, schema.Spec.Name)
	if err != nil {
		return r.fail(ctx, &snap, "InspectFailed", err)
	}
	fingerprint, err := drift.Hash(snapshot)
	if err != nil {
		return r.fail(ctx, &snap, "FingerprintFailed", err)
	}

	// Freeze + checksum.
	records := make([]keystonev1alpha1.AppliedMigrationRecord, 0, len(applied))
	for _, m := range applied {
		t := metav1.NewTime(m.AppliedAt)
		records = append(records, keystonev1alpha1.AppliedMigrationRecord{
			Version:     m.Version,
			ContentHash: m.ContentHash,
			AppliedAt:   &t,
		})
	}

	checksum := computeSnapshotChecksum(records, fingerprint)
	erd := viz.MermaidERD(snapshot)
	structure := structuralSnapshotFromDrift(snapshot)

	patch := client.MergeFrom(snap.DeepCopy())
	snap.Status.ObservedGeneration = snap.Generation
	snap.Status.Phase = "Captured"
	now := metav1.Now()
	snap.Status.CapturedAt = &now
	snap.Status.AppliedMigrations = records
	snap.Status.Fingerprint = fingerprint
	snap.Status.ChecksumSHA256 = checksum
	snap.Status.ERD = erd
	snap.Status.Structure = structure
	snap.Status.Conditions = conditions.MarkReady(snap.Status.Conditions,
		"Captured",
		fmt.Sprintf("captured %d applied migration(s); fingerprint=%s",
			len(records), fingerprint),
		snap.Generation)
	if err := r.Status().Patch(ctx, &snap, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}

	logger.Info("captured", "migrations", len(records), "fingerprint", fingerprint)

	if r.AuditLogger != nil {
		if _, _, err := r.AuditLogger.Append(ctx, audit.Event{
			Verb:        "create",
			Actor:       r.ManagerIdentity,
			ResourceRef: audit.ResourceRefFromObject(&snap),
			Reason: fmt.Sprintf("captured %d migration(s); fingerprint=%s; checksum=%s",
				len(records), fingerprint, checksum),
			Outcome: "success",
			After:   &snap,
		}); err != nil {
			logger.Error(err, "audit append failed",
				"controller", "schemasnapshot",
				"name", snap.Name,
				"verb", "create",
				"outcome", "success")
		}
	}
	return ctrl.Result{}, nil
}

// markPhase writes status.phase without touching other fields.
func (r *SchemaSnapshotReconciler) markPhase(
	ctx context.Context,
	snap *keystonev1alpha1.SchemaSnapshot,
	phase string,
) error {
	patch := client.MergeFrom(snap.DeepCopy())
	snap.Status.Phase = phase
	return r.Status().Patch(ctx, snap, patch)
}

// fail transitions the snapshot to phase=Failed with a structured
// Ready=False condition and emits an audit entry.
func (r *SchemaSnapshotReconciler) fail(
	ctx context.Context,
	snap *keystonev1alpha1.SchemaSnapshot,
	reason string, cause error,
) (ctrl.Result, error) {
	patch := client.MergeFrom(snap.DeepCopy())
	snap.Status.ObservedGeneration = snap.Generation
	snap.Status.Phase = "Failed"
	snap.Status.Conditions = conditions.MarkNotReady(snap.Status.Conditions, reason, cause, snap.Generation)
	if perr := r.Status().Patch(ctx, snap, patch); perr != nil {
		return ctrl.Result{}, fmt.Errorf("patch status (after %s): %w; cause: %v", reason, perr, cause)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(snap, corev1.EventTypeWarning, reason, "%s", cause.Error())
	}
	if r.AuditLogger != nil {
		if _, _, err := r.AuditLogger.Append(ctx, audit.Event{
			Verb:        "create",
			Actor:       r.ManagerIdentity,
			ResourceRef: audit.ResourceRefFromObject(snap),
			Reason:      reason + ": " + cause.Error(),
			Outcome:     "error",
		}); err != nil {
			log.FromContext(ctx).Error(err, "audit append failed",
				"controller", "schemasnapshot",
				"name", snap.Name,
				"verb", "create",
				"outcome", "error")
		}
	}
	return ctrl.Result{}, cause
}

// resolveCredentials duplicates LogicalDatabaseReconciler's helper so
// this controller stays self-contained. Kept intentionally separate
// from the shared helper in the root package so cross-controller
// refactors are narrowly scoped.
func (r *SchemaSnapshotReconciler) resolveCredentials(
	ctx context.Context,
	p *keystonev1alpha1.DatabaseProvider,
) (resolvedCreds, error) {
	ref := p.Spec.AdminCredentialsRef
	userKey := ref.UsernameKey
	if userKey == "" {
		userKey = "username"
	}
	passKey := ref.PasswordKey
	if passKey == "" {
		passKey = "password"
	}
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: r.SystemNamespace, Name: ref.SecretName,
	}, &sec); err != nil {
		return resolvedCreds{}, fmt.Errorf("read admin creds: %w", err)
	}
	u, ok := sec.Data[userKey]
	if !ok || len(u) == 0 {
		return resolvedCreds{}, fmt.Errorf("Secret missing key %q", userKey)
	}
	pw, ok := sec.Data[passKey]
	if !ok || len(pw) == 0 {
		return resolvedCreds{}, fmt.Errorf("Secret missing key %q", passKey)
	}
	return resolvedCreds{Username: string(u), Password: string(pw)}, nil
}

// SetupWithManager wires the reconciler.
func (r *SchemaSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.SystemNamespace == "" {
		r.SystemNamespace = "keystone-system"
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keystone-schemasnapshot")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.SchemaSnapshot{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Named("schemasnapshot").
		Complete(r)
}

// structuralSnapshotFromDrift translates the in-memory drift.Snapshot
// into the CRD-serialisable v1alpha1.StructuralSnapshot shape. One-
// to-one field copy — both structs use deterministic ordering, so the
// ERD, fingerprint, and structured form see exactly the same schema
// observation.
func structuralSnapshotFromDrift(s *drift.Snapshot) *keystonev1alpha1.StructuralSnapshot {
	if s == nil {
		return nil
	}
	out := &keystonev1alpha1.StructuralSnapshot{
		ModelVersion: keystonev1alpha1.StructuralSnapshotModelVersion,
		Schema:       s.Schema,
	}
	for _, t := range s.Tables {
		st := keystonev1alpha1.SnapshotTable{
			Name:           t.Name,
			Kind:           t.Kind,
			ViewDefinition: t.ViewDefinition,
			RLSEnabled:     t.RLSEnabled,
		}
		// Copied by value: the drift snapshot is reused after this
		// call, and aliasing its pointer would let a later mutation
		// reach into a captured, nominally-immutable CR.
		if t.RLSForced != nil {
			forced := *t.RLSForced
			st.RLSForced = &forced
		}
		for _, c := range t.Columns {
			st.Columns = append(st.Columns, keystonev1alpha1.SnapshotColumn{
				Name:     c.Name,
				Ordinal:  int32(c.Ordinal),
				DataType: c.DataType,
				UDTName:  c.UDTName,
				Nullable: c.Nullable,
				Default:  c.Default,
			})
		}
		out.Tables = append(out.Tables, st)
	}
	for _, ix := range s.Indexes {
		out.Indexes = append(out.Indexes, keystonev1alpha1.SnapshotObjectDDL{
			Name: ix.Name, Table: ix.Table, Type: ix.Type, Definition: ix.Definition,
		})
	}
	for _, c := range s.Constraints {
		out.Constraints = append(out.Constraints, keystonev1alpha1.SnapshotObjectDDL{
			Name: c.Name, Table: c.Table, Type: c.Type, Definition: c.Definition,
		})
	}
	for _, p := range s.Policies {
		sp := keystonev1alpha1.SnapshotPolicy{
			Name:       p.Name,
			Table:      p.Table,
			Command:    p.Command,
			Permissive: p.Permissive,
			Using:      p.Using,
			WithCheck:  p.WithCheck,
		}
		if len(p.Roles) > 0 {
			sp.Roles = append([]string(nil), p.Roles...)
		}
		out.Policies = append(out.Policies, sp)
	}
	for _, e := range s.Enums {
		se := keystonev1alpha1.SnapshotEnum{Name: e.Name}
		if len(e.Labels) > 0 {
			se.Labels = append([]string(nil), e.Labels...)
		}
		out.Enums = append(out.Enums, se)
	}
	for _, e := range s.Extensions {
		out.Extensions = append(out.Extensions, keystonev1alpha1.SnapshotExtension{
			Name: e.Name, Schema: e.Schema,
		})
	}
	for _, q := range s.Sequences {
		out.Sequences = append(out.Sequences, keystonev1alpha1.SnapshotSequence{
			Name:        q.Name,
			DataType:    q.DataType,
			IncrementBy: q.IncrementBy,
			MinValue:    q.MinValue,
			MaxValue:    q.MaxValue,
			StartValue:  q.StartValue,
		})
	}
	for _, f := range s.Functions {
		out.Functions = append(out.Functions, keystonev1alpha1.SnapshotFunction{
			Name:       f.Name,
			Args:       f.Args,
			Returns:    f.Returns,
			Language:   f.Language,
			Definition: f.Definition,
		})
	}
	for _, tr := range s.Triggers {
		st := keystonev1alpha1.SnapshotTrigger{
			Name:       tr.Name,
			Table:      tr.Table,
			Timing:     tr.Timing,
			ForEachRow: tr.ForEachRow,
			Function:   tr.Function,
			When:       tr.When,
		}
		if len(tr.Events) > 0 {
			st.Events = append([]string(nil), tr.Events...)
		}
		out.Triggers = append(out.Triggers, st)
	}
	for _, mv := range s.MaterializedViews {
		out.MaterializedViews = append(out.MaterializedViews,
			keystonev1alpha1.SnapshotMaterializedView{
				Name: mv.Name, Definition: mv.Definition,
			})
	}
	return out
}

// computeSnapshotChecksum returns a SHA-256 over a deterministic
// encoding of (version|contentHash|appliedAt) plus the fingerprint.
// Downstream tooling re-runs this to prove the snapshot wasn't
// tampered post-capture.
func computeSnapshotChecksum(records []keystonev1alpha1.AppliedMigrationRecord, fingerprint string) string {
	var b strings.Builder
	for _, r := range records {
		b.WriteString(r.Version)
		b.WriteByte('|')
		b.WriteString(r.ContentHash)
		b.WriteByte('|')
		if r.AppliedAt != nil {
			b.WriteString(r.AppliedAt.UTC().Format(time.RFC3339Nano))
		}
		b.WriteByte('\n')
	}
	b.WriteString("fingerprint=")
	b.WriteString(fingerprint)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
