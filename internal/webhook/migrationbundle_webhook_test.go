// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
	"github.com/dogukanturhal/keystone-sdk/go/analyze"
	policycel "github.com/dogukanturhal/keystone/internal/policy/cel"
)

// newFakeClient returns a controller-runtime fake client pre-seeded
// with the test scheme. Webhooks in these tests don't need envtest —
// the fake client resolves ConfigMap lookups identically from the
// validator's perspective.
func newFakeClient(objs ...runtime.Object) *fake.ClientBuilder {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...)
}

func TestMigrationBundleValidator_EmptySelectorRejected(t *testing.T) {
	v := &MigrationBundleValidator{
		Client:   newFakeClient().Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "b1"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm1", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{},
		},
	}
	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatalf("expected empty-selector rejection, got nil")
	}
	if !strings.Contains(err.Error(), "schemaSelector must not be empty") {
		t.Errorf("error message mismatch: %v", err)
	}
}

func TestMigrationBundleValidator_VersionedWithOperationsRejected(t *testing.T) {
	v := &MigrationBundleValidator{
		Client:   newFakeClient().Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "b2"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm2", FilePattern: "*.up.sql",
				},
			},
			Operations: []keystonev1alpha1.MigrationOperation{
				{Kind: "add_column", Table: "users"},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatalf("expected versioned-with-operations rejection")
	}
	if !strings.Contains(err.Error(), "operations must be empty") {
		t.Errorf("error message mismatch: %v", err)
	}
}

func TestMigrationBundleValidator_LintErrorRejected(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-drop"},
		Data: map[string]string{
			"001_drop.up.sql": "DROP TABLE users;",
		},
	}
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "b3"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-drop", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatalf("expected lint-error rejection")
	}
	if !strings.Contains(err.Error(), "no-drop-table") {
		t.Errorf("expected no-drop-table in error: %v", err)
	}
}

func TestMigrationBundleValidator_ConfigMapMissingWarns(t *testing.T) {
	v := &MigrationBundleValidator{
		Client:   newFakeClient().Build(), // no CM seeded
		Registry: analyze.DefaultRegistry(),
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "b4"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "missing", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), bundle)
	if err != nil {
		t.Errorf("ConfigMap-missing should warn, not reject; got %v", err)
	}
	foundWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "source ConfigMap not yet resolvable") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected missing-CM warning; got %+v", warnings)
	}
}

func TestMigrationBundleValidator_CleanBundleAdmitted(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-clean"},
		Data: map[string]string{
			"001_init.up.sql": "-- clean migration\nCREATE TABLE demo (id uuid PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now());",
		},
	}
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "b5"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-clean", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), bundle)
	if err != nil {
		t.Errorf("clean bundle should admit; got %v", err)
	}
}

func TestMigrationBundleValidator_ImmutabilityCheck(t *testing.T) {
	// First version: initial SQL.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-imm"},
		Data: map[string]string{
			"001_init.up.sql": "-- new version\nCREATE TABLE demo2 (id uuid PRIMARY KEY);",
		},
	}
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm).Build(),
		Registry: analyze.DefaultRegistry(),
	}

	// Old bundle was reconciled with a DIFFERENT content hash.
	old := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "bimm"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-imm", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
		Status: keystonev1alpha1.MigrationBundleStatus{
			ContentHash: "old-hash-that-wont-match",
		},
	}
	// New bundle resolves the ConfigMap with a different hash.
	new := old.DeepCopy()

	_, err := v.ValidateUpdate(context.Background(), old, new)
	if err == nil {
		t.Fatalf("expected immutability rejection")
	}
	if !strings.Contains(err.Error(), "bump spec.version") {
		t.Errorf("expected immutability message; got %v", err)
	}
}

// -- ADR 0027 RecordOnly (adoption baseline) tests --------------------

