// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Standard condition types used across all Keystone CRDs.
//
// These follow the Kubernetes API convention (metav1.Condition) so that
// generic tooling (kubectl wait, ArgoCD health checks, OpenTelemetry
// collectors) can reason about resource state without per-type knowledge.
//
// Every controller MUST set ConditionTypeReady. Resource-specific extra
// conditions (e.g. ConditionTypeApplied for migrations, ConditionTypeDriftFree
// for schemas) are layered on top.
const (
	// ConditionTypeReady is the top-level "is this resource healthy" signal.
	// True  — desired state has been observed and reconciled successfully.
	// False — last reconciliation failed; see Reason/Message.
	// Unknown — controller has not yet observed the resource.
	ConditionTypeReady = "Ready"

	// ConditionTypeAvailable indicates the resource is in a usable state for
	// dependents. For schemas, that means the schema exists and is grantable;
	// for migrations, that means the target schema is at the declared version.
	ConditionTypeAvailable = "Available"

	// ConditionTypeProgressing is set while a long-running operation is in
	// flight (migration applying, cluster registering). Mirrors the Deployment
	// convention so ArgoCD's built-in health checks behave correctly.
	ConditionTypeProgressing = "Progressing"

	// ConditionTypeDegraded indicates the resource is partially functional
	// but has problems that operators should investigate. The resource may
	// still be Ready=True if the degradation is non-fatal.
	ConditionTypeDegraded = "Degraded"
)

// Standard condition reasons. Reason MUST be a single CamelCase token; long
// explanation goes in Message.
const (
	ReasonReconciling      = "Reconciling"
	ReasonReconciled       = "Reconciled"
	ReasonReconcileFailed  = "ReconcileFailed"
	ReasonValidationFailed = "ValidationFailed"
	ReasonAwaitingProvider = "AwaitingProvider"
	ReasonDeleting         = "Deleting"
)

// Finalizers used by the Keystone controllers. Each controller must register
// its finalizer before mutating external state, and must remove it only after
// confirming the external state has been cleaned up.
const (
	FinalizerClusterRegistration = "keystone.hexxlock.io/clusterregistration"
	FinalizerLogicalDatabase     = "keystone.hexxlock.io/logicaldatabase"
	FinalizerDatabaseSchema      = "keystone.hexxlock.io/databaseschema"
	FinalizerDatabaseProvider    = "keystone.hexxlock.io/databaseprovider"
	FinalizerMigrationBundle     = "keystone.hexxlock.io/migrationbundle"
	FinalizerMigrationExecution  = "keystone.hexxlock.io/migrationexecution"
	FinalizerProductInstance     = "keystone.hexxlock.io/productinstance"
)

// DeletionPolicy values shared across resources that own external state.
const (
	DeletionPolicyRetain = "Retain"
	DeletionPolicyDelete = "Delete"
)

// Migration-specific condition types layered on top of the standard set
// (Ready / Available / Progressing / Degraded).
const (
	// ConditionTypeLinted reports whether squawk + conftest passed for
	// a MigrationBundle / MigrationPlan. False blocks promotion to
	// MigrationExecution.
	ConditionTypeLinted = "Linted"

	// ConditionTypeApproved reports whether all matched SchemaPolicies
	// and the Kyverno admission gate accepted the bundle.
	ConditionTypeApproved = "Approved"

	// ConditionTypePlanned reports whether at least one MigrationPlan
	// has been generated from a MigrationBundle.
	ConditionTypePlanned = "Planned"

	// ConditionTypeApplied reports a MigrationExecution committed every
	// statement and recorded the version.
	ConditionTypeApplied = "Applied"

	// ConditionTypeDriftFree reports the live schema matches the
	// recorded baseline. Set by the DriftController on every check.
	ConditionTypeDriftFree = "DriftFree"

	// ConditionTypeIntegrityVerified reports whether a MigrationBundle's
	// resolved source passed the keystone.sum integrity check.
	//   True  — reason=SumValid; every file matches the committed sum.
	//   False — reason ∈ {SumMissing, SumMalformed, SumMismatch}. Missing
	//           only flips to False when SchemaPolicy.spec.integrity
	//           .requireSumFile=true; otherwise it is Unknown.
	//   Unknown — sum is not present and policy does not require one.
	ConditionTypeIntegrityVerified = "IntegrityVerified"

	// ConditionTypeCredentialsReady reports whether a DatabaseProvider's
	// admin Secret is present and carries the required keys. Flipped
	// by DatabaseProviderReconciler on every admin-Secret observation.
	//   True  — secret exists with valid username/password keys
	//   False — secret missing, unreadable, or missing the required keys
	ConditionTypeCredentialsReady = "CredentialsReady"
)

// SchemaDefinition pipeline-stage conditions. These describe the three
// stages of a single reconcile — read the live schema, diff it against
// desired, emit a bundle for the delta — and are re-stated on every
// terminal path, not latched. A reader comparing them against Ready
// must be able to trust that all four describe the same reconcile.
const (
	// ConditionTypeInspected reports the live schema was read.
	//   True  — reason=LiveSchemaRead; every matched schema inspected.
	//   False — reason=NoMatchingSchemas; nothing to inspect.
	ConditionTypeInspected = "Inspected"

	// ConditionTypeDiffComputed reports the differ produced a plan.
	// True with reason=NoDrift means a plan was computed and was empty —
	// that is a successful diff, not an absent one.
	//   True  — reason ∈ {DriftDetected, NoDrift, DestructiveRefused,
	//           MixedVersionsDetected}
	//   False — reason=NoMatchingSchemas; no diff was attempted.
	ConditionTypeDiffComputed = "DiffComputed"

	// ConditionTypeBundleGenerated reports a MigrationBundle was emitted
	// for the computed delta. It tracks Status.CurrentBundleRef: False
	// whenever that field is empty, whatever the reason.
	//   True  — reason=BundleEmitted
	//   False — reason ∈ {NoBundleNeeded, DestructiveRefused,
	//           MixedVersionsDetected, NoMatchingSchemas}
	ConditionTypeBundleGenerated = "BundleGenerated"
)

// DriftReport condition types.
const (
	// ConditionTypeDetected reports that the live schema still diverges
	// from the recorded baseline. Re-stated on every sweep that
	// re-observes the drift.
	ConditionTypeDetected = "Detected"

	// ConditionTypeAccepted explains what became of an operator's
	// accept-drift request. It only ever appears as False: a successful
	// acceptance re-baselines and deletes the report, so there is no
	// object left to carry a True. The condition exists precisely to
	// give a request that did NOT take effect somewhere to say so.
	//   False — reason ∈ {MalformedAnnotation, SchemaNotFound,
	//           SchemaNotReady, RebaselineFailed}
	ConditionTypeAccepted = "Accepted"
)

// Reasons published on ConditionTypeIntegrityVerified. Held as a single
// CamelCase token per the Kubernetes condition convention.
const (
	ReasonSumValid     = "SumValid"
	ReasonSumMissing   = "SumMissing"
	ReasonSumMalformed = "SumMalformed"
	ReasonSumMismatch  = "SumMismatch"
)
