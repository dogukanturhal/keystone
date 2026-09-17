// SPDX-License-Identifier: AGPL-3.0-or-later

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AuditEntrySpec is one immutable entry in Keystone's append-only
// audit log. Entries are chained via SHA-256 hashes so downstream
// verifiers can detect tampering without trusting the apiserver.
//
// Invariant enforced by the admission webhook
// (`internal/webhook/auditentry_webhook.go`): once created, every
// field in this struct is frozen. Updates and deletes are rejected.
//
// Entries are cluster-scoped and ordered by `sequence`. A singleton
// `AuditLog` resource holds the next sequence number and serves as
// the optimistic-concurrency token for concurrent writers.
//
// Map to compliance controls:
//   - SOC 2 CC7.2 + CC8 change management
//   - ISO 27001 Annex A 8.15 logging + 8.16 monitoring + 8.32 change mgmt
//   - HIPAA §164.312(b) audit controls
//   - PCI DSS v4.0.1 Requirement 10
type AuditEntrySpec struct {
	// Sequence is the monotonic cluster-wide entry number. Assigned by
	// the AuditLogger from the singleton AuditLog's status.nextSequence.
	// Gaps are not permitted in a compliant chain — verifier tooling
	// MUST reject a log with missing sequence numbers.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	Sequence int64 `json:"sequence"`

	// Timestamp is when the audited action took place. Not necessarily
	// the same as the AuditEntry's metadata.creationTimestamp — there
	// can be small delays between the action and the entry being
	// persisted.
	//
	// +kubebuilder:validation:Required
	Timestamp metav1.Time `json:"timestamp"`

	// Actor identifies who or what caused the state change.
	//
	// +kubebuilder:validation:Required
	Actor AuditActor `json:"actor"`

	// Verb is the class of action audited.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=create;update;delete;reconcile;approve;deny;drift-ack;rollout-pause;rollout-resume;apply
	Verb string `json:"verb"`

	// ResourceRef points at the object the action concerned.
	//
	// +kubebuilder:validation:Required
	ResourceRef AuditResourceRef `json:"resourceRef"`

	// Before is the JSON-encoded state of the resource prior to the
	// action. Empty for verb=create.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	Before string `json:"before,omitempty"`

	// After is the JSON-encoded state of the resource after the action.
	// Empty for verb=delete.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	After string `json:"after,omitempty"`

	// Reason is a human-readable description of why the action was
	// taken. For reconcile verbs, this is typically the condition
	// reason (e.g. "Reconciled", "LintFailed"). For approve/deny, the
	// approver's note.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`

	// Outcome records success or failure.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=success;error;warn
	Outcome string `json:"outcome"`

	// PrevHash is the SHA-256 (hex) of the previous entry's SelfHash.
	// The first entry in the chain carries the literal string
	// "genesis" — this is how verifiers detect the chain's origin.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=96
	// +kubebuilder:validation:Pattern=`^(genesis|[0-9a-f]{64})$`
	PrevHash string `json:"prevHash"`

	// SelfHash is the SHA-256 (hex) over a canonical encoding of this
	// entry's fields EXCLUDING SelfHash itself. The admission webhook
	// verifies this matches on every create — entries that don't hash
	// to their claimed SelfHash are rejected outright.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	SelfHash string `json:"selfHash"`
}

// AuditActor identifies the entity responsible for the change. Populated
// from Kubernetes admission request userInfo for webhook-sourced entries
// and from the manager's identity for reconcile-sourced entries.
type AuditActor struct {
	// Username is the authenticated user's name.
	// For reconciler-sourced entries, this is
	// "system:serviceaccount:<ns>:<sa>".
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Username string `json:"username"`

	// UID is the Kubernetes-assigned UID for the actor. Stable across
	// renames; preferable to Username for correlation.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid,omitempty"`

	// Groups are the authenticated user's groups.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=64
	Groups []string `json:"groups,omitempty"`
}

// AuditResourceRef locates the audited object.
type AuditResourceRef struct {
	// APIVersion of the referenced object (e.g.
	// "keystone.hexxlock.io/v1alpha1").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=128
	APIVersion string `json:"apiVersion"`

	// Kind of the referenced object (e.g. "MigrationBundle").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=128
	Kind string `json:"kind"`

	// Namespace of the referenced object. Empty for cluster-scoped
	// objects.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace,omitempty"`

	// Name of the referenced object.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// UID of the referenced object at the time of audit.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid,omitempty"`
}

// AuditEntryStatus is intentionally minimal. All load-bearing state
// lives in spec (which is immutable). Status-only fields can change
// without breaking integrity.
type AuditEntryStatus struct {
	// VerifiedAt is when a verifier tool last validated this entry's
	// hash against the preceding entry. Optional — external SIEMs
	// typically carry their own verification receipts.
	//
	// +optional
	VerifiedAt *metav1.Time `json:"verifiedAt,omitempty"`

	// VerifiedBy names the verifier tool.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=256
	VerifiedBy string `json:"verifiedBy,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=aent
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Seq",type="integer",JSONPath=".spec.sequence",priority=0
// +kubebuilder:printcolumn:name="Verb",type="string",JSONPath=".spec.verb"
// +kubebuilder:printcolumn:name="Resource",type="string",JSONPath=".spec.resourceRef.kind"
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.resourceRef.name"
// +kubebuilder:printcolumn:name="Actor",type="string",JSONPath=".spec.actor.username",priority=1
// +kubebuilder:printcolumn:name="Outcome",type="string",JSONPath=".spec.outcome"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AuditEntry is one immutable record in Keystone's append-only audit
// chain. See the package doc of `internal/audit/` for the verifier
// algorithm and the integration guide in
// `docs/compliance-mappings.md`.
type AuditEntry struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AuditEntrySpec   `json:"spec,omitempty"`
	Status AuditEntryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AuditEntryList is the list wrapper.
type AuditEntryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AuditEntry `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AuditEntry{}, &AuditEntryList{})
}
