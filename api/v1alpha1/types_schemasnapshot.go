// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SchemaSnapshotSpec captures a point-in-time record of a
// DatabaseSchema's applied-migration state. Combined with the ability
// to replay a list of MigrationBundles at the recorded versions, it's
// Keystone's backup/restore primitive — the minimum needed to claim
// Red Hat Operator Capability Level 3.
//
// Operator UX is:
//
//   1. Before a risky change, declare a SchemaSnapshot. The reconciler
//      reads schema_migrations + the inspector's drift fingerprint
//      and freezes them into the CR's status.
//
//   2. If the risky change goes wrong, create a MigrationBundle whose
//      source is the DIFFerence between the snapshot and the current
//      state — the declarative differ handles the inverse in Phase
//      12. Meanwhile, operators can use snapshots as evidence for
//      PCI/SOC 2 change-management audits ("this is what state X
//      looked like before we ran migration Y").
//
// Snapshots are immutable once captured. This is enforced by the
// reconciler, which returns immediately for any snapshot already in
// phase Captured or Failed — not by a webhook. Nothing in-tree writes
// to a captured snapshot; new fields must still be added as +optional
// so that archived snapshots stay admissible against a newer CRD.
type SchemaSnapshotSpec struct {
	// SchemaRef names the DatabaseSchema the snapshot captures. The
	// reconciler requires the referenced DatabaseSchema to be Ready
	// at capture time; transient schemas aren't snapshotted.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	SchemaRef string `json:"schemaRef"`

	// Reason is a human-readable description of why the snapshot was
	// taken. Surfaced in audit log entries and restore UIs.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`

	// RetentionDays is the floor before a future cleanup job may prune
	// this snapshot. Default 365. Set higher for regulatory holds.
	//
	// +optional
	// +kubebuilder:default=365
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=36500
	RetentionDays int32 `json:"retentionDays,omitempty"`
}

// SchemaSnapshotStatus captures the observed state at snapshot time.
// Once `phase=Captured`, every field here is immutable — subsequent
// reconciles observe them as-is and do not mutate.
type SchemaSnapshotStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the snapshot lifecycle state.
	//
	// +optional
	// +kubebuilder:validation:Enum=Pending;Capturing;Captured;Failed
	Phase string `json:"phase,omitempty"`

	// CapturedAt is when the snapshot was frozen.
	//
	// +optional
	CapturedAt *metav1.Time `json:"capturedAt,omitempty"`

	// AppliedMigrations is the ordered list of (version, contentHash)
	// pairs from the target's schema_migrations table at capture
	// time. The order matches apply order — lexicographic version
	// ascending, consistent with golang-migrate.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8192
	AppliedMigrations []AppliedMigrationRecord `json:"appliedMigrations,omitempty"`

	// Fingerprint is the drift-inspector fingerprint at capture time.
	// Downstream verifiers compare against a fresh inspection to
	// detect drift since the snapshot.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Fingerprint string `json:"fingerprint,omitempty"`

	// ChecksumSHA256 is the SHA-256 over a canonical encoding of
	// AppliedMigrations + Fingerprint. Immutability evidence:
	// downstream tooling can recompute and verify after restore.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	ChecksumSHA256 string `json:"checksumSha256,omitempty"`

	// ERD is a Mermaid erDiagram representation of the schema at
	// capture time. Rendered from the drift inspector's structural
	// snapshot — tables, columns, PKs, FKs, unique constraints.
	// HexxForge UI and GitLab markdown render this natively.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1048576
	ERD string `json:"erd,omitempty"`

	// Structure is the structured representation of the schema shape
	// — tables, columns, indexes, constraints — captured from the
	// drift inspector at the same instant as Fingerprint/ERD. Serves
	// as the authoritative artifact for snapshot-to-snapshot diff
	// (`keystonectl snapshot diff`). Populated alongside ERD; the ERD
	// stays for human-readable rendering and Structure for machine
	// analysis.
	//
	// +optional
	Structure *StructuralSnapshot `json:"structure,omitempty"`

	// AutoSource identifies the MigrationExecution that triggered
	// automatic capture. Empty for manually-created snapshots. Pattern:
	// "<bundle>@<version>". Useful for querying "find the snapshot
	// that recorded the post-apply state of bundle X version Y".
	//
	// +optional
	// +kubebuilder:validation:MaxLength=320
	AutoSource string `json:"autoSource,omitempty"`

	// Conditions follow Kubernetes conventions.
	//
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// StructuralSnapshotModelVersion is the model the current controller
// captures. Written into every new StructuralSnapshot; see that type's
// ModelVersion field for what each version covers and why old
// snapshots are never upgraded.
const StructuralSnapshotModelVersion int32 = 2

