// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestAcquireTenant_EmptyTenantBypasses(t *testing.T) {
	pc := NewPoolCache()
	// Burning through a tight loop would exhaust ANY rate limit; if
	// empty tenant skips the limiter, this completes immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	for i := 0; i < 100; i++ {
		if err := pc.acquireTenant(ctx, ""); err != nil {
			t.Fatalf("empty tenant should bypass rate limiter: %v", err)
		}
	}
}

func TestAcquireTenant_BurstThenWait(t *testing.T) {
	pc := NewPoolCache()
	pc.TenantLimitsFn = func(_ string) TenantLimits {
		// Tight budget — 1 RPS, 2 burst. First 2 acquires instant,
		// third waits ~1s.
		return TenantLimits{Rate: 1, Burst: 2}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := pc.acquireTenant(ctx, "tenant-a"); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 500*time.Millisecond {
		t.Errorf("3 acquires at 1rps/2burst should take ~1s; took %v", elapsed)
	}
}

func TestAcquireTenant_DeadlineShorterThanWait(t *testing.T) {
	pc := NewPoolCache()
	pc.TenantLimitsFn = func(_ string) TenantLimits {
		// 0.5 RPS, burst 1 — second acquire needs 2s to refill.
		return TenantLimits{Rate: 0.5, Burst: 1}
	}

	// First acquire consumes the burst.
	if err := pc.acquireTenant(context.Background(), "tenant-b"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Second acquire with 100ms deadline — wait can't complete.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := pc.acquireTenant(ctx, "tenant-b")
	if err == nil {
		t.Fatalf("expected rate-limit error")
	}
	if !errors.Is(err, ErrTenantRateLimited) {
		t.Errorf("expected ErrTenantRateLimited, got %v", err)
	}
}

func TestAcquireTenant_PerTenantIsolation(t *testing.T) {
	pc := NewPoolCache()
	pc.TenantLimitsFn = func(_ string) TenantLimits {
		return TenantLimits{Rate: 1, Burst: 1}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Each tenant gets their own burst — two tenants can both acquire
	// immediately.
	if err := pc.acquireTenant(ctx, "tenant-x"); err != nil {
		t.Fatalf("tenant-x: %v", err)
	}
	if err := pc.acquireTenant(ctx, "tenant-y"); err != nil {
		t.Fatalf("tenant-y: %v", err)
	}
}

func TestSweepIdleTenants_RemovesStale(t *testing.T) {
	pc := NewPoolCache()
	pc.TenantLimitsFn = func(_ string) TenantLimits {
		return TenantLimits{Rate: 10, Burst: 10}
	}

	ctx := context.Background()
	_ = pc.acquireTenant(ctx, "stale")
	_ = pc.acquireTenant(ctx, "fresh")

	// Age "stale" by rewriting lastUsed.
	pc.rateMu.Lock()
	pc.tenantBuckets["stale"].lastUsed = time.Now().Add(-1 * time.Hour)
	pc.rateMu.Unlock()

	pc.sweepIdleTenants(30 * time.Minute)

	pc.rateMu.Lock()
	defer pc.rateMu.Unlock()
	if _, ok := pc.tenantBuckets["stale"]; ok {
		t.Errorf("stale tenant not swept")
	}
	if _, ok := pc.tenantBuckets["fresh"]; !ok {
		t.Errorf("fresh tenant swept prematurely")
	}
}

// Silence unused rate package if the imports shift in future refactors.
var _ = rate.Every
