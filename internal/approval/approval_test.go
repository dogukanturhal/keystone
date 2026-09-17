// SPDX-License-Identifier: AGPL-3.0-or-later

package approval

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func bundleWithAnnotations(name string, annos map[string]string, labels map[string]string) *keystonev1alpha1.MigrationBundle {
	return &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "keystone-system",
			Annotations: annos,
			Labels:      labels,
		},
		Spec: keystonev1alpha1.MigrationBundleSpec{Version: "v1"},
	}
}

func policyWithApprovals(name string, aps []keystonev1alpha1.ApprovalPolicy) keystonev1alpha1.SchemaPolicy {
	return keystonev1alpha1.SchemaPolicy{
		ObjectMeta:       metav1.ObjectMeta{Name: name},
		Spec: keystonev1alpha1.SchemaPolicySpec{
			TargetSelector:   metav1.LabelSelector{},
			ApprovalPolicies: aps,
		},
	}
}

// -- ParseEvidence --------------------------------------------------

func TestParseEvidenceExtractsApprovals(t *testing.T) {
	annos := map[string]string{
		keystonev1alpha1.AnnotationAuthor:                              "alice@example.com",
		keystonev1alpha1.AnnotationRiskTier:                            "high",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.bob":          "dba",
		keystonev1alpha1.AnnotationApprovalPrefix + "peer-review.carol": "schema-reviewers",
		"unrelated.annotation":                                         "ignore-me",
	}
	ev := ParseEvidence(annos)

	if ev.Author != "alice@example.com" {
		t.Errorf("author=%q", ev.Author)
	}
	if ev.RiskTier != "high" {
		t.Errorf("riskTier=%q", ev.RiskTier)
	}
	if len(ev.Approvals) != 2 {
		t.Fatalf("want 2 approvals, got %d: %+v", len(ev.Approvals), ev.Approvals)
	}
	// Sorted: dba.bob before peer-review.carol.
	if ev.Approvals[0].PolicyName != "dba" || ev.Approvals[0].ApproverID != "bob" || ev.Approvals[0].Group != "dba" {
		t.Errorf("entry[0]=%+v", ev.Approvals[0])
	}
	if ev.Approvals[1].PolicyName != "peer-review" || ev.Approvals[1].ApproverID != "carol" {
		t.Errorf("entry[1]=%+v", ev.Approvals[1])
	}
}

func TestParseEvidenceDropsMalformedKeys(t *testing.T) {
	annos := map[string]string{
		keystonev1alpha1.AnnotationApprovalPrefix:                  "no-policy-no-id",
		keystonev1alpha1.AnnotationApprovalPrefix + "only-policy":  "no-dot",
		keystonev1alpha1.AnnotationApprovalPrefix + ".":            "empty-both",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.":         "no-id",
		keystonev1alpha1.AnnotationApprovalPrefix + ".alice":       "no-policy",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.alice":    "dba", // valid
	}
	ev := ParseEvidence(annos)
	if len(ev.Approvals) != 1 {
		t.Fatalf("want 1 valid approval, got %d: %+v", len(ev.Approvals), ev.Approvals)
	}
	if ev.Approvals[0].ApproverID != "alice" {
		t.Errorf("entry=%+v", ev.Approvals[0])
	}
}

// -- Evaluate — single policy ------------------------------------------

func TestEvaluateSingleApprovalSatisfied(t *testing.T) {
	bundle := bundleWithAnnotations("b1", map[string]string{
		keystonev1alpha1.AnnotationAuthor:                       "alice",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.bob":   "dba",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.carol": "dba",
	}, nil)
	sp := policyWithApprovals("prod", []keystonev1alpha1.ApprovalPolicy{{
		Name:                 "dba",
		RequiredApprovers:    2,
		FromGroups:           []string{"dba"},
		DisallowSelfApproval: true,
	}})
	res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil)
	if !res.Satisfied {
		t.Fatalf("want satisfied; got %+v", res)
	}
	if len(res.Policies) != 1 || res.Policies[0].Received != 2 {
		t.Fatalf("policies=%+v", res.Policies)
	}
}

func TestEvaluateInsufficientApprovers(t *testing.T) {
	bundle := bundleWithAnnotations("b2", map[string]string{
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.bob": "dba",
	}, nil)
	sp := policyWithApprovals("prod", []keystonev1alpha1.ApprovalPolicy{{
		Name:              "dba",
		RequiredApprovers: 2,
		FromGroups:        []string{"dba"},
	}})
	res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil)
	if res.Satisfied {
		t.Fatal("expected unsatisfied")
	}
	unsat := res.Unsatisfied()
	if len(unsat) != 1 || unsat[0].Received != 1 || unsat[0].Required != 2 {
		t.Fatalf("unsat=%+v", unsat)
	}
}

