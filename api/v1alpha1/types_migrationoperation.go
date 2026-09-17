// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// MigrationOperationKind is the discriminator for the per-operation
// payload. Each kind has its own spec sub-struct; only the matching one
// must be populated. Admission rejects multi-spec operations.
//
// +kubebuilder:validation:Enum=add_column;drop_column;rename_column;add_constraint;alter_column_type;set_not_null;drop_not_null
type MigrationOperationKind string

const (
	// MigrationOperationAddColumn adds a column to an existing table.
	// In Expand phase: ALTER TABLE ADD COLUMN (nullable). Apps that
	// don't reference the new column are unaffected. In Contract phase:
	// optionally SET NOT NULL after the operator confirms backfill.
	MigrationOperationAddColumn MigrationOperationKind = "add_column"

	// MigrationOperationDropColumn drops a column. In Expand phase: no
	// schema change — the column is marked deprecated in the bundle's
	// metadata, giving apps time to deploy without referencing it. In
	// Contract phase: ALTER TABLE DROP COLUMN.
	MigrationOperationDropColumn MigrationOperationKind = "drop_column"

	// MigrationOperationRenameColumn renames a column. In Expand phase:
	// ADD new column, COPY existing values, INSTALL trigger that mirrors
	// writes between the two columns so apps reading either name see
	// fresh data. In Contract phase: DROP TRIGGER, DROP old column.
	MigrationOperationRenameColumn MigrationOperationKind = "rename_column"

	// MigrationOperationAddConstraint adds a CHECK or FOREIGN KEY
	// constraint without locking the table. In Expand phase: ALTER
	// TABLE ADD CONSTRAINT … NOT VALID (no full-table scan, no AccessExclusive
	// lock); in Contract phase: ALTER TABLE … VALIDATE CONSTRAINT
	// (needs SHARE UPDATE EXCLUSIVE — concurrent reads + writes
	// permitted; the validation scan can take a long time on big
	// tables but does not block traffic).
	MigrationOperationAddConstraint MigrationOperationKind = "add_constraint"

	// MigrationOperationAlterColumnType changes a column's type via the
	// shadow-column pattern. Expand: ADD shadow column with new type;
	// COPY converted data; INSTALL trigger to mirror writes both ways.
	// Contract: DROP old column + RENAME shadow → original. Behaves
	// identically to rename_column under the hood, but the new column
	// is created with a different type.
	MigrationOperationAlterColumnType MigrationOperationKind = "alter_column_type"

	// MigrationOperationSetNotNull makes an existing nullable column
	// NOT NULL without a full-table AccessExclusive lock.
	// Expand: ADD CONSTRAINT chk_<col>_notnull CHECK (<col> IS NOT NULL)
	// NOT VALID — flags future writes but scans no rows. Contract:
	// VALIDATE CONSTRAINT (SHARE UPDATE EXCLUSIVE), then SET NOT NULL
	// (instant because PG recognises the proven CHECK), then DROP
	// CONSTRAINT (redundant once NOT NULL is in place).
	MigrationOperationSetNotNull MigrationOperationKind = "set_not_null"

	// MigrationOperationDropNotNull removes a NOT NULL constraint.
	// Single-phase: Expand issues ALTER COLUMN DROP NOT NULL (a metadata-
	// only operation under AccessExclusive lock that PG holds only long
	// enough to flip the flag). Contract is a no-op. Abort is
	// best-effort: once DROP NOT NULL ran, NULL rows may have been
	// inserted, so re-establishing NOT NULL is not a clean rollback —
	// the abort handler records a warning and does not re-apply the
	// constraint.
	MigrationOperationDropNotNull MigrationOperationKind = "drop_not_null"
)

