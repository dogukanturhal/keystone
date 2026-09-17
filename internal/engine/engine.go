// SPDX-License-Identifier: AGPL-3.0-or-later

// Package engine declares the database-engine abstraction Keystone's
// reconcilers program against. PostgreSQL is the only implementation
// shipped in v1alpha1; MySQL lands in Phase 13, and the interface
// exists today so the boundary is reviewable early.
//
// The interface deliberately mirrors the shape of `internal/postgres`
// to keep the refactor cost flat when we add a second engine. See
// ADR 0012 for the design rationale and what was scoped out.
//
// Intentionally NOT in this package:
//
//   - Schema diffing (`internal/migration/declarative`) — works against
//     the inspected Snapshot, engine-agnostic by construction.
//   - Lint analyzers (`internal/migration/analyze`) — operate on SQL
//     strings and pgroll operations, independent of engine.
//   - Migration source resolution (`internal/migration`) — reads
//     ConfigMaps, not databases.
//
// Those layers stay above the engine interface so they can reuse the
// same code for any backend.
package engine

import "context"

// Engine is the minimum surface a database backend must expose for
// Keystone reconcilers. It's a cluster-admin abstraction: the reconciler
// holds admin credentials and calls these methods against an implicit
// connection the Engine was constructed with.
//
// Every method must be idempotent — retries are routine, and
// controller-runtime's rate-limited workqueue relies on safe replays.
type Engine interface {
	// Ping verifies connectivity. Fast-fail; called before any DDL.
	Ping(ctx context.Context) error

	// ServerVersion returns the remote version string
	// (e.g. "16.3"). Used for the ClusterRegistration status and
	// admission checks against minimum-version policies.
	ServerVersion(ctx context.Context) (string, error)

	// EnsureRole creates a role if absent. Role attributes
	// (LOGIN, NOINHERIT, …) are engine-specific; today Keystone uses
	// the default profile for every role it creates.
	EnsureRole(ctx context.Context, name string) error

	// EnsureDatabase CREATEs a database with the provided attributes
	// when absent, and adjusts owner/connection-limit when present.
	// Encoding/collation/ctype are fixed at CREATE time and aren't
	// changed on subsequent calls.
	EnsureDatabase(ctx context.Context, spec DatabaseSpec) error

	// EnsureSchema creates a schema inside the current connection's
	// database (not the maintenance DB — the caller switches pools).
	// Sets owner and applies default privileges.
	EnsureSchema(ctx context.Context, spec SchemaSpec) error

	// EnsureExtension CREATEs an extension inside the current database.
	// Idempotent — IF NOT EXISTS on the underlying DDL.
	EnsureExtension(ctx context.Context, name string) error

	// DropDatabase removes a database. Only called from the
	// LogicalDatabase delete-path when DeletionPolicy=Delete; guarded
	// by finalizer cleanup so misfires are rare.
	DropDatabase(ctx context.Context, name string) error

	// DropSchema removes a schema from the current database. Called
	// from DatabaseSchema delete-path when DeletionPolicy=Delete.
	DropSchema(ctx context.Context, name string) error

	// Inspect returns a snapshot of the current schema's shape:
	// tables, columns, indexes, FKs. Used by the declarative differ
	// and the DriftController.
	//
	// Engines MUST return equivalent Snapshot shapes so the differ can
	// consume them without branching on backend.
	Inspect(ctx context.Context, schema string) (*Snapshot, error)
}

// DatabaseSpec is the engine-agnostic `CREATE DATABASE` input.
// Fields correspond to ANSI-ish concepts that every supported engine
// can honour; engine-specific knobs are expressed via the
// ClusterRegistration's labels and read by the engine implementation.
type DatabaseSpec struct {
	Name            string
	Owner           string
	Encoding        string
	Collation       string
	Ctype           string
	ConnectionLimit int32
}

// SchemaSpec is the engine-agnostic `CREATE SCHEMA` input. PostgreSQL
// uses it verbatim; MySQL's CREATE DATABASE aliases to CREATE SCHEMA
// and ignores SearchPathHints.
type SchemaSpec struct {
	Name            string
	Owner           string
	SearchPathHints []string
	DefaultPrivs    []DefaultPriv
}

// DefaultPriv is one per-role default-privileges entry applied to new
// objects in the schema. Engine-specific translation happens in the
// implementation (PG: `ALTER DEFAULT PRIVILEGES`; MySQL: approximated
// via wrapper grants).
type DefaultPriv struct {
	Role       string
	ObjectType string
	Privileges []string
}

// Snapshot is the read-only view the inspector returns. It matches the
// shape `internal/drift/inspector.go` already produces; moving the type
// here (in a later commit) consolidates the contract.
//
// For v1alpha1 this is a narrow forward declaration — the real type
// lives in `internal/drift.Snapshot` and the concrete engine
// implementations alias to it. Once Phase 13 refactors the postgres
// package under this interface, the alias collapses.
type Snapshot struct {
	Schema      string
	Tables      []Table
	Fingerprint string
}

// Table summarises one table.
type Table struct {
	Name    string
	Columns []Column
	Indexes []Index
}

// Column describes one column.
type Column struct {
	Name     string
	Type     string
	Nullable bool
	Default  string
}

// Index describes one index.
type Index struct {
	Name    string
	Columns []string
	Unique  bool
	Method  string
}
