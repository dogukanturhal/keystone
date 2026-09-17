// SPDX-License-Identifier: AGPL-3.0-or-later

// Package postgres holds the admin-side PostgreSQL client used by the
// SchemaController. It is intentionally minimal: open a pool keyed by
// (provider name, target database), execute idempotent DDL, return
// structured errors. Anything that resembles application-level query
// building does not belong here.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig describes a single pgx pool the manager will hold against
// one (provider, database) pair. Connections are not pre-warmed; the pool
// dials on first use and survives controller restarts via the cache below.
type PoolConfig struct {
	// Host is the PostgreSQL host the pool dials.
	Host string

	// Port is the TCP port; defaults to 5432 if zero.
	Port int

	// Database is the target database (defaults to "postgres" — caller
	// MUST set this to the maintenance database for cluster-level DDL or
	// to the LogicalDatabase target for schema-level DDL).
	Database string

	// Username is the admin user resolved from the provider's Secret.
	Username string

	// Password is the admin password resolved from the provider's Secret.
	Password string

	// SSLMode mirrors libpq sslmode values; "verify-full" is the safe
	// default and what the API surface enforces. "disable" is rejected.
	SSLMode string

	// MaxConns caps pool size. Default 4 if zero. Must match the provider
	// CR's poolMaxConns to make capacity planning predictable.
	MaxConns int32
}

func (c PoolConfig) port() int {
	if c.Port == 0 {
		return 5432
	}
	return c.Port
}

func (c PoolConfig) database() string {
	if c.Database == "" {
		return "postgres"
	}
	return c.Database
}

func (c PoolConfig) sslMode() string {
	if c.SSLMode == "" {
		return "verify-full"
	}
	return c.SSLMode
}

func (c PoolConfig) maxConns() int32 {
	if c.MaxConns == 0 {
		return 4
	}
	return c.MaxConns
}

// connString builds a libpq-style connection URL. All values are properly
// URL-escaped — operator passwords commonly contain special characters.
func (c PoolConfig) connString() string {
	host := fmt.Sprintf("%s:%d", c.Host, c.port())
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.Username, c.Password),
		Host:   host,
		Path:   "/" + c.database(),
	}
	q := url.Values{}
	q.Set("sslmode", c.sslMode())
	q.Set("application_name", "keystone-manager")
	q.Set("statement_timeout", "30000")           // 30s — DDL on busy systems
	q.Set("lock_timeout", "5000")                 //  5s — refuse silent waits
	q.Set("idle_in_transaction_session_timeout", "60000")
	u.RawQuery = q.Encode()
	return u.String()
}

// Key returns a stable cache key for the (provider, database) pair. Uses
// a SHA-256 of the connection string so password rotations result in a new
// pool (the old one is GC'd on next eviction sweep).
func (c PoolConfig) Key() string {
	h := sha256.Sum256([]byte(c.connString()))
	return hex.EncodeToString(h[:8])
}

// PoolCache is a goroutine-safe pgxpool registry. The SchemaController
// reuses pools across reconciles to avoid the cost of dialing for every
// CRUD operation against the same provider.
//
// Rate-limit state (tenantBuckets, rateMu, TenantLimitsFn, rateOnce,
// rateCancel) lives alongside for cohesion — see rate_limit.go for the
// per-tenant token-bucket implementation introduced in Phase 12.
type PoolCache struct {
	mu    sync.Mutex
	pools map[string]*cachedPool

	// Per-tenant rate-limit fields. Safe to leave zero-valued — the
	// first AcquireTenant call lazily populates the map and limiter.
	rateMu         sync.Mutex
	tenantBuckets  map[string]*tenantBucket
	rateOnce       sync.Once
	rateCancel     context.CancelFunc
	TenantLimitsFn func(tenant string) TenantLimits
}

type cachedPool struct {
	pool     *pgxpool.Pool
	cfg      PoolConfig
	lastUsed time.Time
}

// NewPoolCache returns an empty cache.
func NewPoolCache() *PoolCache {
	return &PoolCache{
		pools:         make(map[string]*cachedPool),
		tenantBuckets: make(map[string]*tenantBucket),
	}
}

// Acquire returns a pool for the given config, opening one if necessary.
// The pool is shared — callers MUST NOT Close it. Use the returned
// release function only to update last-used bookkeeping (no-op today;
// reserved for future eviction policies).
func (pc *PoolCache) Acquire(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, func(), error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	key := cfg.Key()
	if cp, ok := pc.pools[key]; ok {
		cp.lastUsed = time.Now()
		return cp.pool, func() {}, nil
	}

	pcfg, err := pgxpool.ParseConfig(cfg.connString())
	if err != nil {
		return nil, nil, fmt.Errorf("parse pool config: %w", err)
	}
	pcfg.MaxConns = cfg.maxConns()
	pcfg.MinConns = 0
	pcfg.MaxConnLifetime = 30 * time.Minute
	pcfg.MaxConnIdleTime = 5 * time.Minute
	pcfg.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, nil, fmt.Errorf("open pool: %w", err)
	}

	pc.pools[key] = &cachedPool{
		pool:     pool,
		cfg:      cfg,
		lastUsed: time.Now(),
	}
	return pool, func() {}, nil
}

// CloseAll shuts down every pool in the cache. Call from the manager's
// cleanup path so backends are released cleanly on operator shutdown.
func (pc *PoolCache) CloseAll() {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for k, cp := range pc.pools {
		cp.pool.Close()
		delete(pc.pools, k)
	}
}

// Drop removes and closes the pool for a specific config. Used after
// credential rotation or when a DatabaseProvider is deleted — without
// this the old credentials linger in the cache forever.
func (pc *PoolCache) Drop(cfg PoolConfig) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	key := cfg.Key()
	if cp, ok := pc.pools[key]; ok {
		cp.pool.Close()
		delete(pc.pools, key)
	}
}

// DropMatching removes and closes every cached pool whose config
// matches the predicate. Used by the DatabaseProviderReconciler on
// credential rotation: the reconciler knows the provider's
// host/port/username but not the old password (passwords are never
// stored outside the Secret), so it can't call Drop directly. The
// predicate closes over the observable half of the config and drops
// every pool whose connection parameters — other than password —
// belong to the rotated provider.
//
// Returns the count of pools dropped, for metrics + logging.
func (pc *PoolCache) DropMatching(pred func(PoolConfig) bool) int {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	var dropped int
	for k, cp := range pc.pools {
		if pred(cp.cfg) {
			cp.pool.Close()
			delete(pc.pools, k)
			dropped++
		}
	}
	return dropped
}
