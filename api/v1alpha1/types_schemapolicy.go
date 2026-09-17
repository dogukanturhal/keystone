// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LintLevel is the severity threshold above which lint findings cause the
// admission webhook to reject a MigrationBundle.
//
// +kubebuilder:validation:Enum=error;warning;notice
type LintLevel string

const (
	LintLevelError   LintLevel = "error"
	LintLevelWarning LintLevel = "warning"
	LintLevelNotice  LintLevel = "notice"
)

// SchemaPolicySpec describes the rules that apply to MigrationBundles
// matching this policy. Multiple policies may apply to the same bundle;
// when they do, the strictest setting wins per-rule.
type SchemaPolicySpec struct {
	// TargetSelector picks the MigrationBundles this policy governs.
	// Match is by metadata.labels using standard label selectors.
	//
	// +kubebuilder:validation:Required
	TargetSelector metav1.LabelSelector `json:"targetSelector"`

	// MinLintLevel is the lowest severity that fails admission. Default
	// is `error` — warnings and notices surface in status but do not
	// block. Set to `warning` to gate stricter, or `notice` for the
	// strictest possible.
	//
	// +kubebuilder:default="error"
	MinLintLevel LintLevel `json:"minLintLevel,omitempty"`

	// AllowedStrategies is the set of MigrationBundle.spec.strategy
	// values this policy permits. Empty means any. Use to forbid raw
	// `versioned` migrations on production schemas, requiring
	// `pgroll-expand-contract` for zero-downtime guarantees.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	AllowedStrategies []string `json:"allowedStrategies,omitempty"`

	// MaxStatementsPerMigration caps a single migration file's statement
	// count. Long migrations correlate with long-running locks; the cap
	// pushes operators toward smaller, reversible chunks.
	//
	// +kubebuilder:default=50
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	MaxStatementsPerMigration int32 `json:"maxStatementsPerMigration,omitempty"`

	// RequireApproval lists annotations that the admission webhook must
	// see on the MigrationBundle. Each entry is the annotation key; the
	// value of the annotation must match the GitLab CODEOWNERS approval
	// signature posted by CI. Empty list = no approval gate (suitable
	// for canary tier).
	//
	// Deprecated: Use ApprovalPolicies for N-of-M / role-based / risk-
	// tier-conditional approvals. RequireApproval is kept for backward
	// compatibility and will be removed in v1beta1.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	RequireApproval []string `json:"requireApproval,omitempty"`

	// ApprovalPolicies is the N-of-M approval workflow. Every matched
	// MigrationBundle must satisfy each policy in turn before
	// admission accepts it. A bundle with zero policies bypasses this
	// gate — suitable for canary tiers; production tiers ship at
	// least one policy.
	//
	// Each policy declares:
	//   - RequiredApprovers — count of distinct approvers needed
	//   - FromGroups        — identity groups that may satisfy the count
	//   - AppliesWhen       — conditional expression on bundle metadata
	//                         (label selector + lint-finding threshold)
	//
	// Satisfaction evidence lives under MigrationBundle.metadata.annotations
	// with keys of the form:
	//   keystone.hexxlock.io/approval.<policy-name>.<approver-id>=<token>
	//
	// Tokens are short-lived signed JWTs issued by the Keystone
	// approval-issuer webhook (Phase 12.2). Today we accept raw
	// CODEOWNERS signatures; JWT issuance lands with the UI.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=16
	ApprovalPolicies []ApprovalPolicy `json:"approvalPolicies,omitempty"`

	// BlockedKeywords is a list of SQL keywords the policy refuses to
	// permit. Common values: DROP TABLE, DROP COLUMN, TRUNCATE,
	// DROP DATABASE. Case-insensitive substring match. Bypass is by
	// adding `keystone.hexxlock.io/policy-override: true` annotation
	// + a justification (validated separately by Kyverno).
	//
	// +optional
	// +kubebuilder:validation:MaxItems=32
	BlockedKeywords []string `json:"blockedKeywords,omitempty"`

	// Integrity opts in to keystone.sum-based directory integrity
	// enforcement for matched bundles. Default (nil) is advisory — the
	// controller still verifies any committed sum, but a missing sum
	// does not block admission. See docs/adrs/0015 for rollout guidance.
	//
	// +optional
	Integrity *IntegrityPolicy `json:"integrity,omitempty"`

	// DevDatabaseRef is an optional reference to a DatabaseProvider
	// that hosts a dev database for semantic migration analysis. When
	// set, the MigrationBundle reconciler replays migrations against
	// an ephemeral schema in this database to catch semantic errors
	// that static analysis misses (invalid SQL, type mismatches, FK
	// reference errors, etc.). This is Keystone's equivalent of
	// Atlas's --dev-url pattern.
	//
	// The referenced DatabaseProvider should point to a dedicated dev
	// database — NOT a production database. The reconciler creates
	// temporary schemas (ks_dev_*) and drops them after each lint run.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	DevDatabaseRef string `json:"devDatabaseRef,omitempty"`

	// PlanApproval selects the Plan-Gate-Apply mode for matched
	// MigrationBundles. Auto (default) flips each plan's
	// spec.approved=true automatically so executions fire without a
	// human in the loop. Manual leaves plans at spec.approved=false
	// until a reviewer explicitly approves — the canonical production
	// gate. When multiple matched policies disagree, Manual wins
	// (strictest rule). See docs/adrs/0016 for the approval trust model
	// and docs/adrs/0017 for the Plan-Gate-Apply lifecycle.
	//
	// +optional
	// +kubebuilder:validation:Enum=Auto;Manual
	PlanApproval PlanApprovalMode `json:"planApproval,omitempty"`

	// Expressions is a list of CEL (Common Expression Language) rules
	// the webhook evaluates at admission time against every matched
	// MigrationBundle. Each rule's expression must evaluate to a bool;
	// true means the rule fires (warning or rejection, depending on
	// severity). See docs/adrs/0023 for the activation variable shape
	// and examples.
	//
	// CEL rules coexist with hardcoded fields above — they don't
	// replace AllowedStrategies / BlockedKeywords / etc. Use CEL for
	// policy dimensions Keystone doesn't model as first-class fields
	// ("forbid prod bundles that include DROP COLUMN AND have no
	// approval annotation", "reject any bundle missing a Jira ticket
	// label", etc.).
	//
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Expressions []CELRule `json:"expressions,omitempty"`
}

