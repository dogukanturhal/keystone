// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/conditions"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

func sampleReport() pg.OwnershipReport {
	return pg.OwnershipReport{
		ConnRole: "keystone_admin",
		Groups: []pg.OwnershipGroup{
			{Kind: "function", Owner: "postgres", Count: 64, Sample: []string{"public.f(uuid)", "public.g()"}},
			{Kind: "enum", Owner: "keystone_admin", ConnOwned: true, Count: 1, Sample: []string{"public.e"}},
		},
	}
}

func TestBuildOwnershipMessage_Deterministic(t *testing.T) {
	rep := sampleReport()
	a := buildOwnershipMessage(rep, "own", "Report", "")
	b := buildOwnershipMessage(rep, "own", "Report", "")
	if a != b {
		t.Fatalf("not deterministic:\n%s\n%s", a, b)
	}
	for _, want := range []string{
		"65 objects not owned by own: function/postgres=64, enum/keystone_admin=1.",
		"Remediable (keystone_admin): 1 [policy=Report].",
		"Not remediable by keystone (postgres=64)",
		"never REASSIGN OWNED BY postgres",
		"Sample: public.f(uuid), public.g(), public.e",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("message missing %q:\n%s", want, a)
		}
	}
}

func TestBuildOwnershipMessage_Bounded(t *testing.T) {
	rep := pg.OwnershipReport{ConnRole: "keystone_admin"}
	long := strings.Repeat("é", 300)
	for i := 0; i < 40; i++ {
		rep.Groups = append(rep.Groups, pg.OwnershipGroup{
			Kind: long, Owner: long + string(rune('a'+i%26)), Count: 100 - i, Sample: []string{long, long},
		})
	}
	msg := buildOwnershipMessage(rep, "own", "Report", "")
	if len(msg) > ownershipMaxMessageBytes {
		t.Fatalf("message %d bytes > %d", len(msg), ownershipMaxMessageBytes)
	}
	if !isValidUTF8(msg) {
		t.Fatal("truncation split a rune")
	}
}

func isValidUTF8(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }

func TestBuildOwnershipOutcome_StatusBounded(t *testing.T) {
	rep := pg.OwnershipReport{ConnRole: "ks"}
	for i := 0; i < 50; i++ {
		rep.Groups = append(rep.Groups, pg.OwnershipGroup{Kind: "table", Owner: "o", Count: 1,
			Sample: []string{strings.Repeat("x", 600)}})
	}
	o := buildOwnershipOutcome(rep, "own", "Report", "", time.Now())
	if o.Status != metav1.ConditionTrue || o.Reason != keystonev1alpha1.ReasonOwnershipDriftDetected {
		t.Fatalf("outcome %v/%v", o.Status, o.Reason)
	}
	if len(o.Summary.Groups) != ownershipMaxStatusGroups {
		t.Errorf("groups %d want %d", len(o.Summary.Groups), ownershipMaxStatusGroups)
	}
	if got := len(o.Summary.Groups[0].Sample[0]); got != ownershipMaxSampleLen {
		t.Errorf("sample len %d want %d", got, ownershipMaxSampleLen)
	}
	if o.Summary.DriftedObjects != 50 {
		t.Errorf("DriftedObjects=%d", o.Summary.DriftedObjects)
	}
	clean := buildOwnershipOutcome(pg.OwnershipReport{}, "own", "Report", "", time.Now())
	if clean.Status != metav1.ConditionFalse || clean.Reason != keystonev1alpha1.ReasonOwnershipNoDrift {
		t.Errorf("clean outcome %v/%v", clean.Status, clean.Reason)
	}
}

func TestResolveOwnershipPolicy(t *testing.T) {
	re := keystonev1alpha1.OwnershipRemediationReassignFromConnectionRole
	rp := keystonev1alpha1.OwnershipRemediationReport
	mk := func(ldb, prov string) (*keystonev1alpha1.LogicalDatabase, *keystonev1alpha1.DatabaseProvider) {
		l := &keystonev1alpha1.LogicalDatabase{}
		l.Spec.OwnershipRemediation = ldb
		p := &keystonev1alpha1.DatabaseProvider{}
		p.Spec.OwnershipRemediation = prov
		return l, p
	}
	cases := []struct{ ldb, prov, want string }{
		{"", "", rp},
		{"", re, re},
		{"", rp, rp},
		{re, rp, re},
		{rp, re, rp},
		{re, "", re},
	}
	for _, c := range cases {
		l, p := mk(c.ldb, c.prov)
		if got := resolveOwnershipPolicy(l, p); got != c.want {
			t.Errorf("ldb=%q prov=%q: got %q want %q", c.ldb, c.prov, got, c.want)
		}
	}
	l, _ := mk("", "")
	if got := resolveOwnershipPolicy(l, nil); got != rp {
		t.Errorf("nil provider: %q", got)
	}
}

