// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cel evaluates SchemaPolicy.spec.expressions — a list of
// Common Expression Language rules that the admission webhook and
// reconciler run against every MigrationBundle. CEL is the same
// expression language Kubernetes uses for ValidatingAdmissionPolicy
// and CRD x-kubernetes-validations, so policy authors who have
// written either will find the activation surface familiar.
//
// Activation variables:
//
//	bundle       — MigrationBundle in map form. spec.*, metadata.*
//	findings     — []LintFinding in map form, one per analyzer hit
//	strategy     — shortcut for bundle.spec.strategy
//	labels       — shortcut for bundle.metadata.labels
//	annotations  — shortcut for bundle.metadata.annotations
//	version      — shortcut for bundle.spec.version
//
// Compilation errors surface as Verdict entries with Kind=Compile,
// so operators see them in admission output alongside rule-fire
// results. This is a deliberate design — a CEL rule that silently
// disables on typo is a footgun.
package cel

import (
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// Evaluator is a reusable program cache for CEL rules. Compilation
// is the expensive step (parse + type-check + compile to AST);
// Evaluate walks the cached programs against a fresh activation.
// Safe for concurrent use.
type Evaluator struct {
	env *cel.Env

	cacheMu sync.RWMutex
	cache   map[string]*cel.Program // key: CELRule.Expression (raw)
}

// NewEvaluator returns an Evaluator with the standard Keystone
// activation environment. Errors only on cel.NewEnv setup failures
// (broken cel-go installation); callers should treat it as fatal.
func NewEvaluator() (*Evaluator, error) {
	env, err := cel.NewEnv(
		cel.Variable("bundle", cel.DynType),
		cel.Variable("findings", cel.ListType(cel.DynType)),
		cel.Variable("strategy", cel.StringType),
		cel.Variable("labels", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("annotations", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("version", cel.StringType),
	)
	if err != nil {
		return nil, fmt.Errorf("cel: build env: %w", err)
	}
	return &Evaluator{env: env, cache: make(map[string]*cel.Program)}, nil
}

// Verdict is one rule's outcome.
type Verdict struct {
	// Rule is the CELRule.Name that produced this verdict.
	Rule string

	// Kind classifies the outcome:
	//   - "pass":    expression evaluated false (rule did not fire)
	//   - "warn":    expression evaluated true + severity=Warning
	//   - "fail":    expression evaluated true + severity=Error
	//   - "compile": expression failed to compile
	//   - "error":   runtime error during evaluation (non-bool result, nil deref, etc.)
	Kind string

	// Message is the human-readable diagnostic. For pass verdicts
	// this is the rule's own Message (for event logs); for
	// compile/error it is the CEL error text.
	Message string
}

// Blocking reports whether a Verdict stops admission. True for
// Kind=fail, compile, and error — compile/error are treated as
// blocking because a policy author's typo should not silently open
// a gap in enforcement.
func (v Verdict) Blocking() bool {
	return v.Kind == "fail" || v.Kind == "compile" || v.Kind == "error"
}

// BundleContext is the evaluation input. Caller provides the
// MigrationBundle and the latest lint findings; the evaluator
// derives all activation variables from those.
type BundleContext struct {
	Bundle   *keystonev1alpha1.MigrationBundle
	Findings []keystonev1alpha1.LintFinding
}

// Evaluate runs every rule against ctx. Returns one Verdict per rule
// in the same order. Thread-safe.
func (e *Evaluator) Evaluate(rules []keystonev1alpha1.CELRule, ctx BundleContext) []Verdict {
	activation := e.activation(ctx)
	out := make([]Verdict, 0, len(rules))
	for _, r := range rules {
		out = append(out, e.evaluateOne(r, activation))
	}
	return out
}

// evaluateOne is the per-rule kernel. Compiles (cached), evaluates,
// and classifies the outcome.
func (e *Evaluator) evaluateOne(r keystonev1alpha1.CELRule, activation map[string]any) Verdict {
	prog, err := e.compile(r.Expression)
	if err != nil {
		return Verdict{
			Rule: r.Name, Kind: "compile",
			Message: fmt.Sprintf("compile %q: %v", r.Name, err),
		}
	}
	val, _, err := (*prog).Eval(activation)
	if err != nil {
		return Verdict{
			Rule: r.Name, Kind: "error",
			Message: fmt.Sprintf("eval %q: %v", r.Name, err),
		}
	}
	fired, ok := toBool(val)
	if !ok {
		return Verdict{
			Rule: r.Name, Kind: "error",
			Message: fmt.Sprintf("rule %q: expression must return bool, got %T", r.Name, val.Value()),
		}
	}
	if !fired {
		return Verdict{Rule: r.Name, Kind: "pass"}
	}
	msg := r.Message
	if msg == "" {
		msg = fmt.Sprintf("rule %q fired", r.Name)
	}
	if r.Severity == keystonev1alpha1.CELSeverityWarning {
		return Verdict{Rule: r.Name, Kind: "warn", Message: msg}
	}
	return Verdict{Rule: r.Name, Kind: "fail", Message: msg}
}

// compile returns the cached cel.Program for expr, compiling + caching
// on first use. Safe for concurrent callers.
func (e *Evaluator) compile(expr string) (*cel.Program, error) {
	e.cacheMu.RLock()
	if p, ok := e.cache[expr]; ok {
		e.cacheMu.RUnlock()
		return p, nil
	}
	e.cacheMu.RUnlock()

	// Compile outside the lock — CEL parse/check is deterministic so
	// a concurrent double-compile just wastes CPU, never races.
	ast, issues := e.env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	prog, err := e.env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("program: %w", err)
	}

	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	if existing, ok := e.cache[expr]; ok {
		return existing, nil
	}
	e.cache[expr] = &prog
	return &prog, nil
}

// activation builds the map handed to Program.Eval. Non-exported so
// the evaluator owns the activation shape — if we add variables
// later, we update here and the ADR in one place.
func (e *Evaluator) activation(ctx BundleContext) map[string]any {
	if ctx.Bundle == nil {
		return map[string]any{
			"bundle":      map[string]any{},
			"findings":    []any{},
			"strategy":    "",
			"labels":      map[string]string{},
			"annotations": map[string]string{},
			"version":     "",
		}
	}
	bundle := ctx.Bundle
	findings := make([]any, 0, len(ctx.Findings))
	for _, f := range ctx.Findings {
		findings = append(findings, map[string]any{
			"rule":     f.Rule,
			"severity": string(f.Severity),
			"file":     f.File,
			"line":     int64(f.Line),
			"message":  f.Message,
		})
	}
	// Bundle is exposed as a map so policies can read arbitrary
	// fields without the evaluator baking a type. Mirror only the
	// parts policies are likely to care about — avoids leaking
	// status into expressions (which would make admission gate
	// on reconcile-time state).
	bundleMap := map[string]any{
		"metadata": map[string]any{
			"name":        bundle.Name,
			"namespace":   bundle.Namespace,
			"labels":      stringMap(bundle.Labels),
			"annotations": stringMap(bundle.Annotations),
		},
		"spec": map[string]any{
			"version":  bundle.Spec.Version,
			"strategy": string(bundle.Spec.Strategy),
			// Operations are exposed as a count + kinds slice so
			// expressions can filter on them without rebuilding
			// the whole struct.
			"operations": operationsToMap(bundle.Spec.Operations),
		},
	}
	return map[string]any{
		"bundle":      bundleMap,
		"findings":    findings,
		"strategy":    string(bundle.Spec.Strategy),
		"labels":      stringMap(bundle.Labels),
		"annotations": stringMap(bundle.Annotations),
		"version":     bundle.Spec.Version,
	}
}

func operationsToMap(ops []keystonev1alpha1.MigrationOperation) []any {
	out := make([]any, 0, len(ops))
	for _, op := range ops {
		out = append(out, map[string]any{
			"kind":  string(op.Kind),
			"table": op.Table,
		})
	}
	return out
}

func stringMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// toBool unwraps a CEL ref.Val into a Go bool, returning ok=false on
// non-bool result types so the caller can report "expression must
// return bool" rather than a silent false.
func toBool(v ref.Val) (bool, bool) {
	val := v.Value()
	b, ok := val.(bool)
	return b, ok
}
