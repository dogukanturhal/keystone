// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audit — NATS JetStream exporter (T2 #25 Phase 4).
//
// NATSExporter is the second writer in the dual-write contract:
//   - AuditEntry CR (etcd) — authoritative, immutable, chain-hashed.
//   - NATS JetStream (stream KEYSTONE_AUDIT) — best-effort secondary;
//     the Phase 3 audit-archiver drains it to MinIO WORM storage.
//
// A dropped publish never fails Append — the CR is the single source
// of truth. Backfill via keystone-backfill (cmd/keystoneadm) recovers
// missed entries after a NATS outage.
//
// Architecture refs:
//   - project_security_stack_tier1_2026_04_27 — 7-year WORM mandate
//   - internal engineering notes on high-cardinality CRD list OOMs — root
//     cause motivating the etcd → NATS → MinIO offload

package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

const (
	// defaultNATSPublishTimeout is applied when NATSExporterOptions.Timeout
	// is zero. 5 s is generous enough for a LAN hop to NATS but tight
	// enough to not stall the CR write path on a dead broker.
	defaultNATSPublishTimeout = 5 * time.Second

	// defaultNATSSubjectPrefix is used when NATSExporterOptions.SubjectPrefix
	// is empty. Maps to the KEYSTONE_AUDIT stream's subject filter
	// "audit.keystone.>".
	defaultNATSSubjectPrefix = "audit.keystone"
)

// NATSExporterOptions configures the exporter constructor.
type NATSExporterOptions struct {
	// URL is the NATS server URL, e.g. "nats://nats.messaging-system.svc.cluster.local:4222".
	// Required — constructor returns an error if empty.
	URL string

	// SubjectPrefix is prepended to the verb to form the publish subject:
	// "<SubjectPrefix>.<verb>", e.g. "audit.keystone.reconcile".
	// Defaults to "audit.keystone".
	SubjectPrefix string

	// Timeout is the per-publish context deadline. Defaults to 5s.
	Timeout time.Duration

	// Log receives structured error and info messages. Nil is safe —
	// the exporter falls back to a no-op zap.Logger.
	Log *zap.Logger

	// NATSOptions are additional functional options forwarded to nats.Connect.
	// Callers can inject mTLS credentials, auth tokens, etc. without
	// changing the NATSExporter constructor signature.
	NATSOptions []nats.Option
}

// NATSExporter publishes committed AuditEntrySpec values to NATS JetStream
// (stream KEYSTONE_AUDIT). Export is best-effort — callers (audit.Logger)
// log but do not propagate errors. The AuditEntry CR is always written first
// and remains the authoritative record.
//
// The Nats-Msg-Id header is set to spec.SelfHash so JetStream's built-in
// deduplication (configured with a 2-minute duplicate_window on the stream)
// suppresses re-deliveries from retried reconcile loops or backfill replays.
type NATSExporter struct {
	nc      *nats.Conn
	js      jetstream.JetStream
	subject string // prefix, e.g. "audit.keystone"
	timeout time.Duration
	log     *zap.Logger
}

// New constructs and connects a NATSExporter. Returns an error if the
// connection or JetStream context cannot be established — the caller
// (cmd/manager/main.go) falls back to NoopExporter so the operator
// starts even when the NATS broker is temporarily unavailable.
func New(opts NATSExporterOptions) (*NATSExporter, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("nats exporter: URL is required")
	}

	prefix := opts.SubjectPrefix
	if prefix == "" {
		prefix = defaultNATSSubjectPrefix
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultNATSPublishTimeout
	}

	log := opts.Log
	if log == nil {
		log = zap.NewNop()
	}

	// Connect. nats.Connect blocks until the server handshake is done or
	// the default connect-timeout (2s) fires.
	nc, err := nats.Connect(opts.URL, opts.NATSOptions...)
	if err != nil {
		return nil, fmt.Errorf("nats exporter: connect to %q: %w", opts.URL, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats exporter: create JetStream context: %w", err)
	}

	log.Info("nats audit exporter connected",
		zap.String("url", opts.URL),
		zap.String("subjectPrefix", prefix),
		zap.Duration("publishTimeout", timeout),
	)

	return &NATSExporter{
		nc:      nc,
		js:      js,
		subject: prefix,
		timeout: timeout,
		log:     log,
	}, nil
}

// Export marshals spec as JSON and publishes it to NATS JetStream under the
// subject "<prefix>.<verb>". The Nats-Msg-Id header is set to spec.SelfHash
// for JetStream idempotent deduplication.
//
// A publish-context timeout of NATSExporterOptions.Timeout is applied so a
// slow or unavailable broker does not stall the operator's reconcile loop
// (the CR has already been committed before Export is called).
func (e *NATSExporter) Export(spec keystonev1alpha1.AuditEntrySpec) error {
	subject := e.subject + "." + spec.Verb

	payload, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("nats exporter: marshal spec (seq=%d): %w", spec.Sequence, err)
	}

	msg := &nats.Msg{
		Subject: subject,
		Data:    payload,
		Header:  nats.Header{},
	}
	// Nats-Msg-Id is the JetStream deduplication key. Setting it to
	// SelfHash (a SHA-256 hex of the entry's canonical fields) means:
	//   - re-delivered publishes within the stream's duplicate_window (2 min)
	//     are silently dropped by the server — Phase 3 archiver is safe.
	//   - backfill replays (keystone-backfill) are idempotent.
	msg.Header.Set(jetstream.MsgIDHeader, spec.SelfHash)

	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	ack, err := e.js.PublishMsg(ctx, msg)
	if err != nil {
		return fmt.Errorf("nats exporter: publish seq=%d subject=%q: %w", spec.Sequence, subject, err)
	}

	e.log.Debug("audit entry published to NATS",
		zap.Int64("sequence", spec.Sequence),
		zap.String("subject", subject),
		zap.String("selfHash", spec.SelfHash),
		zap.Uint64("natsSeq", ack.Sequence),
		zap.Bool("duplicate", ack.Duplicate),
	)
	return nil
}

// Close drains in-flight publishes and closes the NATS connection. Safe
// to call multiple times — subsequent calls are no-ops once the connection
// is closed.
func (e *NATSExporter) Close() error {
	if e.nc == nil || e.nc.IsClosed() {
		return nil
	}
	// Drain flushes the send buffer and unsubscribes; Close releases sockets.
	if err := e.nc.Drain(); err != nil {
		// Drain failing is non-fatal — still close to release the FD.
		e.log.Warn("nats drain failed; closing anyway", zap.Error(err))
	}
	e.nc.Close()
	e.log.Info("nats audit exporter closed")
	return nil
}

// MultiExporter calls each Exporter in order and returns the first non-nil
// error. Subsequent exporters are still called even if an earlier one fails
// so a single bad sink does not silence the others.
type MultiExporter struct {
	exporters []Exporter
}

// NewMultiExporter wraps multiple Exporters into a single Exporter. Nil
// entries are silently skipped. Returns a NoopExporter if the filtered list
// is empty.
func NewMultiExporter(ex ...Exporter) Exporter {
	var filtered []Exporter
	for _, e := range ex {
		if e != nil {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == 0 {
		return NoopExporter{}
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return &MultiExporter{exporters: filtered}
}

// Export calls each wrapped Exporter. Returns the first error encountered;
// all exporters are always invoked regardless.
func (m *MultiExporter) Export(spec keystonev1alpha1.AuditEntrySpec) error {
	var first error
	for _, e := range m.exporters {
		if err := e.Export(spec); err != nil && first == nil {
			first = err
		}
	}
	return first
}
