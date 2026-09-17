// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/time/rate"
)

// ErrTenantRateLimited is returned by PoolCache.AcquireTenant when the
// per-tenant token bucket is empty and the caller's deadline would not
// allow waiting for a refill. Callers should surface this as
// `Ready=False, reason=RateLimited` on the CR; the workqueue's
// exponential backoff handles retry pacing.
var ErrTenantRateLimited = errors.New("postgres: tenant rate-limited on pool acquire")

// TenantLimits is the budget a single tenant (namespace) gets for
// pool-acquire operations. Defaults match "sensible for a tier-1
// shared cluster": 10 acquires/sec steady state with a 20-acquire
// burst for reconcile storms.
//
// Rationale: in the reconciler-heavy workload Keystone runs, a
// tenant that creates 100 MigrationBundles in quick succession
// would otherwise each acquire a pool in parallel, thrashing the
// provider's admin connection. Token buckets smooth that without
// changing the CRD UX.
type TenantLimits struct {
	// Rate is the steady-state acquires-per-second floor.
	Rate rate.Limit

	// Burst is the token bucket capacity. Reconcile storms up to this
	// many acquires proceed immediately; beyond that, callers wait or
	// get ErrTenantRateLimited.
	Burst int
}

// DefaultTenantLimits returns the production defaults. Operators can
// override per-namespace via annotations (Phase 12.3) or cluster-wide
// by constructing a PoolCache with a custom TenantLimitsFn.
func DefaultTenantLimits() TenantLimits {
	return TenantLimits{Rate: 10, Burst: 20}
}

// tenantBucket wraps a rate.Limiter with last-use bookkeeping so the
// PoolCache can garbage-collect tenants that haven't acquired recently.
type tenantBucket struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

// acquireTenant fetches or creates the tenant's bucket and attempts
// to reserve one token within the caller's context.
//
// Implementation lives on the PoolCache because the cache already
// owns the per-provider state; colocating the per-tenant limiter
// keeps the surface narrow.
func (pc *PoolCache) acquireTenant(ctx context.Context, tenant string) error {
	if tenant == "" {
		// No tenant label — not rate-limited. Applies to cluster-
		// scoped reconcilers (DatabaseProvider, etc.) that aren't
		// associated with a namespace.
		return nil
	}
	pc.rateMu.Lock()
	bucket, ok := pc.tenantBuckets[tenant]
	if !ok {
		limits := pc.resolveLimits(tenant)
		bucket = &tenantBucket{
			limiter: rate.NewLimiter(limits.Rate, limits.Burst),
		}
		pc.tenantBuckets[tenant] = bucket
	}
	bucket.lastUsed = time.Now()
	pc.rateMu.Unlock()

	// Wait within the caller's deadline. When the context has no
	// deadline, Wait will block until a token is available — in
	// practice controller-runtime reconciles carry a deadline, so
	// this is safe.
	if err := bucket.limiter.Wait(ctx); err != nil {
		// `rate.Wait` returns an error in two cases:
		//   1. The context was cancelled / deadline elapsed
		//   2. The reservation would exceed the context deadline —
		//      rate.Limiter returns its own string not wrapping
		//      ctx.Err, so errors.Is won't catch it
		// Both are callers-got-rate-limited from our perspective.
		return fmt.Errorf("%w: tenant=%q deadline=%v: %v",
			ErrTenantRateLimited, tenant, deadlineOrNever(ctx), err)
	}
	return nil
}

// resolveLimits returns the effective limits for a tenant. Checks the
// per-tenant override map first (future Phase 12.3 feature), then
// falls back to the cache-level default.
//
// Not locking inside — caller holds pc.rateMu.
func (pc *PoolCache) resolveLimits(tenant string) TenantLimits {
	if pc.TenantLimitsFn != nil {
		return pc.TenantLimitsFn(tenant)
	}
	return DefaultTenantLimits()
}

// AcquireTenant is the rate-limited variant of Acquire. Reconcilers
// that identify a tenant (namespace) pass it here; cluster-scoped
// operations keep using Acquire.
//
// Returns ErrTenantRateLimited wrapped in a descriptive error when
// the bucket is empty and the caller's deadline isn't long enough to
// wait.
func (pc *PoolCache) AcquireTenant(
	ctx context.Context,
	tenant string,
	cfg PoolConfig,
) (pool interface{}, release func(), err error) {
	if err := pc.acquireTenant(ctx, tenant); err != nil {
		return nil, nil, err
	}
	// Delegate to Acquire once rate limiting passes. Returned pool
	// type preserved via interface{} to keep rate_limit.go decoupled
	// from pgxpool internals — callers cast back to *pgxpool.Pool.
	p, rel, err := pc.Acquire(ctx, cfg)
	return p, rel, err
}

// rateGCInterval is how often the PoolCache sweeps stale tenant
// buckets. 5 minutes is enough to avoid leaking across pod restarts
// but infrequent enough to not thrash under load.
const rateGCInterval = 5 * time.Minute

// sweepIdleTenants drops tenant buckets unused for longer than the
// given TTL. Called by a goroutine started in `StartRateGC`; never
// blocks the hot path.
func (pc *PoolCache) sweepIdleTenants(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)
	pc.rateMu.Lock()
	defer pc.rateMu.Unlock()
	for tenant, b := range pc.tenantBuckets {
		if b.lastUsed.Before(cutoff) {
			delete(pc.tenantBuckets, tenant)
		}
	}
}

// StartRateGC spawns a goroutine that periodically sweeps idle tenant
// buckets. Safe to call multiple times — only the first call spawns
// the goroutine. Returns the stop function.
func (pc *PoolCache) StartRateGC(ctx context.Context) func() {
	pc.rateOnce.Do(func() {
		gcCtx, cancel := context.WithCancel(ctx)
		pc.rateCancel = cancel
		go func() {
			ticker := time.NewTicker(rateGCInterval)
			defer ticker.Stop()
			for {
				select {
				case <-gcCtx.Done():
					return
				case <-ticker.C:
					pc.sweepIdleTenants(30 * time.Minute)
				}
			}
		}()
	})
	return func() {
		if pc.rateCancel != nil {
			pc.rateCancel()
		}
	}
}

// errors.New sentinel is in the const block above. The ctx import is
// kept live via AcquireTenant's signature; sync is imported by
// pool.go (shared struct) so no unused-import warnings fire.

func deadlineOrNever(ctx context.Context) string {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl).String()
	}
	return "never"
}
