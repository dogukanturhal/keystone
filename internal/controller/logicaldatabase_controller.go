// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
	"github.com/dogukanturhal/keystone/internal/conditions"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
	"github.com/dogukanturhal/keystone/internal/tracing"
)

// LogicalDatabaseReconciler reconciles LogicalDatabase resources.
//
// Each reconcile is fully idempotent: it ensures the role exists, the
// database exists with the requested attributes, and every requested
// extension is installed. Failures set Ready=False with a structured
// reason and are retried by controller-runtime's exponential backoff.
//
// The controller never reads or writes status outside the patch helper —
// status mutations always carry observedGeneration so consumers can tell
// stale state from current.
type LogicalDatabaseReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Pools    *pg.PoolCache

	// AuditLogger appends tamper-evident entries to the audit chain on
	// every success/fail transition. nil = no-op (test flows).
	AuditLogger *audit.Logger

	// ManagerIdentity is the Actor stamped on reconciler-sourced AuditEntries.
	ManagerIdentity keystonev1alpha1.AuditActor

	// SystemNamespace is the namespace where DatabaseProvider admin Secrets
	// live. Defaults to "keystone-system" — pinned in main.go from a flag.
	SystemNamespace string
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=logicaldatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=logicaldatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=logicaldatabases/finalizers,verbs=update
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=databaseproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the entry point for the LogicalDatabase controller.
func (r *LogicalDatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, span := tracing.Tracer().Start(ctx, "LogicalDatabaseReconciler.Reconcile",
		trace.WithAttributes(
			attribute.String("namespace", req.Namespace),
			attribute.String("name", req.Name),
		))
	defer span.End()

	logger := log.FromContext(ctx).WithValues("logicaldatabase", req.NamespacedName)

	var ldb keystonev1alpha1.LogicalDatabase
	if err := r.Get(ctx, req.NamespacedName, &ldb); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get LogicalDatabase: %w", err)
	}

	// Deletion path takes priority over normal reconcile.
	if !ldb.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ldb)
	}

	// Ensure finalizer is present before touching external state. If we
	// crash after creating the DB but before adding the finalizer,
	// deleting the CR would orphan the database.
	if controllerutil.AddFinalizer(&ldb, keystonev1alpha1.FinalizerLogicalDatabase) {
		if err := r.Update(ctx, &ldb); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// Re-queue immediately; the next reconcile picks up with the
		// finalizer in place.
		return ctrl.Result{Requeue: true}, nil
	}

	// Resolve the provider.
	var provider keystonev1alpha1.DatabaseProvider
	if err := r.Get(ctx, types.NamespacedName{Name: ldb.Spec.ProviderRef}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			r.Recorder.Eventf(&ldb, corev1.EventTypeWarning, "ProviderNotFound",
				"DatabaseProvider %q does not exist", ldb.Spec.ProviderRef)
			return r.fail(ctx, &ldb, "ProviderNotFound",
				fmt.Errorf("DatabaseProvider %q not found", ldb.Spec.ProviderRef))
		}
		return ctrl.Result{}, fmt.Errorf("get DatabaseProvider: %w", err)
	}

	// Resolve admin credentials.
	creds, err := r.resolveCredentials(ctx, &provider)
	if err != nil {
		r.Recorder.Eventf(&ldb, corev1.EventTypeWarning, "CredentialsResolutionFailed", err.Error())
		return r.fail(ctx, &ldb, "CredentialsResolutionFailed", err)
	}

	// Ensure DB + role at the maintenance database.
	maintCfg := pg.PoolConfig{
		Host:     provider.Spec.Host,
		Port:     int(provider.Spec.Port),
		Database: provider.Spec.MaintenanceDatabase,
		Username: creds.Username,
		Password: creds.Password,
		SSLMode:  string(provider.Spec.SSLMode),
		MaxConns: provider.Spec.PoolMaxConns,
	}
	maintPool, releaseMaint, err := r.Pools.Acquire(ctx, maintCfg)
	if err != nil {
		return r.fail(ctx, &ldb, "PoolAcquireFailed", err)
	}
	defer releaseMaint()

	maintAdmin := pg.NewAdmin(maintPool)
	if err := maintAdmin.Ping(ctx); err != nil {
		return r.fail(ctx, &ldb, "ProviderUnreachable", err)
	}

	// Resolve the OwnerRole password from spec.ownerRolePasswordSecretRef
	// (when set). Empty when unset → EnsureRole will create the role with
	// no password (legacy / out-of-band-password path).
	ownerPassword, err := r.resolveOwnerRolePassword(ctx, &ldb)
	if err != nil {
		return r.fail(ctx, &ldb, "OwnerPasswordResolutionFailed", err)
	}

	if err := maintAdmin.EnsureRole(ctx, ldb.Spec.OwnerRole, ownerPassword); err != nil {
		return r.fail(ctx, &ldb, "RoleEnsureFailed", err)
	}

	// Password rotation. EnsureRole only sets the password on first
	// CREATE — to rotate without clobbering a Vault-rotated password
	// the operator never observed, we explicitly compute the desired
	// password's hash, compare it against status.observedPasswordHash
	// (the hash of the password the operator most recently applied),
	// and only ALTER ROLE PASSWORD when they differ.
	//
	// First reconcile after enabling spec.ownerRolePasswordSecretRef on
	// an existing role: ObservedPasswordHash is empty and ownerPassword
	// is non-empty → ALTER ROLE PASSWORD runs once, hash recorded, no
	// subsequent ALTERs until the Secret content changes.
	desiredHash := hashPasswordSHA256(ownerPassword)
	if ownerPassword != "" && desiredHash != ldb.Status.ObservedPasswordHash {
		if err := maintAdmin.SetRolePassword(ctx, ldb.Spec.OwnerRole, ownerPassword); err != nil {
			return r.fail(ctx, &ldb, "RolePasswordRotateFailed", err)
		}
		patch := client.MergeFrom(ldb.DeepCopy())
		ldb.Status.ObservedPasswordHash = desiredHash
		if perr := r.Status().Patch(ctx, &ldb, patch); perr != nil {
			return ctrl.Result{}, fmt.Errorf("patch observedPasswordHash: %w", perr)
		}
		r.emitAudit(ctx, &ldb, "rotate-password",
			fmt.Sprintf("owner role %s password rotated to hash %s", ldb.Spec.OwnerRole, desiredHash[:16]),
			"success")
	}

	dbSpec := pg.DatabaseSpec{
		Name:            ldb.Spec.Name,
		Owner:           ldb.Spec.OwnerRole,
		Encoding:        ldb.Spec.Encoding,
		Collation:       ldb.Spec.Collation,
		Ctype:           ldb.Spec.Ctype,
		ConnectionLimit: ldb.Spec.ConnectionLimit,
	}
	if err := maintAdmin.EnsureDatabase(ctx, dbSpec); err != nil {
		return r.fail(ctx, &ldb, "DatabaseEnsureFailed", err)
	}

	// Ensure extensions inside the target database.
	if len(ldb.Spec.Extensions) > 0 {
		targetCfg := maintCfg
		targetCfg.Database = ldb.Spec.Name
		targetPool, releaseTarget, err := r.Pools.Acquire(ctx, targetCfg)
		if err != nil {
			return r.fail(ctx, &ldb, "TargetPoolAcquireFailed", err)
		}
		defer releaseTarget()
		targetAdmin := pg.NewAdmin(targetPool)
		for _, ext := range ldb.Spec.Extensions {
			if err := targetAdmin.EnsureExtension(ctx, ext); err != nil {
				return r.fail(ctx, &ldb, "ExtensionEnsureFailed", err)
			}
		}
	}

	// Status: success.
	return r.success(ctx, &ldb, &provider, logger)
}

