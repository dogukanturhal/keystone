// SPDX-License-Identifier: AGPL-3.0-or-later

// Package approval evaluates MigrationBundle approval state against
// matched SchemaPolicy.spec.approvalPolicies. It is pure logic —
// nothing here touches the kube apiserver. The webhook and controller
// both call into it so admission-time and reconcile-time decisions
// agree.
//
// Model:
//
//	Bundle carries metadata.annotations:
//	  keystone.hexxlock.io/author = <id>
//	  keystone.hexxlock.io/risk-tier = <tier>           (optional)
//	  keystone.hexxlock.io/approval.<policy>.<id> = <group>
//
//	A SchemaPolicy declares N-of-M ApprovalPolicies. Each policy has:
//	  - Name            — stable identifier used in annotation key
//	  - RequiredApprovers
//	  - FromGroups      — allowed groups
//	  - AppliesWhen     — conditional gate (bundle selector, lint
//	                      severity threshold, risk tier)
//	  - DisallowSelfApproval
//
// For each matched SchemaPolicy × ApprovalPolicy, Evaluate decides:
//
//  1. Does AppliesWhen fire against this bundle? If not, policy is
//     "not applicable" — contributes nothing to Satisfied.
//  2. Parse approval annotations whose key matches this policy's Name.
//     Each annotation is (approver-id, group); drop approvers whose
//     group is not in FromGroups. Deduplicate by approver-id.
//  3. If DisallowSelfApproval and the author is in the approver set,
//     remove the author from the count.
//  4. Approvers ≥ RequiredApprovers → policy satisfied.
//
// Overall Satisfied = every applicable policy satisfied.
package approval

import (
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// Evidence is the set of annotations parsed off a MigrationBundle,
// normalised into the form Evaluate consumes. Kept as its own type
// so callers (CLI, diagnostics, tests) can inspect what the evaluator
// saw without re-running the parser.
type Evidence struct {
	// Author is the value of metadata.annotations[AnnotationAuthor],
	// trimmed. Empty when the author annotation is absent — which
	// disables DisallowSelfApproval enforcement for that bundle.
	Author string

	// RiskTier is the value of metadata.annotations[AnnotationRiskTier].
	// Empty when absent. Matching against an ApprovalPolicy's RiskTier
	// is case-insensitive.
	RiskTier string

	// Approvals holds one entry per annotation under
	// AnnotationApprovalPrefix, with policy-name and approver-id
	// extracted from the key and group read from the value.
	Approvals []Approval
}

// Approval is one parsed annotation carrying a single approver's
// claim for a single policy.
type Approval struct {
	PolicyName string
	ApproverID string
	Group      string
}

// ParseEvidence reads a bundle's annotations and returns a normalised
// Evidence. Approval annotation keys that don't parse are silently
// dropped — the webhook separately enforces naming conventions at
// admission, so malformed keys in already-admitted bundles surface as
// "missing approver" diagnostics downstream.
func ParseEvidence(annotations map[string]string) Evidence {
	out := Evidence{
		Author:   strings.TrimSpace(annotations[keystonev1alpha1.AnnotationAuthor]),
		RiskTier: strings.TrimSpace(annotations[keystonev1alpha1.AnnotationRiskTier]),
	}
	for k, v := range annotations {
		if !strings.HasPrefix(k, keystonev1alpha1.AnnotationApprovalPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(k, keystonev1alpha1.AnnotationApprovalPrefix)
		// suffix = "<policy-name>.<approver-id>"
		dot := strings.IndexByte(suffix, '.')
		if dot <= 0 || dot == len(suffix)-1 {
			continue
		}
		out.Approvals = append(out.Approvals, Approval{
			PolicyName: suffix[:dot],
			ApproverID: suffix[dot+1:],
			Group:      strings.TrimSpace(v),
		})
	}
	// Sort for deterministic output — tests and kubectl describe alike
	// benefit from stable ordering.
	sort.Slice(out.Approvals, func(i, j int) bool {
		if out.Approvals[i].PolicyName != out.Approvals[j].PolicyName {
			return out.Approvals[i].PolicyName < out.Approvals[j].PolicyName
		}
		return out.Approvals[i].ApproverID < out.Approvals[j].ApproverID
	})
	return out
}

// Result is the outcome of Evaluate. Satisfied reports the boolean
// gate; Policies carries per-policy details for status + diagnostics.
type Result struct {
	// Satisfied is true iff every applicable ApprovalPolicy has
	// enough approvers. Bundles matched by zero applicable policies
	// are trivially satisfied (the approval gate does not block).
	Satisfied bool

	// Policies is the per-policy breakdown, sorted by (SchemaPolicy
	// name, ApprovalPolicy name) for deterministic output.
	Policies []PolicyResult
}

// PolicyResult is Evaluate's report for one ApprovalPolicy under one
// matched SchemaPolicy. An ApprovalPolicy that doesn't match the
// bundle (AppliesWhen fails) still appears here with Applied=false,
// so operators can see what rules were considered.
type PolicyResult struct {
	// SchemaPolicy is the owning SchemaPolicy.Name.
	SchemaPolicy string

	// ApprovalPolicy is the ApprovalPolicy.Name within the SchemaPolicy.
	ApprovalPolicy string

	// Applied is true when AppliesWhen fired against this bundle. If
	// false, Required / Received / Satisfied are meaningless.
	Applied bool

	// Required is the count of distinct approvers needed.
	Required int32

	// Received is the count of distinct valid approvers counted. Only
	// approvers whose declared group appears in FromGroups, minus the
	// author when DisallowSelfApproval is set, contribute.
	Received int32

	// Approvers lists the approver-ids that counted. Sorted.
	Approvers []string

	// Satisfied = Received >= Required. False when Applied=false is
	// also false (policy didn't apply → nothing to satisfy).
	Satisfied bool

	// Violation is a human-readable diagnostic when Satisfied=false:
	// what approvals were expected vs what was seen. Empty when
	// Applied=false (nothing to report) or Satisfied=true.
	Violation string

	// MinLintSeverity is the AppliesWhen.MinLintSeverity threshold that
	// caused this policy to fire (zero-value when AppliesWhen has no
	// MinLintSeverity gate). Used by the webhook to determine which
	// approved policies "cover" lint-error refusals — an applied +
	// satisfied policy with MinLintSeverity ≤ error lifts the lint
	// block, exactly as the AppliesWhen.MinLintSeverity comment
	// promises.
	MinLintSeverity keystonev1alpha1.LintLevel
}

// LiftsLintErrors reports whether at least one applied + satisfied
// ApprovalPolicy carries a non-empty MinLintSeverity gate. Any non-
// empty value (notice/warning/error) covers `error` findings because
// severity is monotonic — a "fires on warning+" policy by definition
// fires on "error" too, since error is more severe than warning.
//
// Empty MinLintSeverity means the policy fired for a different
// AppliesWhen reason (BundleSelector / RiskTier) — it does NOT lift
// the lint-error block, because the approver may not have specifically
// reviewed the destructive findings.
//
// Without this, the webhook's early lint-error refuse short-circuits
// before approvals run, contradicting the AppliesWhen.MinLintSeverity
// design intent (per the comment in approval/approval.go and the
// SchemaPolicy CRD docs).
func (r Result) LiftsLintErrors() bool {
	for _, pr := range r.Policies {
		if pr.Applied && pr.Satisfied && pr.MinLintSeverity != "" {
			return true
		}
	}
	return false
}

// Evaluate runs the approval check. Callers pass:
//
//   - bundle: the MigrationBundle being evaluated.
//   - policies: the SchemaPolicies whose TargetSelector matched the
//     bundle. Caller is responsible for filtering — the evaluator
//     trusts this list.
//   - findings: lint findings for the bundle's current source. Used
//     by ApprovalCondition.MinLintSeverity (e.g., "needs DBA review
//     when there is any warning"). Nil is fine when no lint has run.
//
// When policies is empty or every ApprovalPolicy has AppliesWhen false,
// Result.Satisfied is true — the bundle is not gated.
func Evaluate(
	bundle *keystonev1alpha1.MigrationBundle,
	policies []keystonev1alpha1.SchemaPolicy,
	findings []keystonev1alpha1.LintFinding,
) Result {
	evidence := ParseEvidence(bundle.Annotations)
	maxSeverity := highestSeverity(findings)

	result := Result{Satisfied: true}
	for i := range policies {
		p := &policies[i]
		for j := range p.Spec.ApprovalPolicies {
			ap := &p.Spec.ApprovalPolicies[j]
			pr := evaluatePolicy(bundle, p, ap, evidence, maxSeverity)
			result.Policies = append(result.Policies, pr)
			if pr.Applied && !pr.Satisfied {
				result.Satisfied = false
			}
		}
	}

	// Deterministic order — callers pipe this straight to the status
	// subresource and dashboards; stable diffs > insertion order.
	sort.Slice(result.Policies, func(i, j int) bool {
		if result.Policies[i].SchemaPolicy != result.Policies[j].SchemaPolicy {
			return result.Policies[i].SchemaPolicy < result.Policies[j].SchemaPolicy
		}
		return result.Policies[i].ApprovalPolicy < result.Policies[j].ApprovalPolicy
	})
	return result
}

// evaluatePolicy is the per-(SchemaPolicy, ApprovalPolicy) kernel.
// Split out so tests can exercise single-policy behavior without
// constructing a multi-policy harness.
func evaluatePolicy(
	bundle *keystonev1alpha1.MigrationBundle,
	sp *keystonev1alpha1.SchemaPolicy,
	ap *keystonev1alpha1.ApprovalPolicy,
	evidence Evidence,
	maxSeverity keystonev1alpha1.LintLevel,
) PolicyResult {
	pr := PolicyResult{
		SchemaPolicy:   sp.Name,
		ApprovalPolicy: ap.Name,
		Required:       ap.RequiredApprovers,
	}
	if ap.AppliesWhen != nil {
		pr.MinLintSeverity = ap.AppliesWhen.MinLintSeverity
	}

	applies, reason := conditionFires(ap.AppliesWhen, bundle, evidence, maxSeverity)
	pr.Applied = applies
	if !applies {
		pr.Violation = reason // "not applicable: <why>" for diagnostics
		return pr
	}

	groups := make(map[string]bool, len(ap.FromGroups))
	for _, g := range ap.FromGroups {
		groups[strings.ToLower(strings.TrimSpace(g))] = true
	}

	counted := make(map[string]bool)
	for _, a := range evidence.Approvals {
		if a.PolicyName != ap.Name {
			continue
		}
		if a.Group == "" {
			continue
		}
		if !groups[strings.ToLower(a.Group)] {
			continue
		}
		if ap.DisallowSelfApproval && evidence.Author != "" && a.ApproverID == evidence.Author {
			continue
		}
		counted[a.ApproverID] = true
	}

	pr.Received = int32(len(counted))
	pr.Approvers = make([]string, 0, len(counted))
	for id := range counted {
		pr.Approvers = append(pr.Approvers, id)
	}
	sort.Strings(pr.Approvers)

	pr.Satisfied = pr.Received >= pr.Required
	if !pr.Satisfied {
		pr.Violation = fmt.Sprintf(
			"policy %q/%q needs %d approver(s) from groups %v; received %d (%v)",
			sp.Name, ap.Name, ap.RequiredApprovers, ap.FromGroups, pr.Received, pr.Approvers,
		)
	}
	return pr
}

// conditionFires decides whether an ApprovalPolicy's AppliesWhen gate
// matches the bundle. The nil-condition case applies to every bundle.
// Returns (applies, reason); reason is populated when applies=false.
func conditionFires(
	cond *keystonev1alpha1.ApprovalCondition,
	bundle *keystonev1alpha1.MigrationBundle,
	evidence Evidence,
	maxSeverity keystonev1alpha1.LintLevel,
) (bool, string) {
	if cond == nil {
		return true, ""
	}
	if cond.BundleSelector != nil {
		sel, err := metav1.LabelSelectorAsSelector(cond.BundleSelector)
		if err != nil {
			return false, fmt.Sprintf("invalid bundleSelector on policy: %v", err)
		}
		if !sel.Matches(labels.Set(bundle.Labels)) {
			return false, "bundleSelector does not match bundle labels"
		}
	}
	if cond.RiskTier != "" {
		if !strings.EqualFold(cond.RiskTier, evidence.RiskTier) {
			return false, fmt.Sprintf("riskTier %q required; bundle carries %q",
				cond.RiskTier, evidence.RiskTier)
		}
	}
	if cond.MinLintSeverity != "" {
		if !severityMeets(maxSeverity, cond.MinLintSeverity) {
			return false, fmt.Sprintf("minLintSeverity %q required; bundle max severity is %q",
				cond.MinLintSeverity, maxSeverity)
		}
	}
	return true, ""
}

// highestSeverity returns the most severe LintLevel across findings
// (error > warning > notice). Empty when findings is empty.
func highestSeverity(findings []keystonev1alpha1.LintFinding) keystonev1alpha1.LintLevel {
	rank := map[keystonev1alpha1.LintLevel]int{
		keystonev1alpha1.LintLevelNotice:  1,
		keystonev1alpha1.LintLevelWarning: 2,
		keystonev1alpha1.LintLevelError:   3,
	}
	var top keystonev1alpha1.LintLevel
	topR := 0
	for _, f := range findings {
		if r := rank[f.Severity]; r > topR {
			top = f.Severity
			topR = r
		}
	}
	return top
}

// severityMeets returns true when observed ≥ threshold.
func severityMeets(observed, threshold keystonev1alpha1.LintLevel) bool {
	rank := map[keystonev1alpha1.LintLevel]int{
		keystonev1alpha1.LintLevelNotice:  1,
		keystonev1alpha1.LintLevelWarning: 2,
		keystonev1alpha1.LintLevelError:   3,
	}
	o := rank[observed]
	t := rank[threshold]
	// Unknown severities never meet a valid threshold.
	if t == 0 {
		return false
	}
	return o >= t
}

// Unsatisfied returns the subset of PolicyResults that applied but
// failed, in deterministic order. Used by the webhook to build a
// single rejection message listing every missing policy.
func (r Result) Unsatisfied() []PolicyResult {
	var out []PolicyResult
	for _, pr := range r.Policies {
		if pr.Applied && !pr.Satisfied {
			out = append(out, pr)
		}
	}
	return out
}
