// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func baseSpec() keystonev1alpha1.AuditEntrySpec {
	return keystonev1alpha1.AuditEntrySpec{
		Sequence:  1,
		Timestamp: metav1.NewTime(time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)),
		Actor: keystonev1alpha1.AuditActor{
			Username: "system:serviceaccount:keystone-system:manager",
			Groups:   []string{"system:serviceaccounts"},
		},
		Verb: "create",
		ResourceRef: keystonev1alpha1.AuditResourceRef{
			APIVersion: "keystone.hexxlock.io/v1alpha1",
			Kind:       "MigrationBundle",
			Namespace:  "app-a",
			Name:       "mb-v1",
			UID:        "deadbeef",
		},
		Outcome:  "success",
		Reason:   "Reconciled",
		PrevHash: GenesisHash,
	}
}

func TestCanonicalBytes_Deterministic(t *testing.T) {
	s1 := baseSpec()
	s2 := baseSpec()
	if string(CanonicalBytes(s1)) != string(CanonicalBytes(s2)) {
		t.Errorf("canonical encoding not deterministic for identical specs")
	}
}

func TestCanonicalBytes_FieldOrderInvariant(t *testing.T) {
	// Reordering groups should reorder canonical output — it's part of
	// the hash. Lexicographic sort inside CanonicalBytes means
	// callers can pass groups in any order and get the same hash.
	s1 := baseSpec()
	s1.Actor.Groups = []string{"a", "b", "c"}
	s2 := baseSpec()
	s2.Actor.Groups = []string{"c", "a", "b"}
	if ComputeSelfHash(s1) != ComputeSelfHash(s2) {
		t.Errorf("group order affected hash; should be sorted before hashing")
	}
}

func TestComputeSelfHash_ChangesWithSpec(t *testing.T) {
	s1 := baseSpec()
	h1 := ComputeSelfHash(s1)

	for _, mut := range []struct {
		name string
		m    func(*keystonev1alpha1.AuditEntrySpec)
	}{
		{"sequence", func(s *keystonev1alpha1.AuditEntrySpec) { s.Sequence++ }},
		{"verb", func(s *keystonev1alpha1.AuditEntrySpec) { s.Verb = "delete" }},
		{"name", func(s *keystonev1alpha1.AuditEntrySpec) { s.ResourceRef.Name = "mb-v2" }},
		{"reason", func(s *keystonev1alpha1.AuditEntrySpec) { s.Reason = "Different" }},
		{"timestamp", func(s *keystonev1alpha1.AuditEntrySpec) {
			s.Timestamp = metav1.NewTime(s.Timestamp.Time.Add(time.Second))
		}},
		{"prevHash", func(s *keystonev1alpha1.AuditEntrySpec) { s.PrevHash = strings.Repeat("a", 64) }},
	} {
		t.Run(mut.name, func(t *testing.T) {
			s2 := baseSpec()
			mut.m(&s2)
			h2 := ComputeSelfHash(s2)
			if h1 == h2 {
				t.Errorf("mutating %s did not change hash (collision or missing field in canonical form)", mut.name)
			}
		})
	}
}

func TestVerifyEntry_Tampered(t *testing.T) {
	s := baseSpec()
	s.SelfHash = ComputeSelfHash(s)

	// Tamper with the reason AFTER the hash was computed — simulates
	// a post-hoc attacker with apiserver write access.
	s.Reason = "I was never here"
	if err := VerifyEntry(s); err == nil {
		t.Errorf("expected VerifyEntry to detect tamper; got nil")
	}
}

func TestVerifyEntry_Clean(t *testing.T) {
	s := baseSpec()
	s.SelfHash = ComputeSelfHash(s)
	if err := VerifyEntry(s); err != nil {
		t.Errorf("unexpected VerifyEntry error on clean spec: %v", err)
	}
}

func TestVerifyChain_CleanChain(t *testing.T) {
	entries := buildChain(t, 5)
	n, err := VerifyChain(entries)
	if err != nil {
		t.Fatalf("clean chain rejected: %v", err)
	}
	if n != 5 {
		t.Errorf("verified %d, want 5", n)
	}
}

func TestVerifyChain_BrokenLink(t *testing.T) {
	entries := buildChain(t, 4)
	// Break position 2 by changing its spec without updating SelfHash.
	entries[2].Spec.Reason = "tampered"
	_, err := VerifyChain(entries)
	if err == nil {
		t.Errorf("expected tampered chain to fail verification")
	}
}

func TestVerifyChain_GapInSequence(t *testing.T) {
	entries := buildChain(t, 5)
	// Remove entry 3 → sequence gap.
	entries = append(entries[:2], entries[3:]...)
	_, err := VerifyChain(entries)
	if err == nil {
		t.Errorf("expected sequence-gap chain to fail verification")
	}
}

func TestVerifyChain_BrokenPrevHash(t *testing.T) {
	entries := buildChain(t, 4)
	// Change entry 2's PrevHash to a bogus hex string of the right
	// shape; Verify should catch it via the chain walk.
	entries[2].Spec.PrevHash = strings.Repeat("f", 64)
	entries[2].Spec.SelfHash = ComputeSelfHash(entries[2].Spec)
	_, err := VerifyChain(entries)
	if err == nil {
		t.Errorf("expected broken prevHash chain to fail verification")
	}
}

// buildChain produces n valid, linked entries starting from sequence 1.
func buildChain(t *testing.T, n int) []keystonev1alpha1.AuditEntry {
	t.Helper()
	entries := make([]keystonev1alpha1.AuditEntry, n)
	prevHash := GenesisHash
	for i := 0; i < n; i++ {
		spec := baseSpec()
		spec.Sequence = int64(i + 1)
		spec.Reason = "entry-" + string(rune('a'+i))
		spec.PrevHash = prevHash
		spec.Timestamp = metav1.NewTime(time.Date(2026, 4, 16, 12, i, 0, 0, time.UTC))
		spec.SelfHash = ComputeSelfHash(spec)
		entries[i] = keystonev1alpha1.AuditEntry{Spec: spec}
		prevHash = spec.SelfHash
	}
	return entries
}
