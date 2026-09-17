// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
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

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
	"github.com/dogukanturhal/keystone/internal/conditions"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// DatabaseSchemaReconciler reconciles DatabaseSchema resources.
//
// Depends on the LogicalDatabase being Ready first — otherwise waits and
// requeues. Each reconcile:
//   1. resolves LogicalDatabase + DatabaseProvider via cross-CR refs,
//   2. dials the target database (NOT maintenance) with admin creds,
//   3. ensures the owner role + schema + grants + default privileges.
//
// Idempotent throughout. Failures set Ready=False with a structured
// reason; controller-runtime backs off and retries.
type DatabaseSchemaReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Pools    *pg.PoolCache

	// AuditLogger appends tamper-evident entries on success/fail.
	AuditLogger     *audit.Logger
	ManagerIdentity keystonev1alpha1.AuditActor

	SystemNamespace string
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=databaseschemas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=databaseschemas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=databaseschemas/finalizers,verbs=update
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=logicaldatabases,verbs=get;list;watch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=databaseproviders,verbs=get;list;watch

// Reconcile is the entry point.
func (r *DatabaseSchemaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("databaseschema", req.NamespacedName)

	var schema keystonev1alpha1.DatabaseSchema
	if err := r.Get(ctx, req.NamespacedName, &schema); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get DatabaseSchema: %w", err)
	}

	if !schema.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &schema)
	}

	if controllerutil.AddFinalizer(&schema, keystonev1alpha1.FinalizerDatabaseSchema) {
		if err := r.Update(ctx, &schema); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Ensure keystone.hexxlock.io/name label matches the CR's name. The
	// SchemaDefinitionReconciler emits MigrationBundles whose
	// schemaSelector hard-codes this label; a DatabaseSchema authored
	// without it would never match any bundle and no MigrationExecution
	// would ever fire. Self-healing the label here means operators can
	// create DSs with the minimal spec and the bundle pipeline still
	// works. Tracked from an incident where a large batch of SDs sat in
	// "Progressing" with MATCHED=0 because every DS lacked this label.
	if schema.Labels == nil {
		schema.Labels = map[string]string{}
	}
	if schema.Labels["keystone.hexxlock.io/name"] != schema.Name {
		patch := client.MergeFrom(schema.DeepCopy())
		schema.Labels["keystone.hexxlock.io/name"] = schema.Name
		if err := r.Patch(ctx, &schema, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("label keystone.hexxlock.io/name: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Resolve the parent LogicalDatabase (same namespace).
	var ldb keystonev1alpha1.LogicalDatabase
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: schema.Namespace, Name: schema.Spec.LogicalDatabaseRef,
	}, &ldb); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, &schema, "LogicalDatabaseNotFound",
				fmt.Errorf("LogicalDatabase %s/%s not found",
					schema.Namespace, schema.Spec.LogicalDatabaseRef))
		}
		return ctrl.Result{}, fmt.Errorf("get LogicalDatabase: %w", err)
	}

	// Wait for the parent to be Ready.
	if !conditions.IsTrue(ldb.Status.Conditions, keystonev1alpha1.ConditionTypeReady) {
		// Surface a Progressing condition; don't mark Ready=False because
		// this isn't a failure, just a wait.
		patch := client.MergeFrom(schema.DeepCopy())
		schema.Status.Conditions = conditions.MarkProgressing(schema.Status.Conditions,
			"AwaitingLogicalDatabase",
			fmt.Sprintf("waiting for LogicalDatabase %q to become Ready", ldb.Name),
			schema.Generation)
		_ = r.Status().Patch(ctx, &schema, patch)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Resolve provider + credentials.
	var provider keystonev1alpha1.DatabaseProvider
	if err := r.Get(ctx, types.NamespacedName{Name: ldb.Spec.ProviderRef}, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, &schema, "ProviderNotFound",
				fmt.Errorf("DatabaseProvider %q not found", ldb.Spec.ProviderRef))
		}
		return ctrl.Result{}, fmt.Errorf("get provider: %w", err)
	}
	creds, err := r.resolveCredentials(ctx, &provider)
	if err != nil {
		return r.fail(ctx, &schema, "CredentialsResolutionFailed", err)
	}

	// Open a pool against the TARGET database.
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
		return r.fail(ctx, &schema, "PoolAcquireFailed", err)
	}
	defer release()

	admin := pg.NewAdmin(pool)
	if err := admin.Ping(ctx); err != nil {
		return r.fail(ctx, &schema, "TargetUnreachable", err)
	}
	// DatabaseSchema-level role ensures don't manage password — the
	// password lifecycle for the OwnerRole sits with the LogicalDatabase
	// reconciler (which owns spec.ownerRolePasswordSecretRef and rotation).
	// Here the schema reconciler just guarantees the role exists so it
	// can own the schema; if the role is created here for the first time
	// it gets no password (out-of-band-managed).
	if err := admin.EnsureRole(ctx, schema.Spec.OwnerRole, ""); err != nil {
		return r.fail(ctx, &schema, "RoleEnsureFailed", err)
	}

	// Translate API-level default privileges into the postgres-package shape.
	defaults := make([]pg.DefaultPriv, 0, len(schema.Spec.DefaultPrivileges))
	for _, dp := range schema.Spec.DefaultPrivileges {
		defaults = append(defaults, pg.DefaultPriv{
			Role:       dp.Role,
			ObjectType: dp.ObjectType,
			Privileges: dp.Privileges,
		})
	}

	if err := admin.EnsureSchema(ctx, pg.SchemaSpec{
		Name:            schema.Spec.Name,
		Owner:           schema.Spec.OwnerRole,
		SearchPathHints: schema.Spec.SearchPathHints,
		DefaultPrivs:    defaults,
	}); err != nil {
		return r.fail(ctx, &schema, "SchemaEnsureFailed", err)
	}

	return r.success(ctx, &schema, logger)
}

