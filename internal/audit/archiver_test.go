// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// TestArchiver_Noop returns dormant on every call so the retention
// controller can detect "configured-but-inert" without deleting CRs.
func TestArchiver_Noop(t *testing.T) {
	a := NoopArchiver{}
	if a.Kind() != "noop" {
		t.Errorf("Kind = %q, want noop", a.Kind())
	}
	_, err := a.Archive(context.Background(), nil)
	if !IsDormant(err) {
		t.Errorf("Archive on NoopArchiver should return dormant; got %v", err)
	}
}

// TestArchiver_ECSRoundTrip — the gzip+JSON Lines format the
// S3Archiver writes must be losslessly readable for chain
// verification on rehydration. Pack a batch into a buffer using the
// same machinery as PutObject (without the network call), then
// unpack and verify Sequence + SelfHash + PrevHash survive the
// round-trip on every entry.
func TestArchiver_ECSRoundTrip(t *testing.T) {
	entries := []keystonev1alpha1.AuditEntry{
		mkTestEntry(1, "", "h1"),
		mkTestEntry(2, "h1", "h2"),
		mkTestEntry(3, "h2", "h3"),
	}

	// Reproduce the body-build path from S3Archiver.Archive without
	// dialing minio so we can call this in unit tests.
	body := &bytes.Buffer{}
	gz := gzip.NewWriter(body)
	enc := json.NewEncoder(gz)
	for i := range entries {
		if err := enc.Encode(toECSRecord(entries[i].Spec)); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	// Decode.
	got, err := readAllArchive(bytes.NewReader(body.Bytes()))
	if err != nil {
		t.Fatalf("readAllArchive: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("got %d entries, want %d", len(got), len(entries))
	}
	for i := range entries {
		if got[i].Sequence != entries[i].Spec.Sequence {
			t.Errorf("entry %d: Sequence %d != %d", i, got[i].Sequence, entries[i].Spec.Sequence)
		}
		if got[i].SelfHash != entries[i].Spec.SelfHash {
			t.Errorf("entry %d: SelfHash %q != %q", i, got[i].SelfHash, entries[i].Spec.SelfHash)
		}
		if got[i].PrevHash != entries[i].Spec.PrevHash {
			t.Errorf("entry %d: PrevHash %q != %q", i, got[i].PrevHash, entries[i].Spec.PrevHash)
		}
		if got[i].Verb != entries[i].Spec.Verb {
			t.Errorf("entry %d: Verb %q != %q", i, got[i].Verb, entries[i].Spec.Verb)
		}
	}
}

// TestArchiver_SHA256Stable — manifest's SHA-256 must be reproducible
// from the body bytes alone (no time dependence in the hash domain).
// This is the property an operator relies on when running
// `mc cat ... | sha256sum` to verify integrity off-cluster.
func TestArchiver_SHA256Stable(t *testing.T) {
	entries := []keystonev1alpha1.AuditEntry{mkTestEntry(1, "", "h1")}

	build := func() []byte {
		body := &bytes.Buffer{}
		gz := gzip.NewWriter(body)
		enc := json.NewEncoder(gz)
		for i := range entries {
			_ = enc.Encode(toECSRecord(entries[i].Spec))
		}
		_ = gz.Close()
		return body.Bytes()
	}

	a := build()
	b := build()
	ha := sha256.Sum256(a)
	hb := sha256.Sum256(b)
	if hex.EncodeToString(ha[:]) != hex.EncodeToString(hb[:]) {
		t.Errorf("SHA-256 not stable across runs: %x vs %x", ha, hb)
	}
}

// TestArchiver_ObjectKeyShape — operator-visible naming convention
// must include date prefix (for archival lifecycle policies) and
// zero-padded sequence range (so lex sort matches numeric sort).
func TestArchiver_ObjectKeyShape(t *testing.T) {
	a := &S3Archiver{pathPrefix: "prod-eu-west-1-hub"}
	got := a.objectKey(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC), 100, 999)
	if !strings.HasPrefix(got, "prod-eu-west-1-hub/2026/05/02/auditentries-") {
		t.Errorf("objectKey = %q; missing date prefix", got)
	}
	if !strings.Contains(got, "00000000000000000100-00000000000000000999") {
		t.Errorf("objectKey = %q; sequence range not zero-padded to 20", got)
	}
	if !strings.HasSuffix(got, ".jsonl.gz") {
		t.Errorf("objectKey = %q; missing .jsonl.gz suffix", got)
	}
}

// TestArchiver_PathPrefixSlashHandling — pathPrefix MUST work with or
// without trailing slash; operators frequently set "prod" vs "prod/".
func TestArchiver_PathPrefixSlashHandling(t *testing.T) {
	for _, pfx := range []string{"prod", "prod/"} {
		a := &S3Archiver{pathPrefix: pfx}
		got := a.objectKey(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC), 1, 1)
		// Both should produce exactly one slash between prefix and date.
		if !strings.HasPrefix(got, "prod/2026/") {
			t.Errorf("pathPrefix=%q: objectKey=%q; want prod/2026/...", pfx, got)
		}
	}
}

func mkTestEntry(seq int64, prev, self string) keystonev1alpha1.AuditEntry {
	return keystonev1alpha1.AuditEntry{
		Spec: keystonev1alpha1.AuditEntrySpec{
			Sequence:  seq,
			Verb:      "reconcile",
			Outcome:   "success",
			Reason:    "test",
			Timestamp: metav1.NewTime(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)),
			SelfHash:  self,
			PrevHash:  prev,
			Actor: keystonev1alpha1.AuditActor{
				Username: "system:serviceaccount:keystone-system:keystone-manager",
				Groups:   []string{"system:serviceaccounts"},
			},
			ResourceRef: keystonev1alpha1.AuditResourceRef{
				APIVersion: "keystone.hexxlock.io/v1alpha1",
				Kind:       "MigrationBundle",
				Name:       "test-bundle",
			},
		},
	}
}
