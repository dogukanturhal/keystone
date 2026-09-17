// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MigrationStrategy controls how the MigrationController applies a bundle.
//
// +kubebuilder:validation:Enum=versioned;declarative;pgroll-expand-contract
type MigrationStrategy string

const (
	// StrategyVersioned applies sequential numbered SQL files (e.g.
	// 001_create_users.up.sql) inside a single transaction per file.
	// Compatible with golang-migrate file naming. Default.
	StrategyVersioned MigrationStrategy = "versioned"

	// StrategyDeclarative diffs the desired schema (declared as a single
	// CREATE TABLE / CREATE INDEX … bundle) against the live schema and
	// generates the ALTER chain. Mirrors SchemaHero / Atlas declarative
	// mode. Phase 6+ — not implemented in Phase 3.
	StrategyDeclarative MigrationStrategy = "declarative"

	// StrategyPgrollExpandContract uses pgroll's multi-version views to
	// stage column renames / drops without locking. Phase 6 implements
	// this via the embedded pgroll engine.
	StrategyPgrollExpandContract MigrationStrategy = "pgroll-expand-contract"
)

// ExecutionMode controls what a MigrationExecution does with the
// bundle's SQL.
//
// +kubebuilder:validation:Enum=Apply;RecordOnly
type ExecutionMode string

const (
	// ExecutionModeApply executes the bundle's SQL and records the
	// version in the schema's tracking table. Default.
	ExecutionModeApply ExecutionMode = "Apply"

	// ExecutionModeRecordOnly upserts the (version, contentHash) row
	// into the tracking table WITHOUT executing any SQL. This is the
	// adoption-baseline primitive (ADR 0008 / ADR 0027): the schema's
	// current live state is declared as already-applied so subsequent
	// incremental migrations build on top of it. Identical semantics
	// to `flyway baseline`, Liquibase `changelog-sync`, and
	// `atlas migrate apply --baseline`.
	//
	// Because a RecordOnly bundle never executes, execution-safety
	// lint findings are advisory: they are still published to
	// status.lintFindings but never block admission or reconcile.
	// Integrity (keystone.sum), per-version immutability, and
	// approval gates apply in full.
	ExecutionModeRecordOnly ExecutionMode = "RecordOnly"
)

// MigrationSourceType discriminates between the supported source layers.
//
// +kubebuilder:validation:Enum=ConfigMap;OCIArtifact;Git
type MigrationSourceType string

const (
	// SourceConfigMap reads SQL files from a Kubernetes ConfigMap. Each
	// data key is treated as a migration file; sorted lexicographically.
	// Recommended for module-internal migrations that ship with the
	// operator's GitOps tree.
	SourceConfigMap MigrationSourceType = "ConfigMap"

	// SourceOCIArtifact (Phase 4+) pulls a signed OCI artifact containing
	// the SQL files. Production model: every release builds an artifact,
	// cosign-signs it, and the controller verifies before applying.
	SourceOCIArtifact MigrationSourceType = "OCIArtifact"

	// SourceGit (Phase 4+) clones a Git ref and reads SQL from a path.
	// Useful for tenant-supplied migrations.
	SourceGit MigrationSourceType = "Git"
)

// MigrationSource describes where the controller should read SQL files.
// Exactly one of ConfigMapRef / OCIArtifactRef / GitRef must be set,
// matching Type.
type MigrationSource struct {
	// Type discriminates the source layer.
	//
	// +kubebuilder:validation:Required
	Type MigrationSourceType `json:"type"`

	// ConfigMapRef is required when Type=ConfigMap. Lookup is in the
	// same namespace as the MigrationBundle.
	//
	// +optional
	ConfigMapRef *MigrationConfigMapSource `json:"configMapRef,omitempty"`

	// OCIArtifactRef is required when Type=OCIArtifact. Points to an
	// OCI artifact in a registry (e.g., Harbor). The artifact's layers
	// contain SQL migration files. Credentials are resolved from an
	// imagePullSecret in the bundle's namespace.
	//
	// +optional
	OCIArtifactRef *MigrationOCISource `json:"ociArtifactRef,omitempty"`
}

