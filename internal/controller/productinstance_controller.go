// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/log"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
	"github.com/dogukanturhal/keystone/internal/conditions"
)

// ProductInstanceReconciler is the TenancyController. It reads a
// ProductInstance, resolves its ProductDefinition, and materialises one
// LogicalDatabase + N DatabaseSchema CRs per ProductDatabase. The
// SchemaController and MigrationController do the actual PostgreSQL
// work; this controller is the orchestrator.
//
// State machine:
//   Pending → Provisioning (children created) →
//   Migrating (waits for child resources Ready + initial migrations) →
//   ConfiguringAuth (Phase 4.1 — SpiceDB tuples) →
//   RegisteringRoutes (Phase 4.1 — APISIX) →
//   Active.
// Failure at any phase → Failed; user can fix the spec and re-reconcile.
type ProductInstanceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// AuditLogger appends tamper-evident entries on phase transitions.
	AuditLogger     *audit.Logger
	ManagerIdentity keystonev1alpha1.AuditActor
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=productinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=productinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=productinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=productdefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=productdefinitions/status,verbs=get;update;patch

func (r *ProductInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("productinstance", req.NamespacedName)

	var instance keystonev1alpha1.ProductInstance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !instance.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &instance, logger)
	}

	if controllerutil.AddFinalizer(&instance, keystonev1alpha1.FinalizerProductInstance) {
		if err := r.Update(ctx, &instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Resolve the ProductDefinition (cluster-scoped).
	var product keystonev1alpha1.ProductDefinition
	if err := r.Get(ctx, types.NamespacedName{Name: instance.Spec.ProductRef}, &product); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, &instance, "ProductDefinitionNotFound",
				fmt.Errorf("ProductDefinition %q not found", instance.Spec.ProductRef))
		}
		return ctrl.Result{}, err
	}

	// First reconcile — set Phase=Provisioning if still empty.
	if instance.Status.Phase == "" || instance.Status.Phase == keystonev1alpha1.ProductInstancePhasePending {
		if err := r.transition(ctx, &instance, keystonev1alpha1.ProductInstancePhaseProvisioning,
			"Provisioning", "creating child LogicalDatabase + DatabaseSchema resources"); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Materialise child resources.
	dbStatuses, err := r.ensureChildResources(ctx, &instance, &product)
	if err != nil {
		return r.fail(ctx, &instance, "ChildResourceCreationFailed", err)
	}

	// Aggregate child readiness.
	allDBsReady, allSchemasReady, schemaCount := r.summariseChildren(ctx, &instance, dbStatuses)

	patch := client.MergeFrom(instance.DeepCopy())
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.Databases = dbStatuses
	instance.Status.Conditions = conditions.Set(instance.Status.Conditions,
		"DatabasesReady", boolToConditionStatus(allDBsReady),
		"Aggregated",
		fmt.Sprintf("%d database(s) declared; all ready=%t", len(dbStatuses), allDBsReady),
		instance.Generation)
	instance.Status.Conditions = conditions.Set(instance.Status.Conditions,
		"SchemasReady", boolToConditionStatus(allSchemasReady),
		"Aggregated",
		fmt.Sprintf("%d schema(s) declared; all ready=%t", schemaCount, allSchemasReady),
		instance.Generation)

	switch {
	case !allDBsReady:
		instance.Status.Phase = keystonev1alpha1.ProductInstancePhaseProvisioning
		instance.Status.Conditions = conditions.MarkProgressing(instance.Status.Conditions,
			"WaitingForDatabases",
			"waiting for child LogicalDatabase resources to become Ready",
			instance.Generation)
	case !allSchemasReady:
		instance.Status.Phase = keystonev1alpha1.ProductInstancePhaseProvisioning
		instance.Status.Conditions = conditions.MarkProgressing(instance.Status.Conditions,
			"WaitingForSchemas",
			"waiting for child DatabaseSchema resources to become Ready",
			instance.Generation)
	default:
		// Phase 4: skip ConfiguringAuth + RegisteringRoutes (those are
		// Phase 4.1 — they'd be SpiceDB and APISIX HTTP calls). Mark
		// the conditions as awaiting-implementation rather than
		// pretending they succeeded.
		instance.Status.Conditions = conditions.Set(instance.Status.Conditions,
			"AuthConfigured", metav1.ConditionUnknown,
			"AwaitingPhase41",
			"SpiceDB integration deferred to Phase 4.1",
			instance.Generation)
		instance.Status.Conditions = conditions.Set(instance.Status.Conditions,
			"RoutesRegistered", metav1.ConditionUnknown,
			"AwaitingPhase41",
			"APISIX integration deferred to Phase 4.1",
			instance.Generation)

		instance.Status.Phase = keystonev1alpha1.ProductInstancePhaseActive
		now := metav1.Now()
		instance.Status.LastTransitionTime = &now
		instance.Status.Conditions = conditions.MarkReady(instance.Status.Conditions,
			"Active",
			"all databases and schemas Ready; auth + routes deferred to Phase 4.1",
			instance.Generation)
		instance.Status.Conditions = conditions.ClearProgressing(instance.Status.Conditions, instance.Generation)
	}

	if err := r.Status().Patch(ctx, &instance, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}

	logger.V(1).Info("reconciled", "phase", instance.Status.Phase,
		"databases", len(dbStatuses), "schemas", schemaCount,
		"allReady", allDBsReady && allSchemasReady)

	if allDBsReady && allSchemasReady {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// ensureChildResources is idempotent. For each ProductDatabase + each
// ProductSchema, create-or-update the corresponding child CR with owner
// reference set so deletion cascades through Argo CD prune.
func (r *ProductInstanceReconciler) ensureChildResources(
	ctx context.Context,
	instance *keystonev1alpha1.ProductInstance,
	product *keystonev1alpha1.ProductDefinition,
) ([]keystonev1alpha1.InstanceDatabase, error) {
	out := make([]keystonev1alpha1.InstanceDatabase, 0, len(product.Spec.Databases))
	for _, db := range product.Spec.Databases {
		dbName := substituteTokens(db.NameTemplate, instance, product)
		if err := pgIdentValid(dbName); err != nil {
			return nil, fmt.Errorf("template %q for database key %q: %w", db.NameTemplate, db.Key, err)
		}

		ldbName := childResourceName(instance.Name, db.Key)
		ldb := &keystonev1alpha1.LogicalDatabase{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: instance.Namespace,
				Name:      ldbName,
				Labels:    childLabels(instance, product, db.Key),
			},
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ldb, func() error {
			ldb.Spec = keystonev1alpha1.LogicalDatabaseSpec{
				Name:           dbName,
				ClusterRef:     product.Name, // hub registers itself; Phase 7 splits cluster
				ProviderRef:    db.ProviderRef,
				OwnerRole:      dbName + "_owner",
				Extensions:     db.Extensions,
				DeletionPolicy: instance.Spec.DeletionPolicy,
			}
			return controllerutil.SetControllerReference(instance, ldb, r.Scheme)
		}); err != nil {
			return nil, fmt.Errorf("upsert LogicalDatabase %s: %w", ldbName, err)
		}

		entry := keystonev1alpha1.InstanceDatabase{
			Key:                db.Key,
			LogicalDatabaseRef: ldbName,
		}

		// Schemas inside this database.
		for _, sch := range db.Schemas {
			schName := substituteTokens(sch.NameTemplate, instance, product)
			if err := pgIdentValid(schName); err != nil {
				return nil, fmt.Errorf("template %q for schema key %q: %w", sch.NameTemplate, sch.Key, err)
			}
			schemaResName := childResourceName(instance.Name, db.Key+"-"+sch.Key)
			schema := &keystonev1alpha1.DatabaseSchema{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: instance.Namespace,
					Name:      schemaResName,
					Labels:    childLabels(instance, product, sch.Key),
				},
			}
			if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, schema, func() error {
				schema.Spec = keystonev1alpha1.DatabaseSchemaSpec{
					Name:               schName,
					LogicalDatabaseRef: ldbName,
					OwnerRole:          schName + "_owner",
					DeletionPolicy:     instance.Spec.DeletionPolicy,
				}
				return controllerutil.SetControllerReference(instance, schema, r.Scheme)
			}); err != nil {
				return nil, fmt.Errorf("upsert DatabaseSchema %s: %w", schemaResName, err)
			}
			entry.DatabaseSchemaRefs = append(entry.DatabaseSchemaRefs, schemaResName)
		}

		out = append(out, entry)
	}
	return out, nil
}

