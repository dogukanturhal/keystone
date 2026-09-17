// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProductInstancePhase is the coarse lifecycle state for a tenant
// instance of a product. Mirrors the legacy db-provisioner state machine
// but graduated into a CR.
//
// +kubebuilder:validation:Enum=Pending;Provisioning;Migrating;ConfiguringAuth;RegisteringRoutes;Active;Failed;Deactivating;Deactivated
type ProductInstancePhase string

const (
	ProductInstancePhasePending           ProductInstancePhase = "Pending"
	ProductInstancePhaseProvisioning      ProductInstancePhase = "Provisioning"
	ProductInstancePhaseMigrating         ProductInstancePhase = "Migrating"
	ProductInstancePhaseConfiguringAuth   ProductInstancePhase = "ConfiguringAuth"
	ProductInstancePhaseRegisteringRoutes ProductInstancePhase = "RegisteringRoutes"
	ProductInstancePhaseActive            ProductInstancePhase = "Active"
	ProductInstancePhaseFailed            ProductInstancePhase = "Failed"
	ProductInstancePhaseDeactivating      ProductInstancePhase = "Deactivating"
	ProductInstancePhaseDeactivated       ProductInstancePhase = "Deactivated"
)

// ProductInstanceSpec describes one tenant's instance of a product.
type ProductInstanceSpec struct {
	// TenantID is the UUID identifying the tenant. Cross-referenced
	// against Example Service's tenant directory; the controller does NOT
	// validate the UUID exists there (that's the orchestrator's
	// responsibility upstream).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	TenantID string `json:"tenantID"`

	// TenantSlug is a human-readable tenant identifier surfaced in audit
	// logs and dashboards. Optional; many SaaS shops use the email
	// domain or organisation name.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=63
	TenantSlug string `json:"tenantSlug,omitempty"`

	// ProductRef is the ProductDefinition.metadata.name. Cluster-scoped
	// reference (ProductDefinition is cluster-scoped).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	ProductRef string `json:"productRef"`

	// IsolationOverride forces a specific isolation mode for this
	// instance, regardless of the product's defaultIsolation. Use to
	// silo a high-compliance customer in an otherwise pool/bridge
	// product. Empty = use the product's default per-database setting.
	//
	// +optional
	IsolationOverride IsolationMode `json:"isolationOverride,omitempty"`

	// Region pins the instance to a region (matches
	// ClusterRegistration.spec.region). Used by the RolloutController
	// (Phase 7) for region-scoped rollouts and by
	// data-residency policies.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Region string `json:"region,omitempty"`

	// DeletionPolicy controls what happens to the underlying databases
	// when this CR is deleted. Defaults to Retain — customer data is
	// never destroyed by an accidental kubectl delete.
	//
	// +kubebuilder:default="Retain"
	// +kubebuilder:validation:Enum=Retain;Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// InstanceDatabase reports the concrete LogicalDatabase + DatabaseSchema
// resources the controller created for this instance. One entry per
// ProductDatabase declared in the ProductDefinition.
type InstanceDatabase struct {
	// Key matches ProductDatabase.Key.
	Key string `json:"key"`

	// LogicalDatabaseRef is the namespaced name of the LogicalDatabase CR.
	LogicalDatabaseRef string `json:"logicalDatabaseRef"`

	// DatabaseSchemaRefs lists the DatabaseSchema CRs for this database.
	//
	// +optional
	DatabaseSchemaRefs []string `json:"databaseSchemaRefs,omitempty"`
}

// ProductInstanceStatus reports state machine progress + child resources.
type ProductInstanceStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the coarse state. ArgoCD health checks read this field.
	//
	// +optional
	Phase ProductInstancePhase `json:"phase,omitempty"`

	// Conditions reports per-step status:
	//   Ready             — Phase=Active and every child resource is Ready
	//   DatabasesReady    — every InstanceDatabase has Ready LogicalDatabase
	//   SchemasReady      — every DatabaseSchema is Ready
	//   MigrationsApplied — initial migrations from product schemas have
	//                       all reached Succeeded
	//   AuthConfigured    — SpiceDB tuples written (Phase 4.1)
	//   RoutesRegistered  — APISIX routes upserted (Phase 4.1)
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Databases lists child LogicalDatabase + DatabaseSchema refs.
	// Maintained on every reconcile; clearing it requires the deletion
	// flow to remove the children first.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Databases []InstanceDatabase `json:"databases,omitempty"`

	// AppliedRoutes are the APISIX route IDs the controller created.
	// Empty in Phase 4 (HTTP integration is Phase 4.1).
	//
	// +optional
	// +kubebuilder:validation:MaxItems=64
	AppliedRoutes []string `json:"appliedRoutes,omitempty"`

	// LastTransitionTime is when Phase last changed.
	//
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=pi;instance
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Tenant",type="string",JSONPath=".spec.tenantID"
// +kubebuilder:printcolumn:name="Product",type="string",JSONPath=".spec.productRef"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Region",type="string",JSONPath=".spec.region"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ProductInstance represents one tenant's subscription to a product.
// Replaces the iam_product_instances row in the legacy db-provisioner.
// The TenancyController reconciles it through the lifecycle phases by
// creating child LogicalDatabase + DatabaseSchema CRs.
type ProductInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProductInstanceSpec   `json:"spec,omitempty"`
	Status ProductInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProductInstanceList is the list wrapper required by client-go.
type ProductInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProductInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProductInstance{}, &ProductInstanceList{})
}