// CELRule is one Common Expression Language rule evaluated at
// admission time. Expressions return a bool — true means "this rule
// fires". Severity controls whether the fire blocks admission
// (Error) or surfaces as a warning (Warning).
//
// Activation variables available to expressions:
//
//   - bundle:       MigrationBundle fields (metadata, spec)
//   - findings:     []LintFinding from the analyzer pass
//   - strategy:     shortcut for bundle.spec.strategy
//   - labels:       shortcut for bundle.metadata.labels (map)
//   - annotations:  shortcut for bundle.metadata.annotations (map)
//   - version:      shortcut for bundle.spec.version
//
// Example expressions:
//
//   - `strategy == "versioned" && size(bundle.spec.operations) > 0`
//   - `labels["tier"] == "prod" && findings.exists(f, f.severity == "warning")`
//   - `!("keystone.hexxlock.io/jira" in annotations)`
type CELRule struct {
	// Name is the rule's stable identifier. Surfaced in rejection
	// messages and warning output. Required.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Expression is the CEL source. Must compile against Keystone's
	// activation variables and return a boolean. Rules that fail to
	// compile produce a ConfigurationError at admission — they do
	// NOT silently disable.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=4096
	Expression string `json:"expression"`

	// Severity selects the admission action when the expression
	// evaluates true:
	//
	//   - Warning: appended to admission warnings; bundle admits.
	//   - Error:   rejects admission with Message + expression text.
	//
	// Defaults to Error (strictest outcome) so forgetting severity
	// fails loud.
	//
	// +kubebuilder:default=Error
	// +kubebuilder:validation:Enum=Warning;Error
	Severity CELSeverity `json:"severity,omitempty"`

	// Message is the human-readable rejection/warning text. Keep it
	// specific — operators reading admission output want to know
	// what's wrong and how to fix. Leave empty for a generic
	// "<rule-name> fired" message.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}

// CELSeverity is the admission action taken when a rule fires.
//
// +kubebuilder:validation:Enum=Warning;Error
type CELSeverity string

const (
	// CELSeverityWarning surfaces the rule as an admission warning
	// (kubectl prints it above the accepted/rejected line) but does
	// not block admission.
	CELSeverityWarning CELSeverity = "Warning"

	// CELSeverityError blocks admission with the rule's message.
	CELSeverityError CELSeverity = "Error"
)

// IntegrityPolicy governs how strictly the admission webhook reacts to
// a MigrationBundle's keystone.sum state. Keep the surface small: the
// webhook has two dimensions of discretion — "require the file at all"
// and "reject on mismatch" — and both collapse to straightforward
// booleans. Richer CEL-based policy lands in Phase C2.
type IntegrityPolicy struct {
	// RequireSumFile rejects any matched MigrationBundle whose resolved
	// source does not contain a keystone.sum. Default false — Phase A1
	// ships in advisory mode so existing bundles keep admitting while
	// authors add sums. Flip to true in Phase A3 once every bundle in
	// the cluster carries a sum.
	//
	// +kubebuilder:default=false
	RequireSumFile bool `json:"requireSumFile,omitempty"`
}

// ApprovalPolicy is one row in a SchemaPolicy's N-of-M approval
// workflow. Modelled after PCI DSS Req 6.5 / SOC 2 CC8 change-
// management segregation-of-duties: an author cannot approve their
// own change; approvals must come from named identity groups; the
// apiserver records the approver's identity (see AuditEntry).
//
// Conditional application lets operators declare, e.g.:
//   "production tier bundles that touch the 'users' schema need 2
//    approvals from group 'data-platform-seniors'"
//   "any bundle with ≥1 warning lint finding needs 1 approval from
//    group 'schema-reviewers'"
type ApprovalPolicy struct {
	// Name is the stable identifier used in the annotation-key pattern
	// `keystone.hexxlock.io/approval.<name>.<approver>=<token>`.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// RequiredApprovers is the count of distinct approver identities
	// required to satisfy this policy. One identity cannot count
	// twice regardless of which groups they belong to.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	RequiredApprovers int32 `json:"requiredApprovers"`

	// FromGroups is the allowed identity groups. An approver's
	// membership in any listed group satisfies the "from" clause.
	// Enforced by the approval-issuer webhook — the admission webhook
	// accepts the signed token without re-checking group membership
	// (JWT carries the subject + group claims at issuance time).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	FromGroups []string `json:"fromGroups"`

	// AppliesWhen is the conditional gate that decides whether this
	// policy fires against a given bundle. Default (empty) = always.
	//
	// +optional
	AppliesWhen *ApprovalCondition `json:"appliesWhen,omitempty"`

	// DisallowSelfApproval requires at least one approver to differ
	// from the bundle's author (recorded in metadata.annotations
	// under `keystone.hexxlock.io/author`). Default true — mandatory
	// separation of duties per SOC 2 CC8. Set to false only for
	// non-production tiers with documented compensating controls.
	//
	// +kubebuilder:default=true
	DisallowSelfApproval bool `json:"disallowSelfApproval,omitempty"`
}

// ApprovalCondition is the conditional gate on an ApprovalPolicy.
// A policy fires when ALL fields here match.
type ApprovalCondition struct {
	// BundleSelector is a label selector on the MigrationBundle. An
	// empty selector matches every bundle.
	//
	// +optional
	BundleSelector *metav1.LabelSelector `json:"bundleSelector,omitempty"`

	// MinLintSeverity, if set, fires the policy only when the bundle
	// has at least one lint finding at or above this severity. Useful
	// for "needs senior review when there's any warning."
	//
	// +optional
	MinLintSeverity LintLevel `json:"minLintSeverity,omitempty"`

	// RiskTier names the risk category the bundle must carry to
	// trigger this policy. Read from the annotation
	// `keystone.hexxlock.io/risk-tier` on the MigrationBundle. Typical
	// values: low, medium, high, critical. Empty = any tier.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	RiskTier string `json:"riskTier,omitempty"`
}

// SchemaPolicyStatus tracks how often the policy fires.
type SchemaPolicyStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions reports the current state.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// MatchingBundles is the count of MigrationBundles currently matched
	// by this policy. Surfaced for ops visibility — a policy with zero
	// matches is dead code.
	//
	// +optional
	MatchingBundles int32 `json:"matchingBundles,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=sp;schemapol
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="MinLevel",type="string",JSONPath=".spec.minLintLevel"
// +kubebuilder:printcolumn:name="Bundles",type="integer",JSONPath=".status.matchingBundles"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SchemaPolicy declares the rules MigrationBundles must satisfy. Cluster-
// scoped because policies are platform-wide governance.
type SchemaPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SchemaPolicySpec   `json:"spec,omitempty"`
	Status SchemaPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SchemaPolicyList is the list wrapper required by client-go.
type SchemaPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SchemaPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SchemaPolicy{}, &SchemaPolicyList{})
}