// MigrationOperation is one declarative pgroll-style operation. The
// MigrationBundle authoring contract is "exactly one operation per
// MigrationBundle" so plans + status + Argo CD diffs stay readable.
// Multi-op bundles are deferred to Phase 6.1.
type MigrationOperation struct {
	// Kind selects which sub-spec the controller reads.
	//
	// +kubebuilder:validation:Required
	Kind MigrationOperationKind `json:"kind"`

	// Table is the target table the operation acts on. Required for
	// every kind today.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Table string `json:"table"`

	// AddColumn is populated when Kind=add_column.
	//
	// +optional
	AddColumn *AddColumnOp `json:"addColumn,omitempty"`

	// DropColumn is populated when Kind=drop_column.
	//
	// +optional
	DropColumn *DropColumnOp `json:"dropColumn,omitempty"`

	// RenameColumn is populated when Kind=rename_column.
	//
	// +optional
	RenameColumn *RenameColumnOp `json:"renameColumn,omitempty"`

	// AddConstraint is populated when Kind=add_constraint.
	//
	// +optional
	AddConstraint *AddConstraintOp `json:"addConstraint,omitempty"`

	// AlterColumnType is populated when Kind=alter_column_type.
	//
	// +optional
	AlterColumnType *AlterColumnTypeOp `json:"alterColumnType,omitempty"`

	// SetNotNull is populated when Kind=set_not_null.
	//
	// +optional
	SetNotNull *SetNotNullOp `json:"setNotNull,omitempty"`

	// DropNotNull is populated when Kind=drop_not_null.
	//
	// +optional
	DropNotNull *DropNotNullOp `json:"dropNotNull,omitempty"`
}

// SetNotNullOp is the payload for Kind=set_not_null.
type SetNotNullOp struct {
	// Column is the name of the column to transition to NOT NULL. The
	// column must already exist and must be nullable; admission
	// enforces neither — the engine errors with a clear diagnostic.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Column string `json:"column"`
}

// DropNotNullOp is the payload for Kind=drop_not_null.
type DropNotNullOp struct {
	// Column is the name of the column to transition to NULLABLE.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Column string `json:"column"`
}

// AddConstraintOp adds a CHECK or FOREIGN KEY without locking. Other
// constraint kinds (UNIQUE, PRIMARY KEY) require a different shape
// (CREATE INDEX CONCURRENTLY first, then ADD CONSTRAINT USING INDEX);
// admission webhooks should refuse those for now.
type AddConstraintOp struct {
	// Name of the constraint.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Name string `json:"name"`

	// Type discriminates the constraint shape.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=check;foreign_key
	Type string `json:"type"`

	// Definition is the constraint body inserted between CONSTRAINT
	// and NOT VALID. For check: the predicate, e.g. "(score >= 0 AND
	// score <= 100)". For foreign_key: the full FK clause, e.g.
	// "FOREIGN KEY (lead_id) REFERENCES leads(id)".
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=2048
	Definition string `json:"definition"`
}

// AlterColumnTypeOp changes a column's type via shadow column.
type AlterColumnTypeOp struct {
	// Column to retype.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Column string `json:"column"`

	// NewType is the target PostgreSQL type. Same charset restrictions
	// as AddColumnOp.Type.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=128
	NewType string `json:"newType"`

	// CastExpression is the SQL fragment used to convert the existing
	// value to NewType. Default: "<column>::<newType>" (PostgreSQL's
	// implicit cast). Specify a custom expression for non-trivial
	// conversions, e.g. "to_jsonb(<column>)".
	//
	// +optional
	// +kubebuilder:validation:MaxLength=512
	CastExpression string `json:"castExpression,omitempty"`
}

// AddColumnOp adds a column.
type AddColumnOp struct {
	// Name of the new column.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Name string `json:"name"`

	// Type is the PostgreSQL type (TEXT, INTEGER, NUMERIC(10,2), …).
	// Validated only at API level by length; controller hands it through
	// after identifier-style sanity checks (no semicolons or quotes).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=128
	Type string `json:"type"`

	// Default is the column DEFAULT clause. Applied at column creation
	// time, so existing rows get the default. Empty = no default.
	//
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Default string `json:"default,omitempty"`

	// Nullable controls whether the column accepts NULL. Default true
	// to keep the Expand phase non-blocking; flip to false in Contract
	// phase only after backfill is confirmed.
	//
	// +kubebuilder:default=true
	Nullable bool `json:"nullable,omitempty"`

	// EnforceNotNullInContract — when true and Nullable is true, the
	// Contract phase issues SET NOT NULL after the operator confirms
	// backfill via the complete annotation. Default false.
	//
	// +optional
	EnforceNotNullInContract bool `json:"enforceNotNullInContract,omitempty"`
}

// DropColumnOp drops a column. Two-phase: Expand records the deprecation
// (status only); Contract issues DROP COLUMN.
type DropColumnOp struct {
	// Name of the column to drop.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	Name string `json:"name"`
}

// RenameColumnOp renames a column. Expand: ADD new + COPY + TRIGGER for
// dual-write; Contract: DROP TRIGGER + DROP old.
type RenameColumnOp struct {
	// From is the existing column name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	From string `json:"from"`

	// To is the new column name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	To string `json:"to"`
}
