// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"context"
	"testing"
	"time"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// writeCRCaptureExporter records specs passed to Export for WriteCR-path assertions.
// Named to avoid collision with the captureExporter in exporter_test.go (same package).
type writeCRCaptureExporter struct {
	calls []keystonev1alpha1.AuditEntrySpec
}

func (c *writeCRCaptureExporter) Export(spec keystonev1alpha1.AuditEntrySpec) error {
	c.calls = append(c.calls, spec)
	return nil
}

// baseEvent returns a minimal valid Event for tests.
func baseEvent() Event {
	return Event{
		Verb:  "create",
		Actor: keystonev1alpha1.AuditActor{Username: "system:serviceaccount:keystone-system:manager"},
		ResourceRef: keystonev1alpha1.AuditResourceRef{
			APIVersion: "keystone.hexxlock.io/v1alpha1",
			Kind:       "MigrationBundle",
			Name:       "mb-phase5a",
		},
		Outcome: "success",
		Reason:  "phase5a-test",
	}
}

// TestLogger_WriteCR_False_NoCRCreated verifies that when WriteCR=false,
// Append does not create an AuditEntry CR or an AuditLog singleton.
// The fake client starts empty; the test asserts it is still empty after
// Append returns, proving no etcd writes occurred.
func TestLogger_WriteCR_False_NoCRCreated(t *testing.T) {
	c := newFakeClient() // empty cluster
	exp := &writeCRCaptureExporter{}
	fixedTime := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)

	l := &Logger{
		Client:  c,
		WriteCR: false,
		ClockFn: func() time.Time { return fixedTime },
		Exporter: exp,
	}

	seq, hash, err := l.Append(context.Background(), baseEvent())
	if err != nil {
		t.Fatalf("Append(WriteCR=false): unexpected error: %v", err)
	}

	// Sequence must be 0 — no etcd counter was allocated.
	if seq != 0 {
		t.Errorf("seq=%d, want 0 when WriteCR=false", seq)
	}

	// Hash must be a non-empty SHA-256 hex string (64 chars).
	if len(hash) != 64 {
		t.Errorf("hash=%q, want 64-char sha256 hex", hash)
	}

	// No AuditLog singleton should have been created.
	var alogList keystonev1alpha1.AuditLogList
	if err := c.List(context.Background(), &alogList); err != nil {
		t.Fatalf("list AuditLog: %v", err)
	}
	if len(alogList.Items) != 0 {
		t.Errorf("AuditLog created when WriteCR=false, want none")
	}

	// No AuditEntry CRs should have been created.
	var entryList keystonev1alpha1.AuditEntryList
	if err := c.List(context.Background(), &entryList); err != nil {
		t.Fatalf("list AuditEntry: %v", err)
	}
	if len(entryList.Items) != 0 {
		t.Errorf("AuditEntry CRs created when WriteCR=false, want none: got %d", len(entryList.Items))
	}
}

// TestLogger_WriteCR_False_ExporterCalledOnce verifies that when WriteCR=false,
// the Exporter is called exactly once with a spec that has a valid SelfHash,
// Sequence=0, and PrevHash="".
func TestLogger_WriteCR_False_ExporterCalledOnce(t *testing.T) {
	c := newFakeClient()
	exp := &writeCRCaptureExporter{}
	fixedTime := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)

	l := &Logger{
		Client:  c,
		WriteCR: false,
		ClockFn: func() time.Time { return fixedTime },
		Exporter: exp,
	}

	_, hash, err := l.Append(context.Background(), baseEvent())
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	if len(exp.calls) != 1 {
		t.Fatalf("exporter called %d times, want 1", len(exp.calls))
	}

	got := exp.calls[0]

	// Sequence must be zero — chain head lives in MinIO.
	if got.Sequence != 0 {
		t.Errorf("spec.Sequence=%d, want 0 (chain-head in MinIO per Phase 5a)", got.Sequence)
	}

	// PrevHash must be empty — no etcd-side chain pointer.
	if got.PrevHash != "" {
		t.Errorf("spec.PrevHash=%q, want empty when WriteCR=false", got.PrevHash)
	}

	// SelfHash must be consistent: the returned hash must equal spec.SelfHash,
	// and recomputing from the spec must yield the same value.
	if got.SelfHash != hash {
		t.Errorf("spec.SelfHash=%q != returned hash=%q", got.SelfHash, hash)
	}
	recomputed := ComputeSelfHash(got)
	if got.SelfHash != recomputed {
		t.Errorf("SelfHash mismatch: stored=%q, recomputed=%q — hash is not self-consistent", got.SelfHash, recomputed)
	}

	// Event fields must be forwarded faithfully.
	ev := baseEvent()
	if got.Verb != ev.Verb {
		t.Errorf("spec.Verb=%q, want %q", got.Verb, ev.Verb)
	}
	if got.Actor.Username != ev.Actor.Username {
		t.Errorf("spec.Actor.Username=%q, want %q", got.Actor.Username, ev.Actor.Username)
	}
	if got.ResourceRef.Name != ev.ResourceRef.Name {
		t.Errorf("spec.ResourceRef.Name=%q, want %q", got.ResourceRef.Name, ev.ResourceRef.Name)
	}
	if got.Outcome != ev.Outcome {
		t.Errorf("spec.Outcome=%q, want %q", got.Outcome, ev.Outcome)
	}
}

