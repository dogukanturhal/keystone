// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SchemaPrivilegeDefault declares the default privileges granted to roles
// for objects subsequently created in the schema. Mirrors PostgreSQL
// `ALTER DEFAULT PRIVILEGES`. Mutating after creation requires the
// controller to re-apply ALTER DEFAULT PRIVILEGES; existing objects are
// not retroactively re-granted.
type SchemaPrivilegeDefault struct {
	// Role is the role being granted to (or revoked from).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Role string `json:"role"`

	// ObjectType is the PostgreSQL object class the defaults apply to.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=tables;sequences;functions;types;schemas
	ObjectType string `json:"objectType"`

	// Privileges is the list of privileges to grant. Empty means "revoke
	// all defaults" — distinct from omitting the entry, which leaves
	// existing defaults untouched.
	//
	// +kubebuilder:validation:MaxItems=16
	Privileges []string `json:"privileges"`
}

// DatabaseSchemaSpec describes one PostgreSQL schema (`CREATE SCHEMA`).
// Schemas live inside a LogicalDatabase, declared by reference. The
// controller creates the schema, sets ownership, and manages default
// privileges.
type DatabaseSchemaSpec struct {
	// Name is the actual PostgreSQL schema name. Distinct from
	// metadata.name for the same reason as LogicalDatabase.spec.name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Name string `json:"name"`

	// LogicalDatabaseRef is the name of the LogicalDatabase this schema
	// lives in. Must be in the same namespace as this DatabaseSchema —
	// cross-namespace references would create an authorisation hole.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	LogicalDatabaseRef string `json:"logicalDatabaseRef"`

	// OwnerRole is the PostgreSQL role that owns the schema. The
	// controller creates the role if absent and runs `ALTER SCHEMA OWNER
	// TO`. Required for the same reasons as LogicalDatabase.spec.ownerRole.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	OwnerRole string `json:"ownerRole"`

	// DefaultPrivileges declares ALTER DEFAULT PRIVILEGES rules applied at
	// schema creation. See the CNPG-managed-roles landmine documented in
	// project memory: pre-existing schemas need GRANT / REASSIGN OWNED
	// before role hand-off, which the controller handles automatically
	// when ownership changes.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=32
	DefaultPrivileges []SchemaPrivilegeDefault `json:"defaultPrivileges,omitempty"`

	// SearchPathHints is an optional list of schemas to prepend to the
	// owner role's search_path. Empty means leave the role's search_path
	// unmodified. Useful for cross-schema modules (e.g. a `crm` schema
	// that reads from `master_data`).
	//
	// +optional
	// +kubebuilder:validation:MaxItems=16
	SearchPathHints []string `json:"searchPathHints,omitempty"`

	// DeletionPolicy controls what happens to the underlying PostgreSQL
	// schema when this resource is deleted. Defaults to Retain.
	//
	// +kubebuilder:default="Retain"
	// +kubebuilder:validation:Enum=Retain;Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// DatabaseSchemaStatus reports observed state.
type DatabaseSchemaStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions reports the current state. Standard set: Ready, Available,
	// Progressing, Degraded. Migration-specific conditions (Applied,
	// DriftFree) are added by the MigrationController in Phase 3.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// MigrationVersion is the highest schema_migrations version applied to
	// this schema. Surfaced by the MigrationController; null until the
	// first migration runs.
	//
	// +optional
	MigrationVersion *string `json:"migrationVersion,omitempty"`

	// LastAppliedFingerprint is the SchemaDefinition.Status.Fingerprint
	// value of the most recent SD-emitted MigrationBundle that the
	// runner applied to this schema successfully. The bundle controller
	// stamps it after a MigrationExecution reaches Phase=Succeeded for
	// (this schema, the bundle carrying the SD fingerprint annotation).
	//
	// SDK consumers (pkg/sdk/keystone.WaitSchemaFingerprint) compare
	// this field against SchemaDefinition.Status.Fingerprint to verify
	// per-schema convergence — race-free vs the matchedSchemas-count
	// baseline that WaitSchemaUpToDate (v0.1.47) uses.
	//
	// Empty until the SD-emit-then-apply cycle has run at least once
	// for this schema. Hand-authored MigrationBundles (no SD owner)
	// don't carry the SD fingerprint annotation and won't update this
	// field; that's intentional — fingerprint convergence is an
	// SD-owned concept.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	LastAppliedFingerprint string `json:"lastAppliedFingerprint,omitempty"`

	// LastReconcileTime is when the controller last successfully reconciled
	// this resource.
	//
	// +optional
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dbs;dschema
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Schema",type="string",JSONPath=".spec.name"
// +kubebuilder:printcolumn:name="Database",type="string",JSONPath=".spec.logicalDatabaseRef"
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".spec.ownerRole"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.migrationVersion"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// DatabaseSchema declares a PostgreSQL schema that should exist inside a
// referenced LogicalDatabase. The combination of LogicalDatabase +
// DatabaseSchema replaces the hardcoded {SharedDB, Schema} pairs in the
// legacy db-migrator.
type DatabaseSchema struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseSchemaSpec   `json:"spec,omitempty"`
	Status DatabaseSchemaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DatabaseSchemaList is the list wrapper required by client-go.
type DatabaseSchemaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DatabaseSchema `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DatabaseSchema{}, &DatabaseSchemaList{})
}