// StructuralSnapshot is the machine-readable shape of a schema at
// capture time. Mirrors the drift-inspector output so downstream diff
// tools can compare two snapshots without re-querying the database.
// Deterministic JSON ordering — tables sorted by name, columns by
// ordinal — so two snapshots that observe identical schemas compute
// byte-identical serialisations.
type StructuralSnapshot struct {
	// ModelVersion records which model of this struct the capturing
	// controller was built against, so a reader can tell "this schema
	// had no policies" from "this snapshot could not see policies".
	//
	//	0 — implicit. Captured before row-level security was modelled:
	//	    tables, indexes and constraints only.
	//	1 — adds row-level security: per-table RLSEnabled/RLSForced and
	//	    the schema's Policies list.
	//	2 — adds the remaining objects the inspector reads: Enums,
	//	    Extensions, Sequences, Functions, Triggers,
	//	    MaterializedViews, and per-view ViewDefinition.
	//
	// The version is a single counter over the whole struct rather than
	// a flag per dimension, because it answers one question — "was this
	// field populated by a controller that knew about it?" — and every
	// dimension added in the same release shares one answer. A reader
	// compares a dimension only at or above the version that introduced
	// it, and says so out loud below that.
	//
	// Snapshots are immutable once captured and are retained for at
	// least a year — up to a century under a regulatory hold — so the
	// registry holds documents from every model that has ever shipped,
	// permanently. There is no migration to run and no point at which
	// version 0 stops appearing.
	//
	// Deliberately NOT +kubebuilder:default. Structural-schema
	// defaulting is applied when an unset field is read back out of
	// etcd, so a default would relabel every older snapshot in the
	// registry as aware of dimensions nobody captured — inverting the
	// exact property this field exists to provide.
	//
	// +optional
	// +kubebuilder:validation:Minimum=0
	ModelVersion int32 `json:"modelVersion,omitempty"`

	// Schema is the target schema name. Copied from the parent
	// SchemaSnapshot for self-contained diffing; any mismatch
	// between this and SchemaRef indicates CR corruption.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Schema string `json:"schema"`

	// Tables is the ordered list of base tables and views in the
	// schema, excluding Keystone's bookkeeping tables
	// (schema_migrations, keystone_baselines).
	//
	// +optional
	// +kubebuilder:validation:MaxItems=2048
	Tables []SnapshotTable `json:"tables,omitempty"`

	// Indexes is the list of CREATE INDEX definitions harvested from
	// pg_indexes. Type carries btree/hash/gin/etc.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Indexes []SnapshotObjectDDL `json:"indexes,omitempty"`

	// Constraints covers primary keys, unique constraints, checks,
	// foreign keys, exclusions — read from pg_constraint.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Constraints []SnapshotObjectDDL `json:"constraints,omitempty"`

	// Policies is every row-level security policy in the schema, read
	// from pg_policies and sorted by (table, name).
	//
	// Empty means "no policies" only when ModelVersion >= 1; at
	// version 0 it means the capturing controller did not look.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=4096
	Policies []SnapshotPolicy `json:"policies,omitempty"`

	// Enums is every enum type owned by the schema, read from pg_type
	// and pg_enum in sort order.
	//
	// Empty means "no enums" only when ModelVersion >= 2; below that it
	// means the capturing controller did not look. The same holds for
	// every field below.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	Enums []SnapshotEnum `json:"enums,omitempty"`

	// Extensions is every extension installed *into* this schema. An
	// extension the schema merely depends on but does not own — the
	// usual pgcrypto in public — belongs to LogicalDatabase.spec, which
	// is the layer that owns database-scoped objects.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Extensions []SnapshotExtension `json:"extensions,omitempty"`

	// Sequences is every sequence in the schema with its bounds and
	// step. The current value is deliberately absent: it changes on
	// every INSERT, and a structural snapshot that moved whenever the
	// data moved would report drift continuously.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=2048
	Sequences []SnapshotSequence `json:"sequences,omitempty"`

	// Functions is every function and procedure in the schema,
	// including bodies.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	Functions []SnapshotFunction `json:"functions,omitempty"`

	// Triggers is every user trigger in the schema, sorted by (table,
	// name). Internal triggers — the ones PostgreSQL creates to enforce
	// foreign keys — are excluded by the inspector; they are an
	// implementation detail of the constraints already captured, and
	// recording them would report the same fact twice.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=2048
	Triggers []SnapshotTrigger `json:"triggers,omitempty"`

	// MaterializedViews is every materialized view in the schema with
	// its defining SELECT. Refresh state is not recorded, for the same
	// reason sequence values are not.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=512
	MaterializedViews []SnapshotMaterializedView `json:"materializedViews,omitempty"`
}

