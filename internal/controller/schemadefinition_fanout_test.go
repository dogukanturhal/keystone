// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/declarative"
)

func TestSelectCanonicalPlan_SingleSchema(t *testing.T) {
	s := &keystonev1alpha1.DatabaseSchema{ObjectMeta: metav1.ObjectMeta{Name: "only"}}
	plan := &declarative.Plan{Statements: []string{"CREATE TABLE t (id INT)"}}
	results := []schemaDiff{{schema: s, plan: plan, inspectedAt: metav1.Now()}}

	canonical, gotPlan, drifted := selectCanonicalPlan(results)
	if canonical != s {
		t.Fatalf("canonical = %v, want %v", canonical, s)
	}
	if gotPlan != plan {
		t.Fatalf("plan != input")
	}
	if drifted != nil {
		t.Fatalf("drifted = %v, want nil for single schema", drifted)
	}
}

func TestSelectCanonicalPlan_LockstepFanout(t *testing.T) {
	stmts := []string{"CREATE TABLE t (id INT)", "CREATE INDEX idx ON t(id)"}
	mkR := func(name string) schemaDiff {
		return schemaDiff{
			schema:      &keystonev1alpha1.DatabaseSchema{ObjectMeta: metav1.ObjectMeta{Name: name}},
			plan:        &declarative.Plan{Statements: append([]string{}, stmts...)},
			inspectedAt: metav1.Now(),
		}
	}
	results := []schemaDiff{mkR("a"), mkR("b"), mkR("c")}

	_, plan, drifted := selectCanonicalPlan(results)
	if plan == nil || len(plan.Statements) != 2 {
		t.Fatalf("plan = %v, want 2 stmts", plan)
	}
	if drifted != nil {
		t.Fatalf("drifted = %v, want nil for lockstep fan-out", drifted)
	}
}

func TestSelectCanonicalPlan_LockstepReorderedTolerated(t *testing.T) {
	mkR := func(name string, stmts []string) schemaDiff {
		return schemaDiff{
			schema:      &keystonev1alpha1.DatabaseSchema{ObjectMeta: metav1.ObjectMeta{Name: name}},
			plan:        &declarative.Plan{Statements: stmts},
			inspectedAt: metav1.Now(),
		}
	}
	// Two independent CREATE INDEX in different orders — same set.
	results := []schemaDiff{
		mkR("a", []string{
			"CREATE INDEX idx1 ON t(a)",
			"CREATE INDEX idx2 ON t(b)",
		}),
		mkR("b", []string{
			"CREATE INDEX idx2 ON t(b)",
			"CREATE INDEX idx1 ON t(a)",
		}),
	}

	_, _, drifted := selectCanonicalPlan(results)
	if drifted != nil {
		t.Errorf("reordered-equal-set should be lockstep, got drifted=%v", drifted)
	}
}

func TestSelectCanonicalPlan_DriftDetected(t *testing.T) {
	results := []schemaDiff{
		{
			schema:      &keystonev1alpha1.DatabaseSchema{ObjectMeta: metav1.ObjectMeta{Name: "ahead"}},
			plan:        &declarative.Plan{Statements: []string{"CREATE INDEX idx ON t(c)"}},
			inspectedAt: metav1.Now(),
		},
		{
			schema:      &keystonev1alpha1.DatabaseSchema{ObjectMeta: metav1.ObjectMeta{Name: "behind"}},
			plan:        &declarative.Plan{Statements: []string{"CREATE TABLE t (id INT)", "CREATE INDEX idx ON t(c)"}},
			inspectedAt: metav1.Now(),
		},
	}

	canonical, plan, drifted := selectCanonicalPlan(results)
	if canonical.Name != "behind" {
		t.Errorf("canonical = %s, want 'behind' (most-behind)", canonical.Name)
	}
	if len(plan.Statements) != 2 {
		t.Errorf("canonical plan should have 2 stmts (most-behind), got %d", len(plan.Statements))
	}
	if len(drifted) != 1 || drifted[0].SchemaRef != "ahead" {
		t.Errorf("drifted = %v, want [ahead]", drifted)
	}
	if drifted[0].PendingOperations != 1 {
		t.Errorf("drifted PendingOperations = %d, want 1", drifted[0].PendingOperations)
	}
}

func TestSameStatementSet(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", nil, nil, true},
		{"identical order", []string{"a", "b"}, []string{"a", "b"}, true},
		{"reordered", []string{"a", "b"}, []string{"b", "a"}, true},
		{"different len", []string{"a"}, []string{"a", "b"}, false},
		{"same len different", []string{"a", "b"}, []string{"a", "c"}, false},
		{"duplicates respected", []string{"a", "a", "b"}, []string{"a", "b", "b"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameStatementSet(tc.a, tc.b); got != tc.want {
				t.Errorf("sameStatementSet(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestPolicyOf_DefaultsToRefuse(t *testing.T) {
	sd := &keystonev1alpha1.SchemaDefinition{}
	if got := policyOf(sd); got != keystonev1alpha1.MixedVersionPolicyRefuse {
		t.Errorf("policyOf empty = %v, want Refuse", got)
	}
	sd.Spec.MixedVersionPolicy = keystonev1alpha1.MixedVersionPolicyMostBehindWins
	if got := policyOf(sd); got != keystonev1alpha1.MixedVersionPolicyMostBehindWins {
		t.Errorf("policyOf MostBehindWins = %v", got)
	}
}

func TestBundleSchemaSelector_SchemaRefPath(t *testing.T) {
	sd := &keystonev1alpha1.SchemaDefinition{
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef: "my-schema",
		},
	}
	canonical := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "my-schema"},
	}
	got := bundleSchemaSelector(sd, canonical)
	want := "my-schema"
	if got.MatchLabels["keystone.hexxlock.io/name"] != want {
		t.Errorf("schemaRef path: got %v, want match-label name=%s", got, want)
	}
}

func TestBundleSchemaSelector_SchemaSelectorPath(t *testing.T) {
	sd := &keystonev1alpha1.SchemaDefinition{
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"keystone.hexxlock.io/scope": "example-service",
					"keystone.hexxlock.io/tier":  "tenant",
				},
			},
		},
	}
	canonical := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-001"},
	}
	got := bundleSchemaSelector(sd, canonical)
	if got.MatchLabels["keystone.hexxlock.io/scope"] != "example-service" ||
		got.MatchLabels["keystone.hexxlock.io/tier"] != "tenant" {
		t.Errorf("schemaSelector path should mirror SD selector: got %v", got)
	}
	if _, hasName := got.MatchLabels["keystone.hexxlock.io/name"]; hasName {
		t.Errorf("schemaSelector path must not narrow to a single schema by name; got %v", got)
	}
	// Verify deep-copy: mutating the returned selector must not poison the SD's.
	got.MatchLabels["mutated"] = "x"
	if _, leaked := sd.Spec.SchemaSelector.MatchLabels["mutated"]; leaked {
		t.Errorf("bundleSchemaSelector returned a shallow alias; SD selector was mutated")
	}
}