func (r *LogicalDatabaseReconciler) reconcileDelete(ctx context.Context, ldb *keystonev1alpha1.LogicalDatabase) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ldb, keystonev1alpha1.FinalizerLogicalDatabase) {
		return ctrl.Result{}, nil
	}

	if ldb.Spec.DeletionPolicy == keystonev1alpha1.DeletionPolicyDelete {
		// Resolve provider again — may have been deleted already; tolerate that.
		var provider keystonev1alpha1.DatabaseProvider
		err := r.Get(ctx, types.NamespacedName{Name: ldb.Spec.ProviderRef}, &provider)
		switch {
		case apierrors.IsNotFound(err):
			// Provider already gone; treat as cleaned.
		case err != nil:
			return ctrl.Result{}, fmt.Errorf("get provider for delete: %w", err)
		default:
			creds, cerr := r.resolveCredentials(ctx, &provider)
			if cerr != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, cerr
			}
			cfg := pg.PoolConfig{
				Host: provider.Spec.Host, Port: int(provider.Spec.Port),
				Database: provider.Spec.MaintenanceDatabase,
				Username: creds.Username, Password: creds.Password,
				SSLMode:  string(provider.Spec.SSLMode),
				MaxConns: provider.Spec.PoolMaxConns,
			}
			pool, release, perr := r.Pools.Acquire(ctx, cfg)
			if perr != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, perr
			}
			defer release()
			if derr := pg.NewAdmin(pool).DropDatabase(ctx, ldb.Spec.Name); derr != nil {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, derr
			}
		}
	}

	controllerutil.RemoveFinalizer(ldb, keystonev1alpha1.FinalizerLogicalDatabase)
	if err := r.Update(ctx, ldb); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// resolvedCreds is the local materialisation of (username, password)
