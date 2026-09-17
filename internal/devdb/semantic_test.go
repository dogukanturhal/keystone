// SPDX-License-Identifier: AGPL-3.0-or-later

package devdb

import (
	"testing"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/drift"
	"github.com/dogukanturhal/keystone-sdk/go/analyze"
)

func TestSemanticAnalyzer_NilPool(t *testing.T) {
	a := NewSemanticAnalyzer(nil)
	findings, err := a.Check(nil, &analyze.Migration{
		Files: []analyze.FileBody{{Name: "001.sql", Body: "CREATE TABLE t (id int)"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("nil pool should return no findings, got %v", findings)
	}
}

func TestSemanticAnalyzer_NoFiles(t *testing.T) {
	a := NewSemanticAnalyzer(&Pool{})
	findings, err := a.Check(nil, &analyze.Migration{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("no files should return no findings, got %v", findings)
	}
}

func TestSemanticAnalyzer_ID(t *testing.T) {
	a := NewSemanticAnalyzer(nil)
	if a.ID() != "semantic-replay" {
		t.Errorf("unexpected ID: %q", a.ID())
	}
}

func TestCheckOrphanedFKs(t *testing.T) {
	snap := &drift.Snapshot{
		Schema: "test",
		Tables: []drift.TableShape{
			{Name: "orders", Kind: "BASE TABLE"},
			// Note: "customers" table is missing.
		},
		Constraints: []drift.ObjectDDL{
			{Name: "orders_customer_fk", Table: "orders", Type: "FOREIGN KEY",
				Definition: "FOREIGN KEY (customer_id) REFERENCES customers(id)"},
		},
	}

	findings := checkOrphanedFKs(snap, &analyze.Migration{})
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Severity != keystonev1alpha1.LintLevelError {
		t.Errorf("expected severity=error, got %q", findings[0].Severity)
	}
	if findings[0].Rule != "semantic-orphaned-fk" {
		t.Errorf("expected rule=semantic-orphaned-fk, got %q", findings[0].Rule)
	}
}

func TestCheckOrphanedFKs_AllPresent(t *testing.T) {
	snap := &drift.Snapshot{
		Schema: "test",
		Tables: []drift.TableShape{
			{Name: "orders", Kind: "BASE TABLE"},
			{Name: "customers", Kind: "BASE TABLE"},
		},
		Constraints: []drift.ObjectDDL{
			{Name: "orders_customer_fk", Table: "orders", Type: "FOREIGN KEY",
				Definition: "FOREIGN KEY (customer_id) REFERENCES customers(id)"},
		},
	}

	findings := checkOrphanedFKs(snap, &analyze.Migration{})
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %v", findings)
	}
}

func TestCheckEmptyTables(t *testing.T) {
	snap := &drift.Snapshot{
		Schema: "test",
		Tables: []drift.TableShape{
			{Name: "empty_table", Kind: "BASE TABLE", Columns: nil},
			{Name: "normal_table", Kind: "BASE TABLE", Columns: []drift.ColumnShape{
				{Name: "id", Ordinal: 1, DataType: "uuid"},
			}},
		},
	}

	findings := checkEmptyTables(snap, &analyze.Migration{})
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Rule != "semantic-empty-table" {
		t.Errorf("expected rule=semantic-empty-table, got %q", findings[0].Rule)
	}
}

func TestTruncateError(t *testing.T) {
	short := "short error"
	if truncateError(errStr(short)) != short {
		t.Errorf("short error should not be truncated")
	}

	long := make([]byte, 600)
	for i := range long {
		long[i] = 'x'
	}
	result := truncateError(errStr(string(long)))
	if len(result) != 512 {
		t.Errorf("expected 512 chars, got %d", len(result))
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }
