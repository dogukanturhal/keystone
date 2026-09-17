// SPDX-License-Identifier: AGPL-3.0-or-later

// Package webhook hosts Keystone's admission webhooks.
//
// Why a webhook when Phase 11 already lints at reconcile time:
// reconcile-time lint marks a bundle Ready=False *after* it's stored in
// etcd. Admission-time lint refuses the CR outright, so bad SQL never
// lands in the cluster. Attackers, typo-ing admins, and CI mistakes all
// bounce at the apiserver instead of generating DriftReports later.
//
// Each webhook is a CustomValidator registered via NewWebhookManagedBy.
// Errors returned are surfaced verbatim to kubectl; warnings appear as
// "Warning:" lines above the accepted/rejected message.
package webhook

import (
	"context"
	"fmt"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/approval"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
	"github.com/dogukanturhal/keystone-sdk/go/analyze"
	policycel "github.com/dogukanturhal/keystone/internal/policy/cel"
)

// apiReaderFallbackWarn fires the missing-APIReader warning at most
// once per process. The fallback to v.Client is harmless in tests
// (envtest's Client and APIReader point at the same store) but in
// production it means user-authored ConfigMap-sourced bundles route
// through the cached client and miss the cache scope's label filter
// (the cache returns NotFound; the resolver fails to admit), so an
// operator who lands here in production has a SetupWithManager bug
// to fix.
var apiReaderFallbackWarn sync.Once

// +kubebuilder:webhook:path=/validate-keystone-hexxlock-io-v1alpha1-migrationbundle,mutating=false,failurePolicy=fail,sideEffects=None,groups=keystone.hexxlock.io,resources=migrationbundles,verbs=create;update,versions=v1alpha1,name=vmigrationbundle.kb.io,admissionReviewVersions=v1

// The webhook consults SchemaPolicy objects to decide whether
// keystone.sum is mandatory. Cluster-scoped list access is the minimum
// viable permission — we read, never write.
//
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=schemapolicies,verbs=get;list;watch

// MigrationBundleValidator runs Keystone's analyzer pack against a
// MigrationBundle at admission time.
//
// The validator consults only what it can see at admission — spec +
// the referenced ConfigMap (if present). It does NOT attempt to reach
// target databases; those checks belong to the reconciler.
//
// If the referenced ConfigMap hasn't been applied yet (ordering
// timing), the webhook emits a warning and admits the bundle; the
// reconciler's lint pass will catch it when the ConfigMap arrives.
type MigrationBundleValidator struct {
	Client client.Client

	// APIReader is the cache-bypassing reader used to resolve
	// user-authored ConfigMap-sourced bundles whose CMs fall outside
	// the operator's label-scoped ConfigMap cache. SetupWithManager
	// auto-wires it from the manager when nil.
	APIReader client.Reader

	Registry *analyze.Registry

	// CELEvaluator compiles and evaluates SchemaPolicy.spec.expressions
	// at admission time. Nil is allowed — the validator auto-constructs
	// one in SetupWithManager; tests may inject a shared instance to
	// verify compilation caching across admission calls.
	CELEvaluator *policycel.Evaluator
}

var _ admission.Validator[*keystonev1alpha1.MigrationBundle] = &MigrationBundleValidator{}

// SetupWithManager registers the validator against the manager's
// webhook server. Uses the typed (generic) Validator API from
// controller-runtime v0.23.
func (v *MigrationBundleValidator) SetupWithManager(mgr ctrl.Manager) error {
	if v.Client == nil {
		v.Client = mgr.GetClient()
	}
	if v.APIReader == nil {
		v.APIReader = mgr.GetAPIReader()
	}
	if v.Registry == nil {
		v.Registry = analyze.DefaultRegistry()
	}
	if v.CELEvaluator == nil {
		eval, err := policycel.NewEvaluator()
		if err != nil {
			return fmt.Errorf("setup CEL evaluator: %w", err)
		}
		v.CELEvaluator = eval
	}
	return ctrl.NewWebhookManagedBy(mgr, &keystonev1alpha1.MigrationBundle{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate is called on every `kubectl apply` of a new bundle.
func (v *MigrationBundleValidator) ValidateCreate(ctx context.Context, bundle *keystonev1alpha1.MigrationBundle) (admission.Warnings, error) {
	return v.validate(ctx, bundle, nil)
}

// ValidateUpdate is called on every update. The old object is passed so
// the validator can reject field mutations that would break the
// bundle's immutability contract (same spec.version with different SQL).
func (v *MigrationBundleValidator) ValidateUpdate(ctx context.Context, oldBundle, newBundle *keystonev1alpha1.MigrationBundle) (admission.Warnings, error) {
	return v.validate(ctx, newBundle, oldBundle)
}

// ValidateDelete is a no-op — bundle deletion is a CR concern, not a
// SQL concern. Deletions pass through to finalizer cleanup.
func (v *MigrationBundleValidator) ValidateDelete(_ context.Context, _ *keystonev1alpha1.MigrationBundle) (admission.Warnings, error) {
	return nil, nil
}

// validate is the shared logic for create and update. It:
//  1. Enforces mutual exclusion of Source vs Operations based on strategy
//  2. Refuses empty schemaSelector (silent no-op is worse than error)
//  3. For versioned+ConfigMap source: resolves the ConfigMap and runs
//     the analyzer registry; rejects on any severity=error finding
//  4. On update: verifies the spec.version didn't change its contentHash
//     (bundle immutability; once shipped, can't rewrite history)
func (v *MigrationBundleValidator) validate(
	ctx context.Context,
	bundle, old *keystonev1alpha1.MigrationBundle,
) (admission.Warnings, error) {
	var warnings admission.Warnings

	// 1. Source vs Operations mutual exclusion.
	if bundle.Spec.Strategy == keystonev1alpha1.StrategyVersioned {
		if bundle.Spec.Source.Type == "" {
			return nil, fmt.Errorf("spec.source.type is required when strategy=versioned")
		}
		if len(bundle.Spec.Operations) > 0 {
			return nil, fmt.Errorf(
				"spec.operations must be empty when strategy=versioned " +
					"(operations apply to pgroll-expand-contract only)")
		}
	}
	if bundle.Spec.Strategy == keystonev1alpha1.StrategyPgrollExpandContract {
		if len(bundle.Spec.Operations) == 0 {
			return nil, fmt.Errorf("spec.operations must have at least one entry when strategy=pgroll-expand-contract")
		}
		if bundle.Spec.Source.Type != "" {
			return nil, fmt.Errorf(
				"spec.source must be empty when strategy=pgroll-expand-contract " +
					"(source applies to versioned only)")
		}
	}

	// 1b. RecordOnly guards (ADR 0027). The mode never executes SQL,
	// so a rollback source is contradictory and pgroll's expand/
	// contract phases have no record-without-execute semantics.
	recordOnly := bundle.Spec.ExecutionMode == keystonev1alpha1.ExecutionModeRecordOnly
	if recordOnly {
		if bundle.Spec.Strategy == keystonev1alpha1.StrategyPgrollExpandContract {
			return nil, fmt.Errorf(
				"spec.executionMode=RecordOnly requires strategy=versioned — " +
					"pgroll expand/contract phases cannot be recorded without executing")
		}
		if bundle.Spec.AutoRollback {
			return nil, fmt.Errorf(
				"spec.autoRollback must be false when executionMode=RecordOnly — " +
					"a never-executed baseline has nothing to roll back")
		}
	}
	// ExecutionMode is immutable: flipping a shipped bundle between
	// Apply and RecordOnly would rewrite the meaning of its tracking-
	// table row (executed vs declared-as-applied).
	if old != nil && effectiveExecutionMode(old) != effectiveExecutionMode(bundle) {
		return nil, fmt.Errorf(
			"spec.executionMode is immutable (was %q, now %q) — create a new "+
				"bundle with a new spec.version instead",
			effectiveExecutionMode(old), effectiveExecutionMode(bundle))
	}

	// 2. Empty selector guard. A LabelSelector with no matchLabels or
	// matchExpressions matches everything — always a mistake for a
	// schema-mutation bundle. Catches this before the reconciler does.
	sel := bundle.Spec.SchemaSelector
	if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return nil, fmt.Errorf(
			"spec.schemaSelector must not be empty — an empty selector " +
				"matches every DatabaseSchema cluster-wide, which is never intended")
	}

	// 3. ConfigMap-source lint. Other source types (OCIArtifact, Git)
	// are Phase 4+; when we add them, extend here.
	if bundle.Spec.Strategy == keystonev1alpha1.StrategyVersioned &&
		bundle.Spec.Source.Type == keystonev1alpha1.SourceConfigMap {

		// APIReader (uncached) — see MigrationBundleValidator.APIReader
		// docstring; ConfigMap cache is label-scoped to SD-emitted
		// bundles only. Tests that construct the validator without
		// SetupWithManager (e.g. envtest fakes) leave APIReader nil
		// and rely on the Client (which is the same fake store) for
		// CM resolution; this fall-back keeps the test ergonomic.
		// In production the fall-back is a SetupWithManager bug —
		// the cached client's ConfigMap lookups are filtered by the
		// label selector and would NotFound on user-authored bundles.
		// Log once so the misconfiguration surfaces in operator logs.
		reader := client.Reader(v.APIReader)
		if reader == nil {
			apiReaderFallbackWarn.Do(func() {
				log.FromContext(ctx).Info(
					"MigrationBundleValidator.APIReader is nil; falling back to cached Client. "+
						"In production this means user-authored ConfigMap-sourced bundles will fail "+
						"to resolve at admission. Ensure SetupWithManager runs (it auto-wires "+
						"APIReader from mgr.GetAPIReader()).",
					"component", "validator",
					"validator", "MigrationBundleValidator")
			})
			reader = v.Client
		}
		resolver := migration.NewConfigMapResolver(reader)
		src, err := resolver.Resolve(ctx, bundle.Namespace, bundle.Spec.Source)
		if err != nil {
			// The ConfigMap may be applied in the same `kubectl apply`
			// as the bundle and not yet visible. Warn, don't block —
			// the reconciler's own lint pass catches it later.
			warnings = append(warnings,
				fmt.Sprintf("source ConfigMap not yet resolvable (%v); "+
					"admission lint skipped — reconcile-time lint still applies", err))
			// Continue to content-hash check below.
		} else {
			// A1 — keystone.sum gates. Malformed and Mismatch are always
			// hard rejects (the committer shipped a broken or stale sum).
			// Missing is a warning today; SchemaPolicy.spec.integrity
			// .requireSumFile flips it to an error in Phase A3 once
			// every in-tree bundle carries a sum.
			if err := v.validateIntegrity(ctx, bundle, src); err != nil {
				return warnings, err
			}
			if src.IntegrityStatus == migration.SumStatusMissing {
				warnings = append(warnings,
					"bundle has no keystone.sum — integrity check skipped (advisory mode). "+
						"Run `keystonectl sum <dir>` to generate one.")
			}

			files := make([]analyze.FileBody, 0, len(src.Names))
			for _, name := range src.Names {
				files = append(files, analyze.FileBody{Name: name, Body: src.Files[name]})
			}
			findings, _ := v.Registry.Run(ctx, &analyze.Migration{
				BundleName: bundle.Name,
				Version:    bundle.Spec.Version,
				Operations: bundle.Spec.Operations,
				Files:      files,
			})

			// RecordOnly bundles are never executed, so execution-
			// safety findings cannot block them — vendor consensus:
			// `flyway baseline`, Liquibase `changelog-sync`, and
			// `atlas migrate apply --baseline` all record adoption
			// baselines without execution analysis. Findings are
			// downgraded to warnings so they stay visible (ADR 0027).
			var errs []string
			var advisory int
			for _, f := range findings {
				line := fmt.Sprintf("[%s] %s:%d — %s", f.Rule, f.File, f.Line, f.Message)
				if f.Severity == keystonev1alpha1.LintLevelError && !recordOnly {
					errs = append(errs, line)
				} else {
					if f.Severity == keystonev1alpha1.LintLevelError {
						advisory++
					}
					warnings = append(warnings, line)
				}
			}
			if advisory > 0 {
				warnings = append(warnings, fmt.Sprintf(
					"%d error-severity lint finding(s) downgraded to advisory: "+
						"executionMode=RecordOnly never executes the SQL", advisory))
			}

			// A3 — N-of-M approval gate. Run BEFORE the lint-error
			// refuse so an ApprovalPolicy whose AppliesWhen.MinLint
			// Severity covers `error` can lift the block via N-of-M
			// approver annotations. Without this re-order, the
			// AppliesWhen.MinLintSeverity feature in the SchemaPolicy
			// CRD never fires for lint-error bundles — directly
			// contradicting the design intent documented on that
			// field.
			//
			// validateApprovals returns:
			//   nil when no policy applies OR all applied policies
			//        are satisfied
			//   error when a policy applies + is unsatisfied
			//
			// We capture both (err) and the cover-result (which uses
			// the per-policy result list) to decide:
			//   - lint-error + approval covers it    → admit
			//   - lint-error + no approval covers it → refuse with
			//                                          combined message
			//   - approval needed + unsatisfied      → refuse with
			//                                          approval message
			lintFindings := findingsToLintFindings(findings)
			matched, _ := v.matchedPolicies(ctx, bundle)
			approvalResult := approval.Evaluate(bundle, matched, lintFindings)

			if !approvalResult.Satisfied {
				// One or more applied policies are unsatisfied —
				// approval-needed wins over the lint-error message
				// because the operator workflow is "add approver
				// annotation" not "remove the DROP TABLE".
				violations := approvalResult.Unsatisfied()
				msgs := make([]string, 0, len(violations))
				for _, pr := range violations {
					msgs = append(msgs, pr.Violation)
				}
				return warnings, fmt.Errorf(
					"admission refused: %d approval requirement(s) unmet — %s. "+
						"Add `%s<policy-name>.<approver-id>=<group>` annotations; "+
						"full schema in docs/adrs/0016.",
					len(violations), strings.Join(msgs, "; "),
					keystonev1alpha1.AnnotationApprovalPrefix)
			}

			if len(errs) > 0 && !approvalResult.LiftsLintErrors() {
				return warnings, fmt.Errorf(
					"admission refused: %d lint error(s) — %s. "+
						"To admit a destructive bundle, attach a SchemaPolicy with an "+
						"ApprovalPolicy whose AppliesWhen.MinLintSeverity ≤ `error` and "+
						"satisfy it via `%s<policy>.<approver>=<group>` annotations.",
					len(errs), strings.Join(errs, "; "),
					keystonev1alpha1.AnnotationApprovalPrefix)
			}

			// C2 — CEL expression gate. Evaluates every matched
			// SchemaPolicy.spec.expressions against the bundle.
			// Blocking verdicts (fail / compile / error) reject
			// admission; Warnings are surfaced.
			celWarnings, celErr := v.validateCELExpressions(ctx, bundle, findingsToLintFindings(findings))
			warnings = append(warnings, celWarnings...)
			if celErr != nil {
				return warnings, celErr
			}

			// 4. Immutability check: same version must hash identically
			// across updates. The reconciler also checks this after
			// apply, but catching it at admission means no ordering
			// race can sneak past.
			if old != nil &&
				old.Spec.Version == bundle.Spec.Version &&
				old.Status.ContentHash != "" &&
				src.ContentHash != old.Status.ContentHash {
				return warnings, fmt.Errorf(
					"admission refused: spec.version %q was previously reconciled "+
						"with contentHash %q but the ConfigMap now hashes to %q — "+
						"bump spec.version to ship new SQL",
					bundle.Spec.Version, old.Status.ContentHash, src.ContentHash)
			}
		}
	}

	return warnings, nil
}

// validateIntegrity enforces the keystone.sum gates at admission.
// Malformed/Mismatch are always hard errors. Missing is an error only
// when at least one matched SchemaPolicy opts in via
// spec.integrity.requireSumFile=true; otherwise the caller surfaces it
// as a warning. Returns nil when the bundle passes.
func (v *MigrationBundleValidator) validateIntegrity(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	src *migration.ResolvedSource,
) error {
	switch src.IntegrityStatus {
	case migration.SumStatusMalformed:
		return fmt.Errorf(
			"admission refused: keystone.sum in bundle %q failed to parse: %v. "+
				"Regenerate with `keystonectl sum <dir>` and commit the new file.",
			bundle.Name, src.IntegrityError)
	case migration.SumStatusMismatch:
		return fmt.Errorf(
			"admission refused: bundle %q diverges from its keystone.sum: %v. "+
				"Either revert the SQL change or regenerate keystone.sum to accept it.",
			bundle.Name, src.IntegrityError)
	case migration.SumStatusMissing:
		required, policyName, err := v.sumRequiredForBundle(ctx, bundle)
		if err != nil {
			// Don't fail admission on SchemaPolicy lookup errors — log
			// to the warnings channel and admit. If we hard-reject here
			// a broken policy controller could brick every bundle apply.
			return nil
		}
		if required {
			return fmt.Errorf(
				"admission refused: SchemaPolicy %q requires keystone.sum but bundle %q "+
					"does not contain one. Run `keystonectl sum <dir>` and commit the file.",
				policyName, bundle.Name)
		}
	}
	return nil
}

// sumRequiredForBundle returns true iff at least one SchemaPolicy that
// matches this bundle's labels declares spec.integrity.requireSumFile.
// Mirrors the existing "strictest rule wins" model of other policy
// fields: any policy requiring the sum is sufficient to require it.
//
// Returns the policy's name so the rejection message can point at the
// specific gate — operators chasing "why did this bundle fail?" land
// on the right CR immediately.
func (v *MigrationBundleValidator) sumRequiredForBundle(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
) (bool, string, error) {
	matched, err := v.matchedPolicies(ctx, bundle)
	if err != nil {
		return false, "", err
	}
	for i := range matched {
		p := &matched[i]
		if p.Spec.Integrity != nil && p.Spec.Integrity.RequireSumFile {
			return true, p.Name, nil
		}
	}
	return false, "", nil
}

// matchedPolicies returns the SchemaPolicies that govern this bundle.
// A policy matches if EITHER:
//   - bundle.spec.policyRef explicitly names it (pinned), OR
//   - the policy's TargetSelector matches the bundle's labels.
//
// Both forms contribute additively. The previous behaviour treated
// PolicyRef as exclusive (skipping selector evaluation when pinned),
// which broke N-of-M approval flow for SD-emitted bundles: the SD
// reconciler defaults PolicyRef to "production-safety", so any policy
// that targets the bundle by label (e.g. a destructive-approval policy
// scoped to `keystone.hexxlock.io/scope: <product>`) was silently
// excluded. Result: declarative-authored destructive bundles couldn't
// be admitted via the approval-evidence path, defeating the entire
// design intent of TargetSelector-based policy matching.
//
// The fix is union semantics: pinned policy fires AND label-selected
// policies fire. Operators can opt out of selector matching by simply
// not labelling the bundle. There's no use case where you want to
// pin a single policy AND silence all label-matched ones; if such a
// case ever surfaces, add an explicit `targetSelectorMode: pinned-only`
// field to the SchemaPolicy/MigrationBundle spec.
//
// Returned slice is stable-ordered by policy name so downstream
// diagnostics don't flap. Duplicates are de-duped by name.
//
// Shared between sumRequiredForBundle (A1) and validateApprovals (A3)
// so admission-time lookups walk the SchemaPolicy list once per
// request.
func (v *MigrationBundleValidator) matchedPolicies(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
) ([]keystonev1alpha1.SchemaPolicy, error) {
	var policies keystonev1alpha1.SchemaPolicyList
	if err := v.Client.List(ctx, &policies); err != nil {
		return nil, fmt.Errorf("list SchemaPolicies: %w", err)
	}
	seen := make(map[string]bool, len(policies.Items))
	var matched []keystonev1alpha1.SchemaPolicy
	for i := range policies.Items {
		p := &policies.Items[i]
		// Pinned policyRef matches additively.
		if bundle.Spec.PolicyRef != "" && bundle.Spec.PolicyRef == p.Name {
			if !seen[p.Name] {
				matched = append(matched, *p)
				seen[p.Name] = true
			}
			continue
		}
		ok, err := policyMatches(p, bundle)
		if err != nil {
			continue
		}
		if ok && !seen[p.Name] {
			matched = append(matched, *p)
			seen[p.Name] = true
		}
	}
	return matched, nil
}

// validateApprovals enforces the N-of-M approval gate at admission.
// Runs approval.Evaluate against every matched SchemaPolicy and
// rejects the bundle when any applicable ApprovalPolicy is
// under-quorum. Lint findings are passed so ApprovalCondition.
// MinLintSeverity can fire.
//
// Returns nil when the bundle is approved (or no policy applies).
func (v *MigrationBundleValidator) validateApprovals(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	findings []keystonev1alpha1.LintFinding,
) error {
	matched, err := v.matchedPolicies(ctx, bundle)
	if err != nil {
		// Soft-fail: a broken SchemaPolicy listing path should not
		// brick admission of every bundle cluster-wide. The reconciler
		// re-runs the check every loop; drift surfaces in the
		// condition there.
		return nil
	}
	res := approval.Evaluate(bundle, matched, findings)
	if res.Satisfied {
		return nil
	}
	violations := res.Unsatisfied()
	if len(violations) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(violations))
	for _, pr := range violations {
		msgs = append(msgs, pr.Violation)
	}
	return fmt.Errorf(
		"admission refused: %d approval requirement(s) unmet — %s. "+
			"Add `%s<policy-name>.<approver-id>=<group>` annotations; "+
			"full schema in docs/adrs/0016.",
		len(violations), strings.Join(msgs, "; "),
		keystonev1alpha1.AnnotationApprovalPrefix)
}

// validateCELExpressions evaluates every CELRule from matched
// SchemaPolicies. Returns admission warnings + a rejection error
// when any rule is Blocking (fail / compile / error).
//
// Behaviour mirrors the lint gate — the webhook collects warnings
// from all rules even when one errors, so operators see the full
// picture on a single kubectl reject rather than fix-retry-fix-retry.
func (v *MigrationBundleValidator) validateCELExpressions(
	ctx context.Context,
	bundle *keystonev1alpha1.MigrationBundle,
	findings []keystonev1alpha1.LintFinding,
) (admission.Warnings, error) {
	matched, err := v.matchedPolicies(ctx, bundle)
	if err != nil {
		// Soft-fail — a broken policy listing shouldn't brick
		// admission. Reconcile-time evaluation catches drift.
		return nil, nil
	}
	if v.CELEvaluator == nil {
		return nil, nil
	}
	var warnings admission.Warnings
	var blocking []string
	for i := range matched {
		p := &matched[i]
		if len(p.Spec.Expressions) == 0 {
			continue
		}
		verdicts := v.CELEvaluator.Evaluate(p.Spec.Expressions, policycel.BundleContext{
			Bundle:   bundle,
			Findings: findings,
		})
		for _, vd := range verdicts {
			switch vd.Kind {
			case "warn":
				warnings = append(warnings,
					fmt.Sprintf("policy %q/%q: %s", p.Name, vd.Rule, vd.Message))
			case "fail", "compile", "error":
				blocking = append(blocking,
					fmt.Sprintf("policy %q/%q (%s): %s", p.Name, vd.Rule, vd.Kind, vd.Message))
			}
		}
	}
	if len(blocking) > 0 {
		return warnings, fmt.Errorf(
			"admission refused: %d CEL rule(s) blocked — %s",
			len(blocking), strings.Join(blocking, "; "))
	}
	return warnings, nil
}

// findingsToLintFindings converts the analyzer-package Finding type
// (internal to analyze) into the CRD-exposed LintFinding type that
// the approval evaluator consumes. Kept local so the webhook doesn't
// leak internal types across the package boundary.
func findingsToLintFindings(in []analyze.Finding) []keystonev1alpha1.LintFinding {
	out := make([]keystonev1alpha1.LintFinding, 0, len(in))
	for _, f := range in {
		out = append(out, keystonev1alpha1.LintFinding{
			Rule:     f.Rule,
			Severity: f.Severity,
			File:     f.File,
			Line:     f.Line,
			Message:  f.Message,
		})
	}
	return out
}

// policyMatches returns true when the given SchemaPolicy's
// targetSelector matches the bundle's labels. Split out of
// sumRequiredForBundle so future policy fields can reuse it without
// re-plumbing label logic.
func policyMatches(p *keystonev1alpha1.SchemaPolicy, bundle *keystonev1alpha1.MigrationBundle) (bool, error) {
	sel, err := metav1.LabelSelectorAsSelector(&p.Spec.TargetSelector)
	if err != nil {
		return false, err
	}
	if sel.Empty() {
		// Empty selector matches everything; intentional design so
		// global "all bundles need sums in prod" policies are one-liner.
		return true, nil
	}
	return sel.Matches(labels.Set(bundle.Labels)), nil
}

// effectiveExecutionMode normalises the empty string (objects admitted
// before the field existed, or before CRD defaulting ran) to Apply so
// the immutability comparison doesn't flag a no-op update of a
// pre-ADR-0027 bundle as a mode flip.
func effectiveExecutionMode(b *keystonev1alpha1.MigrationBundle) keystonev1alpha1.ExecutionMode {
	if b.Spec.ExecutionMode == "" {
		return keystonev1alpha1.ExecutionModeApply
	}
	return b.Spec.ExecutionMode
}
