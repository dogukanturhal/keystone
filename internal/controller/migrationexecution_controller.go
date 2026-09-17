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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
	"github.com/dogukanturhal/keystone/internal/conditions"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
	"github.com/dogukanturhal/keystone/internal/migration/pgroll"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// MigrationExecutionReconciler runs the SQL declared by a
// MigrationExecution. Once Phase = Succeeded it never reconciles again
// (idempotency via the IsApplied check + the schema_migrations table).
type MigrationExecutionReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Pools    *pg.PoolCache
	Resolver migration.SourceResolver

	// AuditLogger appends tamper-evident entries to the audit chain on
	// every Phase transition (Succeeded/Failed). Optional in tests
	// where the chain isn't reconstructed; nil means no-op.
	AuditLogger *audit.Logger

	// ManagerIdentity is the Actor stamped on reconciler-sourced
	// AuditEntries (the controller's ServiceAccount username/groups).
	ManagerIdentity keystonev1alpha1.AuditActor

	SystemNamespace string
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=migrationexecutions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=migrationexecutions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=migrationexecutions/finalizers,verbs=update

func (r *MigrationExecutionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("migrationexecution", req.NamespacedName)

	var exec keystonev1alpha1.MigrationExecution
	if err := r.Get(ctx, req.NamespacedName, &exec); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !exec.DeletionTimestamp.IsZero() {
		// Executions are kept for audit; deletion is admin-driven and
		// the controller does not undo applied SQL.
		if controllerutil.RemoveFinalizer(&exec, keystonev1alpha1.FinalizerMigrationExecution) {
			if err := r.Update(ctx, &exec); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Terminal phases: no further work. Expanded is NOT terminal — it
	// waits for the operator's complete annotation.
	if exec.Status.Phase == keystonev1alpha1.ExecutionPhaseSucceeded ||
		exec.Status.Phase == keystonev1alpha1.ExecutionPhaseFailed ||
		exec.Status.Phase == keystonev1alpha1.ExecutionPhaseAborted {
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&exec, keystonev1alpha1.FinalizerMigrationExecution) {
		if err := r.Update(ctx, &exec); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Resolve bundle (same namespace as execution).
	var bundle keystonev1alpha1.MigrationBundle
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: exec.Namespace, Name: exec.Spec.BundleRef,
	}, &bundle); err != nil {
		return r.fail(ctx, &exec, "BundleNotFound", err)
	}

	// A4 — Plan-Gate-Apply. The MigrationPlanReconciler owns
	// spec.approved; we refuse to execute SQL until the gate is open.
	// Requeue (don't fail) on un-approved plans so Manual-mode
	// workflows can approve a plan via kubectl patch / CI and see
	// the execution fire without deleting the CR.
	if exec.Spec.PlanRef != "" {
		approved, reason, err := r.isPlanApproved(ctx, exec.Namespace, exec.Spec.PlanRef)
		if err != nil {
			return r.fail(ctx, &exec, "PlanLookupFailed", err)
		}
		if !approved {
			return r.awaitingApproval(ctx, &exec, reason)
		}
	}

	// Resolve schema (same namespace as execution).
	var schema keystonev1alpha1.DatabaseSchema
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: exec.Namespace, Name: exec.Spec.SchemaRef,
	}, &schema); err != nil {
		return r.fail(ctx, &exec, "SchemaNotFound", err)
	}
	if !conditions.IsTrue(schema.Status.Conditions, keystonev1alpha1.ConditionTypeReady) {
		// Wait for the schema to be ready.
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Resolve LogicalDatabase + DatabaseProvider for connectivity.
	var ldb keystonev1alpha1.LogicalDatabase
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: schema.Namespace, Name: schema.Spec.LogicalDatabaseRef,
	}, &ldb); err != nil {
		return r.fail(ctx, &exec, "LogicalDatabaseNotFound", err)
	}
	var provider keystonev1alpha1.DatabaseProvider
	if err := r.Get(ctx, types.NamespacedName{Name: ldb.Spec.ProviderRef}, &provider); err != nil {
		return r.fail(ctx, &exec, "ProviderNotFound", err)
	}

	creds, err := r.resolveCredentials(ctx, &provider)
	if err != nil {
		return r.fail(ctx, &exec, "CredentialsResolutionFailed", err)
	}

	// Open pool against the target database.
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
		return r.fail(ctx, &exec, "PoolAcquireFailed", err)
	}
	defer release()

	// Branch by strategy.
	if bundle.Spec.Strategy == keystonev1alpha1.StrategyPgrollExpandContract {
		return r.reconcilePgroll(ctx, &exec, &bundle, &schema, pool, logger)
	}

	// Default: versioned strategy. Re-resolve source; verify ContentHash
	// matches the spec (anti-replay TOCTOU defence).
	src, err := r.Resolver.Resolve(ctx, bundle.Namespace, bundle.Spec.Source)
	if err != nil {
		return r.fail(ctx, &exec, "SourceResolutionFailed", err)
	}
	if src.ContentHash != exec.Spec.ContentHash {
		return r.fail(ctx, &exec, "ContentHashMismatch", fmt.Errorf(
			"spec hash %q vs source hash %q — bundle SQL changed since plan; "+
				"refusing to apply (this is the TOCTOU defence)",
			exec.Spec.ContentHash, src.ContentHash))
	}

	// trackingTableName comes from the LogicalDatabase (defaults to
	// "schema_migrations"). Example Service overrides this to
	// "keystone_schema_migrations" so Keystone's TEXT-PK tracking table
	// co-exists with Example Service's existing INTEGER-PK schema_migrations.
	//
	// ownerRole is the LogicalDatabase's spec.ownerRole — the runner
	// SET LOCAL ROLE's to this PG role inside the migration tx so
	// CREATE TABLE etc. attribute ownership to _owner instead of the
	// connection user (keystone_admin). Without this, the runtime
	// _app role can't read its own tables via inRoles inheritance.
	runner, err := migration.NewRunner(pool, schema.Spec.Name, ldb.Spec.TrackingTableName, ldb.Spec.OwnerRole)
	if err != nil {
		return r.fail(ctx, &exec, "RunnerInitFailed", err)
	}
	if err := runner.EnsureBookkeeping(ctx); err != nil {
		return r.fail(ctx, &exec, "BookkeepingFailed", err)
	}

	// Already applied?
	already, prior, err := runner.IsApplied(ctx, exec.Spec.BundleVersion)
	if err != nil {
		return r.fail(ctx, &exec, "ApplyCheckFailed", err)
	}
	if already {
		if prior.ContentHash != exec.Spec.ContentHash {
			return r.fail(ctx, &exec, "PriorVersionContentMismatch", fmt.Errorf(
				"version %q already applied with different contentHash %q vs spec %q — "+
					"bundle is immutable per-version, the prior application stands; "+
					"bump spec.version to ship the new SQL",
				exec.Spec.BundleVersion, prior.ContentHash, exec.Spec.ContentHash))
		}
		return r.markSucceeded(ctx, &exec, nil, 0, "AlreadyApplied",
			fmt.Sprintf("version %s was already applied at %s", exec.Spec.BundleVersion, prior.AppliedAt))
	}

	// Mark Running.
	if err := r.markRunning(ctx, &exec); err != nil {
		return ctrl.Result{}, err
	}

	// RecordOnly (adoption baseline, ADR 0027): upsert the tracking-
	// table row and execute nothing — `flyway baseline` / Liquibase
	// `changelog-sync` semantics, declaratively. The ContentHash was
	// TOCTOU-verified above, so the recorded hash provably matches
	// the reviewed baseline SQL; the DriftController then compares the
	// declared baseline against live structure as the safety net.
	if exec.Spec.ExecutionMode == keystonev1alpha1.ExecutionModeRecordOnly {
		if err := runner.Record(ctx, exec.Spec.BundleVersion, exec.Spec.ContentHash); err != nil {
			return r.markFailed(ctx, &exec, nil, err)
		}
		logger.Info("recorded as applied (adoption baseline; SQL not executed)",
			"version", exec.Spec.BundleVersion, "files", len(src.Names))
		return r.markSucceeded(ctx, &exec, nil, 0, "RecordedOnly",
			fmt.Sprintf("version %s recorded as applied (adoption baseline); %d file(s) not executed",
				exec.Spec.BundleVersion, len(src.Names)))
	}

	// Apply.
	result, applyErr := runner.Apply(ctx, exec.Spec.BundleVersion, exec.Spec.ContentHash, src)
	if applyErr != nil {
		return r.markFailed(ctx, &exec, result, applyErr)
	}

	logger.Info("applied", "version", exec.Spec.BundleVersion, "files", len(result.Files), "duration_ms", result.TotalDurationMS)
	return r.markSucceeded(ctx, &exec, result.Files, result.TotalDurationMS, "Applied",
		fmt.Sprintf("applied %d file(s) in %dms", len(result.Files), result.TotalDurationMS))
}

