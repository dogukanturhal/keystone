// SPDX-License-Identifier: AGPL-3.0-or-later

// Package devdb manages ephemeral database schemas for semantic
// migration analysis. Instead of shelling out to external tools or
// spinning up CNPG ephemeral clusters (too heavy for a 10-second lint
// run), the pool creates temporary schemas inside a long-lived dev
// database, replays migrations, inspects the result, then drops the
// schema.
//
// This is Keystone's equivalent of Atlas's --dev-url pattern: every
// migration is validated against a real PostgreSQL instance, catching
// semantic issues that AST-based static analysis misses (type
// coercions, invalid defaults, FK reference errors, NOT NULL on
// nullable data, etc.).
package devdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dogukanturhal/keystone-sdk/go/drift"
	"github.com/dogukanturhal/keystone-sdk/go/sqlquote"
)

// Pool manages connections to a dev database and provides ephemeral
// schema lifecycle for migration replay + inspection.
type Pool struct {
	pool *pgxpool.Pool
}

// NewPool creates a DevDatabasePool bound to the given pgx pool.
// The pool MUST point to a database dedicated to dev/lint use.
func NewPool(pool *pgxpool.Pool) *Pool {
	return &Pool{pool: pool}
}

// Session represents an ephemeral schema for a single lint run.
// Create with Acquire, clean up with Release.
type Session struct {
	pool       *pgxpool.Pool
	SchemaName string
	inspector  *drift.Inspector
}

// Acquire creates a temporary schema with a random name and returns
// a Session. The caller MUST call Release when done.
func (p *Pool) Acquire(ctx context.Context) (*Session, error) {
	name, err := randomSchemaName()
	if err != nil {
		return nil, fmt.Errorf("generate schema name: %w", err)
	}

	// CREATE SCHEMA — the name is hex-safe, no injection risk.
	_, err = p.pool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", sqlquote.QuoteIdentifier(name)))
	if err != nil {
		return nil, fmt.Errorf("create dev schema %s: %w", name, err)
	}

	return &Session{
		pool:       p.pool,
		SchemaName: name,
		inspector:  drift.NewInspector(p.pool),
	}, nil
}

// Release drops the ephemeral schema and all objects within it.
func (s *Session) Release(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", sqlquote.QuoteIdentifier(s.SchemaName)))
	return err
}

// ReplayStatements executes a list of SQL statements against the
// ephemeral schema. Each statement is run in the schema's search_path.
// Returns the index and error of the first failing statement, or
// (len(stmts), nil) if all succeed.
//
// Uses a single acquired connection (not the pool) to ensure
// search_path persists across all statements.
func (s *Session) ReplayStatements(ctx context.Context, stmts []string) (failedAt int, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	_, err = conn.Exec(ctx,
		fmt.Sprintf("SET search_path TO %s, public", sqlquote.QuoteIdentifier(s.SchemaName)))
	if err != nil {
		return 0, fmt.Errorf("set search_path: %w", err)
	}

	for i, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, execErr := conn.Exec(ctx, stmt); execErr != nil {
			return i, execErr
		}
	}
	return len(stmts), nil
}

// ReplaySQL executes raw SQL (potentially multi-statement) against
// the ephemeral schema. Used for replaying migration file contents.
//
// Uses a single acquired connection to ensure search_path persists.
func (s *Session) ReplaySQL(ctx context.Context, sql string) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	_, err = conn.Exec(ctx,
		fmt.Sprintf("SET search_path TO %s, public", sqlquote.QuoteIdentifier(s.SchemaName)))
	if err != nil {
		return fmt.Errorf("set search_path: %w", err)
	}

	_, err = conn.Exec(ctx, sql)
	return err
}

// Inspect returns a drift.Snapshot of the ephemeral schema's current
// state, using the same information_schema introspection as the drift
// inspector. This is the "after" snapshot for semantic diffing.
func (s *Session) Inspect(ctx context.Context) (*drift.Snapshot, error) {
	return s.inspector.Inspect(ctx, s.SchemaName)
}

// randomSchemaName generates a collision-resistant schema name for
// ephemeral use: "ks_dev_" + 8 random hex bytes.
func randomSchemaName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ks_dev_" + hex.EncodeToString(b), nil
}