// SnapshotEnum is one enum type and its labels.
type SnapshotEnum struct {
	// Name of the enum type.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Labels in PostgreSQL's sort order (pg_enum.enumsortorder), which
	// is the order the labels compare in, not the order they were
	// declared. Order is therefore semantic here — unlike a policy's
	// role list — and a reordered label list is a real change: it
	// silently reverses every ORDER BY and range comparison on the
	// column.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=1024
	Labels []string `json:"labels,omitempty"`
}

// SnapshotExtension is one extension installed into the schema.
type SnapshotExtension struct {
	// Name of the extension.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Schema the extension is installed into. Recorded even though the
	// snapshot describes one schema, because the inspector reports it
	// and a mismatch is a signal the capture is not what it claims.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Schema string `json:"schema,omitempty"`
}

// SnapshotSequence is one sequence's shape.
type SnapshotSequence struct {
	// Name of the sequence.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// DataType backing the sequence: smallint, integer or bigint. This
	// is the ceiling on how many values it can ever hand out, so a
	// change to it is a change to the table's capacity.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=32
	DataType string `json:"dataType,omitempty"`

	// IncrementBy is the step. Negative for a descending sequence.
	//
	// +optional
	IncrementBy int64 `json:"incrementBy,omitempty"`

	// MinValue is the lower bound.
	//
	// +optional
	MinValue int64 `json:"minValue,omitempty"`

	// MaxValue is the upper bound.
	//
	// +optional
	MaxValue int64 `json:"maxValue,omitempty"`

	// StartValue is the value the sequence restarts to.
	//
	// +optional
	StartValue int64 `json:"startValue,omitempty"`
}

// SnapshotFunction is one function or procedure, body included.
//
// Functions are overloadable, so Name alone does not identify one —
// (Name, Args) does. A reader that keys on the name will collapse an
// overload set and can report a dropped overload as unchanged, the same
// way keying a policy on its bare name collapses two tables' policies.
type SnapshotFunction struct {
	// Name of the function.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Args is the argument list as pg_get_function_arguments renders
	// it. Part of the identity, not a detail.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Args string `json:"args,omitempty"`

	// Returns is the result type as pg_get_function_result renders it.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Returns string `json:"returns,omitempty"`

	// Language the function is written in: sql, plpgsql, c, …
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Language string `json:"language,omitempty"`

	// Definition is the function body verbatim.
	//
	// Captured because the body is where a security-relevant change
	// hides: a SECURITY DEFINER helper, or the current_tenant() a
	// policy's USING clause calls, can be rewritten without touching
	// its signature. A snapshot that recorded only the signature would
	// report no change across exactly that edit.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=131072
	Definition string `json:"definition,omitempty"`
}

// SnapshotTrigger is one trigger binding.
//
// Trigger names are unique per table, not per schema, so (Table, Name)
// is the identity.
type SnapshotTrigger struct {
	// Name of the trigger.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Table the trigger is attached to.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Table string `json:"table"`

	// Timing is BEFORE, AFTER or INSTEAD OF.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=16
	Timing string `json:"timing,omitempty"`

	// Events the trigger fires on: INSERT, UPDATE, DELETE, TRUNCATE.
	//
	// The inspector builds this list positionally from pg_trigger's
	// tgtype bitfield, so its order is fixed by the query rather than
	// by the catalog and is stable across captures.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=4
	Events []string `json:"events,omitempty"`

	// ForEachRow distinguishes a row trigger from a statement trigger,
	// which decides how many times the function runs.
	//
	// Required, like SnapshotPolicy.Permissive and unlike
	// SnapshotTable.RLSEnabled. The rule is the same in all three
	// places: a field is optional exactly when an archived snapshot
	// could lack it. No snapshot captured below model version 2 carries
	// a trigger list at all, so there is no archive to keep admissible,
	// and requiring it stops a hand-authored trigger that omits the key
	// from silently meaning FOR EACH STATEMENT.
	ForEachRow bool `json:"forEachRow"`

	// Function the trigger calls. Its body is captured separately, in
	// Functions — a rewritten trigger function changes nothing here.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Function string `json:"function,omitempty"`

	// When is the optional WHEN clause, extracted from
	// pg_get_triggerdef. Empty when the trigger has none.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	When string `json:"when,omitempty"`
}

