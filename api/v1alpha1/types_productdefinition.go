// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsolationMode mirrors the legacy db-provisioner's pool/bridge/silo
// taxonomy. Each tenant of a product gets storage shaped according to
// the chosen mode.
//
// +kubebuilder:validation:Enum=pool;bridge;silo
type IsolationMode string

const (
	// IsolationModePool — shared database AND shared schema; per-tenant
	// data segregation enforced by row-level security policies on a
	// tenant_id column. Cheapest; only suitable for products with
	// strict data-shape uniformity and weak compliance ceiling.
	IsolationModePool IsolationMode = "pool"

	// IsolationModeBridge — shared database, schema-per-tenant. The
	// industry sweet spot: tenants are SQL-namespace isolated but share
	// connection pools, extensions, and admin overhead.
	IsolationModeBridge IsolationMode = "bridge"

	// IsolationModeSilo — database-per-tenant. Strongest isolation,
	// highest overhead; required for regulated workloads (HIPAA, PCI
	// large merchant) or air-gapped deployments.
	IsolationModeSilo IsolationMode = "silo"
)

// IsImplemented reports whether the TenancyController can actually
// provision this isolation mode today.
//
// Only bridge is implemented. The CRD enum accepts all three modes for
// forward compatibility, but ProductInstance reconciliation materialises
// exactly one LogicalDatabase per ProductDatabase with schema-per-tenant
// naming — the bridge topology — regardless of what was declared.
//
// This matters beyond tidiness: a ProductDefinition declaring silo for a
// regulated workload would otherwise be provisioned onto a SHARED
// database with no error and no status condition, so the operator would
// silently deliver weaker data isolation than the resource requested.
// Admission rejects unimplemented modes rather than accepting a
// guarantee the controller cannot keep.
//
// When pool or silo land, add them here and to the admission tests; the
// webhook and controller read this single source of truth.
func (m IsolationMode) IsImplemented() bool {
	return m == IsolationModeBridge
}

// ImplementedIsolationModes lists the modes IsImplemented accepts, for
// use in error messages.
func ImplementedIsolationModes() []string {
	return []string{string(IsolationModeBridge)}
}

// ResolveIsolation returns the isolation mode that would actually govern
// a ProductDatabase, applying the documented precedence:
//
//	ProductInstance.spec.isolationOverride  (highest)
//	ProductDatabase.isolation
//	ProductDefinition.spec.defaultIsolation
//	bridge                                  (lowest / implicit default)
//
// Empty values are treated as "not specified" and fall through. Callers
// pass "" for override when no ProductInstance is in scope.
func ResolveIsolation(override, perDatabase, productDefault IsolationMode) IsolationMode {
	for _, candidate := range []IsolationMode{override, perDatabase, productDefault} {
		if candidate != "" {
			return candidate
		}
	}
	return IsolationModeBridge
}

// ProductDatabase describes one logical database the product needs.
// A product may declare multiple databases (e.g. a core database
// plus a separate financials database).
type ProductDatabase struct {
	// Key is a stable identifier the product code uses to refer to
	// this database (e.g. "core", "billing"). Required so the controller
	// can name child LogicalDatabase resources predictably.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_-]*$`
	Key string `json:"key"`

	// Name is the actual PostgreSQL database name template. Tokens
	// {{tenantID}} and {{productSlug}} are substituted by the
	// TenancyController at instantiation. Example: "example_suite_{{tenantID}}".
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	NameTemplate string `json:"nameTemplate"`

	// ProviderRef pins the DatabaseProvider this database lands on. A
	// product instance with multiple databases may spread across
	// providers; it's a deliberate decision the product author makes.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	ProviderRef string `json:"providerRef"`

	// Isolation overrides the product-level default for this database.
	// Use to silo PCI-relevant data (billing) while keeping the rest in
	// bridge mode.
	//
	// +optional
	Isolation IsolationMode `json:"isolation,omitempty"`

	// Schemas declares the schemas that should exist within this
	// database. Each becomes a DatabaseSchema CR.
	//
	// +kubebuilder:validation:MaxItems=128
	Schemas []ProductSchema `json:"schemas,omitempty"`

	// Extensions to install in the database. Same allow-list semantics
	// as LogicalDatabase.spec.extensions.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Extensions []string `json:"extensions,omitempty"`
}