// MigrationOCISource references an OCI artifact containing SQL files.
type MigrationOCISource struct {
	// Repository is the full OCI reference without the tag/digest,
	// e.g., "ghcr.io/dogukanturhal/migrations/billing".
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=512
	Repository string `json:"repository"`

	// Tag is the OCI tag to pull. Mutually exclusive with Digest.
	// When both are empty, defaults to "latest".
	//
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Tag string `json:"tag,omitempty"`

	// Digest is the OCI manifest digest (e.g., sha256:abc123...).
	// Preferred over Tag for immutable references. When set, Tag is
	// ignored.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^(sha256:[0-9a-f]{64})?$`
	Digest string `json:"digest,omitempty"`

	// FilePattern filters which files from the artifact are treated
	// as SQL migrations. Defaults to "*.up.sql".
	//
	// +kubebuilder:default="*.up.sql"
	// +kubebuilder:validation:MaxLength=64
	FilePattern string `json:"filePattern,omitempty"`

	// PullSecretRef names the Secret containing OCI registry
	// credentials. The Secret must have .dockerconfigjson data
	// (type kubernetes.io/dockerconfigjson) or plain username/password
	// keys. Looked up in the MigrationBundle's namespace.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PullSecretRef string `json:"pullSecretRef,omitempty"`

	// PlainHTTP allows pulling from registries without TLS. Default
	// false. Only set to true for development registries.
	//
	// +optional
	PlainHTTP bool `json:"plainHTTP,omitempty"`
}

// MigrationConfigMapSource references a ConfigMap holding SQL files.
type MigrationConfigMapSource struct {
	// Name of the ConfigMap.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// FilePattern filters which keys are treated as SQL files. Defaults
	// to `*.up.sql`.
	//
	// +kubebuilder:default="*.up.sql"
	// +kubebuilder:validation:MaxLength=64
	FilePattern string `json:"filePattern,omitempty"`
}