// SnapshotMaterializedView is one materialized view and its defining
// SELECT.
type SnapshotMaterializedView struct {
	// Name of the materialized view.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Definition is the SELECT verbatim, as pg_matviews reports it.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=131072
	Definition string `json:"definition,omitempty"`
}

// SnapshotPolicy is one row-level security policy as pg_policies
// reports it. Together with SnapshotTable's RLSEnabled/RLSForced this
// is the whole of a schema's tenant-isolation posture, which is why a
// snapshot that omits it cannot serve as the change-management
// evidence this type claims to be: dropping a policy, or flipping a
// table to NO FORCE, left the captured structure byte-identical.
type SnapshotPolicy struct {
	// Name of the policy. Unique per table, not per schema.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Table the policy is attached to.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Table string `json:"table"`

	// Command the policy governs: ALL, SELECT, INSERT, UPDATE or
	// DELETE.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=16
	Command string `json:"command,omitempty"`

	// Permissive is true for PERMISSIVE policies (OR-combined, the
	// PostgreSQL default) and false for RESTRICTIVE ones (AND-combined).
	// Recorded explicitly rather than by omission: the two combine in
	// opposite directions, so an evidence document has to state which.
	//
	// Required, unlike SnapshotTable.RLSEnabled. The back-compat hazard
	// that forces that one optional does not exist here: no snapshot
	// captured before model version 1 carries a policy list at all, so
	// there is no stored SnapshotPolicy that could be missing the
	// field. Requiring it stops a hand-authored policy from omitting it
	// and silently meaning RESTRICTIVE.
	Permissive bool `json:"permissive"`

	// Roles the policy applies to. A single "public" entry means all
	// roles.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Roles []string `json:"roles,omitempty"`

	// Using is the USING expression — the row filter applied to reads
	// and to the pre-image of writes.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Using string `json:"using,omitempty"`

	// WithCheck is the WITH CHECK expression — the predicate new rows
	// must satisfy. Empty means PostgreSQL falls back to Using.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	WithCheck string `json:"withCheck,omitempty"`
}

// SnapshotTable captures one table's columns.
type SnapshotTable struct {
	// Name of the table.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Kind is the PostgreSQL-reported table_type ("BASE TABLE",
	// "VIEW", "FOREIGN", etc.).
	//
	// +kubebuilder:validation:MaxLength=32
	Kind string `json:"kind,omitempty"`

	// Columns in ordinal (position) order.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=1600
	Columns []SnapshotColumn `json:"columns,omitempty"`

	// ViewDefinition is the defining SELECT, populated only when Kind
	// is VIEW. Empty for base tables.
	//
	// A view's columns are the same before and after its WHERE clause
	// is rewritten, so a snapshot holding only the column list cannot
	// tell a view that filters by tenant from one that no longer does.
	//
	// Only meaningful when the parent's ModelVersion >= 2.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=131072
	ViewDefinition string `json:"viewDefinition,omitempty"`

	// RLSEnabled mirrors pg_class.relrowsecurity — whether row-level
	// security is turned on for this table at all.
	//
	// Only meaningful when the parent's ModelVersion >= 1.
	//
	// The json tag carries no omitempty, so the controller always
	// writes it: this is a security fact and an evidence document
	// should assert it rather than imply it by absence.
	//
	// It is nonetheless +optional in the schema. Tables captured before
	// this field existed do not carry it, and this type's whole purpose
	// is that an archived snapshot can be brought back — replayed as
	// restore evidence, or re-applied into a rebuilt cluster. A
	// required field would make every pre-RLS snapshot YAML in the
	// archive fail admission against the new CRD, which is precisely
	// the artifact the registry exists to keep usable.
	//
	// +optional
	RLSEnabled bool `json:"rlsEnabled"`

	// RLSForced mirrors pg_class.relforcerowsecurity — whether RLS
	// also applies to the table's OWNER. This is the bit that usually
	// matters: ENABLE alone exempts the owner, so if the application
	// connects as the role that owns its tables (the common case) then
	// ENABLE-without-FORCE means every policy on the table is bypassed
	// for exactly the connection the policies exist to constrain. On a
	// multi-tenant schema that is a cross-tenant read.
	//
	// A pointer because nil must mean "this snapshot predates the
	// field", not "not forced". Snapshots are immutable, so pre-RLS
	// captures keep a nil here for as long as they are retained;
	// decoding that as false would report FORCE as newly added on
	// every table of every old snapshot compared against a new one.
	// The parent's ModelVersion is the authoritative signal — this
	// nil is the per-table corroboration of it.
	//
	// +optional
	RLSForced *bool `json:"rlsForced,omitempty"`
}

