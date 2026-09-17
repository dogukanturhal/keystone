// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// maxBeforeAfterBytes caps the size of the diff fields we persist.
// etcd max is 1MB; we stay well under that per entry since an
// AuditLog with 100-entry bursts would otherwise blow through the
// default request size limit.
const maxBeforeAfterBytes = 32 * 1024

// Logger writes AuditEntries to the cluster via a controller-runtime
// client. Constructed once per manager and shared across reconcilers.
//
// Logger.Append is the single entry point. It handles:
//
//   - Creating or reading the singleton AuditLog
//   - Assigning the next Sequence under optimistic concurrency
//   - Computing SelfHash over a canonical encoding
//   - Creating the AuditEntry CR
//   - Updating AuditLog status with the new LastEntryHash + sequence
//   - Streaming the committed entry to the configured Exporter (C1)
//
// Contention model: write conflicts on AuditLog.status are retried
// up to `maxRetries` times. Under leader election (the intended
// deployment), retries should be near-zero — only one manager writes.
// Under multi-writer bug scenarios we prefer a retry-loop failure
// over a silently broken chain, so conflicts surface as errors
// rather than panics.
//
// T2 #25 Phase 5a — WriteCR toggle:
// When WriteCR=false the entire etcd-write path (AuditLog singleton
// get/create, sequence allocation, AuditEntry CR create, AuditLog
// status patch) is bypassed. Only the spec hash + Exporter call
// happen. This is the "NATS-only" mode activated after Phase 4's
// dual-write has been proven live for 7 days. The chain ordering
// responsibility moves from AuditLog.status (etcd) to the archiver's
// _chain/head.json stored in the MinIO WORM bucket — see the Phase 3
// audit-archiver service for the head-pointer protocol.
type Logger struct {
	Client client.Client

	// MaxRetries on optimistic concurrency conflicts. Default 5.
	MaxRetries int

	// ClockFn is injectable for tests. Default time.Now.
	ClockFn func() time.Time

	// Exporter streams committed audit entries to a SIEM sink (C1).
	// Nil is equivalent to NoopExporter — the CR remains the
	// authoritative record. Export is best-effort: errors are logged
	// via ExportErrorFn (or discarded when nil) but do NOT fail the
	// Append call, because dropping a SIEM line is recoverable but
	// retrying a CR-committed sequence is not.
	Exporter Exporter

	// ExportErrorFn receives Exporter.Export errors. Wired to the
	// controller-runtime logger in production; nil = silent (test
	// default).
	ExportErrorFn func(err error, spec keystonev1alpha1.AuditEntrySpec)

	// WriteCR controls whether Append persists an AuditEntry CR to etcd.
	// Default true for backward compatibility (legacy and dual-write modes).
	//
	// Set to false (Phase 5a) once the NATS path has been proven live for
	// 7 days and the Phase 3 archiver is confirmed draining entries into
	// MinIO with WORM object-lock active. In this mode:
	//
	//   - Sequence is always 0 and PrevHash is always "" in the published
	//     spec — the chain ordering lives in the archiver-side
	//     _chain/head.json in the MinIO WORM bucket (T2 #25 Phase 5a
	//     architectural shift away from etcd-resident chain-head).
	//   - SelfHash is still computed so the Exporter's Nats-Msg-Id JetStream
	//     deduplication key remains stable and backfill replays are idempotent.
	//   - No retry loop is executed; no etcd reads or writes occur.
	//
	// MUST NOT be set to false when Exporter is NoopExporter or nil —
	// that combination would silently drop every audit event. The manager
	// refuses to start in that state (see cmd/manager/main.go::run).
	WriteCR bool
}

// NewLogger constructs a Logger with sane defaults — NoopExporter
// so the controller stays silent unless explicitly wired up.
// WriteCR defaults to true for backward compatibility.
func NewLogger(c client.Client) *Logger {
	return &Logger{
		Client:     c,
		MaxRetries: 5,
		ClockFn:    time.Now,
		Exporter:   NoopExporter{},
		WriteCR:    true,
	}
}