// TestLogger_WriteCR_False_ValidationStillEnforced checks that the input
// validation (required Verb, Actor, ResourceRef) still applies when
// WriteCR=false. The logger must not silently drop bad inputs.
func TestLogger_WriteCR_False_ValidationStillEnforced(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
	}{
		{
			name: "missing verb",
			ev: Event{
				Actor:       keystonev1alpha1.AuditActor{Username: "u"},
				ResourceRef: keystonev1alpha1.AuditResourceRef{Kind: "Pod", Name: "x"},
			},
		},
		{
			name: "missing actor username",
			ev: Event{
				Verb:        "create",
				ResourceRef: keystonev1alpha1.AuditResourceRef{Kind: "Pod", Name: "x"},
			},
		},
		{
			name: "missing resourceref name",
			ev: Event{
				Verb:  "create",
				Actor: keystonev1alpha1.AuditActor{Username: "u"},
				ResourceRef: keystonev1alpha1.AuditResourceRef{Kind: "Pod"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := &Logger{
				Client:   newFakeClient(),
				WriteCR:  false,
				ClockFn:  time.Now,
				Exporter: &writeCRCaptureExporter{},
			}
			_, _, err := l.Append(context.Background(), tc.ev)
			if err == nil {
				t.Errorf("expected validation error for %q, got nil", tc.name)
			}
		})
	}
}

// TestLogger_WriteCR_True_UnchangedBehavior checks that the existing CR-write
// path is not regressed by the WriteCR toggle. With WriteCR=true (the default),
// Append must still create an AuditLog singleton and an AuditEntry CR.
func TestLogger_WriteCR_True_UnchangedBehavior(t *testing.T) {
	c := newFakeClient()
	l := NewLogger(c) // WriteCR=true by default

	if !l.WriteCR {
		t.Fatal("NewLogger should default WriteCR=true")
	}

	seq, hash, err := l.Append(context.Background(), baseEvent())
	if err != nil {
		t.Fatalf("Append(WriteCR=true): %v", err)
	}
	if seq != 1 {
		t.Errorf("seq=%d, want 1 for first CR-path entry", seq)
	}
	if len(hash) != 64 {
		t.Errorf("hash=%q, want 64-char sha256 hex", hash)
	}

	// AuditLog singleton must exist.
	var alogList keystonev1alpha1.AuditLogList
	if err := c.List(context.Background(), &alogList); err != nil {
		t.Fatalf("list AuditLog: %v", err)
	}
	if len(alogList.Items) != 1 {
		t.Errorf("want 1 AuditLog, got %d", len(alogList.Items))
	}

	// AuditEntry CR must exist.
	var entryList keystonev1alpha1.AuditEntryList
	if err := c.List(context.Background(), &entryList); err != nil {
		t.Fatalf("list AuditEntry: %v", err)
	}
	if len(entryList.Items) != 1 {
		t.Errorf("want 1 AuditEntry CR, got %d", len(entryList.Items))
	}
}
