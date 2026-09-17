// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedPool injects a dummy pool into the cache for the given config
// without opening a real connection — the test only cares about
// the cache's bookkeeping, not PG connectivity.
func (pc *PoolCache) seedPool(cfg PoolConfig, p *pgxpool.Pool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.pools[cfg.Key()] = &cachedPool{pool: p, cfg: cfg}
}

func TestPoolCache_DropMatching(t *testing.T) {
	pc := NewPoolCache()
	// Three pools: two for provider-A (same host+port, different
	// databases), one for provider-B.
	cfgA1 := PoolConfig{Host: "a.db", Port: 5432, Database: "app1", Username: "u", Password: "p1", SSLMode: "require", MaxConns: 4}
	cfgA2 := PoolConfig{Host: "a.db", Port: 5432, Database: "app2", Username: "u", Password: "p1", SSLMode: "require", MaxConns: 4}
	cfgB1 := PoolConfig{Host: "b.db", Port: 5432, Database: "app3", Username: "u", Password: "p2", SSLMode: "require", MaxConns: 4}

	// Seed with sentinels — NOT real pools, but good enough because
	// Drop / DropMatching only call .Close(). pgxpool.Pool.Close is
	// nil-safe? It is not, so use a minimal real-ish object:
	// actually nil panics. Use a closed pool object.
	// Simplest: create a real mini-pool pointing at a bogus DSN but
	// never connect — pgxpool.New is async.
	seedWithCloseSentinel := func(cfg PoolConfig) {
		// A pgxpool.Pool is large; we need something with .Close().
		// Use a fresh pool bound to an unreachable target — Close()
		// is safe before any Acquire.
		pool, _ := pgxpool.New(t.Context(), "postgres://nowhere/db?connect_timeout=1")
		// If pgxpool.New returns nil pool on parse error, skip — but
		// `postgres://nowhere/db?connect_timeout=1` parses fine.
		pc.seedPool(cfg, pool)
	}
	seedWithCloseSentinel(cfgA1)
	seedWithCloseSentinel(cfgA2)
	seedWithCloseSentinel(cfgB1)

	if got := len(pc.pools); got != 3 {
		t.Fatalf("expected 3 cached pools after seeding; got %d", got)
	}

	// Drop everything matching provider-A (host="a.db", port=5432).
	dropped := pc.DropMatching(func(cfg PoolConfig) bool {
		return cfg.Host == "a.db" && cfg.Port == 5432
	})
	if dropped != 2 {
		t.Errorf("DropMatching returned %d; want 2", dropped)
	}
	if got := len(pc.pools); got != 1 {
		t.Errorf("expected 1 remaining pool; got %d", got)
	}
	// Remaining entry is for provider-B.
	if _, ok := pc.pools[cfgB1.Key()]; !ok {
		t.Errorf("provider-B pool should remain; got keys=%v", poolKeys(pc))
	}
}

func TestPoolCache_DropMatching_NoMatch(t *testing.T) {
	pc := NewPoolCache()
	cfg := PoolConfig{Host: "a.db", Port: 5432, Database: "app1", Username: "u", Password: "p1", SSLMode: "require", MaxConns: 4}
	pool, _ := pgxpool.New(t.Context(), "postgres://nowhere/db?connect_timeout=1")
	pc.seedPool(cfg, pool)

	dropped := pc.DropMatching(func(cfg PoolConfig) bool { return cfg.Host == "nomatch" })
	if dropped != 0 {
		t.Errorf("expected 0 dropped; got %d", dropped)
	}
	if got := len(pc.pools); got != 1 {
		t.Errorf("pool should remain; got %d entries", got)
	}
}

func TestPoolCache_DropMatching_EmptyCache(t *testing.T) {
	pc := NewPoolCache()
	dropped := pc.DropMatching(func(cfg PoolConfig) bool { return true })
	if dropped != 0 {
		t.Errorf("empty cache DropMatching should return 0; got %d", dropped)
	}
}

// poolKeys returns the cache keys for debug output.
func poolKeys(pc *PoolCache) []string {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	keys := make([]string, 0, len(pc.pools))
	for k := range pc.pools {
		keys = append(keys, k)
	}
	return keys
}
