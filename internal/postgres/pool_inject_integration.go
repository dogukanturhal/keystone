// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package postgres

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// InjectForTest pre-seeds the pool cache with an already-opened *pgxpool.Pool
// under the key derived from cfg. Used by integration tests that drive the
// reconcilers against a testcontainers PostgreSQL where the API-surface
// sslMode enum ("require"|"verify-ca"|"verify-full") cannot be downgraded
// to "disable" but the container does not terminate TLS. The test opens its
// own pool with sslmode=disable, then injects it under the cache key the
// reconciler will compute from the provider CR — so Acquire returns the
// test-owned pool instead of dialing anew.
//
// Only compiled with the `integration` build tag so the production binary
// never carries this test hook.
func (pc *PoolCache) InjectForTest(cfg PoolConfig, pool *pgxpool.Pool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.pools[cfg.Key()] = &cachedPool{
		pool:     pool,
		cfg:      cfg,
		lastUsed: time.Now(),
	}
}
