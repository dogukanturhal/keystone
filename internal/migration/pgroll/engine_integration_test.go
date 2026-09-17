// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Integration tests for the pgroll engine. Covers Expand→Contract
// round-trips, Expand→Abort rollback, and the new set_not_null /
// drop_not_null operations against a real PostgreSQL running under
// testcontainers.
//
// Run with:
//
//	GOWORK=off go test -count=1 -tags integration ./internal/migration/pgroll/
//
// A single postgres:16-alpine container is shared across every test
// via sync.Once to amortise startup cost (~3-5s).
//
// Each test uses a fresh per-test schema so tests don't cross-
// contaminate; the container is shared, the schemas are isolated.

package pgroll

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// -- Shared harness ---------------------------------------------------

var (
	sharedPool = sync.OnceValue(func() *pgxpool.Pool {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		// An already-running PostgreSQL, when one is named. The shared
		// kubernetes executor exposes no Docker socket, so testcontainers
		// cannot start anything in CI and this suite skipped on every
		// pipeline; a `services:` container arrives as a plain DSN.
		if dsn := os.Getenv(testDSNEnv); dsn != "" {
			poolRequested = true
			pool, perr := pgxpool.New(ctx, dsn)
			if perr != nil {
				sharedErr = fmt.Errorf("connect %s: %w", testDSNEnv, perr)
				return nil
			}
			if perr := pool.Ping(ctx); perr != nil {
				sharedErr = fmt.Errorf("ping %s: %w", testDSNEnv, perr)
				return nil
			}
			return pool
		}

		c, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithUsername("keystone_pgroll"),
			tcpostgres.WithPassword("test"),
			tcpostgres.WithDatabase("postgres"),
			tcpostgres.BasicWaitStrategies(),
		)
		if err != nil {
			sharedErr = err
			return nil
		}
		host, err := c.Host(ctx)
		if err != nil {
			sharedErr = err
			return nil
		}
		port, err := c.MappedPort(ctx, "5432/tcp")
		if err != nil {
			sharedErr = err
			return nil
		}
		dsn := fmt.Sprintf("postgres://keystone_pgroll:test@%s:%d/postgres?sslmode=disable", host, port.Num())
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			sharedErr = err
			return nil
		}
		return pool
	})
	sharedErr error
)

// testDSNEnv names an already-running PostgreSQL to test against instead
// of starting a container. Same variable the other suites read, so one
// database serves the whole run.
const testDSNEnv = "KEYSTONE_TEST_DSN"

// poolRequested records that a DSN was named explicitly.
var poolRequested bool

func getPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p := sharedPool()
	if sharedErr != nil || p == nil {
		// Naming a database and not getting it is a failure, not a reason
		// to skip: otherwise CI sets the variable, the database is
		// unreachable, every suite skips, and the job reports success
		// having executed nothing.
		if poolRequested {
			t.Fatalf("%s is set but unusable: %v", testDSNEnv, sharedErr)
		}
		t.Skipf("no dev database: set %s, or start Docker for testcontainers (%v)", testDSNEnv, sharedErr)
	}
	return p
}