func TestOwnershipThrottle(t *testing.T) {
	var th ownershipThrottle
	uid := types.UID("u1")
	now := time.Now()
	if !th.due(uid, 1, "Report", now) {
		t.Fatal("first check must be due")
	}
	th.mark(uid, 1, "Report", now.Add(time.Hour))
	if th.due(uid, 1, "Report", now.Add(time.Minute)) {
		t.Error("before nextDue must not be due")
	}
	if !th.due(uid, 2, "Report", now.Add(time.Minute)) {
		t.Error("generation change must re-check")
	}
	if !th.due(uid, 1, "ReassignFromConnectionRole", now.Add(time.Minute)) {
		t.Error("policy change must re-check")
	}
	if !th.due(uid, 1, "Report", now.Add(2*time.Hour)) {
		t.Error("elapsed nextDue must re-check")
	}
	th.forget(uid)
	if !th.due(uid, 1, "Report", now.Add(time.Minute)) {
		t.Error("forgotten UID must be due")
	}
}

func TestOwnershipNextDue(t *testing.T) {
	now := time.Now()
	const interval = 30 * time.Minute
	cases := []struct {
		name   string
		res    pg.ReassignResult
		capped bool
		want   time.Duration
	}{
		{"clean pass", pg.ReassignResult{}, false, interval},
		{"lock timeout", pg.ReassignResult{StoppedOnLock: true, Failed: map[string]int{"table": 1}}, false, ownershipRetryDelay},
		{"budget exhausted", pg.ReassignResult{BudgetExhausted: true}, false, ownershipRetryDelay},
		{"non-lock failure", pg.ReassignResult{Failed: map[string]int{"view": 1}}, false, interval},
		{"candidate cap hit", pg.ReassignResult{}, true, ownershipRetryDelay},
		{"cap hit with non-lock failure backs off fully", pg.ReassignResult{Failed: map[string]int{"view": 1}}, true, interval},
	}
	for _, tc := range cases {
		if got := ownershipNextDue(now, interval, tc.res, tc.capped).Sub(now); got != tc.want {
			t.Errorf("%s: delay=%v want %v", tc.name, got, tc.want)
		}
	}
	if got := ownershipNextDue(now, time.Minute, pg.ReassignResult{StoppedOnLock: true}, false).Sub(now); got != time.Minute {
		t.Errorf("short retry must not exceed the interval, got %v", got)
	}
}

func TestForgetOwnershipMetrics_DropsCounters(t *testing.T) {
	count := func(c prometheus.Collector) int {
		ch := make(chan prometheus.Metric, 1024)
		c.Collect(ch)
		close(ch)
		return len(ch)
	}
	cols := map[string]prometheus.Collector{
		"checks": ownershipDriftChecks, "remediations": ownershipRemediations, "last_check": ownershipDriftLastCheck,
	}
	base := map[string]int{}
	for k, c := range cols {
		base[k] = count(c)
	}
	ownershipDriftChecks.WithLabelValues("ns-fg", "ldb-fg", "clean").Inc()
	ownershipRemediations.WithLabelValues("ns-fg", "ldb-fg", "table", "reassigned").Inc()
	ownershipDriftLastCheck.WithLabelValues("ns-fg", "ldb-fg").Set(1)
	for k, c := range cols {
		if count(c) != base[k]+1 {
			t.Fatalf("%s: series not created", k)
		}
	}
	forgetOwnershipMetrics("ns-fg", "ldb-fg")
	for k, c := range cols {
		if got := count(c); got != base[k] {
			t.Errorf("%s: %d series left after forget", k, got-base[k])
		}
	}
}

