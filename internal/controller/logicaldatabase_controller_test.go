// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// TestLogicalDatabaseReconciler_AddsFinalizer verifies the first
// reconcile of a new LogicalDatabase adds the keystone finalizer so
// subsequent deletions can run cleanup before the resource is purged.
func TestLogicalDatabaseReconciler_AddsFinalizer(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "tdb-finalizer",
		},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           "tdb_fin",
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "missing-provider",
			OwnerRole:      "tdb_fin_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create: %v", err)
	}

	// First reconcile — expected to add finalizer + requeue.
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ldb.Namespace, Name: ldb.Name}}
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !res.Requeue {
		t.Errorf("expected Requeue=true after finalizer add; got %+v", res)
	}

	var after keystonev1alpha1.LogicalDatabase
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get after first reconcile: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&after, keystonev1alpha1.FinalizerLogicalDatabase) {
		t.Errorf("expected finalizer %q to be present; finalizers=%v",
			keystonev1alpha1.FinalizerLogicalDatabase, after.Finalizers)
	}
}

// TestLogicalDatabaseReconciler_ProviderNotFound verifies that a
// LogicalDatabase referencing a non-existent DatabaseProvider is marked
// Ready=False with reason=ProviderNotFound and surfaces a warning event.
func TestLogicalDatabaseReconciler_ProviderNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	events := record.NewFakeRecorder(16)
	r := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        events,
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "tdb-missing-provider",
		},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           "tdb_missing",
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "nonexistent-provider",
			OwnerRole:      "tdb_missing_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ldb.Namespace, Name: ldb.Name}}

	// First reconcile adds finalizer.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	// Second reconcile attempts provider lookup → fails with ProviderNotFound.
	_, err := r.Reconcile(ctx, req)
	if err == nil {
		t.Fatalf("expected error from missing provider; got nil")
	}

	var after keystonev1alpha1.LogicalDatabase
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond == nil {
		t.Fatalf("Ready condition not set on failure")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready=%s, want False", cond.Status)
	}
	if cond.Reason != "ProviderNotFound" {
		t.Errorf("Reason=%q, want ProviderNotFound", cond.Reason)
	}
	if after.Status.ObservedGeneration != after.Generation {
		t.Errorf("observedGeneration=%d, want %d", after.Status.ObservedGeneration, after.Generation)
	}

	// FakeRecorder surfaces events on a channel; one of them should
	// carry the ProviderNotFound reason.
	select {
	case evt := <-events.Events:
		if !containsSubstring(evt, "ProviderNotFound") {
			t.Errorf("expected ProviderNotFound event; got %q", evt)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("no event emitted within 2s")
	}
}

// TestLogicalDatabaseReconciler_CredentialsMissing verifies that a
// LogicalDatabase → Provider chain with no admin credentials Secret
// fails at the CredentialsResolutionFailed step.
func TestLogicalDatabaseReconciler_CredentialsMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	events := record.NewFakeRecorder(16)
	r := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        events,
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Provider exists but admin credentials Secret does not.
	provider := &keystonev1alpha1.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "prov-no-secret"},
		Spec: keystonev1alpha1.DatabaseProviderSpec{
			Host:                "pg.invalid",
			Port:                5432,
			MaintenanceDatabase: "postgres",
			Engine:              keystonev1alpha1.DatabaseProviderEnginePostgreSQL,
			AdminCredentialsRef: keystonev1alpha1.DatabaseProviderCredentials{
				SecretName:  "missing-admin-secret",
				UsernameKey: "username",
				PasswordKey: "password",
			},
		},
	}
	if err := k8s.Create(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "tdb-no-creds",
		},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           "tdb_nocreds",
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "prov-no-secret",
			OwnerRole:      "tdb_nocreds_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create ldb: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ldb.Namespace, Name: ldb.Name}}

	// Finalizer add.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	// Provider lookup succeeds; credentials Secret missing → fail.
	_, err := r.Reconcile(ctx, req)
	if err == nil {
		t.Fatalf("expected credentials resolution error; got nil")
	}

	var after keystonev1alpha1.LogicalDatabase
	if err := k8s.Get(ctx, req.NamespacedName, &after); err != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond == nil {
		t.Fatalf("Ready condition not set on failure")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready=%s, want False", cond.Status)
	}
	if cond.Reason != "CredentialsResolutionFailed" {
		t.Errorf("Reason=%q, want CredentialsResolutionFailed", cond.Reason)
	}
}

// TestLogicalDatabaseReconciler_DeletionWithRetainPolicy verifies that
// deleting a LogicalDatabase with Retain policy removes the finalizer
// even when the provider is unreachable — Retain must never block.
func TestLogicalDatabaseReconciler_DeletionWithRetainPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	r := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      "tdb-delete-retain",
		},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           "tdb_del",
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "irrelevant",
			OwnerRole:      "tdb_del_owner",
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create: %v", err)
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ldb.Namespace, Name: ldb.Name}}
	// Add finalizer.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	// Delete the CR — finalizer blocks actual removal until reconciler clears it.
	var fetched keystonev1alpha1.LogicalDatabase
	if err := k8s.Get(ctx, req.NamespacedName, &fetched); err != nil {
		t.Fatalf("get before delete: %v", err)
	}
	if err := k8s.Delete(ctx, &fetched); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Reconcile during deletion — Retain policy skips DropDatabase entirely,
	// so provider absence doesn't matter.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile during deletion: %v", err)
	}

	// After the finalizer is removed, the CR is purged from the apiserver.
	if err := k8s.Get(ctx, req.NamespacedName, &fetched); !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound after finalizer cleanup; got %v", err)
	}
}