func (r *MigrationExecutionReconciler) markRunning(ctx context.Context, exec *keystonev1alpha1.MigrationExecution) error {
	patch := client.MergeFrom(exec.DeepCopy())
	exec.Status.ObservedGeneration = exec.Generation
	exec.Status.Phase = keystonev1alpha1.ExecutionPhaseRunning
	now := metav1.Now()
	exec.Status.StartTime = &now
	exec.Status.Conditions = conditions.Set(exec.Status.Conditions,
		"Started", metav1.ConditionTrue, "Applying",
		"acquired schema and beginning apply", exec.Generation)
	return r.Status().Patch(ctx, exec, patch)
}

// reconcilePgroll handles the strategy=pgroll-expand-contract path.
// Two-phase: Expanding → Expanded (waits for operator annotation) →
// Contracting → Succeeded.
func (r *MigrationExecutionReconciler) reconcilePgroll(
	ctx context.Context,
	exec *keystonev1alpha1.MigrationExecution,
	bundle *keystonev1alpha1.MigrationBundle,
	schema *keystonev1alpha1.DatabaseSchema,
	pool *pgxpool.Pool,
	logger logr.Logger,
) (ctrl.Result, error) {
	if len(bundle.Spec.Operations) == 0 {
		return r.fail(ctx, exec, "PgrollOperationCount",
			fmt.Errorf("strategy=pgroll-expand-contract requires at least one operation"))
	}
	engine, err := pgroll.NewEngine(pool, schema.Spec.Name)
	if err != nil {
		return r.fail(ctx, exec, "PgrollEngineInit", err)
	}

	// B1 — abort gate. If the operator flipped
	// keystone.hexxlock.io/abort=true on a non-terminal execution,
	// short-circuit into Aborting. Contracting aborts are refused:
	// once Contract starts dropping columns, the data is gone and a
	// rollback cannot restore it — we record a warning condition and
	// let the contract finish.
	if exec.Annotations[keystonev1alpha1.AnnotationAbortMigration] == "true" {
		if exec.Status.Phase == keystonev1alpha1.ExecutionPhaseContracting {
			// Clamp. Don't overwrite phase; just emit a warning event
			// so operators see why abort was ignored.
			if r.Recorder != nil {
				r.Recorder.Eventf(exec, corev1.EventTypeWarning, "AbortRefused",
					"abort requested during Contracting phase; Contract is irreversible "+
						"(columns already dropped). Proceeding with contract.")
			}
		} else if exec.Status.Phase != keystonev1alpha1.ExecutionPhaseAborting {
			patch := client.MergeFrom(exec.DeepCopy())
			exec.Status.Phase = keystonev1alpha1.ExecutionPhaseAborting
			exec.Status.Conditions = conditions.MarkProgressing(exec.Status.Conditions,
				"Aborting",
				"operator signalled abort; rolling back expand-phase side effects",
				exec.Generation)
			if err := r.Status().Patch(ctx, exec, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// State machine: dispatch on current Phase. Multi-op: Expand runs
	// operations in array order; Contract runs them in REVERSE so a
	// constraint added in op[0] is validated before op[1]'s contract
	// drops a column it depends on.
	switch exec.Status.Phase {
	case "", keystonev1alpha1.ExecutionPhasePending, keystonev1alpha1.ExecutionPhaseRunning:
		patch := client.MergeFrom(exec.DeepCopy())
		exec.Status.Phase = keystonev1alpha1.ExecutionPhaseExpanding
		now := metav1.Now()
		exec.Status.StartTime = &now
		exec.Status.Conditions = conditions.Set(exec.Status.Conditions,
			"Started", metav1.ConditionTrue, "Expanding",
			fmt.Sprintf("running expand phase for %d operation(s)", len(bundle.Spec.Operations)),
			exec.Generation)
		if err := r.Status().Patch(ctx, exec, patch); err != nil {
			return ctrl.Result{}, err
		}
		descriptions, wErr := runPgrollWaves(ctx, engine, bundle.Spec.Operations,
			pgroll.PhaseExpand, false, bundle.Spec.Parallelism)
		if wErr != nil {
			return r.markFailed(ctx, exec, &migration.ApplyResult{FailureMsg: wErr.Error()}, wErr)
		}
		patch = client.MergeFrom(exec.DeepCopy())
		exec.Status.Phase = keystonev1alpha1.ExecutionPhaseExpanded
		exec.Status.Conditions = conditions.Set(exec.Status.Conditions,
			"Expanded", metav1.ConditionTrue, "ExpandComplete",
			joinLines(descriptions), exec.Generation)
		exec.Status.Conditions = conditions.MarkProgressing(exec.Status.Conditions,
			"AwaitingComplete",
			fmt.Sprintf("set annotation %s=true on this MigrationExecution to run contract phase", keystonev1alpha1.AnnotationCompleteContraction),
			exec.Generation)
		if err := r.Status().Patch(ctx, exec, patch); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("expand complete", "operations", len(bundle.Spec.Operations))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil

	case keystonev1alpha1.ExecutionPhaseExpanded:
		if exec.Annotations[keystonev1alpha1.AnnotationCompleteContraction] != "true" {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		patch := client.MergeFrom(exec.DeepCopy())
		exec.Status.Phase = keystonev1alpha1.ExecutionPhaseContracting
		exec.Status.Conditions = conditions.MarkProgressing(exec.Status.Conditions,
			"Contracting",
			fmt.Sprintf("running contract phase for %d operation(s) in reverse", len(bundle.Spec.Operations)),
			exec.Generation)
		if err := r.Status().Patch(ctx, exec, patch); err != nil {
			return ctrl.Result{}, err
		}
		descriptions, wErr := runPgrollWaves(ctx, engine, bundle.Spec.Operations,
			pgroll.PhaseContract, true, bundle.Spec.Parallelism)
		if wErr != nil {
			return r.markFailed(ctx, exec, &migration.ApplyResult{FailureMsg: wErr.Error()}, wErr)
		}
		patch = client.MergeFrom(exec.DeepCopy())
		exec.Status.Phase = keystonev1alpha1.ExecutionPhaseSucceeded
		now := metav1.Now()
		exec.Status.CompletionTime = &now
		exec.Status.Conditions = conditions.Set(exec.Status.Conditions,
			keystonev1alpha1.ConditionTypeApplied, metav1.ConditionTrue,
			"ContractComplete", joinLines(descriptions), exec.Generation)
		exec.Status.Conditions = conditions.MarkReady(exec.Status.Conditions,
			"Succeeded", joinLines(descriptions), exec.Generation)
		exec.Status.Conditions = conditions.ClearProgressing(exec.Status.Conditions, exec.Generation)
		if err := r.Status().Patch(ctx, exec, patch); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("contract complete", "operations", len(bundle.Spec.Operations))
		return ctrl.Result{}, nil

	case keystonev1alpha1.ExecutionPhaseContracting:
		// Crash-recovery: re-attempt contract idempotently. Engine
		// operations use IF EXISTS / IF NOT EXISTS where possible.
		descriptions, wErr := runPgrollWaves(ctx, engine, bundle.Spec.Operations,
			pgroll.PhaseContract, true, bundle.Spec.Parallelism)
		if wErr != nil {
			return r.markFailed(ctx, exec, &migration.ApplyResult{
				FailureMsg: "recovery " + wErr.Error(),
			}, wErr)
		}
		patch := client.MergeFrom(exec.DeepCopy())
		exec.Status.Phase = keystonev1alpha1.ExecutionPhaseSucceeded
		now := metav1.Now()
		exec.Status.CompletionTime = &now
		exec.Status.Conditions = conditions.MarkReady(exec.Status.Conditions,
			"Succeeded (after recovery)", joinLines(descriptions), exec.Generation)
		exec.Status.Conditions = conditions.ClearProgressing(exec.Status.Conditions, exec.Generation)
		_ = r.Status().Patch(ctx, exec, patch)
		return ctrl.Result{}, nil

	case keystonev1alpha1.ExecutionPhaseAborting:
		// Run per-op abort handlers in reverse wave order so a
		// constraint added in op[0] is dropped before op[1]'s shadow
		// column teardown (mirrors the Contract ordering invariant).
		// Each handler is idempotent via IF EXISTS, so a partially-
		// aborted execution can retry without side effects.
		descriptions, wErr := runPgrollWaves(ctx, engine, bundle.Spec.Operations,
			pgroll.PhaseAbort, true, bundle.Spec.Parallelism)
		if wErr != nil {
			// Abort failure: stick in Aborting and retry. The
			// alternative — marking Failed — leaves operators no
			// path forward short of manual SQL.
			return ctrl.Result{}, fmt.Errorf("abort: %w", wErr)
		}
		patch := client.MergeFrom(exec.DeepCopy())
		exec.Status.Phase = keystonev1alpha1.ExecutionPhaseAborted
		now := metav1.Now()
		exec.Status.CompletionTime = &now
		exec.Status.Conditions = conditions.Set(exec.Status.Conditions,
			keystonev1alpha1.ConditionTypeApplied, metav1.ConditionFalse,
			"Aborted", joinLines(descriptions), exec.Generation)
		exec.Status.Conditions = conditions.MarkNotReady(exec.Status.Conditions,
			"Aborted", fmt.Errorf("operator signalled abort; expand-phase effects rolled back"),
			exec.Generation)
		exec.Status.Conditions = conditions.ClearProgressing(exec.Status.Conditions, exec.Generation)
		if err := r.Status().Patch(ctx, exec, patch); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("abort complete", "operations", len(bundle.Spec.Operations))
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// runPgrollWaves is the shared executor for every pgroll phase
// (Expand / Contract / Abort / Contracting-recovery). It converts
// MigrationBundle.spec.operations into dependency-ordered waves via
// pgroll.PlanWaves and executes each wave either sequentially (when
// parallelism <= 1, pre-B3 behaviour) or concurrently (B3 opt-in).
//
// `reverse` is true for Contract + Abort phases — the wave order and
// the op order within each wave are both reversed so same-table ops
// unwind in reverse-bundle-order (e.g. a constraint added in op[0]
// is dropped before op[1]'s shadow column teardown).
//
// Returns a per-op description slice (indexed by ORIGINAL operation
// index so status output is stable regardless of wave packing) and
// the first error encountered. On error, goroutines still in flight
// observe the cancelled context and unwind; the caller is
// responsible for marking the execution Failed.
func runPgrollWaves(
	ctx context.Context,
	engine *pgroll.Engine,
	ops []keystonev1alpha1.MigrationOperation,
	phase pgroll.Phase,
	reverse bool,
	parallelism int32,
) ([]string, error) {
	waves := pgroll.PlanWaves(ops)
	if reverse {
		// Reverse wave order AND op-within-wave order so same-table
		// ops commit in reverse-bundle-order under Contract / Abort.
		for i, j := 0, len(waves)-1; i < j; i, j = i+1, j-1 {
			waves[i], waves[j] = waves[j], waves[i]
		}
		for _, w := range waves {
			for i, j := 0, len(w)-1; i < j; i, j = i+1, j-1 {
				w[i], w[j] = w[j], w[i]
			}
		}
	}

	// Per-op description buffer indexed by ORIGINAL op index. Pre-
	// allocating keeps the goroutine-safe write story simple: each
	// goroutine owns its own slot, no mutex needed.
	descriptions := make([]string, len(ops))

	for _, wave := range waves {
		cap := pgroll.EffectiveParallelism(parallelism, len(wave))
		if cap <= 1 {
			// Sequential path — preserves pre-B3 behaviour exactly.
			for _, idx := range wave {
				op := ops[idx]
				res, err := engine.Apply(ctx, op, phase)
				if err != nil {
					return descriptions, fmt.Errorf(
						"op[%d] %s/%s: %w", idx, op.Kind, op.Table, err)
				}
				descriptions[idx] = fmt.Sprintf("op[%d]: %s", idx, res.Description)
			}
			continue
		}

		// Parallel path — errgroup caps goroutines at `cap`. Each
		// goroutine takes an index of `wave`, calls engine.Apply, and
		// writes its slot in `descriptions`. First error wins; other
		// goroutines see the cancelled group ctx and return.
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(cap)
		for _, idx := range wave {
			idx := idx
			op := ops[idx]
			g.Go(func() error {
				res, err := engine.Apply(gctx, op, phase)
				if err != nil {
					return fmt.Errorf("op[%d] %s/%s: %w", idx, op.Kind, op.Table, err)
				}
				descriptions[idx] = fmt.Sprintf("op[%d]: %s", idx, res.Description)
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return descriptions, err
		}
	}
	return descriptions, nil
}

// joinLines joins per-operation descriptions for status.message,
// truncated to keep CR size bounded.
func joinLines(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	if len(out) > 4000 {
		out = out[:3950] + "… [truncated]"
	}
	return out
}

func (r *MigrationExecutionReconciler) markSucceeded(
	ctx context.Context,
	exec *keystonev1alpha1.MigrationExecution,
	files []migration.FileResult,
	totalMS int64,
	reason, message string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(exec.DeepCopy())
	exec.Status.ObservedGeneration = exec.Generation
	exec.Status.Phase = keystonev1alpha1.ExecutionPhaseSucceeded
	now := metav1.Now()
	exec.Status.CompletionTime = &now
	if files != nil {
		applied := make([]keystonev1alpha1.AppliedStatement, 0, len(files))
		for _, f := range files {
			applied = append(applied, keystonev1alpha1.AppliedStatement{
				File: f.File, Index: f.Index,
				DurationMS: f.DurationMS, RowsAffected: f.RowsAffected,
			})
		}
		exec.Status.Applied = applied
	}
	exec.Status.Conditions = conditions.Set(exec.Status.Conditions,
		keystonev1alpha1.ConditionTypeApplied, metav1.ConditionTrue, reason, message, exec.Generation)
	exec.Status.Conditions = conditions.MarkReady(exec.Status.Conditions, reason, message, exec.Generation)
	exec.Status.Conditions = conditions.ClearProgressing(exec.Status.Conditions, exec.Generation)
	if err := r.Status().Patch(ctx, exec, patch); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(exec, corev1.EventTypeNormal, reason, "%s", message)
	}
	// RecordOnly executions get a distinct audit verb so the chain
	// distinguishes "SQL ran" from "version declared as applied".
	auditVerb := "apply"
	if exec.Spec.ExecutionMode == keystonev1alpha1.ExecutionModeRecordOnly {
		auditVerb = "record"
	}
	r.emitAudit(ctx, exec, auditVerb, message, "success")
	// B4 — auto-snapshot: after every successful apply, create a
	// SchemaSnapshot so the post-apply structure is immutably
	// captured. Best-effort: a failure here logs a warning event but
	// does not revert the execution's Succeeded phase (the SQL already
	// committed). Retries are idempotent because the snapshot name
	// is deterministic.
	r.ensureAutoSnapshot(ctx, exec)
	// Phase-2b SDK feedback loop — copy the parent bundle's
	// SD-fingerprint annotation onto DatabaseSchema.Status.LastAppliedFingerprint
	// so SDK consumers (keystone-sdk/go/keystone.WaitSchemaFingerprint) can
	// verify identity-based per-schema convergence. Best-effort:
	// failures log + continue (the SQL already committed).
	r.stampDatabaseSchemaFingerprint(ctx, exec)
	return ctrl.Result{}, nil
}

// stampDatabaseSchemaFingerprint copies the parent MigrationBundle's
// `keystone.hexxlock.io/sd-fingerprint` annotation (set by the SD
// reconciler) onto the target DatabaseSchema's
// Status.LastAppliedFingerprint. Hand-authored bundles (no SD owner)
// don't carry the annotation; in that case this is a no-op.
//
// Best-effort by design — runs AFTER the SQL committed and the
// MigrationExecution status is already Succeeded. Failures are logged
// at debug (not warn — operators don't act on these) but never revert
// the Succeeded phase. The DriftController + next SD reconcile will
// converge the field on the next pass if this stamp drops.
func (r *MigrationExecutionReconciler) stampDatabaseSchemaFingerprint(
	ctx context.Context,
	exec *keystonev1alpha1.MigrationExecution,
) {
	logger := log.FromContext(ctx).WithValues("execution", exec.Name)

	// Lookup parent bundle for the SD-fingerprint annotation.
	var bundle keystonev1alpha1.MigrationBundle
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: exec.Namespace, Name: exec.Spec.BundleRef,
	}, &bundle); err != nil {
		logger.V(1).Info("stamp fingerprint: parent bundle not found, skipping",
			"bundle", exec.Spec.BundleRef, "err", err)
		return
	}
	fingerprint := bundle.Annotations[keystonev1alpha1.AnnotationSDFingerprint]
	if fingerprint == "" {
		// Hand-authored bundle — no SD owner, no fingerprint to stamp.
		return
	}

	// Patch the DatabaseSchema status. Best-effort.
	var schema keystonev1alpha1.DatabaseSchema
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: exec.Namespace, Name: exec.Spec.SchemaRef,
	}, &schema); err != nil {
		logger.V(1).Info("stamp fingerprint: target DatabaseSchema not found, skipping",
			"schema", exec.Spec.SchemaRef, "err", err)
		return
	}
	if schema.Status.LastAppliedFingerprint == fingerprint {
		// Already at this fingerprint — idempotent skip.
		return
	}
	patch := client.MergeFrom(schema.DeepCopy())
	schema.Status.LastAppliedFingerprint = fingerprint
	if err := r.Status().Patch(ctx, &schema, patch); err != nil {
		logger.V(1).Info("stamp fingerprint: status patch failed, skipping",
			"schema", schema.Name, "err", err)
		return
	}
	logger.V(1).Info("stamped DatabaseSchema.Status.LastAppliedFingerprint",
		"schema", schema.Name, "fingerprint", fingerprint)
}

// ensureAutoSnapshot creates a SchemaSnapshot CR named
// auto-<bundle>-<version>-<schema> (truncated to 253 chars) for the
// just-applied execution. Deterministic name + already-exists tolerance
// means retries reattach to the original CR. Errors are surfaced as
// Events but do NOT flip the execution back to pre-Succeeded — the
// DDL committed, the snapshot is a nicety.
func (r *MigrationExecutionReconciler) ensureAutoSnapshot(
	ctx context.Context,
	exec *keystonev1alpha1.MigrationExecution,
) {
	name := autoSnapshotName(exec.Spec.BundleRef, exec.Spec.BundleVersion, exec.Spec.SchemaRef)
	snap := &keystonev1alpha1.SchemaSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: exec.Namespace,
			Name:      name,
			Labels: map[string]string{
				"keystone.hexxlock.io/bundle":  exec.Spec.BundleRef,
				"keystone.hexxlock.io/version": exec.Spec.BundleVersion,
				"keystone.hexxlock.io/schema":  exec.Spec.SchemaRef,
				"keystone.hexxlock.io/auto":    "true",
			},
		},
		Spec: keystonev1alpha1.SchemaSnapshotSpec{
			SchemaRef: exec.Spec.SchemaRef,
			Reason: fmt.Sprintf("auto: applied %s@%s",
				exec.Spec.BundleRef, exec.Spec.BundleVersion),
			RetentionDays: 365,
		},
	}
	_ = controllerutil.SetControllerReference(exec, snap, r.Scheme)
	if err := r.Create(ctx, snap); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(exec, corev1.EventTypeWarning, "AutoSnapshotFailed",
				"could not create SchemaSnapshot %s: %v", name, err)
		}
		return
	}
	// Best-effort status stamp for AutoSource (so operators can
	// identify auto-snapshots in `kubectl get snapshots` without the
	// label). Separate patch since the reconciler owns the rest of
	// the status transition.
	patch := client.MergeFrom(snap.DeepCopy())
	snap.Status.AutoSource = fmt.Sprintf("%s@%s", exec.Spec.BundleRef, exec.Spec.BundleVersion)
	_ = r.Status().Patch(ctx, snap, patch)
}