func TestOwnershipMetrics_SeriesResetWhenDriftGone(t *testing.T) {
	ldb := &keystonev1alpha1.LogicalDatabase{ObjectMeta: metav1.ObjectMeta{Namespace: "ns-mt", Name: "ldb-mt"}}
	ldb.Spec.Name = "db_mt"
	count := func() int {
		ch := make(chan prometheus.Metric, 1024)
		ownershipDriftObjects.Collect(ch)
		close(ch)
		return len(ch)
	}
	resetOwnershipSeries("ns-mt", "ldb-mt")
	base := count()
	publishOwnershipMetrics(ldb, sampleReport(), time.Now())
	// function/postgres (64, not remediable) + enum/keystone_admin (1, remediable) = 2 series.
	if got := count() - base; got != 2 {
		t.Fatalf("drift series=%d want 2", got)
	}
	publishOwnershipMetrics(ldb, pg.OwnershipReport{}, time.Now())
	if got := count() - base; got != 0 {
		t.Fatalf("series after drift gone=%d want 0", got)
	}
	publishOwnershipMetrics(ldb, sampleReport(), time.Now())
	forgetOwnershipMetrics("ns-mt", "ldb-mt")
	if got := count() - base; got != 0 {
		t.Fatalf("series after forget=%d want 0", got)
	}
}

func TestApplyOwnership_NoopAndSet(t *testing.T) {
	ldb := &keystonev1alpha1.LogicalDatabase{}
	ldb.Generation = 3
	applyOwnership(ldb, ownershipOutcome{})
	if len(ldb.Status.Conditions) != 0 {
		t.Fatal("zero outcome must not touch status")
	}
	applyOwnership(ldb, buildOwnershipOutcome(sampleReport(), "own", "Report", "", time.Now()))
	c := findCondition(ldb.Status.Conditions, keystonev1alpha1.ConditionTypeOwnershipDrift)
	if c == nil || c.Status != metav1.ConditionTrue || c.ObservedGeneration != 3 {
		t.Fatalf("condition %+v", c)
	}
	if ldb.Status.OwnershipDrift == nil || ldb.Status.OwnershipDrift.DriftedObjects != 65 {
		t.Fatalf("summary %+v", ldb.Status.OwnershipDrift)
	}
}

// -- envtest: CRD round-trip and status persistence -----------------------

// TestOwnership_CRDRoundTrip guards against a missed `make generate`: a field
// absent from the CRD schema is pruned by the apiserver.
func TestOwnership_CRDRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prov := &keystonev1alpha1.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "own-rt-provider"},
		Spec: keystonev1alpha1.DatabaseProviderSpec{
			Engine: keystonev1alpha1.DatabaseProviderEnginePostgreSQL, Host: "h",
			AdminCredentialsRef:  keystonev1alpha1.DatabaseProviderCredentials{SecretName: "s"},
			OwnershipRemediation: keystonev1alpha1.OwnershipRemediationReassignFromConnectionRole,
		},
	}
	if err := k8s.Create(ctx, prov); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	var gotP keystonev1alpha1.DatabaseProvider
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(prov), &gotP); err != nil {
		t.Fatal(err)
	}
	if gotP.Spec.OwnershipRemediation != keystonev1alpha1.OwnershipRemediationReassignFromConnectionRole {
		t.Errorf("provider field pruned: %q", gotP.Spec.OwnershipRemediation)
	}
	// Default applies when omitted.
	def := &keystonev1alpha1.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "own-rt-default"},
		Spec: keystonev1alpha1.DatabaseProviderSpec{
			Engine: keystonev1alpha1.DatabaseProviderEnginePostgreSQL, Host: "h",
			AdminCredentialsRef: keystonev1alpha1.DatabaseProviderCredentials{SecretName: "s"},
		},
	}
	if err := k8s.Create(ctx, def); err != nil {
		t.Fatal(err)
	}
	var gotD keystonev1alpha1.DatabaseProvider
	_ = k8s.Get(ctx, client.ObjectKeyFromObject(def), &gotD)
	if gotD.Spec.OwnershipRemediation != keystonev1alpha1.OwnershipRemediationReport {
		t.Errorf("provider default = %q, want Report", gotD.Spec.OwnershipRemediation)
	}

	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "own-rt-ldb"},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name: "own_rt", ClusterRef: "c", ProviderRef: "own-rt-provider", OwnerRole: "own_rt_owner",
			DeletionPolicy:       keystonev1alpha1.DeletionPolicyRetain,
			OwnershipRemediation: keystonev1alpha1.OwnershipRemediationReassignFromConnectionRole,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create ldb: %v", err)
	}
	var gotL keystonev1alpha1.LogicalDatabase
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(ldb), &gotL); err != nil {
		t.Fatal(err)
	}
	if gotL.Spec.OwnershipRemediation != keystonev1alpha1.OwnershipRemediationReassignFromConnectionRole {
		t.Errorf("ldb field pruned: %q", gotL.Spec.OwnershipRemediation)
	}

	// Invalid enum is rejected.
	bad := ldb.DeepCopy()
	bad.ResourceVersion = ""
	bad.Name = "own-rt-bad"
	bad.Spec.OwnershipRemediation = "Nuke"
	if err := k8s.Create(ctx, bad); err == nil {
		t.Error("invalid ownershipRemediation accepted")
	}

	// No default on the LogicalDatabase: omitted stays empty (inherit).
	inh := ldb.DeepCopy()
	inh.ResourceVersion = ""
	inh.Name = "own-rt-inherit"
	inh.Spec.OwnershipRemediation = ""
	if err := k8s.Create(ctx, inh); err != nil {
		t.Fatal(err)
	}
	var gotI keystonev1alpha1.LogicalDatabase
	_ = k8s.Get(ctx, client.ObjectKeyFromObject(inh), &gotI)
	if gotI.Spec.OwnershipRemediation != "" {
		t.Errorf("ldb must not default, got %q", gotI.Spec.OwnershipRemediation)
	}
}

