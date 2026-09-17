// SPDX-License-Identifier: AGPL-3.0-or-later

package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AuditLogName is the singleton name every AuditLog instance uses.
// Enforced by the admission webhook — any attempt to create a second
// AuditLog under a different name is rejected.
const AuditLogName = "keystone"

// DefaultAuditRetentionDays mirrors the kubebuilder default on
// AuditLogSpec.RetentionDays (~7 years — the SOC 2 / PCI / HIPAA
// retention floor). It applies when the field is unset or non-positive.
const DefaultAuditRetentionDays int32 = 2555

// EffectiveRetentionDays resolves the retention window actually in
// force, substituting the default for an unset or non-positive value.
func EffectiveRetentionDays(retentionDays int32) int32 {
	if retentionDays <= 0 {
		return DefaultAuditRetentionDays
	}
	return retentionDays
}

// RetentionCutoff returns the instant before which an AuditEntry is
// considered expired and therefore eligible for deletion.
//
// Two components depend on agreeing about this exactly: the retention
// controller, which selects entries to archive and delete, and the
// admission webhook, which refuses to delete anything that is not yet
// expired. If they disagree by even a second the controller's own
// deletes are rejected and entries accumulate in etcd, so both call
// this function rather than computing a cutoff of their own.
func RetentionCutoff(retentionDays int32, now time.Time) time.Time {
	return now.Add(-time.Duration(EffectiveRetentionDays(retentionDays)) * 24 * time.Hour)
}

// IsRetentionExpired reports whether this entry is past the retention
// window and may therefore be deleted.
func (e *AuditEntry) IsRetentionExpired(retentionDays int32, now time.Time) bool {
	return e.Spec.Timestamp.Time.Before(RetentionCutoff(retentionDays, now))
}

// AuditLogSpec is static configuration for the audit chain.
type AuditLogSpec struct {
	// RetentionDays is the minimum retention for AuditEntries before a
	// future cleanup job (Phase 12.3) may prune them. Default 2555 =
	// ~7 years, which covers SOC 2 / PCI / HIPAA retention floors.
	//
	// +optional
	// +kubebuilder:default=2555
	// +kubebuilder:validation:Minimum=90
	// +kubebuilder:validation:Maximum=36500
	RetentionDays int32 `json:"retentionDays,omitempty"`

	// ExportEndpoint optionally declares a SIEM / log-aggregator the
	// manager streams entries to. Format:
	//   "otlp-http://collector:4318"
	//   "kafka://broker:9092/audit-topic"
	//   "webhook://https://siem.example.com/ingest"
	// Empty disables external streaming — entries still land in etcd.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	ExportEndpoint string `json:"exportEndpoint,omitempty"`

	// Archive optionally enables the AuditEntry retention controller.
	// When set, entries older than RetentionDays are batch-uploaded
	// to the configured S3-compatible bucket as gzipped JSON Lines and
	// then deleted from etcd, freeing the cluster from unbounded
	// chain growth. When unset (default), the retention controller
	// stays dormant — entries grow forever.
	//
	// The archive backend MUST support object-lock compliance mode
	// (S3 retention + WORM) for tier-1 compliance: even with archive
	// enabled, deleted CRs must remain immutable for the full
	// retention window in the off-cluster store.
	//
	// +optional
	Archive *AuditArchiveSpec `json:"archive,omitempty"`
}

// AuditArchiveSpec configures the off-cluster archive backend used
// by the retention controller. Currently only S3-compatible (MinIO,
// AWS S3) is implemented; future backends (Azure Blob, GCS) would
// add new variants alongside the s3 field.
type AuditArchiveSpec struct {
	// Backend selects the archive implementation. Only "s3" is
	// supported today. "noop" forces the dormant path even when this
	// block is otherwise populated — useful for staging soak.
	//
	// +kubebuilder:validation:Enum=s3;noop
	// +kubebuilder:default=s3
	Backend string `json:"backend,omitempty"`

	// S3 holds the S3-compatible endpoint configuration.
	// Required when Backend = "s3".
	//
	// +optional
	S3 *AuditArchiveS3Spec `json:"s3,omitempty"`

	// CheckInterval controls how often the retention loop re-scans
	// AuditEntries for expiry. Default 1h. Lower for tests, higher
	// for very large clusters where the list cost is non-trivial.
	//
	// +optional
	// +kubebuilder:default="1h"
	CheckInterval metav1.Duration `json:"checkInterval,omitempty"`

	// BatchSize caps the number of expired entries archived in a
	// single archive object. Defaults to 1000 — high enough to keep
	// the per-object overhead low, low enough that one archive batch
	// fits comfortably in the manager Pod's memory limit (128Mi
	// requested by chart default; an entry averages ~4-6KB).
	//
	// +optional
	// +kubebuilder:default=1000
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=10000
	BatchSize int32 `json:"batchSize,omitempty"`

	// DeleteRateLimit caps AuditEntry CR deletes per second to
	// avoid the 2026-04-21-style etcd write-amplification pattern
	// where a sudden flood of writes wedges the cluster. Default 100
	// matches the watermark observed during steady-state recovery in
	// that incident.
	//
	// +optional
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	DeleteRateLimit int32 `json:"deleteRateLimit,omitempty"`
}

