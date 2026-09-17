// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LogicalDatabaseSpec describes one logical PostgreSQL database (a `CREATE
// DATABASE` target). Multiple LogicalDatabases may live in the same
// underlying cluster; the controller is responsible for opening admin
// connections to the cluster, ensuring the DB exists with the requested
// configuration, and creating the owner role.
//
// LogicalDatabases are namespaced because a tenant or product team may own
// the DB and operate on it through Keystone's RBAC. Cross-namespace access
// is allowed only through SchemaPolicy.
type LogicalDatabaseSpec struct {
	// Name is the actual PostgreSQL database name. Distinct from
	// metadata.name: PG identifiers commonly use underscores (e.g.
	// example_suite) while Kubernetes resources prefer hyphens
	// (hexxlock-erp). Required so this never has to be inferred.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Name string `json:"name"`

	// ClusterRef is the name of the ClusterRegistration this database
	// belongs to from a workload-rollout perspective. Used by the
	// RolloutController (Phase 7) and DriftController (Phase 5) to scope
	// per-cluster operations. NOT used by the SchemaController for
	// connectivity — that goes via ProviderRef.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ClusterRef string `json:"clusterRef"`

	// ProviderRef is the name of the DatabaseProvider hosting this
	// database. The SchemaController opens admin sessions against this
	// provider to CREATE DATABASE / CREATE ROLE / CREATE EXTENSION. A
	// LogicalDatabase belongs to exactly one provider; cross-provider
	// replication is out of scope (use a ReplicationPolicy in Phase 6+).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ProviderRef string `json:"providerRef"`

	// DeletionPolicy controls what happens to the underlying PostgreSQL
	// database when the LogicalDatabase resource is deleted. Defaults to
	// Retain — deleting a CR never drops production data. Operators must
	// explicitly opt in to Delete for tear-down workflows.
	//
	// +kubebuilder:default="Retain"
	// +kubebuilder:validation:Enum=Retain;Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// OwnerRole is the PostgreSQL role that owns the database. The
	// controller creates the role if absent and grants it CREATE on the
	// DB. Required because creating a database with no clear owner is an
	// operational landmine.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	OwnerRole string `json:"ownerRole"`

	// Extensions are PostgreSQL extensions to CREATE EXTENSION IF NOT
	// EXISTS. Order is preserved. The admission webhook rejects any
	// extension not present in the cluster's allow-list (declared on the
	// ClusterRegistration's spec.labels).
	//
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Extensions []string `json:"extensions,omitempty"`

	// Encoding overrides the default character encoding (UTF8). Setting
	// this on an existing database is a no-op — encoding is fixed at
	// CREATE time.
	//
	// +optional
	// +kubebuilder:default="UTF8"
	// +kubebuilder:validation:Enum=UTF8;SQL_ASCII;LATIN1
	Encoding string `json:"encoding,omitempty"`

	// Collation overrides the default LC_COLLATE. Like Encoding, this is
	// fixed at CREATE time.
	//
	// +optional
	// +kubebuilder:default="C"
	// +kubebuilder:validation:MaxLength=64
	Collation string `json:"collation,omitempty"`

	// Ctype overrides the default LC_CTYPE. Fixed at CREATE time.
	//
	// +optional
	// +kubebuilder:default="C"
	// +kubebuilder:validation:MaxLength=64
	Ctype string `json:"ctype,omitempty"`

	// ConnectionLimit caps the maximum concurrent connections (PostgreSQL
	// `CONNECTION LIMIT` clause). -1 means unlimited (PostgreSQL default).
	//
	// +optional
	// +kubebuilder:default=-1
	// +kubebuilder:validation:Minimum=-1
	ConnectionLimit int32 `json:"connectionLimit,omitempty"`

	// TrackingTableName is the table name Keystone writes migration
	// bookkeeping into. Defaults to "schema_migrations" — the
	// golang-migrate convention — which is the right choice on greenfield
	// databases.
	//
	// Override when adopting Keystone on a database that already has a
	// migrator managing its own `schema_migrations`. Example Service is the
	// canonical example: its existing table has `version INTEGER PK`
	// while Keystone's runner uses `version TEXT PK`. Setting
	// trackingTableName to "keystone_schema_migrations" lets both
	// trackers co-exist without touching production history.
	//
	// +optional
	// +kubebuilder:default="schema_migrations"
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	TrackingTableName string `json:"trackingTableName,omitempty"`

	// OwnerRolePasswordSecretRef references a Kubernetes Secret containing
	// the password the reconciler should set on OwnerRole when creating it
	// (and when rotating it on subsequent reconciles where the Secret
	// content changes).
	//
	// The Secret MUST exist in the SAME namespace as the LogicalDatabase
	// resource. The reconciler reads spec.ownerRolePasswordSecretRef.name
	// from that namespace and reads the value at spec.ownerRolePassword
	// SecretRef.key. Cross-namespace references are intentionally
	// disallowed — the LogicalDatabase namespace gates who can change the
	// password.
	//
	// When unset, EnsureRole creates the role WITHOUT a password. The
	// PostgreSQL admin can then assign one out-of-band (e.g. via Vault
	// dynamic secrets engine, manual psql, or any other mechanism) — the
	// pre-passwordSecretRef behavior. Backward-compatible: existing
	// LogicalDatabases without this field reconcile identically to v0.1.53.
	//
	// # Standalone-product design note
	//
	// SecretKeySelector is a vanilla corev1 type — the same pattern used
	// by cert-manager (Issuer.spec.vault.auth.appRole.secretRef),
	// CloudNativePG (Cluster.spec.bootstrap.initdb.passwordSecret),
	// Crunchy Postgres Operator (PostgresCluster.spec.users[].password.
	// secretName), Zalando Postgres Operator (postgresql.spec.users) and
	// many others. No vendor lock-in: any Secret-source operator
	// (External Secrets, kubernetes-external-secrets, Vault Agent
	// Injector, Reflector, plain kubectl-created Secrets) can populate
	// the referenced Secret and Keystone reads it the same way.
	//
	// # Rotation
	//
	// On every reconcile the controller compares the password in the
	// referenced Secret with what was last written to the role
	// (cached in status.observedPasswordHash). On change, the
	// controller issues ALTER ROLE … PASSWORD '<new>' to rotate the
	// PostgreSQL password atomically. ExternalSecrets/Vault rotation
	// flows therefore Just Work: rotate the source → ESO refreshes the
	// Secret → next reconcile applies ALTER ROLE.
	//
	// +optional
	OwnerRolePasswordSecretRef *corev1.SecretKeySelector `json:"ownerRolePasswordSecretRef,omitempty"`
}