// MigrationBundleSpec describes a versioned set of SQL migrations that
// the controller should apply to one or more target schemas.
//
// The bundle is immutable in spirit: once a Version is applied, it is
// recorded in the target's schema_migrations table and the same Version
// must never carry different SQL. The admission webhook enforces this by
// computing a content hash and rejecting changes that try to overwrite
// an applied version.
type MigrationBundleSpec struct {
	// Version is the bundle's monotonically-increasing identifier.
	// Display-friendly (semver, date, or integer) but ordering is
	// lexicographic; pad if you mix lengths. Required.
	//
	// MaxLength bumped 64 → 253 (2026-05-20) to accommodate the
	// content-addressed bundle naming introduced in commit 372de32:
	// SchemaDefinitionReconciler emits bundles with
	// `<sd.Name>-declarative-<64-hex-hash>` which exceeds 64 bytes
	// for any SD name longer than a few characters. 253 matches the
	// Kubernetes resource-name limit (RFC 1123 subdomain), which is
	// the natural upper bound on these identifiers anyway.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._-]+$`
	Version string `json:"version"`

	// Strategy controls how migrations are applied. Defaults to
	// versioned; pgroll-expand-contract is Phase 6.
	//
	// +kubebuilder:default="versioned"
	Strategy MigrationStrategy `json:"strategy,omitempty"`

	// ExecutionMode selects between executing the bundle's SQL (Apply,
	// default) and recording it as already-applied without execution
	// (RecordOnly — the adoption-baseline path; see the ExecutionMode
	// type docs). Immutable once set: switching a shipped bundle
	// between modes would rewrite the meaning of its tracking-table
	// row. Requires strategy=versioned and autoRollback=false
	// (enforced at admission).
	//
	// +kubebuilder:default="Apply"
	// +optional
	ExecutionMode ExecutionMode `json:"executionMode,omitempty"`

	// Source declares where the SQL files come from. Required for
	// strategy=versioned. Mutually exclusive with Operations.
	//
	// +optional
	Source MigrationSource `json:"source,omitempty"`

	// DownSource is the optional rollback counterpart to Source. When
	// provided, the controller resolves it the same way as Source
	// (ConfigMap or OCI) and stores the resulting SQL in
	// MigrationExecution.status.reversalStatements. If AutoRollback is
	// also true, a Failed execution automatically transitions to
	// RollingBack and applies the down source.
	//
	// Convention: down files use *.down.sql pattern.
	//
	// +optional
	DownSource *MigrationSource `json:"downSource,omitempty"`

	// AutoRollback, when true and DownSource is present, causes a
	// Failed MigrationExecution to automatically transition to
	// RollingBack. The controller resolves the DownSource and applies
	// it to undo the forward migration. If the rollback itself fails,
	// the execution transitions to RollbackFailed (manual intervention
	// required).
	//
	// Default false — operators must explicitly opt in to automatic
	// rollback. Forward-fix is still the recommended approach for most
	// migration failures.
	//
	// +optional
	// +kubebuilder:default=false
	AutoRollback bool `json:"autoRollback,omitempty"`

	// Operations declares pgroll-style expand/contract operations.
	// Required for strategy=pgroll-expand-contract. Mutually exclusive
	// with Source. The MigrationController applies operations in array
	// order during Expand phase, then in REVERSE order during Contract
	// phase (so e.g. a constraint added in op[0] is validated before
	// the column it depends on is dropped in op[1]'s contract).
	//
	// +optional
	// +kubebuilder:validation:MaxItems=16
	Operations []MigrationOperation `json:"operations,omitempty"`

	// SchemaSelector picks the DatabaseSchemas this bundle applies to.
	// Standard label selector matched against DatabaseSchema.metadata.labels.
	// A bundle with an empty selector applies to nothing — refused at
	// admission to prevent accidental no-ops.
	//
	// +kubebuilder:validation:Required
	SchemaSelector metav1.LabelSelector `json:"schemaSelector"`

	// PolicyRef optionally pins the SchemaPolicy this bundle is
	// validated against. When unset, every SchemaPolicy whose
	// targetSelector matches applies (intersection-of-stricter rules).
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PolicyRef string `json:"policyRef,omitempty"`

	// RolloutPolicyRef optionally pins the staged-rollout policy that
	// the MigrationController uses to fan out executions. When unset
	// the bundle fans out to all matched schemas at once (the Phase 3
	// behaviour, kept for backward compatibility).
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	RolloutPolicyRef string `json:"rolloutPolicyRef,omitempty"`

	// Approval is metadata the admission webhook validates against
	// SchemaPolicy.spec.requireApproval. Each key matches a
	// requireApproval entry; the value MUST be a base64-encoded
	// signature from a CI job that ran in a CODEOWNERS-approved MR.
	//
	// +optional
	Approval map[string]string `json:"approval,omitempty"`

	// MaxConcurrentExecutions caps how many MigrationExecutions the
	// bundle fans out in parallel when rolloutPolicyRef is unset
	// (non-staged path). Staged bundles ignore this field and use
	// RolloutPolicy.stages[].parallelism instead.
	//
	// Default 0 means unlimited — every matched schema gets an
	// Execution on the first reconcile tick, preserving pre-B2
	// behaviour. Non-zero values throttle the fanout so reconcile
	// creates at most N Executions per tick and schedules the rest
	// on subsequent ticks as in-flight Executions complete.
	//
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=256
	MaxConcurrentExecutions int32 `json:"maxConcurrentExecutions,omitempty"`

	// Parallelism controls intra-bundle operation parallelism for the
	// pgroll-expand-contract strategy. Ops are grouped into "waves"
	// by table independence — all ops in a wave target distinct
	// Tables and run concurrently; a new wave starts when the previous
	// wave commits. Default 0 means fully sequential (pre-B3
	// behaviour); 1 is identical to 0; 2+ enables wave concurrency up
	// to the declared cap.
	//
	// Ignored for strategy=versioned — SQL files stay sequential
	// because Keystone does not parse SQL to analyse table
	// dependencies. See docs/adrs/0020 for the wave scheduler
	// rationale.
	//
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=16
	Parallelism int32 `json:"parallelism,omitempty"`
}

