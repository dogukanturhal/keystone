// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func specFixture() keystonev1alpha1.AuditEntrySpec {
	return keystonev1alpha1.AuditEntrySpec{
		Sequence:  42,
		Timestamp: metav1.NewTime(time.Date(2026, 4, 19, 20, 12, 34, 567_000_000, time.UTC)),
		Actor: keystonev1alpha1.AuditActor{
			Username: "system:serviceaccount:keystone-system:keystone-manager",
			UID:      "sa-uid-1",
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:keystone-system"},
		},
		Verb: "reconcile",
		ResourceRef: keystonev1alpha1.AuditResourceRef{
			APIVersion: "keystone.hexxlock.io/v1alpha1",
			Kind:       "MigrationBundle",
			Namespace:  "keystone-system",
			Name:       "users-v1",
			UID:        "bundle-uid-1",
		},
		Reason:   "applied version v1",
		Outcome:  "success",
		PrevHash: "genesis",
		SelfHash: "aabbccdd",
	}
}

// TestStdoutJSONExporter_Shape — verify the emitted JSON matches the
// ECS-inspired schema documented in exporter.go. Field-level asserts
// so a regression doesn't silently drift the on-wire format.
func TestStdoutJSONExporter_Shape(t *testing.T) {
	var buf bytes.Buffer
	exporter := NewWriterExporter(&buf)
	if err := exporter.Export(specFixture()); err != nil {
		t.Fatalf("Export: %v", err)
	}

	raw := buf.Bytes()
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Errorf("expected newline terminator; got %q", raw)
	}
	if n := bytes.Count(raw, []byte("\n")); n != 1 {
		t.Errorf("expected exactly 1 newline; got %d", n)
	}

	var decoded ecsRecord
	if err := json.Unmarshal(bytes.TrimRight(raw, "\n"), &decoded); err != nil {
		t.Fatalf("unmarshal: %v (raw=%q)", err, raw)
	}

	if decoded.Timestamp != "2026-04-19T20:12:34.567Z" {
		t.Errorf("@timestamp=%q, want RFC3339Nano UTC", decoded.Timestamp)
	}
	if decoded.Event.Kind != "event" {
		t.Errorf("event.kind=%q, want event", decoded.Event.Kind)
	}
	if decoded.Event.Dataset != "keystone.audit" {
		t.Errorf("event.dataset=%q, want keystone.audit", decoded.Event.Dataset)
	}
	if len(decoded.Event.Category) != 1 || decoded.Event.Category[0] != "database" {
		t.Errorf("event.category=%v, want [database]", decoded.Event.Category)
	}
	if decoded.Event.Action != "reconcile" {
		t.Errorf("event.action=%q, want reconcile", decoded.Event.Action)
	}
	if decoded.Event.Outcome != "success" {
		t.Errorf("event.outcome=%q, want success", decoded.Event.Outcome)
	}
	if decoded.Event.Sequence != 42 {
		t.Errorf("event.sequence=%d, want 42", decoded.Event.Sequence)
	}

	if decoded.User.Name != "system:serviceaccount:keystone-system:keystone-manager" {
		t.Errorf("user.name=%q", decoded.User.Name)
	}
	if decoded.User.ID != "sa-uid-1" {
		t.Errorf("user.id=%q, want sa-uid-1", decoded.User.ID)
	}
	if len(decoded.User.Groups) != 2 {
		t.Errorf("user.groups=%v, want 2 entries", decoded.User.Groups)
	}

	if decoded.Keystone.Verb != "reconcile" {
		t.Errorf("keystone.verb=%q", decoded.Keystone.Verb)
	}
	if decoded.Keystone.Resource.Kind != "MigrationBundle" {
		t.Errorf("keystone.resource.kind=%q", decoded.Keystone.Resource.Kind)
	}
	if decoded.Keystone.Resource.Name != "users-v1" {
		t.Errorf("keystone.resource.name=%q", decoded.Keystone.Resource.Name)
	}
	if decoded.Keystone.Chain.SelfHash != "aabbccdd" {
		t.Errorf("keystone.chain.self_hash=%q", decoded.Keystone.Chain.SelfHash)
	}
	if decoded.Keystone.Chain.PrevHash != "genesis" {
		t.Errorf("keystone.chain.prev_hash=%q", decoded.Keystone.Chain.PrevHash)
	}
}

