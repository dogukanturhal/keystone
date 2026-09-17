// SPDX-License-Identifier: AGPL-3.0-or-later

// Package audit is Keystone's append-only audit chain.
//
// Every meaningful state change — CR create / update / delete,
// reconcile outcomes, drift acknowledgments, approval grants/denials
// — is persisted as an AuditEntry CR linked to the previous entry by
// SHA-256 hashes. Tampering with any entry in the chain changes its
// SelfHash and invalidates every subsequent PrevHash, which external
// verifiers detect in O(N) time.
//
// Integrity guarantees:
//
//   - Entries are created but never updated or deleted (enforced by
//     the AuditEntry validating webhook at
//     `internal/webhook/auditentry_webhook.go`).
//   - Sequences are strictly monotonic and gap-free; verifiers reject
//     a log with missing sequence numbers.
//   - SelfHash covers every meaningful field of the entry; the
//     canonical-encoding function `canonicalBytes` is the trusted
//     spec.
//   - Cross-cluster chains share no namespace; the chain is per-
//     cluster by construction.
//
// What this package does NOT do:
//
//   - Remove entries. AuditLog.spec.retentionDays is a *minimum*;
//     actual prune logic lands in Phase 12.3 and is out-of-band (a
//     separate controller with its own audit entries explaining the
//     prune).
//   - Encrypt entries at rest. etcd handles at-rest encryption;
//     column-level encryption of sensitive spec.Before/After diffs is
//     a v0.3 feature. Today, redact sensitive bytes at the caller.
//   - Stream to SIEMs. Phase 13 wires AuditLog.spec.exportEndpoint.
//
// See ADR 0015 (TBD) for the design decisions and
// `docs/compliance-mappings.md` for control mappings.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// GenesisHash is the literal PrevHash value used by the first entry
// in a chain. Verifiers check that exactly one entry carries this
// sentinel — more than one means the chain was split.
const GenesisHash = "genesis"

// CanonicalBytes returns the deterministic byte sequence that is
// hashed to produce an AuditEntry's SelfHash.
//
// The encoding is intentionally simple — key=value lines in
// lexicographic order, joined by LF — so that a human reader and a
// forensic verifier can reproduce it without loading the Kubernetes
// API machinery. `SelfHash` is excluded because self-referential hash
// computation is circular; everything else in the spec contributes.
//
// Stability contract: adding a new field to AuditEntrySpec in a
// future version REQUIRES bumping the CanonicalEncodingVersion
// constant and persisting the version alongside the hash (field
// TBD in v1beta1). For v1alpha1, adding a field without a version
// bump would silently invalidate all existing chains.
func CanonicalBytes(spec keystonev1alpha1.AuditEntrySpec) []byte {
	// Field order is lexicographic by key for stability.
	kv := map[string]string{
		"actor.uid":      spec.Actor.UID,
		"actor.username": spec.Actor.Username,
		"actor.groups":   strings.Join(sortedCopy(spec.Actor.Groups), ","),
		"after":          spec.After,
		"before":         spec.Before,
		"outcome":        spec.Outcome,
		"prevHash":       spec.PrevHash,
		"reason":         spec.Reason,
		"resourceRef.apiVersion": spec.ResourceRef.APIVersion,
		"resourceRef.kind":       spec.ResourceRef.Kind,
		"resourceRef.name":       spec.ResourceRef.Name,
		"resourceRef.namespace":  spec.ResourceRef.Namespace,
		"resourceRef.uid":        spec.ResourceRef.UID,
		"sequence":               fmt.Sprintf("%d", spec.Sequence),
		"timestamp":              spec.Timestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		"verb":                   spec.Verb,
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		// Each line: key=<value>\n. No escaping of values — the
		// field-level MaxLength CRD validation keeps values finite,
		// and the sha256 ingest doesn't care about newlines inside
		// values because key boundaries disambiguate.
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(kv[k])
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// ComputeSelfHash returns the lowercase hex SHA-256 of spec's
// canonical encoding.
func ComputeSelfHash(spec keystonev1alpha1.AuditEntrySpec) string {
	sum := sha256.Sum256(CanonicalBytes(spec))
	return hex.EncodeToString(sum[:])
}

// VerifyEntry returns nil if the entry's SelfHash matches its
// canonical encoding. Callers use this when reading an entry from the
// apiserver to detect tampering.
func VerifyEntry(spec keystonev1alpha1.AuditEntrySpec) error {
	expected := ComputeSelfHash(spec)
	if spec.SelfHash != expected {
		return fmt.Errorf(
			"audit: entry sequence=%d selfHash mismatch: claimed=%q computed=%q — entry was tampered after persistence",
			spec.Sequence, spec.SelfHash, expected)
	}
	return nil
}

// VerifyChain validates a slice of entries by:
//
//  1. Sorting by Sequence
//  2. Checking SelfHash of each entry matches the canonical encoding
//  3. Checking each entry's PrevHash equals the previous entry's
//     SelfHash (or GenesisHash for the first entry)
//  4. Checking Sequence is strictly monotonic + gap-free
//
// Returns the number of entries verified and the first error
// encountered. Callers use this from operational tooling
// (`keystonectl audit verify`).
func VerifyChain(entries []keystonev1alpha1.AuditEntry) (int, error) {
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Spec.Sequence < entries[j].Spec.Sequence
	})

	prevHash := GenesisHash
	for i, e := range entries {
		if err := VerifyEntry(e.Spec); err != nil {
			return i, err
		}
		wantSeq := int64(i + 1)
		if entries[0].Spec.Sequence != 1 {
			// Allow partial chains (e.g. post-retention) — anchor on
			// the first seen sequence rather than 1.
			wantSeq = entries[0].Spec.Sequence + int64(i)
		}
		if e.Spec.Sequence != wantSeq {
			return i, fmt.Errorf(
				"audit: chain has gap at position %d — want sequence %d, got %d",
				i, wantSeq, e.Spec.Sequence)
		}
		if e.Spec.PrevHash != prevHash {
			return i, fmt.Errorf(
				"audit: chain broken at sequence %d — claimed prevHash=%q but previous entry's selfHash=%q",
				e.Spec.Sequence, e.Spec.PrevHash, prevHash)
		}
		prevHash = e.Spec.SelfHash
	}
	return len(entries), nil
}

// sortedCopy returns a new slice containing s, sorted.
func sortedCopy(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}