// TestMigrationBundleValidator_RecordOnlyLintErrorAdmitted — the same
// DROP TABLE SQL that hard-rejects an Apply bundle is admitted for a
// RecordOnly bundle, with the error-severity findings surfaced as
// warnings. RecordOnly never executes, so execution-safety lint is
// advisory (flyway baseline / liquibase changelog-sync semantics).
func TestMigrationBundleValidator_RecordOnlyLintErrorAdmitted(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-adopt"},
		Data: map[string]string{
			"0001_baseline.up.sql": "DROP TABLE users;",
		},
	}
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "b-adopt"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:       "0001",
			Strategy:      keystonev1alpha1.StrategyVersioned,
			ExecutionMode: keystonev1alpha1.ExecutionModeRecordOnly,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-adopt", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
	warnings, err := v.ValidateCreate(context.Background(), bundle)
	if err != nil {
		t.Fatalf("RecordOnly bundle with lint errors must be admitted; got %v", err)
	}
	var sawFinding, sawDowngrade bool
	for _, w := range warnings {
		if strings.Contains(w, "no-drop-table") {
			sawFinding = true
		}
		if strings.Contains(w, "downgraded to advisory") {
			sawDowngrade = true
		}
	}
	if !sawFinding {
		t.Errorf("expected the no-drop-table finding as a warning; got %v", warnings)
	}
	if !sawDowngrade {
		t.Errorf("expected the downgrade notice warning; got %v", warnings)
	}
}

// TestMigrationBundleValidator_RecordOnlyAutoRollbackRejected — a
// never-executed baseline has nothing to roll back.
func TestMigrationBundleValidator_RecordOnlyAutoRollbackRejected(t *testing.T) {
	v := &MigrationBundleValidator{
		Client:   newFakeClient().Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "b-adopt-rb"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:       "0001",
			Strategy:      keystonev1alpha1.StrategyVersioned,
			ExecutionMode: keystonev1alpha1.ExecutionModeRecordOnly,
			AutoRollback:  true,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-any", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatalf("expected autoRollback+RecordOnly rejection")
	}
	if !strings.Contains(err.Error(), "nothing to roll back") {
		t.Errorf("error message mismatch: %v", err)
	}
}

// TestMigrationBundleValidator_ExecutionModeImmutable — flipping a
// shipped bundle between Apply and RecordOnly is refused (it would
// rewrite the meaning of the tracking-table row). Empty mode on the
// old object normalises to Apply, so pre-ADR-0027 bundles update
// cleanly as long as the mode isn't actually changed.
func TestMigrationBundleValidator_ExecutionModeImmutable(t *testing.T) {
	v := &MigrationBundleValidator{
		Client:   newFakeClient().Build(),
		Registry: analyze.DefaultRegistry(),
	}
	old := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "b-modeflip"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			// ExecutionMode empty — pre-ADR-0027 object, treated as Apply.
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: "cm-any", FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
	flipped := old.DeepCopy()
	flipped.Spec.ExecutionMode = keystonev1alpha1.ExecutionModeRecordOnly

	if _, err := v.ValidateUpdate(context.Background(), old, flipped); err == nil {
		t.Fatalf("expected executionMode immutability rejection")
	} else if !strings.Contains(err.Error(), "executionMode is immutable") {
		t.Errorf("error message mismatch: %v", err)
	}

	// No-op update of a pre-ADR-0027 bundle (empty → explicit Apply)
	// must NOT be flagged as a mode flip.
	normalised := old.DeepCopy()
	normalised.Spec.ExecutionMode = keystonev1alpha1.ExecutionModeApply
	if _, err := v.ValidateUpdate(context.Background(), old, normalised); err != nil {
		t.Errorf("empty→Apply normalisation must admit; got %v", err)
	}
}

// -- A1 integrity gate tests -----------------------------------------

// makeBundle returns a stock versioned bundle targeted at cmName in the
// given namespace. Used by every integrity test so the Spec fields in
// scope are obvious.
func makeBundle(ns, name, cmName string) *keystonev1alpha1.MigrationBundle {
	return &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  "v1",
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name: cmName, FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{MatchLabels: map[string]string{"m": "x"}},
		},
	}
}

// cmWithSum builds a ConfigMap that already carries a canonical
// keystone.sum for its data files. Use when a test wants the happy
// path.
func cmWithSum(ns, name string, sqlFiles map[string]string) *corev1.ConfigMap {
	sum := string(migration.MarshalSum(migration.BuildSum(sqlFiles)))
	data := make(map[string]string, len(sqlFiles)+1)
	for k, v := range sqlFiles {
		data[k] = v
	}
	data[migration.SumFilename] = sum
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Data:       data,
	}
}

func TestMigrationBundleValidator_IntegrityValidAdmitted(t *testing.T) {
	files := map[string]string{
		"001_init.up.sql": "CREATE TABLE demo (id uuid PRIMARY KEY);",
	}
	cm := cmWithSum("keystone-system", "cm-int-ok", files)
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "bint1", "cm-int-ok")

	warnings, err := v.ValidateCreate(context.Background(), bundle)
	if err != nil {
		t.Fatalf("valid sum should admit; got %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "keystone.sum") {
			t.Errorf("no sum warning expected for valid bundle; got %q", w)
		}
	}
}