// SnapshotColumn captures one column's shape. DataType + UDTName are
// both preserved because PG exposes both (data_type "integer" vs
// udt_name "int4"); downstream tools may prefer one or the other.
type SnapshotColumn struct {
	// Name of the column.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Ordinal is the 1-based column position. Matches information_
	// schema.columns.ordinal_position so reordering is detectable.
	//
	// +kubebuilder:validation:Minimum=1
	Ordinal int32 `json:"ordinal"`

	// DataType is the information_schema.data_type value ("text",
	// "integer", "timestamp with time zone", etc.).
	//
	// +kubebuilder:validation:MaxLength=64
	DataType string `json:"dataType,omitempty"`

	// UDTName is the udt_name ("text", "int4", "timestamptz"). More
	// canonical than DataType for non-standard types.
	//
	// +kubebuilder:validation:MaxLength=64
	UDTName string `json:"udtName,omitempty"`

	// Nullable is the is_nullable flag.
	Nullable bool `json:"nullable"`

	// Default is the column_default expression, empty when no default.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Default string `json:"default,omitempty"`
}

// SnapshotObjectDDL is the generic name+definition pair used for
// indexes and constraints. Definition is the canonical PG-emitted
// DDL (pg_get_indexdef / pg_get_constraintdef) so diffs can compare
// byte-exact definitions without re-parsing.
type SnapshotObjectDDL struct {
	// Name of the object.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Table is the object's parent table, empty for schema-level
	// objects.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Table string `json:"table,omitempty"`

	// Type discriminates the object: btree/hash/gin/... for indexes,
	// PRIMARY KEY/UNIQUE/CHECK/FOREIGN KEY/EXCLUDE for constraints.
	//
	// +kubebuilder:validation:MaxLength=32
	Type string `json:"type,omitempty"`

	// Definition is the canonical DDL. Used for byte-exact diff.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=4096
	Definition string `json:"definition"`
}

// AppliedMigrationRecord is one row captured from the target's
// schema_migrations table. Shape matches
// `internal/migration.AppliedMigration` to keep the reconciler's
// translation trivial.
type AppliedMigrationRecord struct {
	// Version is the migration version as recorded in the tracking
	// table. Preserved verbatim — stringified even for legacy integer-
	// shaped tables.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=128
	Version string `json:"version"`

	// ContentHash is the SHA-256 hex over the migration's resolved
	// source at apply time. Empty string for entries from legacy
	// tables (e.g. golang-migrate's shape) that didn't record it.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=64
	ContentHash string `json:"contentHash,omitempty"`

	// AppliedAt is the timestamp the tracking table carries.
	//
	// +optional
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=snap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Schema",type="string",JSONPath=".spec.schemaRef"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Migrations",type="integer",JSONPath=".status.appliedMigrations[*]",priority=1
// +kubebuilder:printcolumn:name="CapturedAt",type="date",JSONPath=".status.capturedAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SchemaSnapshot is an immutable record of a DatabaseSchema's
// applied-migration state at a point in time. See the type's Spec
// comment for the restore workflow.
type SchemaSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SchemaSnapshotSpec   `json:"spec,omitempty"`
	Status SchemaSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SchemaSnapshotList is the list wrapper.
type SchemaSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SchemaSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SchemaSnapshot{}, &SchemaSnapshotList{})
}
