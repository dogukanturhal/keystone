// SPDX-License-Identifier: AGPL-3.0-or-later

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterTier classifies a workload cluster for staged rollout. The
// RolloutController honours these tiers when fanning out a MigrationPlan:
// canary first, then free in batches, then paid with low parallelism.
//
// +kubebuilder:validation:Enum=canary;free;paid;internal
type ClusterTier string

const (
	// ClusterTierCanary receives every change first. Used to catch breakage
	// before any tenant traffic is exposed.
	ClusterTierCanary ClusterTier = "canary"

	// ClusterTierFree receives changes in moderate-parallelism batches once
	// canary has been observed healthy.
	ClusterTierFree ClusterTier = "free"

	// ClusterTierPaid receives changes last, with low parallelism and an
	// optional approval gate set on the RolloutPolicy.
	ClusterTierPaid ClusterTier = "paid"

	// ClusterTierInternal hosts platform-internal workloads (control plane,
	// observability, CI). Migrations here are scheduled independently of the
	// canary-free-paid pipeline.
	ClusterTierInternal ClusterTier = "internal"
)

// ClusterRegistrationSpec describes a workload cluster that the hub may
// target. The agent running inside the cluster reconciles its own local
// CRs; the hub uses this record to plan multi-cluster rollouts and to
// enumerate targets for ApplicationSet generators.
//
// Spec is intentionally small. Cluster connectivity (kubeconfig, server URL,
// CA bundle) is owned by ArgoCD's cluster secrets — Keystone deliberately
// does not duplicate that state.
type ClusterRegistrationSpec struct {
	// DisplayName is a human-readable label shown in dashboards and audit
	// logs. Defaults to the resource name when empty.
	//
	// +kubebuilder:validation:MaxLength=253
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Tier classifies the cluster for rollout staging. Required: there is
	// no safe default — operators must consciously place a cluster.
	//
	// +kubebuilder:validation:Required
	Tier ClusterTier `json:"tier"`

	// Region is the geographic region this cluster lives in (e.g.
	// "eu-west-1", "us-east-1"). Used by RolloutPolicy region selectors and
	// by audit reporting.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Region string `json:"region"`

	// DataResidency is the legal jurisdiction the cluster's data is
	// permitted to live in (e.g. "eu", "us", "global"). Distinct from
	// Region: a cluster can be in eu-west-1 but bound by EU residency rules.
	// Required so that residency-tagged workloads cannot accidentally land
	// in a non-compliant cluster.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
	DataResidency string `json:"dataResidency"`

	// Labels are user-defined classifiers consumed by RolloutPolicy and
	// MigrationBundle target selectors. Distinct from metadata.labels:
	// metadata.labels are shared with kubectl and other tooling; spec.labels
	// are explicitly part of Keystone's API surface and form a stable
	// selector contract.
	//
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Labels map[string]string `json:"labels,omitempty"`
}

// ClusterRegistrationStatus reports observed runtime state. Only the
// controller writes Status; users must not edit it.
type ClusterRegistrationStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// ArgoCD compares this against .metadata.generation to determine
	// whether the controller has seen the latest spec.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions reports the current state. ConditionTypeReady is always
	// present; controllers may add more (e.g. "Connected").
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// LastHeartbeatTime is the last time the agent in this cluster reported
	// in. Populated by the agent posting back through Git status. A stale
	// heartbeat (older than the agent's reporting interval × 3) is grounds
	// for marking the cluster Degraded.
	//
	// +optional
	LastHeartbeatTime *metav1.Time `json:"lastHeartbeatTime,omitempty"`

	// AgentVersion is the version of keystone-manager running in agent
	// mode in this cluster. Used to gate features that require a minimum
	// agent version.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	AgentVersion string `json:"agentVersion,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cr;clusterreg
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Tier",type="string",JSONPath=".spec.tier"
// +kubebuilder:printcolumn:name="Region",type="string",JSONPath=".spec.region"
// +kubebuilder:printcolumn:name="Residency",type="string",JSONPath=".spec.dataResidency"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Agent",type="string",JSONPath=".status.agentVersion"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ClusterRegistration registers a workload cluster with the Keystone hub.
// Cluster-scoped because clusters are not a tenant resource.
type ClusterRegistration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterRegistrationSpec   `json:"spec,omitempty"`
	Status ClusterRegistrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterRegistrationList is the list wrapper required by client-go.
type ClusterRegistrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterRegistration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterRegistration{}, &ClusterRegistrationList{})
}