func TestEvaluateRejectsWrongGroup(t *testing.T) {
	bundle := bundleWithAnnotations("b3", map[string]string{
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.bob":   "dba",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.carol": "sre", // wrong group
	}, nil)
	sp := policyWithApprovals("prod", []keystonev1alpha1.ApprovalPolicy{{
		Name:              "dba",
		RequiredApprovers: 2,
		FromGroups:        []string{"dba"},
	}})
	res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil)
	if res.Satisfied {
		t.Fatal("wrong-group approver should not count")
	}
	if res.Policies[0].Received != 1 {
		t.Errorf("expected 1 counted approver, got %d", res.Policies[0].Received)
	}
}

func TestEvaluateSelfApprovalDisallowed(t *testing.T) {
	bundle := bundleWithAnnotations("b4", map[string]string{
		keystonev1alpha1.AnnotationAuthor:                       "alice",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.alice": "dba", // self
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.bob":   "dba",
	}, nil)
	sp := policyWithApprovals("prod", []keystonev1alpha1.ApprovalPolicy{{
		Name:                 "dba",
		RequiredApprovers:    2,
		FromGroups:           []string{"dba"},
		DisallowSelfApproval: true,
	}})
	res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil)
	if res.Satisfied {
		t.Fatal("self-approval must be filtered; bundle should be unsatisfied")
	}
	if res.Policies[0].Received != 1 {
		t.Errorf("expected 1 counted approver (alice filtered), got %d", res.Policies[0].Received)
	}
}

func TestEvaluateSelfApprovalAllowedWhenBoolFalse(t *testing.T) {
	bundle := bundleWithAnnotations("b5", map[string]string{
		keystonev1alpha1.AnnotationAuthor:                       "alice",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.alice": "dba",
	}, nil)
	sp := policyWithApprovals("dev", []keystonev1alpha1.ApprovalPolicy{{
		Name:                 "dba",
		RequiredApprovers:    1,
		FromGroups:           []string{"dba"},
		DisallowSelfApproval: false,
	}})
	res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil)
	if !res.Satisfied {
		t.Fatalf("DisallowSelfApproval=false: self-approval must count. got %+v", res)
	}
}

// -- Evaluate — AppliesWhen conditions --------------------------------

func TestEvaluateAppliesWhenRiskTier(t *testing.T) {
	bundle := bundleWithAnnotations("b6", map[string]string{
		keystonev1alpha1.AnnotationRiskTier: "low",
	}, nil)
	sp := policyWithApprovals("prod", []keystonev1alpha1.ApprovalPolicy{{
		Name:              "dba",
		RequiredApprovers: 2,
		FromGroups:        []string{"dba"},
		AppliesWhen:       &keystonev1alpha1.ApprovalCondition{RiskTier: "critical"},
	}})
	// Bundle is risk-tier=low; policy requires critical → not applicable.
	res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil)
	if !res.Satisfied {
		t.Fatalf("non-matching riskTier should skip policy; got unsatisfied")
	}
	if res.Policies[0].Applied {
		t.Errorf("policy should be Applied=false")
	}
}

func TestEvaluateAppliesWhenBundleSelector(t *testing.T) {
	// Policy fires only for bundles labelled tier=prod.
	sp := policyWithApprovals("all-tiers", []keystonev1alpha1.ApprovalPolicy{{
		Name:              "dba",
		RequiredApprovers: 1,
		FromGroups:        []string{"dba"},
		AppliesWhen: &keystonev1alpha1.ApprovalCondition{
			BundleSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
		},
	}})
	// Dev bundle: policy must skip.
	devBundle := bundleWithAnnotations("b7-dev", nil, map[string]string{"tier": "dev"})
	if res := Evaluate(devBundle, []keystonev1alpha1.SchemaPolicy{sp}, nil); !res.Satisfied {
		t.Fatalf("dev bundle should pass; got %+v", res)
	}
	// Prod bundle: policy must fire (and fail without approvers).
	prodBundle := bundleWithAnnotations("b7-prod", nil, map[string]string{"tier": "prod"})
	if res := Evaluate(prodBundle, []keystonev1alpha1.SchemaPolicy{sp}, nil); res.Satisfied {
		t.Fatalf("prod bundle with no approvers should fail")
	}
}