func TestMigrationBundleValidator_IntegrityMismatchRejected(t *testing.T) {
	// Build sum from the original files, then tamper one BEFORE writing
	// the ConfigMap — the committed sum references a hash the SQL no
	// longer matches.
	files := map[string]string{
		"001_init.up.sql": "CREATE TABLE demo (id uuid PRIMARY KEY);",
	}
	sum := string(migration.MarshalSum(migration.BuildSum(files)))
	files["001_init.up.sql"] = "DROP TABLE demo;"
	data := map[string]string{migration.SumFilename: sum}
	for k, v := range files {
		data[k] = v
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-int-bad"},
		Data:       data,
	}
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "bint2", "cm-int-bad")

	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatal("tampered bundle must be rejected")
	}
	if !strings.Contains(err.Error(), "diverges from its keystone.sum") {
		t.Errorf("error should name keystone.sum divergence; got %v", err)
	}
}

func TestMigrationBundleValidator_IntegrityMalformedRejected(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-int-malformed"},
		Data: map[string]string{
			"001_init.up.sql":        "CREATE TABLE demo (id uuid PRIMARY KEY);",
			migration.SumFilename:    "not a sum file",
		},
	}
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "bint3", "cm-int-malformed")

	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatal("malformed sum must be rejected")
	}
	if !strings.Contains(err.Error(), "failed to parse") {
		t.Errorf("error should name parse failure; got %v", err)
	}
}

func TestMigrationBundleValidator_IntegrityMissingWarnsWhenNoPolicy(t *testing.T) {
	// No sum, no SchemaPolicy requiring one — advisory mode admits with
	// a warning.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-int-missing"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo (id uuid PRIMARY KEY);",
		},
	}
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "bint4", "cm-int-missing")

	warnings, err := v.ValidateCreate(context.Background(), bundle)
	if err != nil {
		t.Fatalf("missing sum must warn, not reject (advisory mode); got %v", err)
	}
	foundWarning := false
	for _, w := range warnings {
		if strings.Contains(w, "no keystone.sum") {
			foundWarning = true
			break
		}
	}
	if !foundWarning {
		t.Errorf("expected missing-sum warning; got %+v", warnings)
	}
}

func TestMigrationBundleValidator_IntegrityMissingRejectedWhenPolicyRequires(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-int-missing"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo (id uuid PRIMARY KEY);",
		},
	}
	policy := &keystonev1alpha1.SchemaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-strict"},
		Spec: keystonev1alpha1.SchemaPolicySpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			Integrity: &keystonev1alpha1.IntegrityPolicy{
				RequireSumFile: true,
			},
		},
	}
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm, policy).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "bint5", "cm-int-missing")
	bundle.Labels = map[string]string{"tier": "prod"}

	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatal("policy-required sum must reject on missing")
	}
	if !strings.Contains(err.Error(), "prod-strict") {
		t.Errorf("error should cite the offending policy; got %v", err)
	}
}

// -- A3 approval gate tests ------------------------------------------

// approvalPolicy returns a SchemaPolicy with the given target selector
// and a single ApprovalPolicy requiring `required` approvers from the
// given groups. Keeps the test tables compact.
func approvalPolicy(name string, targetLabels map[string]string, approvalName string, required int32, fromGroups []string, disallowSelf bool) *keystonev1alpha1.SchemaPolicy {
	return &keystonev1alpha1.SchemaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: keystonev1alpha1.SchemaPolicySpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: targetLabels},
			ApprovalPolicies: []keystonev1alpha1.ApprovalPolicy{{
				Name:                 approvalName,
				RequiredApprovers:    required,
				FromGroups:           fromGroups,
				DisallowSelfApproval: disallowSelf,
			}},
		},
	}
}

func TestMigrationBundleValidator_ApprovalSufficientAdmits(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-appr-ok"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_a3 (id uuid PRIMARY KEY);",
		},
	}
	policy := approvalPolicy("prod-dba", map[string]string{"tier": "prod"},
		"dba", 1, []string{"dba"}, true)
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm, policy).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "b-appr-ok", "cm-appr-ok")
	bundle.Labels = map[string]string{"tier": "prod"}
	bundle.Annotations = map[string]string{
		keystonev1alpha1.AnnotationAuthor:                     "alice",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.bob": "dba",
	}
	if _, err := v.ValidateCreate(context.Background(), bundle); err != nil {
		t.Fatalf("sufficient approval should admit; got %v", err)
	}
}

