// SPDX-License-Identifier: AGPL-3.0-or-later

package schemadiff

import (
	"testing"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func col(name, dataType string, nullable bool) keystonev1alpha1.SnapshotColumn {
	return keystonev1alpha1.SnapshotColumn{
		Name: name, DataType: dataType, UDTName: dataType, Nullable: nullable,
	}
}

func tbl(name string, cols ...keystonev1alpha1.SnapshotColumn) keystonev1alpha1.SnapshotTable {
	return keystonev1alpha1.SnapshotTable{Name: name, Kind: "BASE TABLE", Columns: cols}
}

// snap builds a current-model snapshot. Tests that need the pre-RLS
// model set ModelVersion back to 0 explicitly, so that the older
// document is always the deliberate thing under test rather than an
// artefact of a helper that forgot to say which model it speaks.
func snap(tables []keystonev1alpha1.SnapshotTable, idx, cons []keystonev1alpha1.SnapshotObjectDDL) *keystonev1alpha1.StructuralSnapshot {
	return &keystonev1alpha1.StructuralSnapshot{
		ModelVersion: keystonev1alpha1.StructuralSnapshotModelVersion,
		Schema:       "public", Tables: tables, Indexes: idx, Constraints: cons,
	}
}

func findChange(changes []Change, kind ChangeKind, object string) *Change {
	for i := range changes {
		if changes[i].Kind == kind && changes[i].Object == object {
			return &changes[i]
		}
	}
	return nil
}

// -- Tables ----------------------------------------------------------

func TestDiff_TableAdded(t *testing.T) {
	from := snap(nil, nil, nil)
	to := snap([]keystonev1alpha1.SnapshotTable{tbl("users", col("id", "uuid", false))}, nil, nil)

	changes := Diff(from, to).Changes
	if c := findChange(changes, KindTableAdded, "users"); c == nil {
		t.Fatalf("TableAdded for users missing; got %+v", changes)
	}
	// New-table column additions are suppressed to avoid noise.
	if c := findChange(changes, KindColumnAdded, "id"); c != nil {
		t.Errorf("column-level adds should be suppressed for newly-added tables; got %+v", c)
	}
}

func TestDiff_TableRemoved(t *testing.T) {
	from := snap([]keystonev1alpha1.SnapshotTable{tbl("legacy", col("id", "int4", false))}, nil, nil)
	to := snap(nil, nil, nil)

	changes := Diff(from, to).Changes
	if c := findChange(changes, KindTableRemoved, "legacy"); c == nil {
		t.Fatalf("TableRemoved for legacy missing; got %+v", changes)
	}
}

// -- Columns ---------------------------------------------------------

func TestDiff_ColumnAdded(t *testing.T) {
	from := snap([]keystonev1alpha1.SnapshotTable{tbl("users", col("id", "uuid", false))}, nil, nil)
	to := snap([]keystonev1alpha1.SnapshotTable{
		tbl("users", col("id", "uuid", false), col("email", "text", true)),
	}, nil, nil)

	changes := Diff(from, to).Changes
	if c := findChange(changes, KindColumnAdded, "email"); c == nil {
		t.Fatalf("ColumnAdded for email missing; got %+v", changes)
	}
}

func TestDiff_ColumnRemoved(t *testing.T) {
	from := snap([]keystonev1alpha1.SnapshotTable{
		tbl("users", col("id", "uuid", false), col("old_field", "text", true)),
	}, nil, nil)
	to := snap([]keystonev1alpha1.SnapshotTable{tbl("users", col("id", "uuid", false))}, nil, nil)

	changes := Diff(from, to).Changes
	if c := findChange(changes, KindColumnRemoved, "old_field"); c == nil {
		t.Fatalf("ColumnRemoved for old_field missing; got %+v", changes)
	}
}

func TestDiff_ColumnRetyped(t *testing.T) {
	from := snap([]keystonev1alpha1.SnapshotTable{tbl("users", col("age", "integer", true))}, nil, nil)
	to := snap([]keystonev1alpha1.SnapshotTable{tbl("users", col("age", "bigint", true))}, nil, nil)

	changes := Diff(from, to).Changes
	c := findChange(changes, KindColumnRetyped, "age")
	if c == nil {
		t.Fatalf("ColumnRetyped for age missing; got %+v", changes)
	}
	if c.Before != "integer" || c.After != "bigint" {
		t.Errorf("Before=%q After=%q", c.Before, c.After)
	}
}

func TestDiff_ColumnNullability(t *testing.T) {
	from := snap([]keystonev1alpha1.SnapshotTable{tbl("users", col("email", "text", true))}, nil, nil)
	to := snap([]keystonev1alpha1.SnapshotTable{tbl("users", col("email", "text", false))}, nil, nil)

	changes := Diff(from, to).Changes
	c := findChange(changes, KindColumnNullability, "email")
	if c == nil {
		t.Fatalf("ColumnNullability for email missing; got %+v", changes)
	}
	if c.Before != "NULL" || c.After != "NOT NULL" {
		t.Errorf("Before=%q After=%q", c.Before, c.After)
	}
}

func TestDiff_ColumnDefaultChanged(t *testing.T) {
	fromCol := col("status", "text", false)
	fromCol.Default = "'pending'"
	toCol := col("status", "text", false)
	toCol.Default = "'active'"

	from := snap([]keystonev1alpha1.SnapshotTable{tbl("orders", fromCol)}, nil, nil)
	to := snap([]keystonev1alpha1.SnapshotTable{tbl("orders", toCol)}, nil, nil)

	changes := Diff(from, to).Changes
	c := findChange(changes, KindColumnDefaultChanged, "status")
	if c == nil {
		t.Fatalf("ColumnDefaultChanged for status missing; got %+v", changes)
	}
	if c.Before != "'pending'" || c.After != "'active'" {
		t.Errorf("Before=%q After=%q", c.Before, c.After)
	}
}

// -- Indexes + constraints -------------------------------------------

func TestDiff_IndexAdded(t *testing.T) {
	from := snap(nil, nil, nil)
	to := snap(nil, []keystonev1alpha1.SnapshotObjectDDL{
		{Name: "idx_users_email", Table: "users", Type: "btree",
			Definition: "CREATE INDEX idx_users_email ON users(email)"},
	}, nil)

	changes := Diff(from, to).Changes
	if c := findChange(changes, KindIndexAdded, "idx_users_email"); c == nil {
		t.Fatalf("IndexAdded missing; got %+v", changes)
	}
}

func TestDiff_IndexDefinitionChanged(t *testing.T) {
	fromIdx := []keystonev1alpha1.SnapshotObjectDDL{
		{Name: "idx_email", Table: "users", Type: "btree",
			Definition: "CREATE INDEX idx_email ON users(email)"},
	}
	toIdx := []keystonev1alpha1.SnapshotObjectDDL{
		{Name: "idx_email", Table: "users", Type: "btree",
			Definition: "CREATE UNIQUE INDEX idx_email ON users(email)"},
	}
	changes := Diff(snap(nil, fromIdx, nil), snap(nil, toIdx, nil)).Changes
	if c := findChange(changes, KindIndexDefinitionChanged, "idx_email"); c == nil {
		t.Fatalf("IndexDefinitionChanged missing; got %+v", changes)
	}
}

func TestDiff_ConstraintAddedRemoved(t *testing.T) {
	from := snap(nil, nil, []keystonev1alpha1.SnapshotObjectDDL{
		{Name: "fk_old", Table: "orders", Type: "FOREIGN KEY",
			Definition: "FOREIGN KEY (customer_id) REFERENCES customers(id)"},
	})
	to := snap(nil, nil, []keystonev1alpha1.SnapshotObjectDDL{
		{Name: "chk_new", Table: "orders", Type: "CHECK",
			Definition: "CHECK (total >= 0)"},
	})

	changes := Diff(from, to).Changes
	if c := findChange(changes, KindConstraintRemoved, "fk_old"); c == nil {
		t.Fatalf("ConstraintRemoved fk_old missing; got %+v", changes)
	}
	if c := findChange(changes, KindConstraintAdded, "chk_new"); c == nil {
		t.Fatalf("ConstraintAdded chk_new missing; got %+v", changes)
	}
}

// -- Nil safety ------------------------------------------------------

func TestDiff_NilFromAllAdded(t *testing.T) {
	to := snap([]keystonev1alpha1.SnapshotTable{tbl("t", col("c", "text", true))}, nil, nil)
	changes := Diff(nil, to).Changes
	if c := findChange(changes, KindTableAdded, "t"); c == nil {
		t.Fatalf("nil from: every table should be added; got %+v", changes)
	}
}

func TestDiff_IdenticalNoChanges(t *testing.T) {
	s := snap([]keystonev1alpha1.SnapshotTable{tbl("users", col("id", "uuid", false))}, nil, nil)
	if changes := Diff(s, s).Changes; len(changes) != 0 {
		t.Errorf("identical snapshots should yield 0 changes; got %d: %+v", len(changes), changes)
	}
}

// -- Change.String ---------------------------------------------------

func TestChangeString(t *testing.T) {
	cases := []struct {
		change Change
		want   string
	}{
		{Change{Kind: KindTableAdded, Object: "users"}, "TableAdded users"},
		{Change{Kind: KindColumnRetyped, Table: "users", Object: "age",
			Before: "integer", After: "bigint"}, "ColumnRetyped users.age: integer → bigint"},
		{Change{Kind: KindColumnRemoved, Table: "users", Object: "old",
			Before: "text NULL"}, "ColumnRemoved users.old (was text NULL)"},
	}
	for _, tc := range cases {
		if got := tc.change.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.change, got, tc.want)
		}
	}
}