// freshSchema creates a per-test schema, returns its name, and
// registers a cleanup that drops it at test end. Using a unique
// schema per test means a panicking test can't corrupt its neighbour.
func freshSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	name := strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	// PostgreSQL identifiers cap at 63 bytes; trim if needed.
	if len(name) > 50 {
		name = name[:50]
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", name)); err != nil {
		t.Fatalf("create schema %s: %v", name, err)
	}
	t.Cleanup(func() {
		cx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_, _ = pool.Exec(cx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", name))
	})
	return name
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, stmt string) {
	t.Helper()
	if _, err := pool.Exec(ctx, stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

// columnNullable returns (exists, nullable) for the given column.
func columnNullable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, table, col string) (bool, bool) {
	t.Helper()
	var isNullable string
	err := pool.QueryRow(ctx, `
		SELECT is_nullable
		  FROM information_schema.columns
		 WHERE table_schema=$1 AND table_name=$2 AND column_name=$3
	`, schema, table, col).Scan(&isNullable)
	if err != nil {
		return false, false
	}
	return true, strings.EqualFold(isNullable, "YES")
}

func constraintExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, name string) bool {
	t.Helper()
	var count int
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM pg_constraint c
		  JOIN pg_namespace n ON n.oid = c.connamespace
		 WHERE n.nspname=$1 AND c.conname=$2
	`, schema, name).Scan(&count)
	if err != nil {
		return false
	}
	return count > 0
}

// -- Tests ------------------------------------------------------------

// TestEngineSetNotNull_FullCycle — Expand adds a NOT VALID CHECK;
// Contract validates + SET NOT NULL + DROP CHECK. Column ends up
// NOT NULL with the check gone.
func TestEngineSetNotNull_FullCycle(t *testing.T) {
	pool := getPool(t)
	ctx := context.Background()
	schema := freshSchema(t, ctx, pool)

	mustExec(t, ctx, pool, fmt.Sprintf(`
		CREATE TABLE %s.users (
			id bigserial PRIMARY KEY,
			email text
		)`, schema))
	// Seed rows with non-NULL emails so VALIDATE succeeds.
	mustExec(t, ctx, pool, fmt.Sprintf(
		"INSERT INTO %s.users (email) VALUES ('a@x.io'), ('b@x.io')", schema))

	eng, err := NewEngine(pool, schema)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	op := keystonev1alpha1.MigrationOperation{
		Kind:       keystonev1alpha1.MigrationOperationSetNotNull,
		Table:      "users",
		SetNotNull: &keystonev1alpha1.SetNotNullOp{Column: "email"},
	}

	// Expand: CHECK is added, column still nullable.
	if _, err := eng.Apply(ctx, op, PhaseExpand); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !constraintExists(t, ctx, pool, schema, setNotNullConstraintName("email")) {
		t.Fatal("NOT VALID constraint missing after Expand")
	}
	if _, nullable := columnNullable(t, ctx, pool, schema, "users", "email"); !nullable {
		t.Fatal("column should still be nullable after Expand")
	}

	// Contract: validate + set not null + drop constraint.
	if _, err := eng.Apply(ctx, op, PhaseContract); err != nil {
		t.Fatalf("contract: %v", err)
	}
	if constraintExists(t, ctx, pool, schema, setNotNullConstraintName("email")) {
		t.Error("CHECK constraint should be dropped after Contract")
	}
	if _, nullable := columnNullable(t, ctx, pool, schema, "users", "email"); nullable {
		t.Error("column should be NOT NULL after Contract")
	}
}

// TestEngineSetNotNull_Abort — Expand then Abort. CHECK dropped,
// column still nullable, no lasting side effects.
func TestEngineSetNotNull_Abort(t *testing.T) {
	pool := getPool(t)
	ctx := context.Background()
	schema := freshSchema(t, ctx, pool)

	mustExec(t, ctx, pool, fmt.Sprintf(`
		CREATE TABLE %s.users (
			id bigserial PRIMARY KEY,
			email text
		)`, schema))

	eng, err := NewEngine(pool, schema)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	op := keystonev1alpha1.MigrationOperation{
		Kind:       keystonev1alpha1.MigrationOperationSetNotNull,
		Table:      "users",
		SetNotNull: &keystonev1alpha1.SetNotNullOp{Column: "email"},
	}

	if _, err := eng.Apply(ctx, op, PhaseExpand); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if _, err := eng.Apply(ctx, op, PhaseAbort); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if constraintExists(t, ctx, pool, schema, setNotNullConstraintName("email")) {
		t.Error("CHECK constraint should be dropped after Abort")
	}
	if _, nullable := columnNullable(t, ctx, pool, schema, "users", "email"); !nullable {
		t.Error("column should stay nullable after Abort (no contract ran)")
	}
}

// TestEngineSetNotNull_AbortIdempotent — Abort called twice (e.g.
// controller crash between runs) must not error. The second run has
// nothing to drop; IF EXISTS makes it a no-op.
func TestEngineSetNotNull_AbortIdempotent(t *testing.T) {
	pool := getPool(t)
	ctx := context.Background()
	schema := freshSchema(t, ctx, pool)

	mustExec(t, ctx, pool, fmt.Sprintf(`
		CREATE TABLE %s.users (id bigserial PRIMARY KEY, email text)`, schema))

	eng, err := NewEngine(pool, schema)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	op := keystonev1alpha1.MigrationOperation{
		Kind:       keystonev1alpha1.MigrationOperationSetNotNull,
		Table:      "users",
		SetNotNull: &keystonev1alpha1.SetNotNullOp{Column: "email"},
	}
	if _, err := eng.Apply(ctx, op, PhaseExpand); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if _, err := eng.Apply(ctx, op, PhaseAbort); err != nil {
		t.Fatalf("first abort: %v", err)
	}
	if _, err := eng.Apply(ctx, op, PhaseAbort); err != nil {
		t.Fatalf("second abort must be idempotent: %v", err)
	}
}

// TestEngineDropNotNull_SinglePhase — DROP NOT NULL is
// single-phase: Expand executes it; Contract is a no-op.
func TestEngineDropNotNull_SinglePhase(t *testing.T) {
	pool := getPool(t)
	ctx := context.Background()
	schema := freshSchema(t, ctx, pool)

	mustExec(t, ctx, pool, fmt.Sprintf(`
		CREATE TABLE %s.items (
			id bigserial PRIMARY KEY,
			name text NOT NULL
		)`, schema))

	eng, err := NewEngine(pool, schema)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	op := keystonev1alpha1.MigrationOperation{
		Kind:        keystonev1alpha1.MigrationOperationDropNotNull,
		Table:       "items",
		DropNotNull: &keystonev1alpha1.DropNotNullOp{Column: "name"},
	}

	if _, err := eng.Apply(ctx, op, PhaseExpand); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if _, nullable := columnNullable(t, ctx, pool, schema, "items", "name"); !nullable {
		t.Error("column should be nullable after Expand")
	}
	res, err := eng.Apply(ctx, op, PhaseContract)
	if err != nil {
		t.Fatalf("contract: %v", err)
	}
	if !strings.Contains(res.Description, "single-phase") {
		t.Errorf("contract message should say single-phase; got %q", res.Description)
	}
}

// TestEngineAddColumn_Abort — Expand adds a nullable column; Abort
// drops it. Prevents leftover shadow columns when a bundle is
// aborted mid-expand.
func TestEngineAddColumn_Abort(t *testing.T) {
	pool := getPool(t)
	ctx := context.Background()
	schema := freshSchema(t, ctx, pool)

	mustExec(t, ctx, pool, fmt.Sprintf(`
		CREATE TABLE %s.orders (id bigserial PRIMARY KEY)`, schema))

	eng, err := NewEngine(pool, schema)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	op := keystonev1alpha1.MigrationOperation{
		Kind:  keystonev1alpha1.MigrationOperationAddColumn,
		Table: "orders",
		AddColumn: &keystonev1alpha1.AddColumnOp{
			Name:     "notes",
			Type:     "text",
			Nullable: true,
		},
	}
	if _, err := eng.Apply(ctx, op, PhaseExpand); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if exists, _ := columnNullable(t, ctx, pool, schema, "orders", "notes"); !exists {
		t.Fatal("column should exist after Expand")
	}
	if _, err := eng.Apply(ctx, op, PhaseAbort); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if exists, _ := columnNullable(t, ctx, pool, schema, "orders", "notes"); exists {
		t.Error("column should be dropped after Abort")
	}
}

// TestEngineConcurrent_DifferentTables — the engine is safe for
// concurrent Apply() calls targeting different tables. Each op gets
// its own pool connection + transaction; no cross-op state in the
// Engine struct. This is the invariant B3's wave scheduler relies on.
func TestEngineConcurrent_DifferentTables(t *testing.T) {
	pool := getPool(t)
	ctx := context.Background()
	schema := freshSchema(t, ctx, pool)

	mustExec(t, ctx, pool, fmt.Sprintf(`
		CREATE TABLE %s.t_users (id bigserial PRIMARY KEY);
		CREATE TABLE %s.t_orders (id bigserial PRIMARY KEY);
		CREATE TABLE %s.t_products (id bigserial PRIMARY KEY);
	`, schema, schema, schema))

	eng, err := NewEngine(pool, schema)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	ops := []keystonev1alpha1.MigrationOperation{
		{Kind: keystonev1alpha1.MigrationOperationAddColumn, Table: "t_users",
			AddColumn: &keystonev1alpha1.AddColumnOp{Name: "email", Type: "text", Nullable: true}},
		{Kind: keystonev1alpha1.MigrationOperationAddColumn, Table: "t_orders",
			AddColumn: &keystonev1alpha1.AddColumnOp{Name: "amount", Type: "numeric(10,2)", Nullable: true}},
		{Kind: keystonev1alpha1.MigrationOperationAddColumn, Table: "t_products",
			AddColumn: &keystonev1alpha1.AddColumnOp{Name: "sku", Type: "text", Nullable: true}},
	}

	// Fire all three concurrently — distinct tables, so no lock
	// contention. Errgroup collects any failure.
	errCh := make(chan error, len(ops))
	start := time.Now()
	for i := range ops {
		op := ops[i]
		go func() {
			_, err := eng.Apply(ctx, op, PhaseExpand)
			errCh <- err
		}()
	}
	for range ops {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent Apply: %v", err)
		}
	}
	elapsed := time.Since(start)

	// All three columns should exist.
	for _, tc := range []struct{ table, col string }{
		{"t_users", "email"},
		{"t_orders", "amount"},
		{"t_products", "sku"},
	} {
		if exists, _ := columnNullable(t, ctx, pool, schema, tc.table, tc.col); !exists {
			t.Errorf("%s.%s missing after concurrent Apply", tc.table, tc.col)
		}
	}
	// Sanity: 3 concurrent ADD COLUMNs on distinct tables should
	// finish in well under the sum of serial times. On a warm pool,
	// each ADD COLUMN takes ~5ms; 3 serial = ~15ms, 3 parallel = ~5ms.
	// Use a lenient cap (1s) so slow CI doesn't false-fail.
	if elapsed > time.Second {
		t.Errorf("3 concurrent distinct-table ADD COLUMNs took %v; suggests serialisation", elapsed)
	}
	t.Logf("3 concurrent distinct-table ops completed in %v", elapsed)
}

// TestEngineRenameColumn_Abort — after an Expand that installs the
// shadow column + trigger, Abort removes them all. Original column
// is unaffected throughout.
func TestEngineRenameColumn_Abort(t *testing.T) {
	pool := getPool(t)
	ctx := context.Background()
	schema := freshSchema(t, ctx, pool)

	mustExec(t, ctx, pool, fmt.Sprintf(`
		CREATE TABLE %s.people (
			id bigserial PRIMARY KEY,
			full_name text
		)`, schema))
	mustExec(t, ctx, pool, fmt.Sprintf(
		"INSERT INTO %s.people (full_name) VALUES ('Alice'), ('Bob')", schema))

	eng, err := NewEngine(pool, schema)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	op := keystonev1alpha1.MigrationOperation{
		Kind:  keystonev1alpha1.MigrationOperationRenameColumn,
		Table: "people",
		RenameColumn: &keystonev1alpha1.RenameColumnOp{
			From: "full_name",
			To:   "display_name",
		},
	}

	if _, err := eng.Apply(ctx, op, PhaseExpand); err != nil {
		t.Fatalf("expand: %v", err)
	}
	if exists, _ := columnNullable(t, ctx, pool, schema, "people", "display_name"); !exists {
		t.Fatal("display_name should exist after Expand")
	}

	if _, err := eng.Apply(ctx, op, PhaseAbort); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if exists, _ := columnNullable(t, ctx, pool, schema, "people", "display_name"); exists {
		t.Error("display_name should be dropped after Abort")
	}
	// Original full_name still present with data intact.
	if exists, _ := columnNullable(t, ctx, pool, schema, "people", "full_name"); !exists {
		t.Fatal("original full_name must survive Abort")
	}
	var count int
	if err := pool.QueryRow(ctx,
		fmt.Sprintf("SELECT count(*) FROM %s.people WHERE full_name IS NOT NULL", schema)).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 rows with full_name, got %d", count)
	}
}