// ProductSchema describes one schema within a ProductDatabase.
type ProductSchema struct {
	// Key is a stable identifier (e.g. "crm", "billing").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=32
	Key string `json:"key"`

	// NameTemplate is the schema name template (same tokens as
	// ProductDatabase.NameTemplate).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	NameTemplate string `json:"nameTemplate"`

	// MigrationBundleRef is the MigrationBundle name that ships the
	// initial DDL for this schema. Optional — schemas without an
	// initial migration just become empty CREATE SCHEMA targets.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=253
	MigrationBundleRef string `json:"migrationBundleRef,omitempty"`
}

// LifecycleHook describes a webhook the controller calls during
// transitions. Phase 4 implements this as a stub (records intent in
// status); Phase 4.1 wires real HTTP delivery with HMAC signing.
type LifecycleHook struct {
	// On selects the transition that fires the hook.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Activated;Deactivated;Failed
	On string `json:"on"`

	// URL is the receiver endpoint. HTTPS only — admission rejects http://.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`

	// SigningSecretRef references a Secret in keystone-system holding
	// the HMAC key used to sign the payload.
	//
	// +optional
	SigningSecretRef string `json:"signingSecretRef,omitempty"`
}

// ProductDefinitionSpec is the platform-author's blueprint for a
// product. Cluster-scoped because products are platform infrastructure.
type ProductDefinitionSpec struct {
	// Slug is the URL-safe product identifier. Used in API paths,
	// metrics labels, audit logs.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	Slug string `json:"slug"`

	// DisplayName is the human-friendly label.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`

	// DefaultIsolation is the isolation applied to ProductDatabases that
	// don't override it. Defaults to bridge — schema-per-tenant, the
	// industry sweet spot.
	//
	// +kubebuilder:default="bridge"
	DefaultIsolation IsolationMode `json:"defaultIsolation,omitempty"`

	// Databases declares everything storage-related the product needs.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Databases []ProductDatabase `json:"databases"`

	// LifecycleHooks fire on tenant lifecycle transitions. Phase 4.1
	// wires real HTTP delivery; Phase 4 surfaces intent only.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	LifecycleHooks []LifecycleHook `json:"lifecycleHooks,omitempty"`

	// APIRoutes declares the APISIX routes the product exposes. Phase
	// 4.1 turns these into actual APISIX upstreams + routes; Phase 4
	// records them in status only.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=64
	APIRoutes []ProductAPIRoute `json:"apiRoutes,omitempty"`
}

// ProductAPIRoute describes one external route the product exposes.
type ProductAPIRoute struct {
	// Path is the public URL path (e.g. "/api/v1/crm").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=512
	Path string `json:"path"`

	// UpstreamService is the in-cluster Service the route forwards to.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	UpstreamService string `json:"upstreamService"`

	// UpstreamPort is the Service port number.
	//
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	UpstreamPort int32 `json:"upstreamPort,omitempty"`
}

// ProductDefinitionStatus tracks observability rollups.
type ProductDefinitionStatus struct {
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

	// InstanceCount is the number of ProductInstances currently
	// referencing this definition.
	//
	// +optional
	InstanceCount int32 `json:"instanceCount,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=pd;product
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Slug",type="string",JSONPath=".spec.slug"
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Isolation",type="string",JSONPath=".spec.defaultIsolation"
// +kubebuilder:printcolumn:name="Databases",type="integer",JSONPath=".spec.databases[*]"
// +kubebuilder:printcolumn:name="Instances",type="integer",JSONPath=".status.instanceCount"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ProductDefinition is the platform-author's blueprint for a sellable
// product. Cluster-scoped because products are platform-wide.
// Replaces the iam_products row in the legacy db-provisioner.
type ProductDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProductDefinitionSpec   `json:"spec,omitempty"`
	Status ProductDefinitionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProductDefinitionList is the list wrapper required by client-go.
type ProductDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProductDefinition `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ProductDefinition{}, &ProductDefinitionList{})
}