// Event is the caller-facing input to Append. It's everything the
// audit entry carries except the chain-level bookkeeping (sequence,
// hashes), which Logger owns.
type Event struct {
	// Verb is one of create/update/delete/reconcile/approve/deny/...
	// See the CRD's enum.
	Verb string

	// Actor identifies the caller. Populate from admission-webhook
	// userInfo for webhook-sourced events, or from the manager's own
	// identity for reconciler-sourced events.
	Actor keystonev1alpha1.AuditActor

	// ResourceRef points at the audited object.
	ResourceRef keystonev1alpha1.AuditResourceRef

	// Before / After are the pre/post states (runtime.Object). Logger
	// JSON-encodes them and truncates to maxBeforeAfterBytes.
	Before runtime.Object
	After  runtime.Object

	// Outcome is one of success/error/warn. Empty defaults to success.
	Outcome string

	// Reason is a short human-readable description.
	Reason string

	// Timestamp overrides ClockFn when non-zero. Tests set this for
	// determinism.
	Timestamp time.Time
}

// Append persists one AuditEntry. Returns the assigned sequence + the
// entry's SelfHash. Safe to call concurrently — the optimistic
// concurrency on AuditLog.status serialises writers.
func (l *Logger) Append(ctx context.Context, ev Event) (int64, string, error) {
	if ev.Verb == "" {
		return 0, "", errors.New("audit: Event.Verb is required")
	}
	if ev.Actor.Username == "" {
		return 0, "", errors.New("audit: Event.Actor.Username is required")
	}
	if ev.ResourceRef.Name == "" || ev.ResourceRef.Kind == "" {
		return 0, "", errors.New("audit: Event.ResourceRef.Kind+Name are required")
	}
	if ev.Outcome == "" {
		ev.Outcome = "success"
	}

	beforeJSON, afterJSON, err := encodeDiffs(ev.Before, ev.After)
	if err != nil {
		return 0, "", fmt.Errorf("audit: encode diffs: %w", err)
	}

	ts := ev.Timestamp
	if ts.IsZero() {
		ts = l.ClockFn()
	}

	// T2 #25 Phase 5a — NATS-only path (WriteCR=false).
	//
	// When WriteCR is false the etcd-write path is bypassed entirely:
	// no AuditLog singleton read/create, no sequence allocation, no
	// AuditEntry CR creation, no AuditLog.Status patch. Only the spec
	// hash computation and Exporter call happen.
	//
	// Sequence=0 and PrevHash="" are intentional: the chain ordering
	// responsibility has shifted to the archiver-side _chain/head.json
	// stored in the MinIO WORM bucket (T2 #25 Phase 5a architectural
	// shift). The archiver maintains the monotonic head pointer in
	// object storage; the operator no longer owns chain sequencing.
	//
	// SelfHash is still computed so the NATSExporter's Nats-Msg-Id
	// JetStream deduplication key remains stable and backfill replays
	// stay idempotent across operator restarts.
	if !l.WriteCR {
		spec := keystonev1alpha1.AuditEntrySpec{
			// Sequence and PrevHash intentionally zero/empty —
			// the chain ordering lives in the archiver's
			// _chain/head.json (T2 #25 Phase 5a architectural shift).
			Timestamp:   metav1.NewTime(ts),
			Actor:       ev.Actor,
			Verb:        ev.Verb,
			ResourceRef: ev.ResourceRef,
			Before:      beforeJSON,
			After:       afterJSON,
			Reason:      ev.Reason,
			Outcome:     ev.Outcome,
		}
		spec.SelfHash = ComputeSelfHash(spec)
		RecordAppend("success_nocr")
		l.exportEntry(spec)
		return 0, spec.SelfHash, nil
	}

	// Retry loop — AuditLog.status conflicts resolve by reading the
	// latest status + re-attempting the write. On AlreadyExists we
	// re-derive the next sequence from max(AuditEntry.spec.sequence)
	// across existing entries, NOT from AuditLog.status.nextSequence,
	// which can lag behind reality after a status-patch conflict
	// path succeeded without bumping the counter. Without this
	// re-derivation the retry loop pounds etcd with the same doomed
	// Name on every iteration (2026-04-21 incident: 150-180 writes/sec
	// for 50m on entry-00000000000000114604 under concurrent
	// reconcilers).
	var lastErr error
	for attempt := 0; attempt <= l.maxRetries(); attempt++ {
		if attempt > 0 {
			// Exponential backoff with ±25% jitter on every retry to
			// avoid synchronised hot-retries across concurrent
			// Append callers. Aborts early if ctx is cancelled so
			// manager shutdown doesn't have to wait out the backoff.
			if err := l.sleepBackoff(ctx, attempt); err != nil {
				return 0, "", err
			}
		}

		alog := &keystonev1alpha1.AuditLog{}
		err := l.Client.Get(ctx, types.NamespacedName{Name: keystonev1alpha1.AuditLogName}, alog)
		switch {
		case apierrors.IsNotFound(err):
			// Bootstrap: seed the singleton.
			if err := l.bootstrapAuditLog(ctx); err != nil {
				lastErr = fmt.Errorf("audit: bootstrap AuditLog: %w", err)
				continue
			}
			continue
		case err != nil:
			return 0, "", fmt.Errorf("audit: get AuditLog: %w", err)
		}

		// Derive next sequence: prefer AuditLog.status.nextSequence,
		// but if a prior attempt on this goroutine hit AlreadyExists
		// the status is stale relative to existing AuditEntries, so
		// fall through to the max-scan below when that was the case.
		seq := alog.Status.NextSequence
		if seq == 0 {
			seq = 1
		}
		if apierrors.IsAlreadyExists(lastErr) {
			trueMax, err := l.maxEntrySequence(ctx)
			if err != nil {
				return 0, "", fmt.Errorf("audit: max sequence scan: %w", err)
			}
			if trueMax+1 > seq {
				seq = trueMax + 1
			}
		}

		prevHash := alog.Status.LastEntryHash
		if prevHash == "" {
			prevHash = GenesisHash
		}

		spec := keystonev1alpha1.AuditEntrySpec{
			Sequence:    seq,
			Timestamp:   metav1.NewTime(ts),
			Actor:       ev.Actor,
			Verb:        ev.Verb,
			ResourceRef: ev.ResourceRef,
			Before:      beforeJSON,
			After:       afterJSON,
			Reason:      ev.Reason,
			Outcome:     ev.Outcome,
			PrevHash:    prevHash,
		}
		spec.SelfHash = ComputeSelfHash(spec)

		entry := &keystonev1alpha1.AuditEntry{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("entry-%020d", seq),
			},
			Spec: spec,
		}
		if err := l.Client.Create(ctx, entry); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Another writer grabbed this sequence. The next
				// loop iteration will scan max(AuditEntry.sequence)
				// and pick a fresh number, avoiding the hot-retry
				// on the same stale name.
				lastErr = err
				RecordRetry("alreadyexists")
				continue
			}
			RecordAppend("error")
			return 0, "", fmt.Errorf("audit: create AuditEntry: %w", err)
		}

		// Advance AuditLog.status. On conflict we don't recreate the
		// entry — its SelfHash is stable; we just re-read and bump
		// from wherever the counter now sits.
		updated := alog.DeepCopy()
		updated.Status.NextSequence = seq + 1
		updated.Status.LastSequence = seq
		updated.Status.LastEntryHash = spec.SelfHash
		now := metav1.NewTime(ts)
		updated.Status.LastEntryTime = &now
		if err := l.Client.Status().Patch(ctx, updated, client.MergeFrom(alog)); err != nil {
			if apierrors.IsConflict(err) {
				// Status races are recoverable — the entry is
				// persisted. The background auditlog_controller
				// reconciler heals status.nextSequence from
				// max(AuditEntry.spec.sequence) on its tick; until
				// then the next Append re-derives via the 409 path.
				RecordRetry("status_conflict")
				RecordAppend("success")
				l.exportEntry(spec)
				return seq, spec.SelfHash, nil
			}
			RecordAppend("error")
			return 0, "", fmt.Errorf("audit: patch AuditLog status: %w", err)
		}
		RecordAppend("success")
		l.exportEntry(spec)
		return seq, spec.SelfHash, nil
	}
	RecordAppend("exhausted")
	return 0, "", fmt.Errorf("audit: exceeded retries: %w", lastErr)
}