// from the provider's referenced Secret.
type resolvedCreds struct {
	Username string
	Password string
}

// resolveOwnerRolePassword reads the password to set on the OwnerRole
// from spec.ownerRolePasswordSecretRef. Returns "" when the field is
// unset (caller treats this as "no password / out-of-band managed").
//
// The Secret MUST exist in the SAME namespace as the LogicalDatabase
// resource. Cross-namespace references are intentionally disallowed —
// the LogicalDatabase namespace gates who can change the password.
//
// Empty values at the referenced key are treated as an error (rather
// than silently rolling back to the no-password branch) — an empty
// Secret value is almost always a misconfiguration / propagation race
// from an ExternalSecret that hasn't synced yet.
func (r *LogicalDatabaseReconciler) resolveOwnerRolePassword(
	ctx context.Context,
	ldb *keystonev1alpha1.LogicalDatabase,
) (string, error) {
	ref := ldb.Spec.OwnerRolePasswordSecretRef
	if ref == nil || ref.Name == "" {
		return "", nil
	}
	key := ref.Key
	if key == "" {
		key = "password"
	}
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: ldb.Namespace,
		Name:      ref.Name,
	}, &sec); err != nil {
		return "", fmt.Errorf(
			"read owner-role password Secret %s/%s: %w",
			ldb.Namespace, ref.Name, err,
		)
	}
	pw, ok := sec.Data[key]
	if !ok {
		return "", fmt.Errorf(
			"Secret %s/%s missing key %q (referenced by spec.ownerRolePasswordSecretRef)",
			ldb.Namespace, ref.Name, key,
		)
	}
	if len(pw) == 0 {
		return "", fmt.Errorf(
			"Secret %s/%s key %q is empty (likely an ExternalSecret that hasn't synced yet, or a misconfigured source)",
			ldb.Namespace, ref.Name, key,
		)
	}
	return string(pw), nil
}

// hashPasswordSHA256 returns hex(sha256(password)) — the format stored
// in LogicalDatabase.Status.ObservedPasswordHash. Storing the hash
// rather than the plaintext means a leaked status can't reveal the
// password itself. Empty input → empty output (signals "no password
// applied").
func hashPasswordSHA256(password string) string {
	if password == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

func (r *LogicalDatabaseReconciler) resolveCredentials(ctx context.Context, p *keystonev1alpha1.DatabaseProvider) (resolvedCreds, error) {
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
		return resolvedCreds{}, fmt.Errorf(
			"read admin credentials Secret %s/%s: %w",
			r.SystemNamespace, ref.SecretName, err,
		)
	}
	u, ok := sec.Data[userKey]
	if !ok || len(u) == 0 {
		return resolvedCreds{}, fmt.Errorf("Secret %s/%s missing key %q",
			r.SystemNamespace, ref.SecretName, userKey)
	}
	pw, ok := sec.Data[passKey]
	if !ok || len(pw) == 0 {
		return resolvedCreds{}, fmt.Errorf("Secret %s/%s missing key %q",
			r.SystemNamespace, ref.SecretName, passKey)
	}
	return resolvedCreds{Username: string(u), Password: string(pw)}, nil
}