func TestMigrationBundleValidator_ApprovalInsufficientRejected(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-appr-bad"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_a3 (id uuid PRIMARY KEY);",
		},
	}
	policy := approvalPolicy("prod-dba", map[string]string{"tier": "prod"},
		"dba", 2, []string{"dba"}, true)
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm, policy).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "b-appr-bad", "cm-appr-bad")
	bundle.Labels = map[string]string{"tier": "prod"}
	bundle.Annotations = map[string]string{
		keystonev1alpha1.AnnotationAuthor:                     "alice",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.bob": "dba",
	}
	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatal("under-quorum approval must reject")
	}
	if !strings.Contains(err.Error(), "approval requirement") {
		t.Errorf("message should name approval violation; got %v", err)
	}
}

func TestMigrationBundleValidator_ApprovalSelfFiltered(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-appr-self"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_a3 (id uuid PRIMARY KEY);",
		},
	}
	policy := approvalPolicy("prod-dba", map[string]string{"tier": "prod"},
		"dba", 1, []string{"dba"}, true)
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm, policy).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "b-appr-self", "cm-appr-self")
	bundle.Labels = map[string]string{"tier": "prod"}
	bundle.Annotations = map[string]string{
		keystonev1alpha1.AnnotationAuthor:                       "alice",
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.alice": "dba",
	}
	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatal("self-approval must be filtered out; admission should reject")
	}
}

func TestMigrationBundleValidator_ApprovalOutOfTierIgnored(t *testing.T) {
	// Policy matches tier=prod only; dev-tier bundle must admit even
	// without approval annotations.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-appr-dev"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_a3 (id uuid PRIMARY KEY);",
		},
	}
	policy := approvalPolicy("prod-only", map[string]string{"tier": "prod"},
		"dba", 1, []string{"dba"}, true)
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm, policy).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "b-appr-dev", "cm-appr-dev")
	bundle.Labels = map[string]string{"tier": "dev"}
	if _, err := v.ValidateCreate(context.Background(), bundle); err != nil {
		t.Fatalf("prod-only policy must not gate dev tier; got %v", err)
	}
}

func TestMigrationBundleValidator_ApprovalWrongGroupIgnored(t *testing.T) {
	// Approver's declared group is not in FromGroups — doesn't count.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-appr-wrong"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_a3 (id uuid PRIMARY KEY);",
		},
	}
	policy := approvalPolicy("prod-dba", map[string]string{"tier": "prod"},
		"dba", 1, []string{"dba"}, true)
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm, policy).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "b-appr-wrong", "cm-appr-wrong")
	bundle.Labels = map[string]string{"tier": "prod"}
	bundle.Annotations = map[string]string{
		keystonev1alpha1.AnnotationApprovalPrefix + "dba.bob": "sre", // wrong group
	}
	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatal("approver with non-matching group must not satisfy policy")
	}
}

// -- C2 CEL expression tests -----------------------------------------

func newCELPolicy(name string, targetLabels map[string]string, rules []keystonev1alpha1.CELRule) *keystonev1alpha1.SchemaPolicy {
	return &keystonev1alpha1.SchemaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: keystonev1alpha1.SchemaPolicySpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: targetLabels},
			Expressions:    rules,
		},
	}
}

func TestMigrationBundleValidator_CELRuleFailRejects(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-cel-fail"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_c2 (id uuid PRIMARY KEY);",
		},
	}
	policy := newCELPolicy("prod-require-jira", map[string]string{"tier": "prod"},
		[]keystonev1alpha1.CELRule{
			{Name: "require-jira", Severity: keystonev1alpha1.CELSeverityError,
				Expression: `!("keystone.hexxlock.io/jira" in annotations)`,
				Message:    "prod bundles require a jira ticket annotation"},
		})
	ev, err := policycel.NewEvaluator()
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	v := &MigrationBundleValidator{
		Client:       newFakeClient(cm, policy).Build(),
		Registry:     analyze.DefaultRegistry(),
		CELEvaluator: ev,
	}
	bundle := makeBundle("keystone-system", "bcel1", "cm-cel-fail")
	bundle.Labels = map[string]string{"tier": "prod"}
	// No jira annotation → rule fires.
	_, err = v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatal("expected CEL rule rejection")
	}
	if !strings.Contains(err.Error(), "require-jira") {
		t.Errorf("error should name the rule; got %v", err)
	}
}

