// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/dogukanturhal/keystone-sdk/go/declarative"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func exists(t *testing.T, c client.Client, obj client.Object, ns, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get %s: %v", name, err)
	}
	return err == nil
}

// Bundle and ConfigMap names are content addresses; existing clusters hold
// objects under these names, so the hash must not drift.
func TestPlanContentHash_Stable(t *testing.T) {
	plan := &declarative.Plan{Statements: []string{"CREATE TABLE a (id int)", "ALTER TABLE a ADD COLUMN b int"}}
	const golden = "ecc9385dca259ba5fe45a587c6346950a15a28eebce61b97e2eadd903e957010"
	if got := planContentHash(plan); got != golden {
		t.Fatalf("planContentHash = %s, want %s", got, golden)
	}
	// Independent of the helper: the original inline algorithm.
	h := sha256.New()
	for _, s := range plan.Statements {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	if got := planContentHash(plan); got != hex.EncodeToString(h.Sum(nil)) {
		t.Fatalf("diverged from legacy algorithm")
	}
}

func TestEmitBundle_NamesUnchanged(t *testing.T) {
	sd := mkSD("billing", "default", nil, nil)
	schema := &keystonev1alpha1.DatabaseSchema{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "test-schema"}}
	c := newCRTTLFakeClient(sd)
	r := &SchemaDefinitionReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(8)}
	plan := &declarative.Plan{Statements: []string{"CREATE TABLE a (id int)", "ALTER TABLE a ADD COLUMN b int"}}
	name, err := r.emitBundle(context.Background(), sd, schema, plan)
	if err != nil {
		t.Fatal(err)
	}
	h := "ecc9385dca259ba5fe45a587c6346950a15a28eebce61b97e2eadd903e957010"
	if name != "billing-"+h {
		t.Fatalf("bundle name %q", name)
	}
	if !exists(t, c, &corev1.ConfigMap{}, "default", "billing-sql-"+h) {
		t.Fatal("ConfigMap billing-sql-<hash> missing")
	}
	var b keystonev1alpha1.MigrationBundle
	exists(t, c, &b, "default", name)
	if b.Spec.Version != "billing-decl-"+h[:16] {
		t.Fatalf("version %q", b.Spec.Version)
	}
}

func readyCond(status metav1.ConditionStatus, reason string) []metav1.Condition {
	return []metav1.Condition{{
		Type: keystonev1alpha1.ConditionTypeReady, Status: status, Reason: reason,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-8 * 24 * time.Hour)),
	}}
}

func TestBundleSuccess_AllFailedIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name         string
		out          executionOutcome
		applied      int32
		wantStatus   metav1.ConditionStatus
		wantReason   string
		wantProgress metav1.ConditionStatus
	}{
		{"all failed", executionOutcome{failed: 2}, 0, metav1.ConditionFalse, reasonExecutionsFailed, metav1.ConditionFalse},
		{"applied+failed", executionOutcome{failed: 1}, 1, metav1.ConditionFalse, reasonExecutionsFailed, metav1.ConditionFalse},
		{"all stale-aborted", executionOutcome{failed: 2, stale: 2}, 0, metav1.ConditionFalse, reasonSuperseded, metav1.ConditionFalse},
		{"mixed stale", executionOutcome{failed: 2, stale: 1}, 0, metav1.ConditionFalse, reasonExecutionsFailed, metav1.ConditionFalse},
		{"in flight keeps progressing", executionOutcome{failed: 1, inFlight: 1}, 0, "", "", metav1.ConditionTrue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := &keystonev1alpha1.MigrationBundle{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mb", Generation: 1},
				Spec:       keystonev1alpha1.MigrationBundleSpec{Version: "v1"},
			}
			c := newCRTTLFakeClient(bundle)
			r := &MigrationBundleReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}
			if _, err := r.successWithOutcome(context.Background(), bundle, "h", 2, tc.applied, tc.out, log.Log); err != nil {
				t.Fatal(err)
			}
			var got keystonev1alpha1.MigrationBundle
			exists(t, c, &got, "default", "mb")
			ready := findReadyLikeCondition(got.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
			if tc.wantReason == "" {
				if ready != nil {
					t.Fatalf("unexpected Ready verdict %+v", ready)
				}
			} else if ready == nil || ready.Status != tc.wantStatus || ready.Reason != tc.wantReason {
				t.Fatalf("Ready = %+v, want %s/%s", ready, tc.wantStatus, tc.wantReason)
			}
			prog := findReadyLikeCondition(got.Status.Conditions, "Progressing")
			if prog == nil || prog.Status != tc.wantProgress {
				t.Fatalf("Progressing = %+v, want %s", prog, tc.wantProgress)
			}
			if tc.wantReason != "" {
				if _, _, terminal := classifyBundle(&got); !terminal {
					t.Fatal("classifyBundle must treat the verdict as terminal")
				}
			}
		})
	}
}

