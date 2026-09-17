// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// StripAuditEntryPayload is the controller-runtime cache TransformFunc
// for *keystonev1alpha1.AuditEntry. It blanks the two largest spec
// fields — Before and After (the JSON snapshots of the resource state
// before/after the audited verb) — at cache-entry time so the
// in-memory shared informer cache holds only the chain-integrity and
// dispatch fields the operator actually reads.
//
// # Why this is safe
//
// AuditEntry is append-only by validating webhook contract. Once
// admitted it is never updated, so a cache-side strip cannot drift
// out of sync with apiserver state — apiserver state is also
// append-only.
//
// Two reconcilers read AuditEntry from the cache:
//
//   - AuditLogReconciler.Reconcile (auditlog_controller.go) — heals
//     AuditLog.status drift by computing max(Sequence) + reading the
//     winning entry's SelfHash + Timestamp. Never reads Before/After.
//
//   - AuditEntryRetentionController.runOnce (auditentry_retention_
//     controller.go) — lists candidates by Spec.Timestamp + Spec.
//     Sequence, batches them through audit.Archiver. The S3Archiver's
//     wire format (toECSRecord in internal/audit/exporter.go) is ECS-
//     flavoured and explicitly does not include Before/After (see the
//     comment block at exporter.go:76-79 — "Size-sensitive fields
//     (Before/After) are NOT emitted ... persisted on the CR and can
//     be retrieved on demand"). So even at archive time, the cache-
//     stripped entries serialise to byte-identical S3 objects.
//
// The audit chain hash (CanonicalBytes in internal/audit/hash.go)
// DOES include Before/After in its canonical encoding, but it is only
// computed by:
//
//   - audit.Logger.Append — write path; builds the spec locally and
//     never touches the cache.
//   - audit.VerifyEntry — called from the validating webhook on Create
//     (internal/webhook/auditentry_webhook.go); the webhook receives
//     the new entry from admission, NOT from the cache.
//   - audit.VerifyChain — currently has no in-process call site (the
//     post-hoc chain verifier is a planned operational tool, see
//     audit/hash.go's docstring); when wired, it MUST read via
//     mgr.GetAPIReader() (uncached) so the strip below stays
//     transparent to chain re-verification.
//
// # Memory rationale
//
// On a representative cluster (2026-05-07: 104,314 AuditEntries in
// keystone-system) Spec.Before + Spec.After are capped at 32 KiB each
// by audit.Logger.encodeDiffs (maxBeforeAfterBytes constant), but
// realistic averages run ~3-8 KiB combined. At 6 KiB × 104K entries
// the cache-strip frees ~625 MiB of resident memory and ~208K heap
// objects (two strings per entry). The full per-entry footprint also
// drops because the controller-runtime informer holds a deep-copy
// per indexed object.
//
// # What this does NOT do
//
// - Does not touch the apiserver / etcd. Originals stay intact.
// - Does not change the audit chain hash — chain integrity and
//   re-verification continue to work because the canonical encoding
//   reads from the on-cluster CR (uncached path), not the cache.
// - Does not affect the write path (Logger.Append) — the local
//   in-flight entry never goes through the cache.
//
// # Invariant for future contributors
//
// If a future reconciler or operational tool needs Spec.Before /
// Spec.After, it MUST read via mgr.GetAPIReader() (uncached) instead
// of the default client. There is no in-tree precedent today (the
// only AuditEntry consumers — AuditLogReconciler, AuditEntryRetention
// Controller, S3Archiver — read fields the strip preserves), so a
// linter cannot enforce this. Treat this docstring as the contract;
// if you find yourself reading Before/After from the cached client,
// switch the read site to APIReader and add a regression test.
func StripAuditEntryPayload(obj any) (any, error) {
	if obj == nil {
		return obj, nil
	}
	entry, ok := obj.(*keystonev1alpha1.AuditEntry)
	if !ok {
		// Not an AuditEntry — return unchanged. controller-runtime
		// only routes a transform through the type-keyed
		// cache.ByObject, so this path is defensive against
		// scheme-level surprises (e.g. partial-object metadata
		// requests).
		return obj, nil
	}
	if entry.Spec.Before == "" && entry.Spec.After == "" {
		// Already stripped (idempotent — informer may re-call this on
		// resync) or genuinely empty. No copy required.
		return entry, nil
	}
	entry.Spec.Before = ""
	entry.Spec.After = ""
	return entry, nil
}
