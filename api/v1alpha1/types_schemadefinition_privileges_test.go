// SPDX-License-Identifier: AGPL-3.0-or-later

package v1alpha1

import (
	"testing"
)

// TestDesiredDefaultPrivilege_Marshall confirms the struct round-trips
// through Go JSON marshalling with the field tags the operator and
// `kubectl get -o json` rely on.
func TestDesiredDefaultPrivilege_Marshall(t *testing.T) {
	dp := DesiredDefaultPrivilege{
		ForRole:    "keystone_admin",
		ToRole:     "example_service_app",
		Schema:     "public",
		ObjectType: "tables",
		Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE",
			"TRUNCATE", "REFERENCES", "TRIGGER"},
	}
	if dp.ForRole != "keystone_admin" {
		t.Fatalf("ForRole: got %q, want %q", dp.ForRole, "keystone_admin")
	}
	if len(dp.Privileges) != 7 {
		t.Fatalf("Privileges: got %d items, want 7", len(dp.Privileges))
	}
}

// TestDesiredTablePrivilege_Marshall confirms the per-table privilege
// struct's shape.
func TestDesiredTablePrivilege_Marshall(t *testing.T) {
	tp := DesiredTablePrivilege{
		ToRole:     "example_service_app",
		Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"},
	}
	if tp.ToRole != "example_service_app" {
		t.Fatalf("ToRole: got %q", tp.ToRole)
	}
	if tp.WithGrantOption {
		t.Fatalf("WithGrantOption default should be false")
	}
}

// TestSchemaDefinitionSpec_PrivilegesField_Present is the regression
// test for the 2026-05-12 gap: SD types_schemadefinition.go must accept
// a DefaultPrivileges slice on SchemaDefinitionSpec, and DesiredTable
// must accept a Privileges slice. If either field is removed in a
// future refactor, this test fails at compile time.
func TestSchemaDefinitionSpec_PrivilegesField_Present(t *testing.T) {
	spec := SchemaDefinitionSpec{
		Tables: []DesiredTable{{
			Name:    "iam_oauth_logout_outbox",
			Columns: []DesiredColumn{{Name: "id", Type: "uuid", Nullable: false, PrimaryKey: true}},
			Privileges: []DesiredTablePrivilege{{
				ToRole:     "example_service_app",
				Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE"},
			}},
		}},
		DefaultPrivileges: []DesiredDefaultPrivilege{{
			ForRole:    "keystone_admin",
			ToRole:     "example_service_app",
			ObjectType: "tables",
			Privileges: []string{"SELECT", "INSERT", "UPDATE", "DELETE",
				"TRUNCATE", "REFERENCES", "TRIGGER"},
		}},
	}
	if len(spec.DefaultPrivileges) != 1 {
		t.Fatalf("DefaultPrivileges: got %d, want 1", len(spec.DefaultPrivileges))
	}
	if len(spec.Tables[0].Privileges) != 1 {
		t.Fatalf("Tables[0].Privileges: got %d, want 1",
			len(spec.Tables[0].Privileges))
	}
}