func TestClassifyBundle_ExecutionsFailedTerminalWithoutLastPlanTime(t *testing.T) {
	for _, reason := range []string{reasonExecutionsFailed, reasonSuperseded} {
		b := mkBundle("b", "default", "sd", metav1.ConditionFalse, 0, 1, 0)
		b.Status.LastPlanTime = nil
		b.Status.Conditions = readyCond(metav1.ConditionFalse, reason)
		if _, _, terminal := classifyBundle(b); !terminal {
			t.Errorf("%s not terminal", reason)
		}
	}
}

func TestCRTTL_KeepsFailedExecUnderNonTerminalBundle(t *testing.T) {
	sd := mkSD("billing", "default", nil, nil)
	mb := mkBundle("mb-1", "default", "billing", metav1.ConditionUnknown, 30*24*time.Hour, 2, 0) // not terminal
	failed := mkExec("e-failed", "default", "mb-1", "s1", keystonev1alpha1.ExecutionPhaseFailed, 30*24*time.Hour)
	aborted := mkExec("e-aborted", "default", "mb-1", "s2", keystonev1alpha1.ExecutionPhaseAborted, 30*24*time.Hour)
	rbf := mkExec("e-rbf", "default", "mb-1", "s3", keystonev1alpha1.ExecutionPhaseRollbackFailed, 30*24*time.Hour)
	c := newCRTTLFakeClient(sd, mb, failed, aborted, rbf)
	(&CRTTLController{Client: c, Enabled: true}).runOnce(context.Background(), &discardLogger{})
	for _, n := range []string{"e-failed", "e-aborted", "e-rbf"} {
		if !exists(t, c, &keystonev1alpha1.MigrationExecution{}, "default", n) {
			t.Errorf("%s deleted while its bundle is still live", n)
		}
	}
}

func TestCRTTL_DeletesTerminalFailedSDOwnedBundle(t *testing.T) {
	sd := mkSD("billing", "default", nil, nil)
	mb := mkBundle("mb-1", "default", "billing", metav1.ConditionFalse, 8*24*time.Hour, 1, 0)
	mb.Status.Conditions = readyCond(metav1.ConditionFalse, reasonExecutionsFailed)
	e := mkExec("e-failed", "default", "mb-1", "s1", keystonev1alpha1.ExecutionPhaseFailed, 8*24*time.Hour)
	c := newCRTTLFakeClient(sd, mb, e)
	(&CRTTLController{Client: c, Enabled: true}).runOnce(context.Background(), &discardLogger{})
	if exists(t, c, &keystonev1alpha1.MigrationBundle{}, "default", "mb-1") {
		t.Fatal("terminal failed SD-owned bundle past window should be deleted")
	}
	// The fake client has no ownerRef GC; the real cluster cascades the exec
	// via its controller reference to the bundle.
}

func TestCRTTL_SkipsArgoTracked(t *testing.T) {
	sd := mkSD("billing", "default", nil, nil)
	mb := mkBundle("mb-1", "default", "billing", metav1.ConditionFalse, 30*24*time.Hour, 1, 0)
	mb.Status.Conditions = readyCond(metav1.ConditionFalse, reasonExecutionsFailed)
	mb.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "app:keystone.hexxlock.io/MigrationBundle:default/mb-1"}
	old := mkExec("e-old-succ", "default", "mb-1", "s1", keystonev1alpha1.ExecutionPhaseSucceeded, 30*24*time.Hour)
	new := mkExec("e-new-succ", "default", "mb-1", "s1", keystonev1alpha1.ExecutionPhaseSucceeded, time.Minute)
	c := newCRTTLFakeClient(sd, mb, old, new)
	(&CRTTLController{Client: c, Enabled: true}).runOnce(context.Background(), &discardLogger{})
	if !exists(t, c, &keystonev1alpha1.MigrationBundle{}, "default", "mb-1") {
		t.Fatal("Argo-tracked bundle deleted")
	}
	if !exists(t, c, &keystonev1alpha1.MigrationExecution{}, "default", "e-old-succ") {
		t.Fatal("execution of an Argo-tracked bundle deleted")
	}
}

