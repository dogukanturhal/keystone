// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolloutStage describes one tier in a staged rollout pipeline. The
// MigrationController processes stages in array order; advancement is
// gated by exit criteria (all targets succeeded + soak elapsed +
// optional approval).
type RolloutStage struct {
	// Name is a stable identifier for the stage. Used in
	// status.stageHistory keys and in the approval annotation
	// (keystone.hexxlock.io/approve-stage-<name>=true).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	Name string `json:"name"`

	// SchemaSelector picks which DatabaseSchemas this stage targets,
	// among the bundle's already-matched set. Stages typically select
	// by tier: matchLabels: {tier: canary}. Empty selector matches
	// everything left over after prior stages — useful for a "rest"
	// stage at the end.
	//
	// +optional
	SchemaSelector metav1.LabelSelector `json:"schemaSelector,omitempty"`

	// ClusterTierSelector restricts the stage to schemas whose
	// LogicalDatabase.spec.clusterRef points at a ClusterRegistration
	// with one of these tiers. Empty = no cluster-tier restriction.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=4
	ClusterTierSelector []ClusterTier `json:"clusterTierSelector,omitempty"`

	// Parallelism caps how many MigrationExecutions can be
	// in-flight (Pending/Running/Expanding/Contracting) inside this
	// stage at once. Smaller = safer but slower. Default 1 — the
	// controller refuses to default to "all at once" because that's
	// the failure mode this whole CRD exists to prevent.
	//
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Parallelism int32 `json:"parallelism,omitempty"`

	// SoakDuration is how long the controller waits AFTER all stage
	// targets reach Succeeded before advancing to the next stage. Use
	// to give SLO/error-budget signals time to register before
	// promoting. Format: Go duration string (e.g. "10m", "1h").
	//
	// +kubebuilder:default="10m"
	// +kubebuilder:validation:MaxLength=16
	SoakDuration string `json:"soakDuration,omitempty"`

	// RequireApproval gates advancement to the NEXT stage on a
	// human acknowledgement. Operator sets the annotation
	// keystone.hexxlock.io/approve-stage-<this.Name>=true on the
	// MigrationBundle to release the gate. Default false — most
	// non-paid stages don't need it.
	//
	// +kubebuilder:default=false
	RequireApproval bool `json:"requireApproval,omitempty"`

	// AbortOnFailure controls what happens when a MigrationExecution
	// in this stage fails. true (default) = freeze the rollout in
	// this stage; the operator must investigate and resolve before
	// advancement. false = continue to the next stage despite
	// failures (rare; useful for best-effort cleanup migrations).
	//
	// +kubebuilder:default=true
	AbortOnFailure bool `json:"abortOnFailure,omitempty"`
}

// RolloutPolicySpec describes a reusable staged-rollout pipeline that
// MigrationBundles can reference via spec.rolloutPolicyRef. Cluster-
// scoped because policies are platform-wide governance.
type RolloutPolicySpec struct {
	// Stages is the ordered pipeline. Empty = error at admission
	// (use spec.source-only mode without a policy if you want
	// all-at-once fanout).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Stages []RolloutStage `json:"stages"`
}

// RolloutPolicyStatus tracks usage.
type RolloutPolicyStatus struct {
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

	// MatchingBundles is the count of MigrationBundles currently
	// referencing this policy. Surfaced for ops visibility — a policy
	// with zero matches is dead code.
	//
	// +optional
	MatchingBundles int32 `json:"matchingBundles,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=rp;rollout
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Stages",type="integer",JSONPath=".spec.stages[*]"
// +kubebuilder:printcolumn:name="Bundles",type="integer",JSONPath=".status.matchingBundles"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// RolloutPolicy declares an ordered staged-rollout pipeline shared
// across MigrationBundles via spec.rolloutPolicyRef. Cluster-scoped.
type RolloutPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolloutPolicySpec   `json:"spec,omitempty"`
	Status RolloutPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RolloutPolicyList is the list wrapper required by client-go.
type RolloutPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RolloutPolicy `json:"items"`
}

// AnnotationApproveStagePrefix prefixes the annotation key that gates
// advancement out of a stage requiring approval.
//   keystone.hexxlock.io/approve-stage-<stageName>=true
const AnnotationApproveStagePrefix = "keystone.hexxlock.io/approve-stage-"

func init() {
	SchemeBuilder.Register(&RolloutPolicy{}, &RolloutPolicyList{})
}
