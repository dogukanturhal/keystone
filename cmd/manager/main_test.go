// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// TestPprofHandlers asserts the registered handler set matches the
// stable net/http/pprof surface and that each handler responds without
// panicking. The metrics server's mux longest-prefix-matches paths, so
// dropping any of the four named handlers (cmdline/profile/symbol/trace)
// would silently route them through pprof.Index instead — which only
// works for runtime-profile names, not the named handlers.
//
// We don't actually exercise /debug/pprof/profile or /debug/pprof/trace
// because both engage active CPU/trace sampling and would hold the test
// open for the default sample window. A 404 / 200 distinction on Index
// is enough to prove the dispatch table is wired.
func TestPprofHandlers(t *testing.T) {
	handlers := pprofHandlers()

	wantPaths := []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/profile",
		"/debug/pprof/symbol",
		"/debug/pprof/trace",
	}
	for _, p := range wantPaths {
		if _, ok := handlers[p]; !ok {
			t.Errorf("missing handler for %s", p)
		}
	}
	if len(handlers) != len(wantPaths) {
		t.Errorf("unexpected handler count: got %d want %d", len(handlers), len(wantPaths))
	}

	// Index dispatches /debug/pprof/heap to the heap handler. Build a
	// mux mirroring the metrics-server registration (longest-prefix
	// match on / suffix) and prove a heap pull returns a pprof body.
	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.Handle(path, h)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap?debug=1", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/debug/pprof/heap status: got %d want 200", rec.Code)
	}
	// debug=1 returns a text dump that always contains the heap-profile
	// header. Stable across Go versions.
	if !strings.Contains(rec.Body.String(), "heap profile") {
		t.Errorf("/debug/pprof/heap?debug=1 body missing 'heap profile' marker; got %q",
			rec.Body.String()[:min(120, rec.Body.Len())])
	}

	// /debug/pprof/cmdline returns the binary's argv joined with NULs.
	// We don't care about the exact value — just that the dedicated
	// handler is wired, not falling through Index (which 404s on
	// unknown profile names).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/debug/pprof/cmdline status: got %d want 200", rec.Code)
	}
}

// TestRunRefusesUnauthenticatedPprof guards the secure-metrics +
// pprof-enabled fail-closed. Without this guard, --secure-metrics=false
// (no FilterProvider wrapping the metrics-server mux) combined with
// --enable-pprof=true would publish every pprof endpoint plaintext on
// the bind address — runtime function names, ASLR offsets, CPU samples,
// argv. The fail-closed check fires before the manager constructs the
// metrics server.
//
// We exercise the guard via run() rather than parseFlags() because the
// guard sits at run() entry: the binary should refuse to start, not
// merely warn during flag parsing. run() short-circuits on the guard
// before touching the kubeconfig, so this test is hermetic — no
// envtest required.
func TestRunRefusesUnauthenticatedPprof(t *testing.T) {
	o := &options{
		secureMetrics: false,
		enablePprof:   true,
		// All other fields stay zero-value; the guard fires before
		// they're consulted.
	}
	logger := zap.NewNop()
	err := run(o, logger)
	if err == nil {
		t.Fatal("run() should refuse --enable-pprof=true with --secure-metrics=false, but returned nil")
	}
	if !strings.Contains(err.Error(), "secure-metrics") || !strings.Contains(err.Error(), "pprof") {
		t.Errorf("run() error should mention both flags so the operator can correct; got %q", err.Error())
	}
}

// TestCacheManagedConfigMapsSelector pins the cache scope contract:
// every ConfigMap the SchemaDefinitionReconciler emits must match the
// selector, and no incidental cluster ConfigMap should match. The
// selector is the load-bearing piece of the per-type cache scope —
// without it the cache holds every CM in the cluster.
func TestCacheManagedConfigMapsSelector(t *testing.T) {
	sel := cacheManagedConfigMapsSelector()

	// Stamp on every emitted CM (see internal/controller/
	// schemadefinition_controller.go::emitBundle). Must match.
	emitted := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example-service-public-desired-sql-9d589396eb44",
			Namespace: "keystone-system",
			Labels: map[string]string{
				keystonev1alpha1.LabelSchemaDefinition: "example-service-public-desired",
				keystonev1alpha1.LabelSchemaRef:        "example-service-public",
				keystonev1alpha1.LabelSource:           "declarative-diff",
			},
		},
	}
	if !sel.Matches(labelSetOf(emitted)) {
		t.Errorf("emitted CM should match the cache selector, but didn't: labels=%v", emitted.Labels)
	}

	// Foreign CMs commonly seen in the cluster (kube-root-ca.crt,
	// Argo Application config, app-specific CMs) MUST NOT match.
	unrelated := []*corev1.ConfigMap{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kube-root-ca.crt",
				Namespace: "default",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "argocd-cm",
				Namespace: "argocd",
				Labels: map[string]string{
					"app.kubernetes.io/name":    "argocd-cm",
					"app.kubernetes.io/part-of": "argocd",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "user-authored-bundle-sql",
				Namespace: "example-service",
				Labels: map[string]string{
					// User-authored ConfigMap-sourced bundle: legitimate,
					// must NOT be in the cache (resolver reads via
					// APIReader instead). See the resolver call sites.
					"app": "example-service",
				},
			},
		},
	}
	for _, cm := range unrelated {
		if sel.Matches(labelSetOf(cm)) {
			t.Errorf("unrelated CM %s/%s should NOT match cache selector, but did: labels=%v",
				cm.Namespace, cm.Name, cm.Labels)
		}
	}

	// A CM that carries the label as a key but with empty string
	// value also matches — `selection.Exists` is value-agnostic.
	// Stays consistent with the SD reconciler's emit path which
	// always sets a non-empty value, but documents the contract.
	emptyValue := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				keystonev1alpha1.LabelSchemaDefinition: "",
			},
		},
	}
	if !sel.Matches(labelSetOf(emptyValue)) {
		t.Errorf("CM with empty-value LabelSchemaDefinition should still match (Exists predicate)")
	}
}

func labelSetOf(cm *corev1.ConfigMap) labels.Set {
	return labels.Set(cm.Labels)
}

// TestRunRefusesWriteCRFalseWithoutNATS guards the T2 #25 Phase 5a
// fail-fast: WriteCR=false without AUDIT_NATS_URL would silently drop
// every audit event (no durable sink). run() must refuse to start and
// return an error explaining the combination is invalid.
//
// Like TestRunRefusesUnauthenticatedPprof, this exercises run() directly
// so the guard fires before any kubeconfig interaction — test is hermetic.
func TestRunRefusesWriteCRFalseWithoutNATS(t *testing.T) {
	o := &options{
		secureMetrics: true,
		startManager:  false, // guard fires before manager start
		auditWriteCR:  false,
		auditNATSURL:  "", // no NATS sink
	}
	logger := zap.NewNop()
	err := run(o, logger)
	if err == nil {
		t.Fatal("run() should refuse WriteCR=false without AUDIT_NATS_URL, but returned nil")
	}
	wantSubstrings := []string{"AUDIT_WRITE_CR", "AUDIT_NATS_URL"}
	for _, sub := range wantSubstrings {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("run() error should mention %q so the operator can correct; got %q", sub, err.Error())
		}
	}
}
