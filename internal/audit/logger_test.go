// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"context"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func newFakeClient(objs ...runtime.Object) client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&keystonev1alpha1.AuditLog{}, &keystonev1alpha1.AuditEntry{}).
		Build()
}

func TestLogger_AppendBootstrapsAuditLog(t *testing.T) {
	c := newFakeClient()
	l := NewLogger(c)
	l.ClockFn = func() time.Time { return time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC) }

	seq, _, err := l.Append(context.Background(), Event{
		Verb: "create",
		Actor: keystonev1alpha1.AuditActor{
			Username: "system:serviceaccount:keystone-system:manager",
		},
		ResourceRef: keystonev1alpha1.AuditResourceRef{
			APIVersion: "keystone.hexxlock.io/v1alpha1",
			Kind:       "MigrationBundle",
			Namespace:  "app-a",
			Name:       "mb-v1",
		},
		Reason:  "Reconciled",
		Outcome: "success",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq != 1 {
		t.Errorf("seq=%d, want 1 for first entry", seq)
	}

	// AuditLog singleton should exist with next=2, last=1.
	var alog keystonev1alpha1.AuditLog
	if err := c.Get(context.Background(), types.NamespacedName{Name: keystonev1alpha1.AuditLogName}, &alog); err != nil {
		t.Fatalf("get AuditLog: %v", err)
	}
	if alog.Status.NextSequence != 2 {
		t.Errorf("nextSequence=%d, want 2", alog.Status.NextSequence)
	}
	if alog.Status.LastSequence != 1 {
		t.Errorf("lastSequence=%d, want 1", alog.Status.LastSequence)
	}
	if alog.Status.LastEntryHash == "" {
		t.Errorf("lastEntryHash empty after first Append")
	}
}

func TestLogger_AppendChainsEntries(t *testing.T) {
	c := newFakeClient()
	l := NewLogger(c)
	baseTime := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

	hashes := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		l.ClockFn = func() time.Time { return baseTime.Add(time.Duration(i) * time.Second) }
		_, h, err := l.Append(context.Background(), Event{
			Verb: "reconcile",
			Actor: keystonev1alpha1.AuditActor{
				Username: "system:serviceaccount:keystone-system:manager",
			},
			ResourceRef: keystonev1alpha1.AuditResourceRef{
				APIVersion: "keystone.hexxlock.io/v1alpha1",
				Kind:       "MigrationBundle",
				Name:       "mb",
			},
			Reason: "step-n",
		})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		hashes = append(hashes, h)
	}

	// Read all entries back and verify the chain.
	var list keystonev1alpha1.AuditEntryList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if len(list.Items) != 3 {
		t.Fatalf("entries=%d, want 3", len(list.Items))
	}

	if _, err := VerifyChain(list.Items); err != nil {
		t.Errorf("VerifyChain returned error on freshly-built chain: %v", err)
	}

	// Explicitly: entry 2's PrevHash must equal entry 1's SelfHash.
	// (buildChain in the test may shuffle order; VerifyChain sorts.)
	var e1, e2 *keystonev1alpha1.AuditEntry
	for i := range list.Items {
		switch list.Items[i].Spec.Sequence {
		case 1:
			e1 = &list.Items[i]
		case 2:
			e2 = &list.Items[i]
		}
	}
	if e2.Spec.PrevHash != e1.Spec.SelfHash {
		t.Errorf("chain link broken: entry2.prevHash=%q, entry1.selfHash=%q",
			e2.Spec.PrevHash, e1.Spec.SelfHash)
	}
	if e1.Spec.PrevHash != GenesisHash {
		t.Errorf("entry1.prevHash=%q, want genesis", e1.Spec.PrevHash)
	}
}

func TestLogger_Append_RejectsMissingVerb(t *testing.T) {
	c := newFakeClient()
	l := NewLogger(c)
	_, _, err := l.Append(context.Background(), Event{
		Actor: keystonev1alpha1.AuditActor{Username: "u"},
		ResourceRef: keystonev1alpha1.AuditResourceRef{
			APIVersion: "v1", Kind: "Pod", Name: "x",
		},
	})
	if err == nil {
		t.Errorf("expected error for missing Verb")
	}
}

func TestLogger_Append_DefaultsOutcomeToSuccess(t *testing.T) {
	c := newFakeClient()
	l := NewLogger(c)
	seq, _, err := l.Append(context.Background(), Event{
		Verb:  "reconcile",
		Actor: keystonev1alpha1.AuditActor{Username: "u"},
		ResourceRef: keystonev1alpha1.AuditResourceRef{
			APIVersion: "v1", Kind: "Pod", Name: "x",
		},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var entry keystonev1alpha1.AuditEntry
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: fmtEntryName(seq),
	}, &entry); err != nil {
		t.Fatalf("get entry: %v", err)
	}
	if entry.Spec.Outcome != "success" {
		t.Errorf("outcome=%q, want default success", entry.Spec.Outcome)
	}
}

