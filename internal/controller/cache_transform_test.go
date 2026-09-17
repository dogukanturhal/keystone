// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// fixtureAuditEntry returns a representative AuditEntry shaped like
// what audit.Logger.Append would build for a SchemaDefinition reconcile.
// Before/After are non-trivial (a few KB) to model the real footprint.
func fixtureAuditEntry() *keystonev1alpha1.AuditEntry {
	bigSnapshot := make([]byte, 4096)
	for i := range bigSnapshot {
		bigSnapshot[i] = 'x'
	}
	return &keystonev1alpha1.AuditEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "entry-00000000000000012345",
			Namespace:       "keystone-system",
			ResourceVersion: "9001",
		},
		Spec: keystonev1alpha1.AuditEntrySpec{
			Sequence:  12345,
			Verb:      "apply",
			Outcome:   "success",
			Reason:    "successfully applied 1 statements",
			Timestamp: metav1.Now(),
			Actor: keystonev1alpha1.AuditActor{
				Username: "system:serviceaccount:keystone-system:keystone-manager",
			},
			ResourceRef: keystonev1alpha1.AuditResourceRef{
				APIVersion: "keystone.hexxlock.io/v1alpha1",
				Kind:       "MigrationBundle",
				Namespace:  "keystone-system",
				Name:       "example-service-public-desired-9d589396eb44",
			},
			Before:   string(bigSnapshot),
			After:    string(bigSnapshot),
			PrevHash: "abcdef0123456789",
			SelfHash: "fedcba9876543210",
		},
	}
}

func TestStripAuditEntryPayload_StripsBeforeAndAfter(t *testing.T) {
	in := fixtureAuditEntry()
	beforeBytes := len(in.Spec.Before)
	afterBytes := len(in.Spec.After)
	if beforeBytes < 1024 || afterBytes < 1024 {
		t.Fatalf("fixture should carry sizeable Before/After; got before=%d after=%d", beforeBytes, afterBytes)
	}

	out, err := StripAuditEntryPayload(in)
	if err != nil {
		t.Fatalf("transform returned error: %v", err)
	}
	got, ok := out.(*keystonev1alpha1.AuditEntry)
	if !ok {
		t.Fatalf("transform changed object type: got %T", out)
	}
	if got.Spec.Before != "" {
		t.Errorf("Before not stripped (len=%d)", len(got.Spec.Before))
	}
	if got.Spec.After != "" {
		t.Errorf("After not stripped (len=%d)", len(got.Spec.After))
	}
}

func TestStripAuditEntryPayload_PreservesChainAndDispatchFields(t *testing.T) {
	in := fixtureAuditEntry()
	wantSeq := in.Spec.Sequence
	wantSelfHash := in.Spec.SelfHash
	wantPrevHash := in.Spec.PrevHash
	wantTimestamp := in.Spec.Timestamp
	wantVerb := in.Spec.Verb
	wantOutcome := in.Spec.Outcome
	wantReason := in.Spec.Reason
	wantActor := in.Spec.Actor.Username
	wantResource := in.Spec.ResourceRef.Name
	wantName := in.Name
	wantRV := in.ResourceVersion

	out, err := StripAuditEntryPayload(in)
	if err != nil {
		t.Fatalf("transform returned error: %v", err)
	}
	got := out.(*keystonev1alpha1.AuditEntry)

	// Chain integrity fields — used by AuditLogReconciler and the
	// off-cluster archive verification path.
	if got.Spec.Sequence != wantSeq {
		t.Errorf("Sequence drifted: got %d want %d", got.Spec.Sequence, wantSeq)
	}
	if got.Spec.SelfHash != wantSelfHash {
		t.Errorf("SelfHash drifted: got %q want %q", got.Spec.SelfHash, wantSelfHash)
	}
	if got.Spec.PrevHash != wantPrevHash {
		t.Errorf("PrevHash drifted: got %q want %q", got.Spec.PrevHash, wantPrevHash)
	}
	if !got.Spec.Timestamp.Equal(&wantTimestamp) {
		t.Errorf("Timestamp drifted: got %v want %v", got.Spec.Timestamp, wantTimestamp)
	}

	// Dispatch / SIEM fields — used by the retention controller's
	// cutoff filter and the S3 archiver's ECS record builder.
	if got.Spec.Verb != wantVerb {
		t.Errorf("Verb drifted: got %q want %q", got.Spec.Verb, wantVerb)
	}
	if got.Spec.Outcome != wantOutcome {
		t.Errorf("Outcome drifted: got %q want %q", got.Spec.Outcome, wantOutcome)
	}
	if got.Spec.Reason != wantReason {
		t.Errorf("Reason drifted: got %q want %q", got.Spec.Reason, wantReason)
	}
	if got.Spec.Actor.Username != wantActor {
		t.Errorf("Actor.Username drifted: got %q want %q", got.Spec.Actor.Username, wantActor)
	}
	if got.Spec.ResourceRef.Name != wantResource {
		t.Errorf("ResourceRef.Name drifted: got %q want %q", got.Spec.ResourceRef.Name, wantResource)
	}

	// ObjectMeta — the cache uses Name/Namespace/UID/ResourceVersion
	// to key indices and must keep them intact.
	if got.Name != wantName {
		t.Errorf("Name drifted: got %q want %q", got.Name, wantName)
	}
	if got.ResourceVersion != wantRV {
		t.Errorf("ResourceVersion drifted: got %q want %q", got.ResourceVersion, wantRV)
	}
}