func TestEvaluateAppliesWhenMinLintSeverity(t *testing.T) {
	// Policy fires only when there's at least one warning.
	sp := policyWithApprovals("lint-gate", []keystonev1alpha1.ApprovalPolicy{{
		Name:              "reviewer",
		RequiredApprovers: 1,
		FromGroups:        []string{"schema-reviewers"},
		AppliesWhen:       &keystonev1alpha1.ApprovalCondition{MinLintSeverity: keystonev1alpha1.LintLevelWarning},
	}})
	bundle := bundleWithAnnotations("b8", nil, nil)

	// No findings → not applicable → satisfied.
	if res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil); !res.Satisfied {
		t.Fatalf("no-findings bundle should pass")
	}
	// Notice-only → still below threshold → not applicable.
	notices := []keystonev1alpha1.LintFinding{{Severity: keystonev1alpha1.LintLevelNotice}}
	if res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, notices); !res.Satisfied {
		t.Fatalf("notice-only bundle should pass; min is warning")
	}
	// Warning → policy fires → unsatisfied without approvers.
	warnings := []keystonev1alpha1.LintFinding{{Severity: keystonev1alpha1.LintLevelWarning}}
	if res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, warnings); res.Satisfied {
		t.Fatalf("warning bundle with no approvers should fail")
	}
}

// -- Evaluate — tier-scenario composition ------------------------------

func TestEvaluateProdTierTwoOfThree(t *testing.T) {
	// Production scenario: 2 of 3 with DBA required.
	sp := policyWithApprovals("prod-strict", []keystonev1alpha1.ApprovalPolicy{
		{
			Name:                 "peer-review",
			RequiredApprovers:    2,
			FromGroups:           []string{"schema-reviewers"},
			DisallowSelfApproval: true,
		},
		{
			Name:                 "dba",
			RequiredApprovers:    1,
			FromGroups:           []string{"dba"},
			DisallowSelfApproval: true,
		},
	})

	// Missing DBA.
	bundle := bundleWithAnnotations("b9", map[string]string{
		keystonev1alpha1.AnnotationAuthor:                                "alice",
		keystonev1alpha1.AnnotationApprovalPrefix + "peer-review.bob":    "schema-reviewers",
		keystonev1alpha1.AnnotationApprovalPrefix + "peer-review.carol":  "schema-reviewers",
	}, nil)
	if res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil); res.Satisfied {
		t.Fatalf("missing DBA must block; got satisfied")
	}

	// Complete: add DBA approval.
	bundle.Annotations[keystonev1alpha1.AnnotationApprovalPrefix+"dba.dave"] = "dba"
	if res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil); !res.Satisfied {
		t.Fatalf("complete approvals must satisfy; got %+v", res)
	}
}

func TestEvaluateDevTierAutoApprove(t *testing.T) {
	// Dev scenario: no approval policies — every bundle trivially
	// satisfies.
	sp := policyWithApprovals("dev-open", nil)
	bundle := bundleWithAnnotations("b10-dev", nil, nil)
	res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil)
	if !res.Satisfied {
		t.Fatal("dev tier with no ApprovalPolicies must auto-satisfy")
	}
}

func TestEvaluateStagingOneOfTwo(t *testing.T) {
	// Staging: 1 of 2 from schema-reviewers.
	sp := policyWithApprovals("staging", []keystonev1alpha1.ApprovalPolicy{{
		Name:              "review",
		RequiredApprovers: 1,
		FromGroups:        []string{"schema-reviewers"},
	}})
	bundle := bundleWithAnnotations("b11-staging", map[string]string{
		keystonev1alpha1.AnnotationApprovalPrefix + "review.bob": "schema-reviewers",
	}, nil)
	if res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil); !res.Satisfied {
		t.Fatal("single review approval must satisfy staging 1-of-N")
	}
}

// -- Dedup: same approver with multiple group claims -----------------

func TestEvaluateApproverDedup(t *testing.T) {
	// Same approver-id appears twice in different groups — should count once.
	annos := map[string]string{
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.alice": "dba",
	}
	// Re-add alice under another group via the second loop iteration.
	// The map only holds one key-value pair; we simulate via re-entry.
	bundle := bundleWithAnnotations("b12", annos, nil)
	sp := policyWithApprovals("prod", []keystonev1alpha1.ApprovalPolicy{{
		Name:              "dba",
		RequiredApprovers: 2,
		FromGroups:        []string{"dba", "sre"},
	}})
	res := Evaluate(bundle, []keystonev1alpha1.SchemaPolicy{sp}, nil)
	if res.Policies[0].Received != 1 {
		t.Fatalf("one unique approver should yield Received=1, got %d", res.Policies[0].Received)
	}
}