// maxEntrySequence returns the highest spec.sequence across all
// existing AuditEntries, or 0 when none exist. Used on the 409 path
// to break out of the stale-AuditLog.status retry trap.
func (l *Logger) maxEntrySequence(ctx context.Context) (int64, error) {
	list := &keystonev1alpha1.AuditEntryList{}
	if err := l.Client.List(ctx, list); err != nil {
		return 0, err
	}
	var max int64
	for i := range list.Items {
		if s := list.Items[i].Spec.Sequence; s > max {
			max = s
		}
	}
	return max, nil
}

// sleepBackoff waits for an exponentially increasing, jittered
// duration — 50ms, 100ms, 200ms, … capped at 5s. Returns ctx.Err()
// if the context is cancelled during the wait so caller-side
// shutdowns are prompt instead of blocking on the full backoff.
func (l *Logger) sleepBackoff(ctx context.Context, attempt int) error {
	base := 50 * time.Millisecond
	d := base << attempt
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	// ±25% jitter — prevents thundering-herd on synchronised retries.
	jitter := time.Duration(rand.Int63n(int64(d / 2)))
	d = d - d/4 + jitter
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// exportEntry hands a committed AuditEntrySpec to the configured
// Exporter. Best-effort: a nil Exporter no-ops, and export errors
// are surfaced via ExportErrorFn without failing Append. Called
// AFTER the CR is created and the AuditLog status bump is in flight
// — the CR is the authoritative ledger; the exporter is the SIEM
// mirror.
func (l *Logger) exportEntry(spec keystonev1alpha1.AuditEntrySpec) {
	if l.Exporter == nil {
		return
	}
	if err := l.Exporter.Export(spec); err != nil && l.ExportErrorFn != nil {
		l.ExportErrorFn(err, spec)
	}
}

func (l *Logger) maxRetries() int {
	if l.MaxRetries <= 0 {
		// Bumped 5 → 8 alongside exponential backoff. With the
		// max-sequence re-scan on 409 each attempt advances to a
		// new name, so retries converge fast — 8 gives headroom
		// for burst contention without the old hot-retry risk.
		return 8
	}
	return l.MaxRetries
}

// bootstrapAuditLog creates the singleton AuditLog with starting
// sequence=1. Safe to call from multiple writers — AlreadyExists is
// treated as success.
func (l *Logger) bootstrapAuditLog(ctx context.Context) error {
	alog := &keystonev1alpha1.AuditLog{
		ObjectMeta: metav1.ObjectMeta{Name: keystonev1alpha1.AuditLogName},
		Spec: keystonev1alpha1.AuditLogSpec{
			RetentionDays: 2555, // ~7 years
		},
	}
	if err := l.Client.Create(ctx, alog); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}

	// Seed status.nextSequence=1 so the first writer doesn't have to.
	fresh := alog.DeepCopy()
	fresh.Status.NextSequence = 1
	if err := l.Client.Status().Patch(ctx, fresh, client.MergeFrom(alog)); err != nil {
		if apierrors.IsConflict(err) {
			return nil // another writer seeded; benign
		}
		return err
	}
	return nil
}