func declBundle(sd *keystonev1alpha1.SchemaDefinition, name string, ready metav1.ConditionStatus, reason string) *keystonev1alpha1.MigrationBundle {
	b := mkBundle(name, sd.Namespace, sd.Name, ready, time.Hour, 1, 0)
	b.OwnerReferences[0].UID = sd.UID
	b.Labels = map[string]string{
		keystonev1alpha1.LabelSchemaDefinition: sd.Name,
		keystonev1alpha1.LabelSource:           "declarative-diff",
	}
	b.Status.Conditions = readyCond(ready, reason)
	b.Spec.Source = keystonev1alpha1.MigrationSource{
		Type:         keystonev1alpha1.SourceConfigMap,
		ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{Name: sd.Name + "-sql-" + name},
	}
	return b
}

func TestPruneSupersededBundles(t *testing.T) {
	sd := mkSD("billing", "default", nil, nil)
	failed := declBundle(sd, "b-failed", metav1.ConditionFalse, reasonExecutionsFailed)
	superseded := declBundle(sd, "b-superseded", metav1.ConditionFalse, reasonSuperseded)
	ready := declBundle(sd, "b-ready", metav1.ConditionTrue, "AllSchemasApplied")
	keep := declBundle(sd, "b-keep", metav1.ConditionFalse, reasonExecutionsFailed)
	active := declBundle(sd, "b-active", metav1.ConditionFalse, "ApplyingToSchemas")
	hasSucc := declBundle(sd, "b-hassucc", metav1.ConditionFalse, "ApplyingToSchemas")
	foreign := declBundle(sd, "b-foreign", metav1.ConditionFalse, reasonExecutionsFailed)
	foreign.Labels[keystonev1alpha1.LabelSchemaDefinition] = "other"
	handwritten := mkBundle("b-hand", "default", "billing", metav1.ConditionFalse, time.Hour, 1, 0) // no declarative label
	argo := declBundle(sd, "b-argo", metav1.ConditionFalse, reasonExecutionsFailed)
	argo.Annotations = map[string]string{"argocd.argoproj.io/tracking-id": "x"}
	objs := []runtime.Object{sd, failed, superseded, ready, keep, active, hasSucc, foreign, handwritten, argo,
		mkExec("e-act", "default", "b-active", "s", keystonev1alpha1.ExecutionPhaseRunning, time.Minute),
		mkExec("e-succ", "default", "b-hassucc", "s", keystonev1alpha1.ExecutionPhaseSucceeded, time.Minute),
		mkExec("e-fail", "default", "b-failed", "s", keystonev1alpha1.ExecutionPhaseFailed, time.Minute),
	}
	for _, n := range []string{"b-failed", "b-superseded"} {
		objs = append(objs, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "billing-sql-" + n}})
	}
	c := newCRTTLFakeClient(objs...)
	rec := record.NewFakeRecorder(16)
	r := &SchemaDefinitionReconciler{Client: c, Scheme: c.Scheme(), Recorder: rec}
	r.pruneSupersededBundles(context.Background(), sd, "b-keep", log.Log)

	want := map[string]bool{
		"b-failed": false, "b-superseded": false, // pruned
		"b-ready": true, "b-keep": true, "b-active": true, "b-hassucc": true,
		"b-foreign": true, "b-hand": true, "b-argo": true,
	}
	for n, w := range want {
		if got := exists(t, c, &keystonev1alpha1.MigrationBundle{}, "default", n); got != w {
			t.Errorf("bundle %s exists=%v want %v", n, got, w)
		}
	}
	for _, n := range []string{"b-failed", "b-superseded"} {
		if exists(t, c, &corev1.ConfigMap{}, "default", "billing-sql-"+n) {
			t.Errorf("ConfigMap for %s not pruned", n)
		}
	}
	if len(rec.Events) != 2 {
		t.Errorf("want 2 events, got %d", len(rec.Events))
	}
	// keep="" (markReady) also prunes the previously kept failed bundle.
	r.pruneSupersededBundles(context.Background(), sd, "", log.Log)
	if exists(t, c, &keystonev1alpha1.MigrationBundle{}, "default", "b-keep") {
		t.Error("b-keep should be pruned once no longer kept")
	}
	if !exists(t, c, &keystonev1alpha1.MigrationBundle{}, "default", "b-ready") {
		t.Error("Ready bundle must survive")
	}
}

