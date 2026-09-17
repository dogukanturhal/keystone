// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// makeSpec builds the smallest spec exercising all three curation
// classes: one good FK + one dangling FK, one populated view + one empty
// view, one declared trigger + one dangling trigger.
func makeAuditSpec() *keystonev1alpha1.SchemaDefinitionSpec {
	return &keystonev1alpha1.SchemaDefinitionSpec{
		Tables: []keystonev1alpha1.DesiredTable{
			{
				Name: "iam_users",
				Columns: []keystonev1alpha1.DesiredColumn{
					{Name: "id", Type: "uuid", PrimaryKey: true},
					{Name: "tenant_id", Type: "uuid"},
				},
			},
			{
				Name: "iam_tenants",
				Columns: []keystonev1alpha1.DesiredColumn{
					{Name: "id", Type: "uuid", PrimaryKey: true},
				},
			},
			{
				Name: "iam_license_activations",
				Columns: []keystonev1alpha1.DesiredColumn{
					{Name: "id", Type: "uuid", PrimaryKey: true},
					{Name: "license_id", Type: "uuid"},
					{Name: "tenant_id", Type: "uuid"},
				},
				ForeignKeys: []keystonev1alpha1.DesiredForeignKey{
					{
						Name:              "iam_license_activations_tenant_id_fkey",
						Columns:           []string{"tenant_id"},
						ReferencesTable:   "iam_tenants",
						ReferencesColumns: []string{"id"},
					},
					{
						// Dangling: iam_licenses is not declared.
						Name:              "iam_license_activations_license_id_fkey",
						Columns:           []string{"license_id"},
						ReferencesTable:   "iam_licenses",
						ReferencesColumns: []string{"id"},
					},
				},
			},
		},
		Functions: []keystonev1alpha1.DesiredFunction{
			{Name: "set_updated_at", Returns: "trigger", Language: "plpgsql"},
		},
		Views: []keystonev1alpha1.DesiredView{
			{Name: "v_active_users", Query: "SELECT * FROM iam_users WHERE deleted_at IS NULL"},
			{Name: "v_user_tenants_recent", Query: "  "}, // empty (whitespace only)
		},
		Triggers: []keystonev1alpha1.DesiredTrigger{
			{
				Name:     "trig_users_updated_at",
				Table:    "iam_users",
				Function: "set_updated_at",
			},
			{
				// Dangling: log_changes lives in audit schema, not declared here.
				Name:     "audit_iam_users",
				Table:    "iam_users",
				Function: "log_changes",
			},
		},
	}
}

func TestAuditDanglingRefs_FlagOnly(t *testing.T) {
	spec := makeAuditSpec()
	report := auditDanglingRefs(spec, false)

	if got, want := len(report.danglingFKs), 1; got != want {
		t.Errorf("danglingFKs: got %d, want %d", got, want)
	}
	if got, want := len(report.emptyViews), 1; got != want {
		t.Errorf("emptyViews: got %d, want %d", got, want)
	}
	if got, want := len(report.danglingTriggers), 1; got != want {
		t.Errorf("danglingTriggers: got %d, want %d", got, want)
	}

	// strip=false MUST leave the spec untouched.
	if got := len(spec.Tables[2].ForeignKeys); got != 2 {
		t.Errorf("strip=false but FK count changed: got %d, want 2", got)
	}
	if got := len(spec.Views); got != 2 {
		t.Errorf("strip=false but view count changed: got %d, want 2", got)
	}
	if got := len(spec.Triggers); got != 2 {
		t.Errorf("strip=false but trigger count changed: got %d, want 2", got)
	}
}

func TestAuditDanglingRefs_StripRemovesAndReports(t *testing.T) {
	spec := makeAuditSpec()
	report := auditDanglingRefs(spec, true)

	if report.Total() != 3 {
		t.Errorf("expected 3 issues, got %d", report.Total())
	}

	// Dangling FK gone, good FK kept.
	fks := spec.Tables[2].ForeignKeys
	if len(fks) != 1 {
		t.Fatalf("expected 1 FK kept after strip, got %d", len(fks))
	}
	if fks[0].Name != "iam_license_activations_tenant_id_fkey" {
		t.Errorf("wrong FK kept: %s", fks[0].Name)
	}

	// Empty view stripped, populated view kept.
	if len(spec.Views) != 1 {
		t.Fatalf("expected 1 view kept, got %d", len(spec.Views))
	}
	if spec.Views[0].Name != "v_active_users" {
		t.Errorf("wrong view kept: %s", spec.Views[0].Name)
	}

	// Dangling trigger stripped.
	if len(spec.Triggers) != 1 {
		t.Fatalf("expected 1 trigger kept, got %d", len(spec.Triggers))
	}
	if spec.Triggers[0].Name != "trig_users_updated_at" {
		t.Errorf("wrong trigger kept: %s", spec.Triggers[0].Name)
	}
}

func TestAuditDanglingRefs_CleanSpecHasNoIssues(t *testing.T) {
	spec := &keystonev1alpha1.SchemaDefinitionSpec{
		Tables: []keystonev1alpha1.DesiredTable{
			{Name: "users"},
			{Name: "tenants"},
		},
		Functions: []keystonev1alpha1.DesiredFunction{
			{Name: "set_updated_at"},
		},
		Views: []keystonev1alpha1.DesiredView{
			{Name: "v_users", Query: "SELECT * FROM users"},
		},
		Triggers: []keystonev1alpha1.DesiredTrigger{
			{Name: "t1", Table: "users", Function: "set_updated_at"},
		},
	}
	report := auditDanglingRefs(spec, true)
	if report.Total() != 0 {
		t.Errorf("clean spec produced issues: %+v", report)
	}
}

func TestAuditDanglingRefs_NilSpecSafe(t *testing.T) {
	report := auditDanglingRefs(nil, true)
	if report.Total() != 0 {
		t.Errorf("nil spec produced issues: %+v", report)
	}
}

func TestDanglingReport_WriteWarnings_Format(t *testing.T) {
	report := &danglingReport{
		danglingFKs:      []string{"t1.fk1 -> missing_table"},
		emptyViews:       []string{"v_empty"},
		danglingTriggers: []string{"trig on t1 -> log_changes()"},
	}
	var buf bytes.Buffer
	report.WriteWarnings(&buf, true)
	out := buf.String()
	for _, want := range []string{
		"level=warn class=dangling-fk verb=stripped",
		"level=warn class=empty-view verb=stripped",
		"level=warn class=dangling-trigger verb=stripped",
		"level=info class=summary",
		"stripped 1 dangling-fks, 1 empty-views, 1 dangling-triggers",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, out)
		}
	}

	buf.Reset()
	report.WriteWarnings(&buf, false)
	out = buf.String()
	for _, want := range []string{
		"verb=kept",
		"flagged 1 dangling-fks, 1 empty-views, 1 dangling-triggers",
		"--strip-dangling-refs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("flag-only output missing %q\nfull output:\n%s", want, out)
		}
	}
}
