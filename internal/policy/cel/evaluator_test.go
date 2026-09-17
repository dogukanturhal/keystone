// SPDX-License-Identifier: AGPL-3.0-or-later

package cel

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func mustEvaluator(t *testing.T) *Evaluator {
	t.Helper()
	e, err := NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return e
}

func bundle(version string, labels map[string]string, strategy keystonev1alpha1.MigrationStrategy) *keystonev1alpha1.MigrationBundle {
	return &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "b1",
			Labels:    labels,
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  version,
			Strategy: strategy,
		},
	}
}

func TestEvaluator_PassAndFail(t *testing.T) {
	e := mustEvaluator(t)
	rules := []keystonev1alpha1.CELRule{
		{Name: "prod-needs-pgroll", Severity: keystonev1alpha1.CELSeverityError,
			Expression: `labels["tier"] == "prod" && strategy != "pgroll-expand-contract"`,
			Message:    "prod bundles must use pgroll"},
	}

	// Non-prod: rule does not fire.
	ctx := BundleContext{Bundle: bundle("v1", map[string]string{"tier": "dev"}, keystonev1alpha1.StrategyVersioned)}
	v := e.Evaluate(rules, ctx)
	if len(v) != 1 || v[0].Kind != "pass" {
		t.Errorf("non-prod: got %+v, want [pass]", v)
	}

	// Prod + versioned: rule fires with Error → Kind=fail, blocking.
	ctx.Bundle = bundle("v1", map[string]string{"tier": "prod"}, keystonev1alpha1.StrategyVersioned)
	v = e.Evaluate(rules, ctx)
	if len(v) != 1 || v[0].Kind != "fail" {
		t.Fatalf("prod+versioned: got %+v, want [fail]", v)
	}
	if !v[0].Blocking() {
		t.Errorf("fail verdict must be Blocking")
	}
	if v[0].Message != "prod bundles must use pgroll" {
		t.Errorf("message=%q, want the custom message", v[0].Message)
	}

	// Prod + pgroll: rule does not fire.
	ctx.Bundle = bundle("v1", map[string]string{"tier": "prod"}, keystonev1alpha1.StrategyPgrollExpandContract)
	v = e.Evaluate(rules, ctx)
	if v[0].Kind != "pass" {
		t.Errorf("prod+pgroll should pass; got %+v", v)
	}
}

func TestEvaluator_WarningIsNotBlocking(t *testing.T) {
	e := mustEvaluator(t)
	rules := []keystonev1alpha1.CELRule{
		{Name: "notify-only", Severity: keystonev1alpha1.CELSeverityWarning,
			Expression: `true`, Message: "always warns"},
	}
	v := e.Evaluate(rules, BundleContext{Bundle: bundle("v1", nil, keystonev1alpha1.StrategyVersioned)})
	if v[0].Kind != "warn" {
		t.Fatalf("Severity=Warning must produce Kind=warn; got %+v", v)
	}
	if v[0].Blocking() {
		t.Errorf("warn verdict must NOT be Blocking")
	}
}

func TestEvaluator_CompileErrorSurfaces(t *testing.T) {
	e := mustEvaluator(t)
	rules := []keystonev1alpha1.CELRule{
		{Name: "broken", Expression: `labels["tier" == "prod"`}, // missing close bracket
	}
	v := e.Evaluate(rules, BundleContext{Bundle: bundle("v1", nil, "")})
	if v[0].Kind != "compile" {
		t.Fatalf("broken expression should report compile; got %+v", v)
	}
	if !v[0].Blocking() {
		t.Errorf("compile error must be Blocking — silent disable is a footgun")
	}
}

func TestEvaluator_RuntimeNonBoolError(t *testing.T) {
	e := mustEvaluator(t)
	// `version` is a string variable; returning a string isn't a bool.
	rules := []keystonev1alpha1.CELRule{
		{Name: "non-bool", Expression: `version`},
	}
	v := e.Evaluate(rules, BundleContext{Bundle: bundle("v1.0.0", nil, "")})
	if v[0].Kind != "error" {
		t.Fatalf("non-bool result must report error; got %+v", v)
	}
	if !v[0].Blocking() {
		t.Errorf("error verdict must be Blocking")
	}
}

func TestEvaluator_FindingsAccessible(t *testing.T) {
	e := mustEvaluator(t)
	rules := []keystonev1alpha1.CELRule{
		{Name: "no-drop-table-fired", Severity: keystonev1alpha1.CELSeverityError,
			Expression: `findings.exists(f, f.rule == "no-drop-table")`,
			Message:    "no-drop-table flagged"},
	}
	ctx := BundleContext{
		Bundle: bundle("v1", nil, keystonev1alpha1.StrategyVersioned),
		Findings: []keystonev1alpha1.LintFinding{
			{Rule: "no-drop-table", Severity: keystonev1alpha1.LintLevelError},
		},
	}
	v := e.Evaluate(rules, ctx)
	if v[0].Kind != "fail" {
		t.Errorf("findings-driven rule should fire; got %+v", v)
	}

	// Empty findings: rule does not fire.
	ctx.Findings = nil
	v = e.Evaluate(rules, ctx)
	if v[0].Kind != "pass" {
		t.Errorf("empty findings should not fire; got %+v", v)
	}
}

func TestEvaluator_AnnotationExistenceCheck(t *testing.T) {
	e := mustEvaluator(t)
	rules := []keystonev1alpha1.CELRule{
		{Name: "require-jira-annotation", Severity: keystonev1alpha1.CELSeverityError,
			Expression: `!("keystone.hexxlock.io/jira" in annotations)`,
			Message:    "bundle must declare a jira ticket"},
	}

	// Missing annotation: fires.
	ctx := BundleContext{Bundle: bundle("v1", nil, "")}
	if v := e.Evaluate(rules, ctx); v[0].Kind != "fail" {
		t.Errorf("missing annotation should fire; got %+v", v)
	}

	// Annotation present: passes.
	ctx.Bundle.Annotations = map[string]string{"keystone.hexxlock.io/jira": "DATA-42"}
	if v := e.Evaluate(rules, ctx); v[0].Kind != "pass" {
		t.Errorf("present annotation should pass; got %+v", v)
	}
}

func TestEvaluator_CacheReusesProgram(t *testing.T) {
	e := mustEvaluator(t)
	// Same expression twice — second call should hit the cache.
	rules := []keystonev1alpha1.CELRule{
		{Name: "r1", Expression: `version == "v1"`},
	}
	_ = e.Evaluate(rules, BundleContext{Bundle: bundle("v1", nil, "")})
	// No direct cache observation (unexported); assert that a second
	// call still works without re-compile errors.
	_ = e.Evaluate(rules, BundleContext{Bundle: bundle("v1", nil, "")})

	// Internally: size should be 1 after two same-expression calls.
	if len(e.cache) != 1 {
		t.Errorf("cache should have 1 entry for repeated expression; got %d", len(e.cache))
	}
}

func TestEvaluator_NilBundleSafe(t *testing.T) {
	e := mustEvaluator(t)
	// A nil bundle must not panic. Exact verdict depends on the
	// expression — we only assert that the evaluation completes
	// without a runtime error (Kind != "error", "compile").
	rules := []keystonev1alpha1.CELRule{
		{Name: "r", Expression: `size(labels) == 0`},
	}
	v := e.Evaluate(rules, BundleContext{Bundle: nil})
	if v[0].Kind == "error" || v[0].Kind == "compile" {
		t.Errorf("nil bundle should not produce runtime/compile error; got %+v", v)
	}
}
