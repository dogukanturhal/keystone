// SPDX-License-Identifier: AGPL-3.0-or-later

package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEffectiveRetentionDays(t *testing.T) {
	// Unset or nonsensical values must resolve to the long default, not
	// to zero — zero would make every entry immediately deletable.
	for _, in := range []int32{0, -1, -2555} {
		if got := EffectiveRetentionDays(in); got != DefaultAuditRetentionDays {
			t.Errorf("EffectiveRetentionDays(%d) = %d, want %d", in, got, DefaultAuditRetentionDays)
		}
	}
	if got := EffectiveRetentionDays(90); got != 90 {
		t.Errorf("EffectiveRetentionDays(90) = %d, want 90", got)
	}
}

func TestRetentionCutoff(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	if got, want := RetentionCutoff(90, now), now.AddDate(0, 0, -90); !got.Equal(want) {
		t.Errorf("RetentionCutoff(90) = %s, want %s", got, want)
	}
	// The default must be substituted before the arithmetic, not after.
	if got, want := RetentionCutoff(0, now),
		now.Add(-time.Duration(DefaultAuditRetentionDays)*24*time.Hour); !got.Equal(want) {
		t.Errorf("RetentionCutoff(0) = %s, want %s", got, want)
	}
}

func TestIsRetentionExpired_Boundary(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	cutoff := RetentionCutoff(90, now)

	mk := func(ts time.Time) *AuditEntry {
		return &AuditEntry{Spec: AuditEntrySpec{Timestamp: metav1.NewTime(ts)}}
	}

	// Strictly before the cutoff is expired; exactly at it is not. The
	// retention controller uses Time.Before, and the admission webhook
	// must agree exactly or it rejects the controller's own deletes.
	if !mk(cutoff.Add(-time.Nanosecond)).IsRetentionExpired(90, now) {
		t.Error("an entry one nanosecond before the cutoff must be expired")
	}
	if mk(cutoff).IsRetentionExpired(90, now) {
		t.Error("an entry exactly at the cutoff must not yet be expired")
	}
	if mk(now).IsRetentionExpired(90, now) {
		t.Error("a brand-new entry must never be expired")
	}
}
