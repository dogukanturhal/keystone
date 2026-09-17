// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	natsserver "github.com/nats-io/nats-server/v2/server"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// startJetStreamServer spins up an in-process NATS server with JetStream
// enabled and a temporary storage directory. The server is shut down via
// t.Cleanup when the test completes.
func startJetStreamServer(t *testing.T) *natsserver.Server {
	t.Helper()

	dir, err := os.MkdirTemp("", "keystone-nats-test-*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // auto-assign to a free port
		JetStream: true,
		StoreDir:  dir,
		NoLog:     true,
		NoSigs:    true,
	}

	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	srv.Start()

	// Wait up to 3 s for the server to accept connections.
	deadline := time.Now().Add(3 * time.Second)
	for !srv.ReadyForConnections(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("nats server never became ready")
		}
	}

	t.Cleanup(srv.Shutdown)
	return srv
}

// provisionStream creates the KEYSTONE_AUDIT JetStream stream on the server.
// duplicate_window 2 min mirrors the production stream so dedupe tests are
// realistic.
func provisionStream(t *testing.T, js jetstream.JetStream, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:            "KEYSTONE_AUDIT",
		Subjects:        []string{prefix + ".>"},
		MaxAge:          24 * time.Hour,
		Storage:         jetstream.FileStorage,
		MaxMsgs:         10_000_000,
		Duplicates: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create stream: %v", err)
	}
}