func TestStripAuditEntryPayload_NilInput(t *testing.T) {
	out, err := StripAuditEntryPayload(nil)
	if err != nil {
		t.Errorf("nil input returned error: %v", err)
	}
	if out != nil {
		t.Errorf("nil input should pass through unchanged: got %T", out)
	}
}

func TestStripAuditEntryPayload_NonAuditEntry(t *testing.T) {
	// A foreign type passing through (defensive — controller-runtime
	// only routes AuditEntry through a per-type transform, so this
	// is only reached on partial-object metadata or a future code
	// path that mis-keys the ByObject map).
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "default"},
		Data:       map[string]string{"ca.crt": "PEM-DATA"},
	}
	out, err := StripAuditEntryPayload(cm)
	if err != nil {
		t.Errorf("non-AuditEntry returned error: %v", err)
	}
	got, ok := out.(*corev1.ConfigMap)
	if !ok {
		t.Fatalf("transform changed object type: got %T", out)
	}
	if got.Data["ca.crt"] != "PEM-DATA" {
		t.Errorf("transform mutated foreign object: got Data[%q]=%q", "ca.crt", got.Data["ca.crt"])
	}
}

func TestStripAuditEntryPayload_Idempotent(t *testing.T) {
	in := fixtureAuditEntry()
	out1, err := StripAuditEntryPayload(in)
	if err != nil {
		t.Fatalf("first transform returned error: %v", err)
	}
	out2, err := StripAuditEntryPayload(out1)
	if err != nil {
		t.Fatalf("second transform returned error: %v", err)
	}
	got := out2.(*keystonev1alpha1.AuditEntry)
	if got.Spec.Before != "" || got.Spec.After != "" {
		t.Errorf("idempotent re-strip drifted: before=%q after=%q",
			got.Spec.Before, got.Spec.After)
	}
}

func TestStripAuditEntryPayload_AlreadyEmpty(t *testing.T) {
	// Post-archive entries (or freshly-created ones with no diff
	// payload) carry empty Before/After. The transform should be a
	// no-op pass-through, NOT trigger a copy.
	in := &keystonev1alpha1.AuditEntry{
		Spec: keystonev1alpha1.AuditEntrySpec{
			Sequence: 1,
			Verb:     "create",
			SelfHash: "deadbeef",
		},
	}
	out, err := StripAuditEntryPayload(in)
	if err != nil {
		t.Fatalf("empty input returned error: %v", err)
	}
	got := out.(*keystonev1alpha1.AuditEntry)
	if got.Spec.SelfHash != "deadbeef" {
		t.Errorf("transform corrupted empty entry's SelfHash: got %q", got.Spec.SelfHash)
	}
}

// CanonicalBytes-equivalence guard — assert that the transform never
// affects the canonical-encoding inputs used by the on-cluster chain
// (Sequence, Verb, Outcome, Reason, PrevHash, Timestamp, Actor,
// ResourceRef). Prevents a future change to the strip set from silently
// breaking VerifyEntry on uncached reads.
//
// Note: this test does NOT assert that ComputeSelfHash(stripped.Spec)
// equals ComputeSelfHash(original.Spec) — they DO differ (Before/After
// are part of the canonical encoding by design), and that's fine
// because chain verification reads uncached. The test guards only the
// fields we promise to preserve.
func TestStripAuditEntryPayload_NoFieldDriftBeyondPayload(t *testing.T) {
	in := fixtureAuditEntry()
	original := *in
	originalSpec := in.Spec
	originalActor := in.Spec.Actor
	originalRef := in.Spec.ResourceRef

	out, _ := StripAuditEntryPayload(in)
	got := out.(*keystonev1alpha1.AuditEntry)

	if got.Name != original.Name {
		t.Errorf("Name drifted")
	}
	if got.Namespace != original.Namespace {
		t.Errorf("Namespace drifted")
	}
	if got.Spec.Sequence != originalSpec.Sequence {
		t.Errorf("Sequence drifted")
	}
	if got.Spec.Verb != originalSpec.Verb {
		t.Errorf("Verb drifted")
	}
	if got.Spec.Outcome != originalSpec.Outcome {
		t.Errorf("Outcome drifted")
	}
	if got.Spec.Reason != originalSpec.Reason {
		t.Errorf("Reason drifted")
	}
	if got.Spec.PrevHash != originalSpec.PrevHash {
		t.Errorf("PrevHash drifted")
	}
	if got.Spec.SelfHash != originalSpec.SelfHash {
		t.Errorf("SelfHash drifted")
	}
	if !got.Spec.Timestamp.Equal(&originalSpec.Timestamp) {
		t.Errorf("Timestamp drifted")
	}
	if got.Spec.Actor.Username != originalActor.Username {
		t.Errorf("Actor.Username drifted")
	}
	if got.Spec.ResourceRef != originalRef {
		t.Errorf("ResourceRef drifted")
	}
}