func TestApplyPrecheck_CheckSD(t *testing.T) {
	sd := mkSD("billing", "default", nil, nil)
	fp := specFingerprint(&sd.Spec)
	mk := func(name, fingerprint string) *keystonev1alpha1.MigrationBundle {
		b := declBundle(sd, name, metav1.ConditionUnknown, "")
		b.Annotations = map[string]string{keystonev1alpha1.AnnotationSDFingerprint: fingerprint}
		return b
	}
	run := func(c client.Client, b *keystonev1alpha1.MigrationBundle) error {
		p := &applyPrecheck{r: &MigrationExecutionReconciler{Client: c}, bundle: b}
		return p.checkSD(context.Background(), nil)
	}
	isStale := func(err error) bool { var s *stalePlanError; return errors.As(err, &s) }

	// SD gone.
	if err := run(newCRTTLFakeClient(), mk("b", fp)); !isStale(err) {
		t.Errorf("SD gone: want stale, got %v", err)
	}
	// Fingerprint mismatch.
	if err := run(newCRTTLFakeClient(sd), mk("b", "other")); !isStale(err) {
		t.Errorf("fingerprint mismatch: want stale, got %v", err)
	}
	// Matching fingerprint, schemaRef SD, no snapshot: cheap checks pass.
	if err := run(newCRTTLFakeClient(sd), mk("b", fp)); err != nil {
		t.Errorf("fresh bundle: %v", err)
	}
	// Selector SD: must be the SD's current bundle.
	sel := sd.DeepCopy()
	sel.Spec.SchemaRef = ""
	sel.Spec.SchemaSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"k": "v"}}
	selFP := specFingerprint(&sel.Spec)
	sel.Status.CurrentBundleRef = "b-current"
	c := newCRTTLFakeClient(sel)
	if err := run(c, mk("b-old", selFP)); !isStale(err) {
		t.Errorf("selector SD, non-current bundle: want stale, got %v", err)
	}
	if err := run(c, mk("b-current", selFP)); err != nil {
		t.Errorf("selector SD, current bundle: %v", err)
	}
}

func TestMarkStale_AbortsWithoutError(t *testing.T) {
	e := mkExec("e1", "default", "b", "s", keystonev1alpha1.ExecutionPhaseRunning, 0)
	c := newCRTTLFakeClient(e)
	rec := record.NewFakeRecorder(4)
	r := &MigrationExecutionReconciler{Client: c, Recorder: rec}
	res, err := r.markStale(context.Background(), e, &stalePlanError{"x"})
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("res=%v err=%v", res, err)
	}
	var got keystonev1alpha1.MigrationExecution
	exists(t, c, &got, "default", "e1")
	if got.Status.Phase != keystonev1alpha1.ExecutionPhaseAborted || !isStaleAbort(&got) {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestRetryStaleExecutions(t *testing.T) {
	sd := mkSD("billing", "default", nil, nil)
	fp := specFingerprint(&sd.Spec)
	mkB := func(name string) *keystonev1alpha1.MigrationBundle {
		b := declBundle(sd, name, metav1.ConditionFalse, reasonSuperseded)
		b.Annotations = map[string]string{keystonev1alpha1.AnnotationSDFingerprint: fp}
		return b
	}
	stale := func(name, bundle string) *keystonev1alpha1.MigrationExecution {
		e := mkExec(name, "default", bundle, "s", keystonev1alpha1.ExecutionPhaseAborted, time.Hour)
		e.Status.Conditions = readyCond(metav1.ConditionFalse, reasonStaleDeclarativePlan)
		return e
	}
	good := mkB("b-good")
	argo := mkB("b-argo")
	argo.Annotations["argocd.argoproj.io/tracking-id"] = "x"
	oldFP := mkB("b-oldfp")
	oldFP.Annotations[keystonev1alpha1.AnnotationSDFingerprint] = "old"
	hand := mkBundle("b-hand", "default", "billing", metav1.ConditionFalse, time.Hour, 1, 0)
	objs := []runtime.Object{sd, good, argo, oldFP, hand,
		stale("e-stale", "b-good"),
		mkExec("e-sqlfail", "default", "b-good", "s2", keystonev1alpha1.ExecutionPhaseFailed, time.Hour),
		mkExec("e-plain-abort", "default", "b-good", "s3", keystonev1alpha1.ExecutionPhaseAborted, time.Hour),
		stale("e-argo", "b-argo"), stale("e-oldfp", "b-oldfp"), stale("e-hand", "b-hand"),
	}
	c := newCRTTLFakeClient(objs...)
	r := &SchemaDefinitionReconciler{Client: c, Scheme: c.Scheme(), Recorder: record.NewFakeRecorder(16)}
	for i := 0; i < 2; i++ { // second pass: idempotent
		for _, b := range []string{"b-good", "b-argo", "b-oldfp", "b-hand"} {
			r.retryStaleExecutions(context.Background(), sd, b, log.Log)
		}
	}
	for n, want := range map[string]bool{
		"e-stale":   false,                       // retried
		"e-sqlfail": true, "e-plain-abort": true, // real failures / non-stale aborts kept
		"e-argo": true, "e-oldfp": true, "e-hand": true,
	} {
		if got := exists(t, c, &keystonev1alpha1.MigrationExecution{}, "default", n); got != want {
			t.Errorf("%s exists=%v want %v", n, got, want)
		}
	}
}