// autoSnapshotName produces a deterministic snapshot name that is
// idempotent under retries. Trimmed to the K8s 253-char limit.
func autoSnapshotName(bundle, version, schema string) string {
	name := fmt.Sprintf("auto-%s-%s-%s", bundle, version, schema)
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

func (r *MigrationExecutionReconciler) markFailed(
	ctx context.Context,
	exec *keystonev1alpha1.MigrationExecution,
	result *migration.ApplyResult,
	cause error,
) (ctrl.Result, error) {
	patch := client.MergeFrom(exec.DeepCopy())
	exec.Status.ObservedGeneration = exec.Generation
	exec.Status.Phase = keystonev1alpha1.ExecutionPhaseFailed
	now := metav1.Now()
	exec.Status.CompletionTime = &now
	if result != nil {
		exec.Status.FailedAtFile = result.FailedFile
		exec.Status.FailedAtIndex = result.FailedIndex
		exec.Status.FailureMessage = truncate(result.FailureMsg, 4096)
	} else {
		exec.Status.FailureMessage = truncate(cause.Error(), 4096)
	}
	exec.Status.Conditions = conditions.MarkNotReady(exec.Status.Conditions, "ApplyFailed", cause, exec.Generation)
	if err := r.Status().Patch(ctx, exec, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch failure status: %w (cause: %v)", err, cause)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(exec, corev1.EventTypeWarning, "ApplyFailed", "%s", cause.Error())
	}
	r.emitAudit(ctx, exec, "apply", "ApplyFailed: "+cause.Error(), "error")
	return ctrl.Result{}, cause
}

// emitAudit appends a tamper-evident entry to the audit chain for a
// MigrationExecution Phase transition. Errors are logged at warn level
// rather than discarded — silent audit failure is the worst outcome
// for a tier-1 compliance ledger. Missing logger is a no-op (test
// flows don't plumb the audit chain).
func (r *MigrationExecutionReconciler) emitAudit(
	ctx context.Context,
	exec *keystonev1alpha1.MigrationExecution,
	verb, reason, outcome string,
) {
	if r.AuditLogger == nil {
		return
	}
	if _, _, err := r.AuditLogger.Append(ctx, audit.Event{
		Verb:        verb,
		Actor:       r.ManagerIdentity,
		ResourceRef: audit.ResourceRefFromObject(exec),
		Reason:      reason,
		Outcome:     outcome,
		After:       exec,
	}); err != nil {
		log.FromContext(ctx).Error(err, "audit append failed",
			"controller", "migrationexecution",
			"name", exec.Name,
			"verb", verb,
			"outcome", outcome)
	}
}

func (r *MigrationExecutionReconciler) resolveCredentials(ctx context.Context, p *keystonev1alpha1.DatabaseProvider) (resolvedCreds, error) {
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
		return resolvedCreds{}, fmt.Errorf("read admin Secret %s/%s: %w",
			r.SystemNamespace, ref.SecretName, err)
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

func (r *MigrationExecutionReconciler) fail(
	ctx context.Context, exec *keystonev1alpha1.MigrationExecution,
	reason string, cause error,
) (ctrl.Result, error) {
	patch := client.MergeFrom(exec.DeepCopy())
	exec.Status.ObservedGeneration = exec.Generation
	exec.Status.Conditions = conditions.MarkNotReady(exec.Status.Conditions, reason, cause, exec.Generation)
	if perr := r.Status().Patch(ctx, exec, patch); perr != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w (cause: %v)", perr, cause)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(exec, corev1.EventTypeWarning, reason, "%s", cause.Error())
	}
	return ctrl.Result{}, cause
}

func (r *MigrationExecutionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.SystemNamespace == "" {
		r.SystemNamespace = "keystone-system"
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keystone-migrationexecution")
	}
	if r.Resolver == nil {
		// APIReader (uncached) — the operator's ConfigMap cache is
		// label-scoped to SD-emitted bundles; user-authored
		// ConfigMap-sourced bundles fall outside that scope and must
		// be read direct from the apiserver. See cmd/manager/main.go
		// cache.Options.ByObject.
		r.Resolver = migration.NewConfigMapResolver(mgr.GetAPIReader())
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.MigrationExecution{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		// When a MigrationPlan's spec.approved flips, re-enqueue every
		// MigrationExecution referencing it. Without this watch, a
		// manual-mode plan approval would only take effect on the next
		// periodic requeue (up to 30s latency). With it, `keystonectl
		// plan approve` gives sub-second feedback.
		Watches(&keystonev1alpha1.MigrationPlan{},
			handler.EnqueueRequestsFromMapFunc(r.executionsForPlan)).
		Named("migrationexecution").
		Complete(r)
}

// executionsForPlan returns the MigrationExecutions in the plan's
// namespace whose spec.planRef matches the plan's name. Used by the
// watch wiring in SetupWithManager to propagate plan approvals
// without waiting for the periodic requeue.
func (r *MigrationExecutionReconciler) executionsForPlan(ctx context.Context, obj client.Object) []reconcile.Request {
	plan, ok := obj.(*keystonev1alpha1.MigrationPlan)
	if !ok {
		return nil
	}
	var execs keystonev1alpha1.MigrationExecutionList
	if err := r.List(ctx, &execs, client.InNamespace(plan.Namespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range execs.Items {
		e := &execs.Items[i]
		if e.Spec.PlanRef != plan.Name {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: e.Namespace, Name: e.Name,
		}})
	}
	return out
}

// isPlanApproved looks up the MigrationPlan referenced by a
// MigrationExecution and returns whether its gate is open. Returns
// (approved=true, "", nil) when the plan exists and spec.approved is
// true; (false, reason, nil) when the gate is closed; and (false, "",
// err) on an apiserver lookup error.
//
// A missing plan is treated as "not approved" rather than an error —
// a deleted plan should pause executions rather than fail them, so a
// re-creation path is available to operators.
func (r *MigrationExecutionReconciler) isPlanApproved(ctx context.Context, namespace, name string) (bool, string, error) {
	var plan keystonev1alpha1.MigrationPlan
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &plan); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("MigrationPlan %s/%s not found", namespace, name), nil
		}
		return false, "", err
	}
	if plan.Spec.Approved {
		return true, "", nil
	}
	mode := string(plan.Status.ApprovalMode)
	if mode == "" {
		mode = string(keystonev1alpha1.PlanApprovalAuto) + " (pending)"
	}
	return false, fmt.Sprintf("plan %s/%s awaiting approval (mode=%s)", namespace, name, mode), nil
}