// AuditArchiveS3Spec is the S3-compatible endpoint configuration.
// MinIO is the canonical on-prem deployment.
type AuditArchiveS3Spec struct {
	// Endpoint is the S3-compatible API endpoint without scheme,
	// e.g. "minio.minio-system.svc.cluster.local:9000".
	//
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=512
	Endpoint string `json:"endpoint"`

	// Bucket is the destination bucket name. The controller does NOT
	// create the bucket; an operator-managed Crossplane S3Bucket (or
	// equivalent) must provision it with object-lock + WORM retention
	// covering RetentionDays before the controller is enabled.
	//
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`
	Bucket string `json:"bucket"`

	// Region is the S3 region. MinIO accepts any value; "us-east-1"
	// is the conventional default for non-AWS S3 deployments.
	//
	// +optional
	// +kubebuilder:default="us-east-1"
	Region string `json:"region,omitempty"`

	// UseTLS controls https vs http scheme on the endpoint URL.
	// Default true. In-cluster MinIO with mTLS via Linkerd may set
	// this to false (Linkerd handles transport security).
	//
	// +optional
	// +kubebuilder:default=true
	UseTLS bool `json:"useTLS,omitempty"`

	// CredentialsRef references a Secret in the controller's system
	// namespace containing the S3 access key + secret key. Keys
	// "accessKeyID" and "secretAccessKey" by default; override via
	// AccessKeyIDKey and SecretAccessKeyKey.
	//
	// +kubebuilder:validation:Required
	CredentialsRef AuditArchiveCredsRef `json:"credentialsRef"`

	// PathPrefix is prepended to every archive object key. Useful
	// when one bucket carries archives from multiple clusters.
	// Default "" (objects land at the bucket root).
	//
	// +optional
	// +kubebuilder:validation:MaxLength=256
	PathPrefix string `json:"pathPrefix,omitempty"`
}

// AuditArchiveCredsRef points at the Secret holding S3 credentials.
type AuditArchiveCredsRef struct {
	// SecretName is the Secret name in the controller's system namespace.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName"`

	// AccessKeyIDKey is the key in the Secret holding the S3 access
	// key ID. Default "accessKeyID".
	//
	// +optional
	// +kubebuilder:default="accessKeyID"
	AccessKeyIDKey string `json:"accessKeyIDKey,omitempty"`

	// SecretAccessKeyKey is the key in the Secret holding the S3
	// secret access key. Default "secretAccessKey".
	//
	// +optional
	// +kubebuilder:default="secretAccessKey"
	SecretAccessKeyKey string `json:"secretAccessKeyKey,omitempty"`
}

// AuditLogStatus carries the cluster-wide monotonic sequence counter.
// Optimistic-concurrency updates on this subresource are what serialise
// audit writes across manager replicas.
type AuditLogStatus struct {
	// NextSequence is the sequence number the next AuditEntry will be
	// assigned. Starts at 1.
	//
	// +optional
	NextSequence int64 `json:"nextSequence,omitempty"`

	// LastSequence is the highest sequence number that has been
	// successfully persisted. Equals NextSequence-1 outside write
	// windows. Surfaced for observability dashboards.
	//
	// +optional
	LastSequence int64 `json:"lastSequence,omitempty"`

	// LastEntryHash is the SelfHash of the most recently persisted
	// AuditEntry. Writers use this as the PrevHash input for the next
	// entry, so the chain stays linked across restarts.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	LastEntryHash string `json:"lastEntryHash,omitempty"`

	// LastEntryTime is when the most recent entry was persisted. Feeds
	// the `keystone_audit_last_entry_age_seconds` metric which should
	// alert if the chain goes quiet unexpectedly.
	//
	// +optional
	LastEntryTime *metav1.Time `json:"lastEntryTime,omitempty"`

	// LastArchivedSequence is the highest sequence number that has
	// been successfully archived (uploaded to off-cluster storage AND
	// deleted from etcd) by the retention controller. Zero when no
	// entries have ever been archived. Always <= LastSequence.
	//
	// +optional
	LastArchivedSequence int64 `json:"lastArchivedSequence,omitempty"`

	// LastArchivedTime records the timestamp of the most recent
	// successful archive batch. Feeds the
	// `keystone_audit_archive_lag_seconds` gauge — alert if this
	// stops advancing past RetentionDays + 1d.
	//
	// +optional
	LastArchivedTime *metav1.Time `json:"lastArchivedTime,omitempty"`

	// LastArchivedHash is the SelfHash of the highest-sequence
	// AuditEntry in the most recent archive batch. Feeds chain
	// integrity verification: when re-hydrating archived entries,
	// the operator can confirm the genesis-to-LastArchivedHash chain
	// remains intact end-to-end.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	LastArchivedHash string `json:"lastArchivedHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=alog
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Retention",type="integer",JSONPath=".spec.retentionDays"
// +kubebuilder:printcolumn:name="NextSeq",type="integer",JSONPath=".status.nextSequence"
// +kubebuilder:printcolumn:name="LastEntry",type="date",JSONPath=".status.lastEntryTime"

// AuditLog is the singleton coordination CR for the append-only
// audit chain. Exactly one instance named `AuditLogName` ("keystone")
// exists per cluster.
type AuditLog struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AuditLogSpec   `json:"spec,omitempty"`
	Status AuditLogStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AuditLogList is the list wrapper.
type AuditLogList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AuditLog `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AuditLog{}, &AuditLogList{})
}
