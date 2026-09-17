// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DatabaseProviderEngine identifies the database engine. Phase 2 supports
// PostgreSQL only; the enum exists so engine-pluggability lands without an
// API break.
//
// +kubebuilder:validation:Enum=postgresql
type DatabaseProviderEngine string

const (
	DatabaseProviderEnginePostgreSQL DatabaseProviderEngine = "postgresql"
)

// DatabaseProviderSSLMode mirrors libpq's sslmode values. The default is
// `verify-full`; `disable` is intentionally NOT permitted on the API
// surface — operators that want to opt out have to write a custom OPA
// policy to override the admission webhook.
//
// +kubebuilder:validation:Enum=require;verify-ca;verify-full
type DatabaseProviderSSLMode string

const (
	DatabaseProviderSSLModeRequire    DatabaseProviderSSLMode = "require"
	DatabaseProviderSSLModeVerifyCA   DatabaseProviderSSLMode = "verify-ca"
	DatabaseProviderSSLModeVerifyFull DatabaseProviderSSLMode = "verify-full"
)

// DatabaseProviderCredentials references the Secret holding admin
// credentials for the provider. The Secret MUST live in the
// keystone-system namespace; cross-namespace references are rejected to
// avoid privilege escalation through Secret accessibility.
type DatabaseProviderCredentials struct {
	// SecretName is the name of the Secret in the keystone-system
	// namespace. Required.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	SecretName string `json:"secretName"`

	// UsernameKey is the Secret data key for the admin username.
	//
	// +kubebuilder:default="username"
	// +kubebuilder:validation:MaxLength=253
	UsernameKey string `json:"usernameKey,omitempty"`

	// PasswordKey is the Secret data key for the admin password.
	//
	// +kubebuilder:default="password"
	// +kubebuilder:validation:MaxLength=253
	PasswordKey string `json:"passwordKey,omitempty"`
}

// DatabaseProviderSpec describes a physical PostgreSQL cluster reachable
// from the operator. Cluster-scoped because providers are platform
// infrastructure shared across tenants and namespaces.
type DatabaseProviderSpec struct {
	// Engine declares the database engine. PostgreSQL only in Phase 2.
	//
	// +kubebuilder:validation:Required
	Engine DatabaseProviderEngine `json:"engine"`

	// Host is the DNS name or IP of the primary writer endpoint. The
	// controller routes administrative DDL to this endpoint; read replicas
	// are out of scope for SchemaController.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host"`

	// Port is the TCP port the engine listens on.
	//
	// +kubebuilder:default=5432
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`

	// MaintenanceDatabase is the database the controller connects to for
	// administrative work that cannot run inside a target database
	// (e.g. CREATE DATABASE). Defaults to "postgres".
	//
	// +kubebuilder:default="postgres"
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	MaintenanceDatabase string `json:"maintenanceDatabase,omitempty"`

	// SSLMode controls TLS verification. `disable` is intentionally
	// unavailable; downgrade requires an out-of-band policy override.
	//
	// +kubebuilder:default="verify-full"
	SSLMode DatabaseProviderSSLMode `json:"sslMode,omitempty"`

	// AdminCredentialsRef references the Secret holding admin user/password.
	//
	// +kubebuilder:validation:Required
	AdminCredentialsRef DatabaseProviderCredentials `json:"adminCredentialsRef"`

	// PoolMaxConns caps the controller's pgx pool size against this
	// provider. Higher values speed up large-fanout reconciles but consume
	// more PG backends; tune per workload.
	//
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	PoolMaxConns int32 `json:"poolMaxConns,omitempty"`
}

// DatabaseProviderStatus reports observed connectivity.
type DatabaseProviderStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions reports the current state. Standard set: Ready,
	// Available, Progressing, Degraded. Specific to providers: Connected
	// (controller has an open admin session and SELECT 1 succeeded within
	// the last reconcile interval).
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ServerVersion is the PostgreSQL server_version reported by the
	// provider. Surfaced for ops visibility and for migration policy
	// rules that want to refuse running on outdated versions.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	ServerVersion string `json:"serverVersion,omitempty"`

	// LastConnectTime is when the controller last successfully opened an
	// admin connection.
	//
	// +optional
	LastConnectTime *metav1.Time `json:"lastConnectTime,omitempty"`

	// CredentialsObservedVersion is the ResourceVersion of the admin
	// Secret at the last DatabaseProviderReconciler observation. Used
	// to detect rotation: when the live Secret's ResourceVersion
	// differs, the reconciler triggers pool eviction so stale
	// credentials don't outlive the ESO sync.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	CredentialsObservedVersion string `json:"credentialsObservedVersion,omitempty"`

	// CredentialsRotatedAt is the timestamp the reconciler last
	// observed a change in the admin Secret's ResourceVersion. Empty
	// until the first rotation after the provider is admitted.
	// Useful for correlating rotation events with SIEM entries.
	//
	// +optional
	CredentialsRotatedAt *metav1.Time `json:"credentialsRotatedAt,omitempty"`

	// ObservedConnectionProfile is the "host:port" endpoint the
	// reconciler most recently observed on spec. When the spec is
	// repointed (server migration, DNS→IP cutover, local
	// port-forward verification) the reconciler compares the live
	// spec against this value, evicts cached pools still dialing the
	// previous endpoint, and updates the field. Mirrors
	// CredentialsObservedVersion, but for the CR-visible half of the
	// connection profile instead of the Secret.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=320
	ObservedConnectionProfile string `json:"observedConnectionProfile,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=dbp;provider
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Engine",type="string",JSONPath=".spec.engine"
// +kubebuilder:printcolumn:name="Host",type="string",JSONPath=".spec.host"
// +kubebuilder:printcolumn:name="Port",type="integer",JSONPath=".spec.port"
// +kubebuilder:printcolumn:name="SSL",type="string",JSONPath=".spec.sslMode"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.serverVersion"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// DatabaseProvider declares a physical database cluster the operator may
// administer. LogicalDatabase resources reference a DatabaseProvider via
// spec.providerRef.
type DatabaseProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseProviderSpec   `json:"spec,omitempty"`
	Status DatabaseProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DatabaseProviderList is the list wrapper required by client-go.
type DatabaseProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DatabaseProvider `json:"items"`
}

// Compile-time guard: corev1 is imported here for forward-compat (Phase 3
// will add a SecretReference helper) — Go's import linter would complain
// without a use site.
var _ = corev1.SchemeGroupVersion.String()

func init() {
	SchemeBuilder.Register(&DatabaseProvider{}, &DatabaseProviderList{})
}