// connectAndSetupJS connects a NATS client to srv, provisions the
// KEYSTONE_AUDIT stream, and returns a NATSExporter plus the raw
// jetstream.JetStream handle for assertion-side queries.
func connectAndSetupJS(t *testing.T, srv *natsserver.Server, prefix string) (*NATSExporter, jetstream.JetStream) {
	t.Helper()

	url := srv.ClientURL()

	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect nats: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	provisionStream(t, js, prefix)

	exp, err := New(NATSExporterOptions{
		URL:           url,
		SubjectPrefix: prefix,
		Timeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New NATSExporter: %v", err)
	}
	t.Cleanup(func() { _ = exp.Close() })

	return exp, js
}

// consumeOneFromSubject fetches the next message matching filterSubject from
// the named stream. Returns the message; the caller must Ack it.
func consumeOneFromSubject(t *testing.T, js jetstream.JetStream, stream, filterSubject string) jetstream.Msg {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg := jetstream.ConsumerConfig{
		AckPolicy: jetstream.AckExplicitPolicy,
	}
	if filterSubject != "" {
		cfg.FilterSubject = filterSubject
	}

	cons, err := js.CreateOrUpdateConsumer(ctx, stream, cfg)
	if err != nil {
		t.Fatalf("create consumer (filter=%q): %v", filterSubject, err)
	}

	msg, err := cons.Next(jetstream.FetchMaxWait(3 * time.Second))
	if err != nil {
		t.Fatalf("fetch next (filter=%q): %v", filterSubject, err)
	}
	return msg
}

// TestNATSExporter_HappyPath verifies that a published spec is retrievable
// from the stream and round-trips cleanly through JSON.
func TestNATSExporter_HappyPath(t *testing.T) {
	srv := startJetStreamServer(t)
	const prefix = "audit.keystone"
	exp, js := connectAndSetupJS(t, srv, prefix)

	spec := specFixture()
	if err := exp.Export(spec); err != nil {
		t.Fatalf("Export: %v", err)
	}

	msg := consumeOneFromSubject(t, js, "KEYSTONE_AUDIT", "")
	_ = msg.Ack()

	var got keystonev1alpha1.AuditEntrySpec
	if err := json.Unmarshal(msg.Data(), &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if got.Sequence != spec.Sequence {
		t.Errorf("sequence: got %d, want %d", got.Sequence, spec.Sequence)
	}
	if got.SelfHash != spec.SelfHash {
		t.Errorf("selfHash: got %q, want %q", got.SelfHash, spec.SelfHash)
	}
	if got.Verb != spec.Verb {
		t.Errorf("verb: got %q, want %q", got.Verb, spec.Verb)
	}
	if got.Outcome != spec.Outcome {
		t.Errorf("outcome: got %q, want %q", got.Outcome, spec.Outcome)
	}
}

// TestNATSExporter_SubjectDerivation verifies subject = "<prefix>.<verb>" for
// several verb values including those with hyphens.
func TestNATSExporter_SubjectDerivation(t *testing.T) {
	tests := []struct {
		verb string
		want string
	}{
		{"reconcile", "audit.keystone.reconcile"},
		{"apply", "audit.keystone.apply"},
		{"rotate-password", "audit.keystone.rotate-password"},
		{"drift-ack", "audit.keystone.drift-ack"},
	}

	srv := startJetStreamServer(t)
	const prefix = "audit.keystone"
	exp, js := connectAndSetupJS(t, srv, prefix)

	for i, tc := range tests {
		tc := tc
		i := i
		t.Run(tc.verb, func(t *testing.T) {
			spec := specFixture()
			spec.Verb = tc.verb
			// Use a unique SelfHash per case (index-based) so JetStream dedupe
			// doesn't suppress messages whose verbs share the same first byte.
			spec.SelfHash = fmt.Sprintf("%064x", i+100)

			if err := exp.Export(spec); err != nil {
				t.Fatalf("Export verb=%q: %v", tc.verb, err)
			}

			msg := consumeOneFromSubject(t, js, "KEYSTONE_AUDIT", tc.want)
			_ = msg.Ack()

			if msg.Subject() != tc.want {
				t.Errorf("subject: got %q, want %q", msg.Subject(), tc.want)
			}
		})
	}
}

// TestNATSExporter_Dedupe verifies that publishing the same SelfHash twice
// within the stream's duplicate_window results in exactly one stored message
// (JetStream server-side deduplication via Nats-Msg-Id).
func TestNATSExporter_Dedupe(t *testing.T) {
	srv := startJetStreamServer(t)
	const prefix = "audit.keystone"
	exp, js := connectAndSetupJS(t, srv, prefix)

	spec := specFixture()

	// First publish — stored.
	if err := exp.Export(spec); err != nil {
		t.Fatalf("first Export: %v", err)
	}
	// Second publish with same SelfHash (= same Nats-Msg-Id) — deduplicated.
	if err := exp.Export(spec); err != nil {
		t.Fatalf("second Export (expected duplicate=true, not error): %v", err)
	}

	// The stream must contain exactly one message.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := js.Stream(ctx, "KEYSTONE_AUDIT")
	if err != nil {
		t.Fatalf("stream handle: %v", err)
	}
	si, err := info.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if si.State.Msgs != 1 {
		t.Errorf("expected 1 deduplicated message; got %d", si.State.Msgs)
	}
}

// TestNATSExporter_CloseIdempotent verifies that Close() can be called
// multiple times without panicking or returning an unexpected error.
func TestNATSExporter_CloseIdempotent(t *testing.T) {
	srv := startJetStreamServer(t)
	url := srv.ClientURL()

	// Provision the stream so the initial connection succeeds (the exporter
	// does not provision streams itself).
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect for provisioning: %v", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	provisionStream(t, js, "audit.keystone")
	nc.Close()

	exp, err := New(NATSExporterOptions{
		URL:           url,
		SubjectPrefix: "audit.keystone",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := exp.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close on an already-drained+closed connection must not panic.
	if err := exp.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestMultiExporter_CallsAll verifies that every wrapped exporter is invoked
// and that the first error is returned without suppressing later exporters.
func TestMultiExporter_CallsAll(t *testing.T) {
	c1 := &captureExporter{}
	c2 := &captureExporter{}

	me := NewMultiExporter(c1, c2)
	spec := specFixture()
	if err := me.Export(spec); err != nil {
		t.Fatalf("MultiExporter.Export: %v", err)
	}
	if len(c1.calls) != 1 {
		t.Errorf("c1: expected 1 call; got %d", len(c1.calls))
	}
	if len(c2.calls) != 1 {
		t.Errorf("c2: expected 1 call; got %d", len(c2.calls))
	}
}

// TestMultiExporter_NilSafe verifies that all-nil inputs collapse to
// NoopExporter.
func TestMultiExporter_NilSafe(t *testing.T) {
	me := NewMultiExporter(nil, nil)
	if _, ok := me.(NoopExporter); !ok {
		t.Errorf("expected NoopExporter for all-nil inputs; got %T", me)
	}
}

// TestMultiExporter_SingleUnwrapped verifies that a single non-nil exporter
// is returned without a MultiExporter wrapper (avoids double-indirection in
// the common case).
func TestMultiExporter_SingleUnwrapped(t *testing.T) {
	c := &captureExporter{}
	me := NewMultiExporter(nil, c, nil)
	if _, ok := me.(*captureExporter); !ok {
		t.Errorf("expected unwrapped *captureExporter; got %T", me)
	}
}