func (r *DatabaseSchemaReconciler) reconcileDelete(ctx context.Context, schema *keystonev1alpha1.DatabaseSchema) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(schema, keystonev1alpha1.FinalizerDatabaseSchema) {
		return ctrl.Result{}, nil
	}

	if schema.Spec.DeletionPolicy == keystonev1alpha1.DeletionPolicyDelete {
		// Best-effort drop. If the LogicalDatabase or provider is already
		// gone, there's nothing to clean up — proceed to remove finalizer.
		var ldb keystonev1alpha1.LogicalDatabase
		err := r.Get(ctx, types.NamespacedName{
			Namespace: schema.Namespace, Name: schema.Spec.LogicalDatabaseRef,
		}, &ldb)
		if err == nil {
			var provider keystonev1alpha1.DatabaseProvider
			if perr := r.Get(ctx, types.NamespacedName{Name: ldb.Spec.ProviderRef}, &provider); perr == nil {
				creds, cerr := r.resolveCredentials(ctx, &provider)
				if cerr == nil {
					cfg := pg.PoolConfig{
						Host: provider.Spec.Host, Port: int(provider.Spec.Port),
						Database: ldb.Spec.Name,
						Username: creds.Username, Password: creds.Password,
						SSLMode:  string(provider.Spec.SSLMode),
						MaxConns: provider.Spec.PoolMaxConns,
					}
					pool, release, perr := r.Pools.Acquire(ctx, cfg)
					if perr == nil {
						defer release()
						if derr := pg.NewAdmin(pool).DropSchema(ctx, schema.Spec.Name); derr != nil {
							return ctrl.Result{RequeueAfter: 30 * time.Second}, derr
						}
					}
				}
			}
		}
	}

	controllerutil.RemoveFinalizer(schema, keystonev1alpha1.FinalizerDatabaseSchema)
	if err := r.Update(ctx, schema); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *DatabaseSchemaReconciler) resolveCredentials(ctx context.Context, p *keystonev1alpha1.DatabaseProvider) (resolvedCreds, error) {
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

func (r *DatabaseSchemaReconciler) success(
	ctx context.Context,
	schema *keystonev1alpha1.DatabaseSchema,
	logger logr.Logger,
) (ctrl.Result, error) {
	patch := client.MergeFrom(schema.DeepCopy())
	schema.Status.ObservedGeneration = schema.Generation
	now := metav1.Now()
	schema.Status.LastReconcileTime = &now
	schema.Status.Conditions = conditions.MarkReady(schema.Status.Conditions,
		"Reconciled", "schema and grants exist with desired configuration", schema.Generation)
	schema.Status.Conditions = conditions.Set(schema.Status.Conditions,
		keystonev1alpha1.ConditionTypeAvailable, metav1.ConditionTrue,
		"Reconciled", "schema is queryable", schema.Generation)
	schema.Status.Conditions = conditions.ClearProgressing(schema.Status.Conditions, schema.Generation)
	if err := r.Status().Patch(ctx, schema, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}
	logger.V(1).Info("reconciled")
	r.emitAudit(ctx, schema, "reconcile",
		fmt.Sprintf("schema %s reconciled", schema.Spec.Name), "success")
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// emitAudit appends a tamper-evident entry to the audit chain.
func (r *DatabaseSchemaReconciler) emitAudit(
	ctx context.Context,
	schema *keystonev1alpha1.DatabaseSchema,
	verb, reason, outcome string,
) {
	if r.AuditLogger == nil {
		return
	}
	if _, _, err := r.AuditLogger.Append(ctx, audit.Event{
		Verb:        verb,
		Actor:       r.ManagerIdentity,
		ResourceRef: audit.ResourceRefFromObject(schema),
		Reason:      reason,
		Outcome:     outcome,
		After:       schema,
	}); err != nil {
		log.FromContext(ctx).Error(err, "audit append failed",
			"controller", "databaseschema",
			"name", schema.Name,
			"verb", verb,
			"outcome", outcome)
	}
}

func (r *DatabaseSchemaReconciler) fail(
	ctx context.Context,
	schema *keystonev1alpha1.DatabaseSchema,
	reason string,
	cause error,
) (ctrl.Result, error) {
	patch := client.MergeFrom(schema.DeepCopy())
	schema.Status.ObservedGeneration = schema.Generation
	schema.Status.Conditions = conditions.MarkNotReady(schema.Status.Conditions, reason, cause, schema.Generation)
	if perr := r.Status().Patch(ctx, schema, patch); perr != nil {
		return ctrl.Result{}, fmt.Errorf("patch status (after %s): %w; underlying: %v", reason, perr, cause)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(schema, corev1.EventTypeWarning, reason, "%s", cause.Error())
	}
	r.emitAudit(ctx, schema, "reconcile", reason+": "+cause.Error(), "error")
	return ctrl.Result{}, cause
}

// SetupWithManager wires the reconciler.
//
// The DatabaseProvider watch mirrors the LogicalDatabase controller's:
// this reconciler dials the provider endpoint too (via the
// LogicalDatabase's spec.providerRef), so a provider spec repoint must
// enqueue dependent schemas immediately instead of waiting out
// error-backoff. GenerationChangedPredicate filters the provider
// controller's own status patches.
func (r *DatabaseSchemaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.SystemNamespace == "" {
		r.SystemNamespace = "keystone-system"
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keystone-databaseschema")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.DatabaseSchema{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Watches(&keystonev1alpha1.DatabaseProvider{},
			handler.EnqueueRequestsFromMapFunc(r.schemasForProvider),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("databaseschema").
		Complete(r)
}

// schemasForProvider maps a DatabaseProvider event to every
// DatabaseSchema that transitively references it: schema →
// spec.logicalDatabaseRef (same-namespace) → LogicalDatabase →
// spec.providerRef. In-code filter over Lists, matching the
// providersForSecret idiom — both CR populations are low-cardinality.
func (r *DatabaseSchemaReconciler) schemasForProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)
	var ldbs keystonev1alpha1.LogicalDatabaseList
	if err := r.List(ctx, &ldbs); err != nil {
		logger.Error(err, "list LogicalDatabases for DatabaseProvider fanout",
			"provider", obj.GetName())
		return nil
	}
	type ldbKey struct{ namespace, name string }
	referencing := make(map[ldbKey]bool, len(ldbs.Items))
	for i := range ldbs.Items {
		ldb := &ldbs.Items[i]
		if ldb.Spec.ProviderRef == obj.GetName() {
			referencing[ldbKey{ldb.Namespace, ldb.Name}] = true
		}
	}
	if len(referencing) == 0 {
		return nil
	}
	var schemas keystonev1alpha1.DatabaseSchemaList
	if err := r.List(ctx, &schemas); err != nil {
		logger.Error(err, "list DatabaseSchemas for DatabaseProvider fanout",
			"provider", obj.GetName())
		return nil
	}
	var out []reconcile.Request
	for i := range schemas.Items {
		s := &schemas.Items[i]
		if referencing[ldbKey{s.Namespace, s.Spec.LogicalDatabaseRef}] {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: s.Namespace, Name: s.Name,
			}})
		}
	}
	return out
}