// -- Test helpers ------------------------------------------------------

// findCondition scans a condition slice for one matching type.
func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

// containsSubstring reports whether the haystack contains the needle.
// Used to match FakeRecorder event strings which are formatted as
// "<type> <reason> <message>".
func containsSubstring(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}())
}

// TestLogicalDatabasesForProvider_FanoutMapping — the DatabaseProvider
// watch map function returns exactly the LogicalDatabases whose
// spec.providerRef names the changed provider, so a provider spec
// repoint enqueues dependents immediately instead of leaving them in
// error-backoff against the old endpoint.
func TestLogicalDatabasesForProvider_FanoutMapping(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	provA := makeProvider("prov-fanout-a", "fanout-secret")
	provB := makeProvider("prov-fanout-b", "fanout-secret")
	for _, p := range []*keystonev1alpha1.DatabaseProvider{provA, provB} {
		if err := k8s.Create(ctx, p); err != nil {
			t.Fatalf("create provider %s: %v", p.Name, err)
		}
	}
	makeLDB := func(name, providerRef string) *keystonev1alpha1.LogicalDatabase {
		return &keystonev1alpha1.LogicalDatabase{
			ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: name},
			Spec: keystonev1alpha1.LogicalDatabaseSpec{
				Name:           "fanout_db",
				ClusterRef:     "prod-eu-west-1-hub",
				ProviderRef:    providerRef,
				OwnerRole:      "fanout_owner",
				DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
			},
		}
	}
	for _, ldb := range []*keystonev1alpha1.LogicalDatabase{
		makeLDB("fanout-ldb-a", "prov-fanout-a"),
		makeLDB("fanout-ldb-b", "prov-fanout-b"),
	} {
		if err := k8s.Create(ctx, ldb); err != nil {
			t.Fatalf("create ldb %s: %v", ldb.Name, err)
		}
	}

	r := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(16),
		Pools:           pg.NewPoolCache(),
		SystemNamespace: "keystone-system",
	}
	reqs := r.logicalDatabasesForProvider(ctx, provA)
	if len(reqs) != 1 {
		t.Fatalf("expected exactly 1 request for prov-fanout-a; got %d (%v)", len(reqs), reqs)
	}
	if reqs[0].Namespace != "keystone-system" || reqs[0].Name != "fanout-ldb-a" {
		t.Errorf("mapped to %s/%s; want keystone-system/fanout-ldb-a",
			reqs[0].Namespace, reqs[0].Name)
	}
}

// Unused imports silencer — keeps the Secret / corev1 symbol referenced
// for future tests that provision real admin credentials.
var _ = corev1.Secret{}
var _ = client.IgnoreNotFound

// TestHashPasswordSHA256 — passwords hash deterministically to lowercase
// hex sha256; empty input yields empty output (signalling "no password
// applied"). Used by the rotation logic to detect Secret-content changes
// without storing plaintext in status.
func TestHashPasswordSHA256(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"hexxlock-test-pw-1", "" /* asserted as non-empty 64-hex */},
	}
	for i, c := range cases {
		got := hashPasswordSHA256(c.in)
		if c.in == "" && got != "" {
			t.Errorf("case %d: empty input must yield empty hash, got %q", i, got)
			continue
		}
		if c.want != "" && got != c.want {
			t.Errorf("case %d: hashPasswordSHA256(%q) = %q, want %q", i, c.in, got, c.want)
		}
		if c.want == "" && c.in != "" && (len(got) != 64 || !isHex(got)) {
			t.Errorf("case %d: hashPasswordSHA256(%q) = %q, want 64-hex string", i, c.in, got)
		}
	}
}

// TestHashPasswordSHA256_DeterministicAcrossCalls — the rotation
// detector relies on the hash being stable between reconciles.
func TestHashPasswordSHA256_DeterministicAcrossCalls(t *testing.T) {
	in := "rotation-test-pw"
	a := hashPasswordSHA256(in)
	b := hashPasswordSHA256(in)
	if a != b {
		t.Fatalf("hashPasswordSHA256 not deterministic: %q != %q", a, b)
	}
}

// TestHashPasswordSHA256_DistinguishesContent — different passwords
// yield different hashes (else rotation would silently no-op on
// password change).
func TestHashPasswordSHA256_DistinguishesContent(t *testing.T) {
	if a, b := hashPasswordSHA256("aaa"), hashPasswordSHA256("aab"); a == b {
		t.Fatalf("different inputs hashed identically: %q", a)
	}
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