// TestOwnership_ConditionPersistsThroughSuccess: the condition and summary
// survive success()'s merge patch, and Ready stays True while drift is True.
func TestOwnership_ConditionPersistsThroughSuccess(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "own-persist"},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name: "own_persist", ClusterRef: "c", ProviderRef: "p", OwnerRole: "own_persist_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatal(err)
	}
	r := &LogicalDatabaseReconciler{Client: k8s, Scheme: clientgoscheme.Scheme, Recorder: record.NewFakeRecorder(8), SystemNamespace: "keystone-system"}
	prov := &keystonev1alpha1.DatabaseProvider{Spec: keystonev1alpha1.DatabaseProviderSpec{Host: "h", Port: 5432}}

	outcome := buildOwnershipOutcome(sampleReport(), "own_persist_owner", "Report", "", time.Now())
	if _, err := r.success(ctx, ldb, prov, logr.Discard(), outcome); err != nil {
		t.Fatalf("success: %v", err)
	}
	var got keystonev1alpha1.LogicalDatabase
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(ldb), &got); err != nil {
		t.Fatal(err)
	}
	if !conditions.IsTrue(got.Status.Conditions, keystonev1alpha1.ConditionTypeReady) {
		t.Fatalf("Ready must stay True while OwnershipDrift=True: %+v", got.Status.Conditions)
	}
	c := findCondition(got.Status.Conditions, keystonev1alpha1.ConditionTypeOwnershipDrift)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != keystonev1alpha1.ReasonOwnershipDriftDetected {
		t.Fatalf("OwnershipDrift condition lost: %+v", c)
	}
	if got.Status.OwnershipDrift == nil || got.Status.OwnershipDrift.DriftedObjects != 65 ||
		len(got.Status.OwnershipDrift.Groups) != 2 {
		t.Fatalf("status.ownershipDrift lost: %+v", got.Status.OwnershipDrift)
	}
	if findCondition(got.Status.Conditions, keystonev1alpha1.ConditionTypeDegraded) != nil {
		t.Error("drift must not set Degraded")
	}

	// A later success with a not-run outcome keeps the previous condition.
	if _, err := r.success(ctx, &got, prov, logr.Discard(), ownershipOutcome{}); err != nil {
		t.Fatal(err)
	}
	var again keystonev1alpha1.LogicalDatabase
	_ = k8s.Get(ctx, client.ObjectKeyFromObject(ldb), &again)
	if findCondition(again.Status.Conditions, keystonev1alpha1.ConditionTypeOwnershipDrift) == nil {
		t.Error("throttled reconcile dropped the condition")
	}

	// NoManagedSchemas clears the summary but keeps the condition (False).
	clr := ownershipOutcome{Ran: true, Status: metav1.ConditionFalse,
		Reason: keystonev1alpha1.ReasonOwnershipNoManagedSchemas, Message: "none"}
	if _, err := r.success(ctx, &again, prov, logr.Discard(), clr); err != nil {
		t.Fatal(err)
	}
	var last keystonev1alpha1.LogicalDatabase
	_ = k8s.Get(ctx, client.ObjectKeyFromObject(ldb), &last)
	if last.Status.OwnershipDrift != nil {
		t.Errorf("summary not cleared: %+v", last.Status.OwnershipDrift)
	}
	if c := findCondition(last.Status.Conditions, keystonev1alpha1.ConditionTypeOwnershipDrift); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("condition %+v", c)
	}
}