func (r *LogicalDatabaseReconciler) success(
	ctx context.Context,
	ldb *keystonev1alpha1.LogicalDatabase,
	provider *keystonev1alpha1.DatabaseProvider,
	logger logr.Logger,
) (ctrl.Result, error) {
	patch := client.MergeFrom(ldb.DeepCopy())
	ldb.Status.ObservedGeneration = ldb.Generation
	ldb.Status.ResolvedEndpoint = fmt.Sprintf("%s:%d", provider.Spec.Host, provider.Spec.Port)
	now := metav1.Now()
	ldb.Status.LastReconcileTime = &now
	ldb.Status.Conditions = conditions.MarkReady(ldb.Status.Conditions,
		"Reconciled", "database and role exist with desired configuration", ldb.Generation)
	ldb.Status.Conditions = conditions.Set(ldb.Status.Conditions,
		keystonev1alpha1.ConditionTypeAvailable, metav1.ConditionTrue,
		"Reconciled", "database accepts connections", ldb.Generation)
	ldb.Status.Conditions = conditions.ClearProgressing(ldb.Status.Conditions, ldb.Generation)
	if err := r.Status().Patch(ctx, ldb, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}
	logger.V(1).Info("reconciled", "endpoint", ldb.Status.ResolvedEndpoint)
	r.emitAudit(ctx, ldb, "reconcile",
		fmt.Sprintf("database %s endpoint %s", ldb.Spec.Name, ldb.Status.ResolvedEndpoint),
		"success")
	// Re-reconcile every 5 minutes to refresh status (size, drift signal).
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// emitAudit appends a tamper-evident entry to the audit chain. Errors
// are logged at warn level rather than discarded. Missing logger is a
// no-op (test flows don't plumb the audit chain).
func (r *LogicalDatabaseReconciler) emitAudit(
	ctx context.Context,
	ldb *keystonev1alpha1.LogicalDatabase,
	verb, reason, outcome string,
) {
	if r.AuditLogger == nil {
		return
	}
	if _, _, err := r.AuditLogger.Append(ctx, audit.Event{
		Verb:        verb,
		Actor:       r.ManagerIdentity,
		ResourceRef: audit.ResourceRefFromObject(ldb),
		Reason:      reason,
		Outcome:     outcome,
		After:       ldb,
	}); err != nil {
		log.FromContext(ctx).Error(err, "audit append failed",
			"controller", "logicaldatabase",
			"name", ldb.Name,
			"verb", verb,
			"outcome", outcome)
	}
}

func (r *LogicalDatabaseReconciler) fail(
	ctx context.Context,
	ldb *keystonev1alpha1.LogicalDatabase,
	reason string,
	cause error,
) (ctrl.Result, error) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.RecordError(cause)
		span.SetStatus(codes.Error, reason)
		span.SetAttributes(attribute.String("reason", reason))
	}
	patch := client.MergeFrom(ldb.DeepCopy())
	ldb.Status.ObservedGeneration = ldb.Generation
	ldb.Status.Conditions = conditions.MarkNotReady(ldb.Status.Conditions, reason, cause, ldb.Generation)
	if perr := r.Status().Patch(ctx, ldb, patch); perr != nil {
		// Surfacing the patch error masks the underlying cause; fan it
		// into a wrapped multi-error.
		return ctrl.Result{}, fmt.Errorf("patch status (after %s): %w; underlying: %v", reason, perr, cause)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(ldb, corev1.EventTypeWarning, reason, "%s", cause.Error())
	}
	r.emitAudit(ctx, ldb, "reconcile", reason+": "+cause.Error(), "error")
	// Return the underlying error so controller-runtime requeues with backoff.
	return ctrl.Result{}, cause
}

// SetupWithManager wires the reconciler with the manager and sets sane
// defaults: rate-limited workqueue, owns nothing (LogicalDatabase has no
// child resources at this layer).
//
// The DatabaseProvider watch is load-bearing: every reconcile dials
// the endpoint read from the referenced provider's spec, so a
// provider spec change (host/port repoint, sslMode change) must
// enqueue the dependent LogicalDatabases immediately. Without it, a
// LogicalDatabase failing against a stale endpoint converges only via
// error-backoff — which controller-runtime grows toward ~16 minutes —
// and looks exactly like the reconciler "caching" the old host.
// GenerationChangedPredicate keeps the provider controller's own
// status patches (credential observations, every reconcile) from
// fanning out — only spec edits bump metadata.generation.
func (r *LogicalDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.SystemNamespace == "" {
		r.SystemNamespace = "keystone-system"
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keystone-logicaldatabase")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.LogicalDatabase{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Watches(&keystonev1alpha1.DatabaseProvider{},
			handler.EnqueueRequestsFromMapFunc(r.logicalDatabasesForProvider),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("logicaldatabase").
		Complete(r)
}

// logicalDatabasesForProvider maps a DatabaseProvider event to every
// LogicalDatabase referencing it via spec.providerRef. In-code filter
// over a List, matching the established providersForSecret idiom —
// LogicalDatabase cardinality is low (tens, not thousands) so a field
// index would be premature.
func (r *LogicalDatabaseReconciler) logicalDatabasesForProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	var ldbs keystonev1alpha1.LogicalDatabaseList
	if err := r.List(ctx, &ldbs); err != nil {
		log.FromContext(ctx).Error(err,
			"list LogicalDatabases for DatabaseProvider fanout",
			"provider", obj.GetName())
		return nil
	}
	var out []reconcile.Request
	for i := range ldbs.Items {
		ldb := &ldbs.Items[i]
		if ldb.Spec.ProviderRef == obj.GetName() {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: ldb.Namespace, Name: ldb.Name,
			}})
		}
	}
	return out
}