func TestMigrationBundleValidator_CELRulePassAdmits(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-cel-pass"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_c2 (id uuid PRIMARY KEY);",
		},
	}
	policy := newCELPolicy("prod-require-jira", map[string]string{"tier": "prod"},
		[]keystonev1alpha1.CELRule{
			{Name: "require-jira", Severity: keystonev1alpha1.CELSeverityError,
				Expression: `!("keystone.hexxlock.io/jira" in annotations)`,
				Message:    "prod bundles require a jira ticket annotation"},
		})
	ev, _ := policycel.NewEvaluator()
	v := &MigrationBundleValidator{
		Client:       newFakeClient(cm, policy).Build(),
		Registry:     analyze.DefaultRegistry(),
		CELEvaluator: ev,
	}
	bundle := makeBundle("keystone-system", "bcel2", "cm-cel-pass")
	bundle.Labels = map[string]string{"tier": "prod"}
	bundle.Annotations = map[string]string{"keystone.hexxlock.io/jira": "DATA-99"}

	if _, err := v.ValidateCreate(context.Background(), bundle); err != nil {
		t.Fatalf("present annotation should admit; got %v", err)
	}
}

func TestMigrationBundleValidator_CELRuleWarnDoesNotReject(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-cel-warn"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_c2 (id uuid PRIMARY KEY);",
		},
	}
	policy := newCELPolicy("advisory", map[string]string{"tier": "prod"},
		[]keystonev1alpha1.CELRule{
			{Name: "notify", Severity: keystonev1alpha1.CELSeverityWarning,
				Expression: `true`,
				Message:    "heads up"},
		})
	ev, _ := policycel.NewEvaluator()
	v := &MigrationBundleValidator{
		Client:       newFakeClient(cm, policy).Build(),
		Registry:     analyze.DefaultRegistry(),
		CELEvaluator: ev,
	}
	bundle := makeBundle("keystone-system", "bcel3", "cm-cel-warn")
	bundle.Labels = map[string]string{"tier": "prod"}

	warnings, err := v.ValidateCreate(context.Background(), bundle)
	if err != nil {
		t.Fatalf("warn severity should admit; got %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "notify") && strings.Contains(w, "heads up") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning from rule; got %+v", warnings)
	}
}

func TestMigrationBundleValidator_CELCompileErrorBlocks(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-cel-broken"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo_c2 (id uuid PRIMARY KEY);",
		},
	}
	policy := newCELPolicy("broken", map[string]string{"tier": "prod"},
		[]keystonev1alpha1.CELRule{
			{Name: "typo", Expression: `labels["tier" == "prod"`}, // unclosed bracket
		})
	ev, _ := policycel.NewEvaluator()
	v := &MigrationBundleValidator{
		Client:       newFakeClient(cm, policy).Build(),
		Registry:     analyze.DefaultRegistry(),
		CELEvaluator: ev,
	}
	bundle := makeBundle("keystone-system", "bcel4", "cm-cel-broken")
	bundle.Labels = map[string]string{"tier": "prod"}

	_, err := v.ValidateCreate(context.Background(), bundle)
	if err == nil {
		t.Fatal("broken expression should block admission")
	}
	if !strings.Contains(err.Error(), "compile") {
		t.Errorf("error should mention compile failure; got %v", err)
	}
}

func TestMigrationBundleValidator_IntegrityPolicyDoesNotMatchOtherTier(t *testing.T) {
	// Policy requires sum for tier=prod, bundle is tier=dev — missing
	// sum must still be advisory, not a hard reject.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "cm-int-dev"},
		Data: map[string]string{
			"001_init.up.sql": "CREATE TABLE demo (id uuid PRIMARY KEY);",
		},
	}
	policy := &keystonev1alpha1.SchemaPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-only"},
		Spec: keystonev1alpha1.SchemaPolicySpec{
			TargetSelector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			Integrity: &keystonev1alpha1.IntegrityPolicy{
				RequireSumFile: true,
			},
		},
	}
	v := &MigrationBundleValidator{
		Client:   newFakeClient(cm, policy).Build(),
		Registry: analyze.DefaultRegistry(),
	}
	bundle := makeBundle("keystone-system", "bint6", "cm-int-dev")
	bundle.Labels = map[string]string{"tier": "dev"}

	_, err := v.ValidateCreate(context.Background(), bundle)
	if err != nil {
		t.Fatalf("prod-scoped policy must not reject dev bundle; got %v", err)
	}
}