// MigrationBundleStatus reports rollout progress across all selected
// schemas.
type MigrationBundleStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions reports the current state.
	//   Ready   — all matched schemas applied or out-of-scope.
	//   Linted  — squawk/conftest passed; only set after the lint phase.
	//   Planned — at least one MigrationPlan has been created.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// MatchedSchemas is the count of DatabaseSchemas currently selected.
	// Recomputed on every reconcile.
	//
	// +optional
	MatchedSchemas int32 `json:"matchedSchemas,omitempty"`

	// AppliedSchemas is the count of those schemas where the bundle's
	// version is recorded as applied.
	//
	// +optional
	AppliedSchemas int32 `json:"appliedSchemas,omitempty"`

	// LastPlanTime is when the controller last generated a MigrationPlan
	// from this bundle.
	//
	// +optional
	LastPlanTime *metav1.Time `json:"lastPlanTime,omitempty"`

	// CurrentStage is the active rollout stage when a RolloutPolicy is
	// in effect. Empty when the bundle uses no policy.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=63
	CurrentStage string `json:"currentStage,omitempty"`

	// StageHistory records progress through each stage of the
	// referenced RolloutPolicy. Append-only.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=16
	StageHistory []StageProgress `json:"stageHistory,omitempty"`

	// LintFindings holds analyzer output from the plan phase. Populated
	// on every reconcile by the default analyzer registry (20 rules
	// today; configurable via SchemaPolicy in a later phase).
	// Cleared when no findings. Findings at severity >= "error" block
	// Ready=True.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=512
	LintFindings []LintFinding `json:"lintFindings,omitempty"`

	// ContentHash is the SHA-256 of the resolved source content at the
	// last reconcile. Used by the admission webhook to detect
	// version-pinning violations (same Version, different SQL).
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	ContentHash string `json:"contentHash,omitempty"`

	// Integrity reports the outcome of verifying the bundle's resolved
	// source against its committed keystone.sum sidecar. Populated on
	// every reconcile. Empty when the bundle has never been resolved.
	//
	// +optional
	Integrity *IntegrityStatus `json:"integrity,omitempty"`

	// Approval reports the outcome of evaluating the bundle against
	// every matched SchemaPolicy.spec.approvalPolicies. Populated on
	// every reconcile; empty when no matching policy declares approval
	// rules. The matching condition ConditionTypeApproved carries the
	// True/False/Unknown signal; this struct carries the per-policy
	// detail that operators need to triage "why isn't my bundle
	// admitting?" from kubectl describe alone.
	//
	// +optional
	Approval *ApprovalSummary `json:"approval,omitempty"`

	// SchemaProgress is the per-schema rollout breakdown — one entry
	// per DatabaseSchema matched by spec.schemaSelector. Populated on
	// every reconcile; stale entries (schema no longer matched) are
	// pruned. Used for "kubectl describe migrationbundle" visibility
	// and dashboards that need per-tenant granularity rather than the
	// aggregate matched/applied counts above.
	//
	// Capped at 256 entries so the status CR stays within etcd's
	// 1.5 MiB value-size ceiling even on large multi-tenant fanouts;
	// bundles matching more than 256 schemas must rely on the
	// per-execution MigrationExecution CR list for full visibility.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=256
	SchemaProgress []SchemaProgress `json:"schemaProgress,omitempty"`
}