// awaitingApproval marks the execution Pending + sets a condition
// describing what operators need to do to unblock. Requeues with a
// bounded backoff so a plan that will never be approved doesn't hot-
// loop; the plan watch (SetupWithManager) still fires on any approval
// flip for sub-second propagation.
func (r *MigrationExecutionReconciler) awaitingApproval(
	ctx context.Context,
	exec *keystonev1alpha1.MigrationExecution,
	reason string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(exec.DeepCopy())
	if exec.Status.Phase == "" {
		exec.Status.Phase = keystonev1alpha1.ExecutionPhasePending
	}
	exec.Status.Conditions = conditions.Set(exec.Status.Conditions,
		keystonev1alpha1.ConditionTypeReady, metav1.ConditionFalse,
		"AwaitingPlanApproval", reason, exec.Generation)
	if err := r.Status().Patch(ctx, exec, patch); err != nil {
		return ctrl.Result{}, err
	}
	// 60s is the compromise between "stuck-plan hot loop" and "plan
	// approved, waiting forever". The MigrationPlan watch propagates
	// any actual approval event immediately; this requeue exists only
	// for the case where the watch event was missed (leader election
	// handover, apiserver flake).
	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

// markRunning, markSucceeded, markFailed share the truncate helper to
// keep CR sizes manageable.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-32] + "… [truncated]"
}

// loggerKey is unused but reserved for adding zap-style structured fields
// from Reconcile in a future refactor that lifts logging into a sidecar.
type loggerKey struct{}

var _ logr.Logger // keep go-logr in import set; metric controllers use it