func TestActorFromServiceAccount(t *testing.T) {
	a := ActorFromServiceAccount("keystone-system", "manager")
	want := "system:serviceaccount:keystone-system:manager"
	if a.Username != want {
		t.Errorf("username=%q, want %q", a.Username, want)
	}
	if len(a.Groups) != 2 {
		t.Errorf("groups=%v, want 2 entries", a.Groups)
	}
}

// fmtEntryName mirrors the format used by Logger.Append.
func fmtEntryName(seq int64) string {
	return "entry-" + pad20(seq)
}

// pad20 returns n as a 20-digit zero-padded decimal string. Mirrors
// the fmt.Sprintf("%020d", …) in Logger.Append without importing fmt
// in the test.
func pad20(n int64) string {
	// Simple implementation — tests don't need performance.
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	for len(s) < 20 {
		s = "0" + s
	}
	return s
}

// unused placeholder to keep metav1 import grounded for future tests.
var _ = metav1.Now

// TestLogger_Append_RecoversFromStaleAuditLogStatus reproduces the
// 2026-04-21 etcd 409-retry storm. Setup: AuditLog.status.nextSequence
// says 5 but an AuditEntry at sequence=5 already exists (simulating
// a prior status-patch-conflict path that succeeded without bumping
// the counter). Before the fix: Append looped 5× on "entry-...-005"
// AlreadyExists and returned error. After the fix: Append re-scans
// max(spec.sequence), picks 6, and succeeds on the second attempt.
func TestLogger_Append_RecoversFromStaleAuditLogStatus(t *testing.T) {
	// Seed: 4 existing entries + AuditLog with truthful status.
	// Then manually stamp an entry at sequence=5 but LEAVE status.nextSequence
	// at 5 to mimic the bug.
	objs := []runtime.Object{
		&keystonev1alpha1.AuditLog{
			ObjectMeta: metav1.ObjectMeta{Name: keystonev1alpha1.AuditLogName},
			Status: keystonev1alpha1.AuditLogStatus{
				NextSequence:  5,
				LastSequence:  4,
				LastEntryHash: "stub-hash-4",
			},
		},
		&keystonev1alpha1.AuditEntry{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("entry-%020d", 5)},
			Spec: keystonev1alpha1.AuditEntrySpec{
				Sequence:    5,
				Actor:       keystonev1alpha1.AuditActor{Username: "u"},
				Verb:        "create",
				ResourceRef: keystonev1alpha1.AuditResourceRef{APIVersion: "v1", Kind: "Pod", Name: "x"},
				PrevHash:    "stub-hash-4",
				SelfHash:    "stub-hash-5",
			},
		},
	}
	c := newFakeClient(objs...)
	l := NewLogger(c)
	l.MaxRetries = 3 // keep the test fast — with the fix, one retry is enough

	seq, _, err := l.Append(context.Background(), Event{
		Verb:  "reconcile",
		Actor: keystonev1alpha1.AuditActor{Username: "system:serviceaccount:keystone-system:manager"},
		ResourceRef: keystonev1alpha1.AuditResourceRef{
			APIVersion: "keystone.hexxlock.io/v1alpha1",
			Kind:       "MigrationBundle",
			Name:       "mb",
		},
		Reason: "step-after-stale-status",
	})
	if err != nil {
		t.Fatalf("Append should recover from stale AuditLog.status, got: %v", err)
	}
	if seq != 6 {
		t.Errorf("seq=%d, want 6 (max existing sequence + 1)", seq)
	}

	// Verify the new entry was actually persisted at entry-...-006, not at 5.
	var list keystonev1alpha1.AuditEntryList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list entries: %v", err)
	}
	var saw5, saw6 bool
	for i := range list.Items {
		switch list.Items[i].Spec.Sequence {
		case 5:
			saw5 = true
		case 6:
			saw6 = true
		}
	}
	if !saw5 {
		t.Errorf("pre-existing entry 5 disappeared — fix must not mutate others")
	}
	if !saw6 {
		t.Errorf("new entry at seq=6 not persisted")
	}
}

// TestLogger_SleepBackoff_HonoursContext ensures the new exponential
// backoff doesn't block manager shutdown.
func TestLogger_SleepBackoff_HonoursContext(t *testing.T) {
	l := NewLogger(newFakeClient())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel
	start := time.Now()
	err := l.sleepBackoff(ctx, 10) // would be ~5s without cancel
	if err == nil {
		t.Fatalf("expected ctx error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("sleepBackoff blocked %s after ctx cancel, want immediate", elapsed)
	}
}
