// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// fixedNow is the reference instant these tests reason from.
var fixedNow = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func auditLog(retentionDays int32) *keystonev1alpha1.AuditLog {
	return &keystonev1alpha1.AuditLog{
		ObjectMeta: metav1.ObjectMeta{Name: keystonev1alpha1.AuditLogName},
		Spec:       keystonev1alpha1.AuditLogSpec{RetentionDays: retentionDays},
	}
}

func auditEntry(age time.Duration) *keystonev1alpha1.AuditEntry {
	return &keystonev1alpha1.AuditEntry{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "e1"},
		Spec: keystonev1alpha1.AuditEntrySpec{
			Sequence:  42,
			Timestamp: metav1.NewTime(fixedNow.Add(-age)),
		},
	}
}

func newAuditValidator(alog *keystonev1alpha1.AuditLog) *AuditEntryValidator {
	b := newFakeClient()
	if alog != nil {
		b = b.WithObjects(alog)
	}
	return &AuditEntryValidator{
		Client: b.Build(),
		nowFn:  func() time.Time { return fixedNow },
	}
}

func TestAuditEntryValidateDelete_RejectsEntryInsideRetention(t *testing.T) {
	v := newAuditValidator(auditLog(90))
	// One day old against a 90-day window.
	_, err := v.ValidateDelete(context.Background(), auditEntry(24*time.Hour))
	if err == nil {
		t.Fatal("an entry inside the retention window must not be deletable")
	}
	for _, want := range []string{"retention", "90"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestAuditEntryValidateDelete_AllowsExpiredEntry(t *testing.T) {
	v := newAuditValidator(auditLog(90))
	// 91 days old against a 90-day window.
	if _, err := v.ValidateDelete(context.Background(), auditEntry(91*24*time.Hour)); err != nil {
		t.Fatalf("an expired entry must be deletable so the retention controller can prune: %v", err)
	}
}

func TestAuditEntryValidateDelete_BoundaryMatchesRetentionController(t *testing.T) {
	// The retention controller selects entries with
	// Spec.Timestamp.Before(RetentionCutoff(...)). The webhook must
	// accept exactly that set: if the webhook is even marginally
	// stricter, the controller's own deletes are rejected and entries
	// accumulate in etcd forever.
	const retention int32 = 90
	cutoff := keystonev1alpha1.RetentionCutoff(retention, fixedNow)
	v := newAuditValidator(auditLog(retention))

	justExpired := &keystonev1alpha1.AuditEntry{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "just-expired"},
		Spec: keystonev1alpha1.AuditEntrySpec{
			Sequence:  1,
			Timestamp: metav1.NewTime(cutoff.Add(-time.Nanosecond)),
		},
	}
	if !justExpired.IsRetentionExpired(retention, fixedNow) {
		t.Fatal("fixture error: entry should be selected by the controller predicate")
	}
	if _, err := v.ValidateDelete(context.Background(), justExpired); err != nil {
		t.Fatalf("webhook rejected an entry the controller would delete — "+
			"this deadlocks retention: %v", err)
	}

	atCutoff := &keystonev1alpha1.AuditEntry{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "at-cutoff"},
		Spec: keystonev1alpha1.AuditEntrySpec{
			Sequence:  2,
			Timestamp: metav1.NewTime(cutoff),
		},
	}
	if atCutoff.IsRetentionExpired(retention, fixedNow) {
		t.Fatal("fixture error: an entry exactly at the cutoff is not yet expired")
	}
	if _, err := v.ValidateDelete(context.Background(), atCutoff); err == nil {
		t.Fatal("an entry exactly at the cutoff must still be protected")
	}
}

func TestAuditEntryValidateDelete_AppliesDefaultRetentionWhenUnset(t *testing.T) {
	// RetentionDays unset must mean the ~7-year default, not "no
	// retention", or an unconfigured cluster would permit deletes.
	v := newAuditValidator(auditLog(0))
	_, err := v.ValidateDelete(context.Background(), auditEntry(365*24*time.Hour))
	if err == nil {
		t.Fatal("a one-year-old entry must still be protected under the default window")
	}
	if !strings.Contains(err.Error(), "2555") {
		t.Errorf("error should report the effective default window, got: %v", err)
	}
}

func TestAuditEntryValidateDelete_FailsClosedWithoutAuditLog(t *testing.T) {
	// No AuditLog means the window is unknown. Deleting evidence on the
	// strength of a failed lookup is the wrong default.
	v := newAuditValidator(nil)
	_, err := v.ValidateDelete(context.Background(), auditEntry(100*365*24*time.Hour))
	if err == nil {
		t.Fatal("must fail closed when the retention window cannot be read")
	}
	if !strings.Contains(err.Error(), "cannot read AuditLog") {
		t.Errorf("error should explain the lookup failure, got: %v", err)
	}
}

func TestAuditEntryValidateDelete_FailsClosedWithoutClient(t *testing.T) {
	v := &AuditEntryValidator{nowFn: func() time.Time { return fixedNow }}
	if _, err := v.ValidateDelete(context.Background(), auditEntry(100*365*24*time.Hour)); err == nil {
		t.Fatal("a validator with no client must refuse deletes, not allow them")
	}
}

func TestAuditEntryValidateUpdate_AlwaysRejected(t *testing.T) {
	v := newAuditValidator(auditLog(90))
	old := auditEntry(24 * time.Hour)
	if _, err := v.ValidateUpdate(context.Background(), old, old.DeepCopy()); err == nil {
		t.Fatal("audit entries are immutable; updates must never be permitted")
	}
}