// SchemaProgress is one entry in MigrationBundleStatus.SchemaProgress
// — the observed state of a single (bundle, version, schema) triple's
// MigrationExecution. Operators read this for per-tenant triage when
// a bundle fans out across dozens of tenant databases.
type SchemaProgress struct {
	// SchemaRef is the DatabaseSchema's metadata.name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	SchemaRef string `json:"schemaRef"`

	// TenantID carries the value of the DatabaseSchema's
	// keystone.hexxlock.io/tenant-id label when set. Empty when the
	// schema was not labelled. Surfaced so dashboards can group
	// rollouts by tenant without joining against ProductInstance.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	TenantID string `json:"tenantID,omitempty"`

	// ExecutionRef is the MigrationExecution.metadata.name associated
	// with this (bundle, version, schema) triple. Empty when the
	// execution has not yet been created (throttled, awaiting earlier
	// stage).
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ExecutionRef string `json:"executionRef,omitempty"`

	// Phase mirrors the MigrationExecution.status.phase at observation
	// time. Empty when no Execution exists yet.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=32
	Phase string `json:"phase,omitempty"`

	// Message is a short diagnostic — the first line of the
	// execution's Ready condition message. Empty when the execution
	// is in a nominal state.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Message string `json:"message,omitempty"`

	// StartedAt is the execution's StartTime (when Apply began).
	// Empty until the execution enters Running / Expanding.
	//
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is the execution's CompletionTime (when Succeeded,
	// Failed, or Aborted was set). Empty while in-flight.
	//
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// ApprovalSummary is the observable outcome of the N-of-M approval
// evaluation. The overall Satisfied flag mirrors
// ConditionTypeApproved; Policies carries per-ApprovalPolicy detail.
type ApprovalSummary struct {
	// Satisfied is true when every applicable ApprovalPolicy has
	// received its required approver count. False when any policy
	// fires and is under-quorum. True when no policy applies (the
	// approval gate did not block).
	//
	// +kubebuilder:validation:Required
	Satisfied bool `json:"satisfied"`

	// Policies is the per-(SchemaPolicy, ApprovalPolicy) breakdown,
	// sorted lexicographically for deterministic diffs. Entries for
	// policies that didn't fire (AppliesWhen=false) appear with
	// Applied=false so operators see the full rule set that was
	// considered.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Policies []PolicyApprovalState `json:"policies,omitempty"`
}

// PolicyApprovalState is one entry in ApprovalSummary.Policies.
type PolicyApprovalState struct {
	// SchemaPolicy is the owning SchemaPolicy.Name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	SchemaPolicy string `json:"schemaPolicy"`

	// ApprovalPolicy is the nested ApprovalPolicy.Name within
	// SchemaPolicy.spec.approvalPolicies.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	ApprovalPolicy string `json:"approvalPolicy"`

	// Applied is true when AppliesWhen fired against this bundle.
	// When false, Required / Received / Satisfied carry no meaning —
	// the entry is listed for visibility only.
	Applied bool `json:"applied"`

	// Required is the count of distinct approvers the policy demands.
	//
	// +optional
	Required int32 `json:"required,omitempty"`

	// Received is the count of distinct valid approvers counted:
	// those whose annotation group appears in FromGroups, minus the
	// author when DisallowSelfApproval is set.
	//
	// +optional
	Received int32 `json:"received,omitempty"`

	// Approvers lists the approver-ids that counted toward Received,
	// sorted. Useful for audit trails and kubectl describe.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Approvers []string `json:"approvers,omitempty"`

	// Satisfied is Received >= Required for policies where Applied is
	// true. Always false when Applied is false.
	Satisfied bool `json:"satisfied"`

	// Violation is a human-readable diagnostic when Satisfied=false:
	// what was expected, what was received. Empty on satisfaction.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Violation string `json:"violation,omitempty"`
}

// IntegrityStatus is the observable outcome of the keystone.sum
// verification performed at resolve time. The matching condition
// ConditionTypeIntegrityVerified carries the True/False/Unknown signal;
// this struct carries the detail tooling needs to triage.
type IntegrityStatus struct {
	// Status is the outcome classification. One of:
	//   Valid      — keystone.sum present and every hash matches
	//   Missing    — no keystone.sum in the resolved source
	//   Malformed  — keystone.sum present but failed to parse
	//   Mismatch   — keystone.sum present but hashes diverge
	//
	// +kubebuilder:validation:Enum=Valid;Missing;Malformed;Mismatch
	// +kubebuilder:validation:Required
	Status string `json:"status"`

	// RootHash is the directory-level SHA-256 declared by keystone.sum.
	// Empty when Status=Missing. Surfaced so operators can grep audit
	// logs for a specific bundle snapshot.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	RootHash string `json:"rootHash,omitempty"`

	// Message is a human-readable detail. For Mismatch this names the
	// first divergent file; for Malformed it quotes the parse error.
	// Empty for Valid and Missing.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Message string `json:"message,omitempty"`

	// Files carries per-file integrity entries as reported by
	// keystone.sum. Present only when Status=Valid — we do not surface
	// partial sums for Malformed/Mismatch because clients may act on
	// stale data. Empty on Missing.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	Files []FileIntegrity `json:"files,omitempty"`
}

// FileIntegrity is one entry surfaced from a verified keystone.sum.
// Deliberately minimal — the sum file in Git is the authoritative
// artifact; this slice exists for kubectl describe / dashboards.
type FileIntegrity struct {
	// Name is the filename as it appears in keystone.sum.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Hash is the hex-encoded SHA-256 recorded for this file.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=64
	Hash string `json:"hash"`
}

// StageProgress is one entry in MigrationBundleStatus.StageHistory.
type StageProgress struct {
	// Name matches RolloutStage.Name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// StartedAt is when the controller first dispatched executions
	// for this stage.
	//
	// +kubebuilder:validation:Required
	StartedAt metav1.Time `json:"startedAt"`

	// SoakStartedAt is when the controller observed all stage targets
	// reach Succeeded and started the soak timer. Empty until that
	// happens.
	//
	// +optional
	SoakStartedAt *metav1.Time `json:"soakStartedAt,omitempty"`

	// CompletedAt is when the controller advanced past this stage
	// (soak elapsed + approval received if required). Empty until
	// completion.
	//
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Targets is the count of DatabaseSchemas selected by this stage.
	//
	// +optional
	Targets int32 `json:"targets,omitempty"`

	// Succeeded is the count of those that reached Succeeded. When
	// Targets == Succeeded the soak timer starts.
	//
	// +optional
	Succeeded int32 `json:"succeeded,omitempty"`

	// Failed is the count of executions that ended in Failed. When >0
	// AND the stage's AbortOnFailure is true, the rollout freezes.
	//
	// +optional
	Failed int32 `json:"failed,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=mb;bundle
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.version"
// +kubebuilder:printcolumn:name="Strategy",type="string",JSONPath=".spec.strategy"
// +kubebuilder:printcolumn:name="Matched",type="integer",JSONPath=".status.matchedSchemas"
// +kubebuilder:printcolumn:name="Applied",type="integer",JSONPath=".status.appliedSchemas"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MigrationBundle declares a versioned set of SQL migrations. The
// MigrationController reads it, generates per-target MigrationPlans,
// then per-target MigrationExecutions.
type MigrationBundle struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MigrationBundleSpec   `json:"spec,omitempty"`
	Status MigrationBundleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MigrationBundleList is the list wrapper required by client-go.
type MigrationBundleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MigrationBundle `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MigrationBundle{}, &MigrationBundleList{})
}
