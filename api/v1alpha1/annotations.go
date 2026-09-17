// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Annotation keys carried on MigrationBundle objects. These are the
// string contract between CI / authors / webhook / controller —
// defining them here keeps the references symbol-tracked and catches
// typos at compile time.
const (
	// AnnotationAuthor carries the identity of the person or CI
	// principal who authored the bundle. Used by the approval engine
	// to enforce DisallowSelfApproval — the author cannot also appear
	// in the set of approvers.
	//
	// Value shape: a stable identity string (email, GitLab username,
	// OIDC sub). Opaque to Keystone beyond equality comparison.
	AnnotationAuthor = "keystone.hexxlock.io/author"

	// AnnotationRiskTier carries the author-declared risk tier for
	// the bundle. Matched against SchemaPolicy's
	// ApprovalCondition.RiskTier field: a policy with RiskTier="high"
	// fires only for bundles whose AnnotationRiskTier equals "high".
	//
	// Typical values: low | medium | high | critical. Free-form —
	// Keystone does no enumeration beyond the pattern constraint
	// declared on the CRD field.
	AnnotationRiskTier = "keystone.hexxlock.io/risk-tier"

	// AnnotationApprovalPrefix prefixes the annotation keys that
	// carry approval evidence. Full key shape:
	//   keystone.hexxlock.io/approval.<policy-name>.<approver-id>
	//
	// Annotation value is the identity group the approver belongs to
	// (must appear in the matched ApprovalPolicy.FromGroups). A value
	// that does not match any allowed group is counted as an invalid
	// approval and contributes zero toward the RequiredApprovers
	// threshold.
	//
	// The approver-id must be unique per policy; two annotations with
	// the same approver-id count as a single approver regardless of
	// how many groups they list.
	AnnotationApprovalPrefix = "keystone.hexxlock.io/approval."

	// LabelTenantID is the canonical tenant-identity label on a
	// DatabaseSchema. When set, the MigrationBundle controller copies
	// the value into Status.SchemaProgress[].TenantID on every
	// reconcile so dashboards can group fanouts by tenant without
	// joining against ProductInstance.
	//
	// Label, not annotation — tenant-id is a selectable cohort
	// attribute. Operators can write RolloutPolicy stages like
	// `schemaSelector: { tier: canary, tenant-id: pilot-customer }`
	// to roll out to one tenant first.
	//
	// Convention is not enforced — bundles referencing unlabelled
	// schemas still reconcile, but SchemaProgress entries carry an
	// empty TenantID and dashboards lose the tenant dimension. See
	// docs/adrs/0019 for the rollout guidance.
	LabelTenantID = "keystone.hexxlock.io/tenant-id"

	// LabelSchemaDefinition is set on every ConfigMap and MigrationBundle
	// the SchemaDefinitionReconciler emits, with the parent SchemaDefinition's
	// Name as the value. Two purposes:
	//
	//   1. Lets operators query via `kubectl get cm -l keystone.hexxlock.io/
	//      schemadefinition=<sd-name>` to inspect the auto-emitted SQL
	//      bundles for one SD.
	//
	//   2. Acts as the cache-inclusion predicate the keystone-manager's
	//      controller-runtime cache uses to scope its ConfigMap watch —
	//      without this label, the cache would have to hold every
	//      ConfigMap in the cluster (hundreds, often each MB-sized due
	//      to last-applied-configuration annotations) just to satisfy
	//      the SchemaDefinition's Owns() relationship to its emitted
	//      child CMs. With this label-existence selector wired into
	//      cache.Options.ByObject, only Keystone-emitted CMs are
	//      cached, while user-authored bundle ConfigMaps remain
	//      resolvable via APIReader (uncached) at admission and
	//      reconcile time.
	LabelSchemaDefinition = "keystone.hexxlock.io/schemadefinition"

	// LabelSchemaRef is set on every ConfigMap and MigrationBundle the
	// SchemaDefinitionReconciler emits, with the matched DatabaseSchema's
	// Name as the value. Used by SchemaPolicy/RolloutPolicy selectors and
	// by operators inspecting per-schema bundle activity.
	LabelSchemaRef = "keystone.hexxlock.io/schemaref"

	// LabelSource identifies the origin of a Keystone-managed object. The
	// SchemaDefinitionReconciler stamps `declarative-diff` on its emitted
	// ConfigMaps and MigrationBundles. Future authoring paths (e.g.
	// imperative-bundle-with-keystonectl-render) will use distinct values
	// so dashboards can split metrics by authoring path.
	LabelSource = "keystone.hexxlock.io/source"

	// LabelScope is the canonical product/team-scope label on Keystone
	// CRs. SchemaPolicy.spec.targetSelector typically matches by this
	// label so a single policy can govern every bundle (hand-authored
	// or SD-emitted) for one product.
	//
	// The SchemaDefinition reconciler propagates this label from the
	// SD to every emitted MigrationBundle so policy-matching works
	// for both authoring paths uniformly.
	//
	// Convention values: `example-service`, `example-suite`, `talon`, etc.
	LabelScope = "keystone.hexxlock.io/scope"

	// AnnotationSDFingerprint identifies the SchemaDefinition's
	// Status.Fingerprint at the moment a MigrationBundle was emitted.
	// The SchemaDefinition reconciler stamps it on every emitted bundle;
	// the MigrationExecution reconciler reads it on Phase=Succeeded to
	// stamp DatabaseSchema.Status.LastAppliedFingerprint = annotation
	// value, enabling identity-based per-schema convergence checks
	// (pkg/sdk/keystone.WaitSchemaFingerprint).
	//
	// Hand-authored MigrationBundles don't carry this annotation;
	// MigrationExecution reconciler skips the stamp in that case.
	AnnotationSDFingerprint = "keystone.hexxlock.io/sd-fingerprint"
)