// LogicalDatabaseStatus reports observed state.
type LogicalDatabaseStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions reports the current state. Ready, Available, Progressing
	// and Degraded follow Kubernetes conventions; the controller may add
	// more (e.g. ExtensionsInstalled).
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ResolvedEndpoint is the host:port the controller resolved from the
	// referenced ClusterRegistration. Surfaced in status so operators can
	// confirm where the controller is actually connecting.
	//
	// +optional
	ResolvedEndpoint string `json:"resolvedEndpoint,omitempty"`

	// SizeBytes is the on-disk size of the database, refreshed periodically
	// by the SchemaController. Approximate (snapshot at last reconcile).
	//
	// +optional
	SizeBytes *resource.Quantity `json:"sizeBytes,omitempty"`

	// LastReconcileTime is when the controller last successfully reconciled
	// this resource. Used to detect stuck reconciliations.
	//
	// +optional
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`

	// ObservedPasswordHash is the SHA-256 (hex) of the password the
	// controller most recently applied to spec.ownerRole via CREATE ROLE
	// or ALTER ROLE PASSWORD. Compared against the live password in the
	// referenced spec.ownerRolePasswordSecretRef on each reconcile so
	// rotation (Secret content change) triggers exactly one ALTER ROLE
	// PASSWORD per change.
	//
	// Empty when spec.ownerRolePasswordSecretRef is unset, OR when the
	// role was created with no password (legacy / out-of-band-password
	// path).
	//
	// Stored as a hash, not the password itself, so leaking status never
	// leaks credentials.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	ObservedPasswordHash string `json:"observedPasswordHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ldb
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Name",type="string",JSONPath=".spec.name"
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef"
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".spec.ownerRole"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Size",type="string",JSONPath=".status.sizeBytes"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// LogicalDatabase declares a PostgreSQL database that should exist on a
// referenced cluster. The controller is idempotent: declaring the same
// database twice is harmless; deleting the resource (with finalizer) will
// drop the database only if spec.deletionPolicy permits it (Phase 4+).
type LogicalDatabase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LogicalDatabaseSpec   `json:"spec,omitempty"`
	Status LogicalDatabaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LogicalDatabaseList is the list wrapper required by client-go.
type LogicalDatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LogicalDatabase `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LogicalDatabase{}, &LogicalDatabaseList{})
}