// TestStdoutJSONExporter_DoesNotEmitBeforeAfter — Before / After
// fields are persisted on the CR but NOT streamed to the SIEM; this
// keeps per-event cost bounded at scale.
func TestStdoutJSONExporter_DoesNotEmitBeforeAfter(t *testing.T) {
	spec := specFixture()
	spec.Before = `{"status":"old"}`
	spec.After = `{"status":"new"}`

	var buf bytes.Buffer
	_ = NewWriterExporter(&buf).Export(spec)
	if strings.Contains(buf.String(), `"before"`) || strings.Contains(buf.String(), `"after"`) {
		t.Errorf("exporter emitted Before/After; should be omitted to keep SIEM cost bounded. Output: %s", buf.String())
	}
}

// TestStdoutJSONExporter_ConcurrentWrites — multiple goroutines
// appending simultaneously must not interleave bytes on stdout.
func TestStdoutJSONExporter_ConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	exporter := NewWriterExporter(&buf)

	const N = 20
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			s := specFixture()
			s.Sequence = int64(i)
			errs <- exporter.Export(s)
		}(i)
	}
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Export: %v", err)
		}
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != N {
		t.Fatalf("expected %d lines; got %d", N, len(lines))
	}
	for i, line := range lines {
		var r ecsRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Errorf("line %d unparseable (interleaved bytes?): %v", i, err)
		}
	}
}

// TestNoopExporter — the default silently accepts everything.
func TestNoopExporter(t *testing.T) {
	if err := (NoopExporter{}).Export(specFixture()); err != nil {
		t.Errorf("NoopExporter returned error: %v", err)
	}
}

// -- Logger integration — exporter is called on success -----------

type captureExporter struct {
	calls []keystonev1alpha1.AuditEntrySpec
	err   error
}

func (c *captureExporter) Export(spec keystonev1alpha1.AuditEntrySpec) error {
	c.calls = append(c.calls, spec)
	return c.err
}

// TestLogger_CallsExporterOnAppend — ensures the exporter hook fires
// exactly once per successful Append.
func TestLogger_CallsExporterOnAppend(t *testing.T) {
	capt := &captureExporter{}
	l := &Logger{Exporter: capt}
	// Direct call to the unexported helper mirrors what Append does
	// post-commit; keeps the test package-local without needing a
	// full envtest harness.
	spec := specFixture()
	l.exportEntry(spec)

	if len(capt.calls) != 1 {
		t.Fatalf("expected 1 exporter call; got %d", len(capt.calls))
	}
	if capt.calls[0].Sequence != 42 {
		t.Errorf("exporter saw sequence %d, want 42", capt.calls[0].Sequence)
	}
}

// TestLogger_ExporterErrorDoesNotFail — a sink failure is logged via
// ExportErrorFn but must not propagate out of Append (we tested the
// underlying helper directly since it IS the propagation boundary).
func TestLogger_ExporterErrorDoesNotFail(t *testing.T) {
	capt := &captureExporter{err: errors.New("siem down")}
	var caughtErr error
	l := &Logger{
		Exporter: capt,
		ExportErrorFn: func(err error, _ keystonev1alpha1.AuditEntrySpec) {
			caughtErr = err
		},
	}
	l.exportEntry(specFixture())

	if caughtErr == nil {
		t.Fatal("ExportErrorFn should have been called")
	}
	if !strings.Contains(caughtErr.Error(), "siem down") {
		t.Errorf("error should mention sink failure; got %v", caughtErr)
	}
}

// TestLogger_NilExporterIsSafe — the default path (no wiring) must
// be a no-op.
func TestLogger_NilExporterIsSafe(t *testing.T) {
	l := &Logger{} // zero-value; Exporter=nil, ExportErrorFn=nil
	// Should not panic.
	l.exportEntry(specFixture())
}