// encodeDiffs JSON-encodes before/after runtime objects and truncates
// to the configured budget. Truncation marker is explicit so auditors
// can tell a snapshot from a truncated one.
func encodeDiffs(before, after runtime.Object) (string, string, error) {
	encode := func(o runtime.Object) (string, error) {
		if o == nil {
			return "", nil
		}
		b, err := json.Marshal(o)
		if err != nil {
			return "", err
		}
		if len(b) > maxBeforeAfterBytes {
			return string(b[:maxBeforeAfterBytes-64]) +
				`..."<TRUNCATED_BY_KEYSTONE_AUDIT_LOGGER>"`, nil
		}
		return string(b), nil
	}
	bj, err := encode(before)
	if err != nil {
		return "", "", err
	}
	aj, err := encode(after)
	if err != nil {
		return "", "", err
	}
	return bj, aj, nil
}

// ActorFromServiceAccount returns the Actor for a reconciler-driven
// event. Use when the change originates from the manager itself
// (no admission webhook userInfo available).
func ActorFromServiceAccount(namespace, name string) keystonev1alpha1.AuditActor {
	return keystonev1alpha1.AuditActor{
		Username: fmt.Sprintf("system:serviceaccount:%s:%s", namespace, name),
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace},
	}
}

// ResourceRefFromObject fills an AuditResourceRef from any
// kubernetes runtime.Object that implements metav1.Object + TypeMeta.
func ResourceRefFromObject(obj client.Object) keystonev1alpha1.AuditResourceRef {
	gvk := obj.GetObjectKind().GroupVersionKind()
	return keystonev1alpha1.AuditResourceRef{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		UID:        string(obj.GetUID()),
	}
}