func (r *ProductInstanceReconciler) summariseChildren(
	ctx context.Context,
	instance *keystonev1alpha1.ProductInstance,
	dbs []keystonev1alpha1.InstanceDatabase,
) (allDBsReady, allSchemasReady bool, schemaCount int) {
	allDBsReady = true
	allSchemasReady = true
	for _, d := range dbs {
		var ldb keystonev1alpha1.LogicalDatabase
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: instance.Namespace, Name: d.LogicalDatabaseRef,
		}, &ldb); err != nil || !conditions.IsTrue(ldb.Status.Conditions, keystonev1alpha1.ConditionTypeReady) {
			allDBsReady = false
		}
		for _, s := range d.DatabaseSchemaRefs {
			schemaCount++
			var schema keystonev1alpha1.DatabaseSchema
			if err := r.Get(ctx, types.NamespacedName{
				Namespace: instance.Namespace, Name: s,
			}, &schema); err != nil || !conditions.IsTrue(schema.Status.Conditions, keystonev1alpha1.ConditionTypeReady) {
				allSchemasReady = false
			}
		}
	}
	return
}

func (r *ProductInstanceReconciler) reconcileDelete(
	ctx context.Context,
	instance *keystonev1alpha1.ProductInstance,
	logger logr.Logger,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(instance, keystonev1alpha1.FinalizerProductInstance) {
		return ctrl.Result{}, nil
	}

	// Argo CD's prune handles owned child deletion through ownerReferences.
	// We just need to wait until they're gone before removing our finalizer
	// (otherwise children orphan).
	var ldbs keystonev1alpha1.LogicalDatabaseList
	if err := r.List(ctx, &ldbs, client.InNamespace(instance.Namespace),
		client.MatchingLabels{"keystone.hexxlock.io/instance": instance.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list child LogicalDatabases: %w", err)
	}
	if len(ldbs.Items) > 0 {
		// Trigger deletion if not already deleting.
		for i := range ldbs.Items {
			if ldbs.Items[i].DeletionTimestamp.IsZero() {
				if err := r.Delete(ctx, &ldbs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("delete child LogicalDatabase: %w", err)
				}
			}
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	patch := client.MergeFrom(instance.DeepCopy())
	instance.Status.Phase = keystonev1alpha1.ProductInstancePhaseDeactivated
	now := metav1.Now()
	instance.Status.LastTransitionTime = &now
	_ = r.Status().Patch(ctx, instance, patch)

	controllerutil.RemoveFinalizer(instance, keystonev1alpha1.FinalizerProductInstance)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	logger.Info("instance deactivated; finalizer removed")
	return ctrl.Result{}, nil
}

func (r *ProductInstanceReconciler) transition(
	ctx context.Context,
	instance *keystonev1alpha1.ProductInstance,
	phase keystonev1alpha1.ProductInstancePhase,
	reason, message string,
) error {
	patch := client.MergeFrom(instance.DeepCopy())
	instance.Status.Phase = phase
	now := metav1.Now()
	instance.Status.LastTransitionTime = &now
	instance.Status.Conditions = conditions.MarkProgressing(instance.Status.Conditions,
		reason, message, instance.Generation)
	if err := r.Status().Patch(ctx, instance, patch); err != nil {
		return fmt.Errorf("patch transition to %s: %w", phase, err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(instance, corev1.EventTypeNormal, string(phase), "%s", message)
	}
	r.emitAudit(ctx, instance, "transition", string(phase)+": "+message, "success")
	return nil
}

// emitAudit appends a tamper-evident entry on phase transitions and
// fail() invocations. Errors logged at warn level rather than discarded.
func (r *ProductInstanceReconciler) emitAudit(
	ctx context.Context,
	instance *keystonev1alpha1.ProductInstance,
	verb, reason, outcome string,
) {
	if r.AuditLogger == nil {
		return
	}
	if _, _, err := r.AuditLogger.Append(ctx, audit.Event{
		Verb:        verb,
		Actor:       r.ManagerIdentity,
		ResourceRef: audit.ResourceRefFromObject(instance),
		Reason:      reason,
		Outcome:     outcome,
		After:       instance,
	}); err != nil {
		log.FromContext(ctx).Error(err, "audit append failed",
			"controller", "productinstance",
			"name", instance.Name,
			"verb", verb,
			"outcome", outcome)
	}
}

func (r *ProductInstanceReconciler) fail(
	ctx context.Context,
	instance *keystonev1alpha1.ProductInstance,
	reason string, cause error,
) (ctrl.Result, error) {
	patch := client.MergeFrom(instance.DeepCopy())
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.Phase = keystonev1alpha1.ProductInstancePhaseFailed
	instance.Status.Conditions = conditions.MarkNotReady(instance.Status.Conditions, reason, cause, instance.Generation)
	if perr := r.Status().Patch(ctx, instance, patch); perr != nil {
		return ctrl.Result{}, fmt.Errorf("patch failure: %w (cause: %v)", perr, cause)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(instance, corev1.EventTypeWarning, reason, "%s", cause.Error())
	}
	r.emitAudit(ctx, instance, "transition", reason+": "+cause.Error(), "error")
	return ctrl.Result{}, cause
}

func (r *ProductInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keystone-productinstance")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.ProductInstance{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Owns(&keystonev1alpha1.LogicalDatabase{}).
		Owns(&keystonev1alpha1.DatabaseSchema{}).
		Named("productinstance").
		Complete(r)
}

// substituteTokens expands {{tenantID}}, {{tenantSlug}}, {{productSlug}}
// in templated names. Tenant IDs are UUIDs containing hyphens, which
// PG identifiers reject — we replace hyphens with underscores
// automatically. The result is re-validated by pgIdentValid downstream.
func substituteTokens(tmpl string, inst *keystonev1alpha1.ProductInstance, prod *keystonev1alpha1.ProductDefinition) string {
	out := tmpl
	out = strings.ReplaceAll(out, "{{tenantID}}", strings.ReplaceAll(inst.Spec.TenantID, "-", "_"))
	out = strings.ReplaceAll(out, "{{tenantSlug}}", strings.ReplaceAll(inst.Spec.TenantSlug, "-", "_"))
	out = strings.ReplaceAll(out, "{{productSlug}}", strings.ReplaceAll(prod.Spec.Slug, "-", "_"))
	return out
}

// pgIdentValid is a thin wrapper over the postgres package's regex —
// avoids the controller package importing internal/postgres just for
// the validator (which would create a cycle once Phase 5+ adds drift
// detection in the postgres package).
func pgIdentValid(name string) error {
	for i, r := range name {
		if i == 0 && !(r == '_' || (r >= 'a' && r <= 'z')) {
			return fmt.Errorf("invalid PG identifier %q: must start with [a-z_]", name)
		}
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("invalid PG identifier %q: contains %q", name, r)
		}
	}
	if name == "" || len(name) > 63 {
		return fmt.Errorf("invalid PG identifier length: %d (must be 1..63)", len(name))
	}
	return nil
}

// childResourceName composes a deterministic name for a child resource
// scoped to the parent ProductInstance. Trimmed to fit K8s 253-char limit.
func childResourceName(instance, suffix string) string {
	name := fmt.Sprintf("%s-%s", instance, suffix)
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

// childLabels are applied to every child created by this controller so
// the deletion path can list them with a single LIST request.
func childLabels(inst *keystonev1alpha1.ProductInstance, prod *keystonev1alpha1.ProductDefinition, key string) map[string]string {
	return map[string]string{
		"keystone.hexxlock.io/instance":  inst.Name,
		"keystone.hexxlock.io/tenant-id": inst.Spec.TenantID,
		"keystone.hexxlock.io/product":   prod.Spec.Slug,
		"keystone.hexxlock.io/role":      key,
	}
}

// boolToConditionStatus maps Go booleans onto metav1 condition statuses.
// Helper kept tiny and inlined where used so callers do not need to
// remember the type.
func boolToConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}
