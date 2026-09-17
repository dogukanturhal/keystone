// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dogukanturhal/keystone-sdk/go/sqlquote"
)

// Admin offers idempotent administrative DDL helpers against a single
// pgx pool. All methods are safe to call repeatedly; first call mutates,
// subsequent calls are no-ops (or update-in-place if an attribute drifted).
//
// Every helper validates its identifiers before issuing DDL. Unvalidated
// input would let an attacker who somehow bypassed the admission webhook
// inject SQL through the schema/database/role names.
type Admin struct {
	pool *pgxpool.Pool
}

// NewAdmin wraps a pgxpool with administrative helpers.
func NewAdmin(pool *pgxpool.Pool) *Admin {
	return &Admin{pool: pool}
}

// ServerVersion returns the SHOW server_version output. Used by the
// DatabaseProvider controller to populate status.serverVersion.
func (a *Admin) ServerVersion(ctx context.Context) (string, error) {
	var v string
	if err := a.pool.QueryRow(ctx, `SHOW server_version`).Scan(&v); err != nil {
		return "", fmt.Errorf("show server_version: %w", err)
	}
	return v, nil
}

// Ping issues SELECT 1 to verify the pool can dial. Cheap; safe in a
// reconcile inner loop.
func (a *Admin) Ping(ctx context.Context) error {
	var n int
	if err := a.pool.QueryRow(ctx, `SELECT 1`).Scan(&n); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// EnsureRole creates a LOGIN role if missing.
//
// password (optional) is the PostgreSQL password to set on the role.
//
//   - Empty string → create the role WITHOUT a password (legacy behavior;
//     caller manages password out-of-band, e.g. Vault dynamic secrets,
//     manual psql, ALTER ROLE elsewhere). Pre-existing roles are left
//     untouched.
//   - Non-empty → on first create, CREATE ROLE LOGIN PASSWORD '<password>'
//     in a single statement. On a pre-existing role, no-op (caller
//     drives rotation via SetRolePassword separately so we don't ALTER
//     unconditionally and clobber a Vault-rotated password).
//
// Passwords flow into SQL via QuoteString — embedded single quotes are
// doubled, no SQL injection vector. Use crypto-strong random material
// at the call site.
//
// Idempotent: the existence check uses pg_roles, not CREATE ROLE IF NOT
// EXISTS (which doesn't exist for roles in PostgreSQL).
//
// # PG16 compatibility (set_option/inherit_option)
//
// PostgreSQL 16 changed how CREATEROLE-having users acquire membership
// in the roles they create. The auto-grant gives ADMIN OPTION but
// explicitly omits SET OPTION and INHERIT OPTION (PG16 release notes:
// "Make CREATEROLE creators members of new roles, with WITH ADMIN
// OPTION but WITHOUT INHERIT OPTION OR SET OPTION").
//
// CREATE DATABASE ... OWNER newrole and ALTER DATABASE ... OWNER TO
// newrole both require the executor to be able to SET ROLE to the new
// owner. With set_option=false, a non-superuser admin cannot — even
// though it just created the role and holds admin_option on it.
//
// Failure mode without an explicit GRANT WITH SET TRUE (observed in
// cluster on 2026-05-04 against example-pg PG16.4):
//
//	create database "realm_xyz": ERROR: must be able to SET ROLE
//	"realm_xyz_owner" (SQLSTATE 42501)
//
// # Scope of the GRANT
//
// The GRANT runs ONLY on the create branch — when EnsureRole has just
// issued CREATE ROLE in this same call. PG16's auto-grant gives the
// CREATEROLE-having creator ADMIN OPTION on the new role, so the
// follow-up "GRANT TO CURRENT_USER WITH SET TRUE" succeeds.
//
// We deliberately do NOT GRANT on the role-already-exists branch
// because:
//
//  1. Pre-existing roles created out-of-band (e.g. cluster init,
//     manual psql) may not have given the keystone admin ADMIN OPTION.
//     Without ADMIN OPTION, the GRANT fails with:
//
//     ERROR: permission denied to grant role "<role>"  (SQLSTATE 42501)
//
//     If we ran it on every reconcile, the controller would loop on a
//     permanent error and OOM via the audit-write storm (observed
//     2026-05-04: a v0.1.51 deploy that ran the unconditional GRANT
//     against pre-existing example_service_app + example_service_control_
//     owner roles took the operator into CrashLoopBackOff).
//
//  2. Pre-existing roles are by definition usable by the admin —
//     they were good enough for prior CREATE/ALTER DATABASE OWNER
//     operations in earlier reconciles, so the SET privilege is
//     already there or not needed.
//
// The grant uses CURRENT_USER (the live connection's role) rather
// than a hard-coded admin name, keeping the call site agnostic to
// whichever admin secret the DatabaseProvider is bound to.
func (a *Admin) EnsureRole(ctx context.Context, name string, password string) error {
	if err := sqlquote.ValidateIdentifier("role", name); err != nil {
		return err
	}
	var exists bool
	err := a.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check role exists: %w", err)
	}
	if exists {
		// Pre-existing role — leave its membership flags alone. See
		// "Scope of the GRANT" in the doc comment for why. Password
		// rotation on existing roles is the caller's job (SetRolePassword)
		// — we never ALTER PASSWORD here because that would clobber a
		// Vault-rotated password the operator never observed.
		return nil
	}
	// Use a DO block so concurrent reconciles racing on the existence
	// check don't both attempt CREATE — the second loses to a
	// duplicate object error which we'd then have to filter out. The
	// DO block re-checks under the implicit transaction.
	//
	// password embedded via QuoteString. crypto/rand-generated bytes
	// at the call site contain no quotes; even if they did, double-
	// quoting in QuoteString neutralises them. Empty password emits
	// CREATE ROLE … LOGIN with no PASSWORD clause.
	var createSQL string
	if password == "" {
		createSQL = fmt.Sprintf(
			`DO $$ BEGIN
				IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
					CREATE ROLE %s LOGIN;
				END IF;
			END $$`,
			sqlquote.QuoteString(name), sqlquote.QuoteIdentifier(name),
		)
	} else {
		createSQL = fmt.Sprintf(
			`DO $$ BEGIN
				IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
					CREATE ROLE %s LOGIN PASSWORD %s;
				END IF;
			END $$`,
			sqlquote.QuoteString(name), sqlquote.QuoteIdentifier(name), sqlquote.QuoteString(password),
		)
	}
	if _, err := a.pool.Exec(ctx, createSQL); err != nil {
		return fmt.Errorf("create role %q: %w", name, err)
	}

	// PG16: explicit GRANT WITH SET TRUE, INHERIT TRUE so the admin can
	// SET ROLE / CREATE DATABASE OWNER newrole / ALTER OWNER. Safe here
	// because we just CREATE'd the role with CREATEROLE-having admin —
	// PG16 auto-grant gave us ADMIN OPTION, so this GRANT succeeds.
	grantStmt := fmt.Sprintf(
		`GRANT %s TO CURRENT_USER WITH SET TRUE, INHERIT TRUE`,
		sqlquote.QuoteIdentifier(name),
	)
	if _, err := a.pool.Exec(ctx, grantStmt); err != nil {
		return fmt.Errorf("grant role %q to current_user with SET TRUE: %w", name, err)
	}
	return nil
}

// SetRolePassword rotates the password on an existing PostgreSQL role
// via ALTER ROLE … PASSWORD '<password>'. Caller is responsible for
// computing whether rotation is needed (e.g. by comparing the new
// password's hash against the cached observedPasswordHash on
// LogicalDatabase status).
//
// The role MUST already exist — call EnsureRole first. Using ALTER
// instead of CREATE OR REPLACE because PostgreSQL has no atomic
// upsert for roles.
//
// Like EnsureRole(name, password), the password flows in via
// QuoteString. Empty password is rejected — passing "" is almost
// certainly a bug at the call site (use EnsureRole's no-password
// branch instead if you genuinely want to clear the password).
func (a *Admin) SetRolePassword(ctx context.Context, name, password string) error {
	if err := sqlquote.ValidateIdentifier("role", name); err != nil {
		return err
	}
	if password == "" {
		return fmt.Errorf("set role password %q: password is empty (use EnsureRole no-password branch instead)", name)
	}
	stmt := fmt.Sprintf(
		`ALTER ROLE %s WITH PASSWORD %s`,
		sqlquote.QuoteIdentifier(name), sqlquote.QuoteString(password),
	)
	if _, err := a.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("alter role password %q: %w", name, err)
	}
	return nil
}

// DatabaseSpec describes the desired state of a logical database.
type DatabaseSpec struct {
	Name            string
	Owner           string
	Encoding        string
	Collation       string
	Ctype           string
	ConnectionLimit int32
}

// EnsureDatabase creates the database if missing with the requested owner
// and locale settings. Encoding/collation/ctype are enforced at CREATE
// time only — PostgreSQL does not permit altering them on an existing
// database. The reconciler reports a Degraded condition if a drift is
// detected (Phase 5).
//
// CREATE DATABASE cannot run inside a transaction, so this method must be
// called against the maintenance database connection (not the target).
func (a *Admin) EnsureDatabase(ctx context.Context, spec DatabaseSpec) error {
	if err := sqlquote.ValidateIdentifier("database", spec.Name); err != nil {
		return err
	}
	if spec.Owner != "" {
		if err := sqlquote.ValidateIdentifier("role", spec.Owner); err != nil {
			return err
		}
	}

	var exists bool
	err := a.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, spec.Name,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check database exists: %w", err)
	}
	if exists {
		// Adjust mutable attributes (owner, connection limit). Encoding/
		// collation/ctype are immutable; drift on those becomes a status
		// Degraded condition at the controller layer.
		if spec.Owner != "" {
			alter := fmt.Sprintf(`ALTER DATABASE %s OWNER TO %s`,
				sqlquote.QuoteIdentifier(spec.Name), sqlquote.QuoteIdentifier(spec.Owner))
			if _, err := a.pool.Exec(ctx, alter); err != nil {
				return fmt.Errorf("alter database owner: %w", err)
			}
		}
		if spec.ConnectionLimit != 0 {
			alter := fmt.Sprintf(`ALTER DATABASE %s WITH CONNECTION LIMIT %d`,
				sqlquote.QuoteIdentifier(spec.Name), spec.ConnectionLimit)
			if _, err := a.pool.Exec(ctx, alter); err != nil {
				return fmt.Errorf("alter database connection limit: %w", err)
			}
		}
		return nil
	}

	// Build the CREATE DATABASE statement with optional clauses.
	create := fmt.Sprintf(`CREATE DATABASE %s`, sqlquote.QuoteIdentifier(spec.Name))
	if spec.Owner != "" {
		create += fmt.Sprintf(` OWNER %s`, sqlquote.QuoteIdentifier(spec.Owner))
	}
	if spec.Encoding != "" {
		create += fmt.Sprintf(` ENCODING %s`, sqlquote.QuoteString(spec.Encoding))
	}
	if spec.Collation != "" {
		create += fmt.Sprintf(` LC_COLLATE %s`, sqlquote.QuoteString(spec.Collation))
	}
	if spec.Ctype != "" {
		create += fmt.Sprintf(` LC_CTYPE %s`, sqlquote.QuoteString(spec.Ctype))
	}
	create += ` TEMPLATE template0`
	if spec.ConnectionLimit != 0 {
		create += fmt.Sprintf(` CONNECTION LIMIT %d`, spec.ConnectionLimit)
	}

	if _, err := a.pool.Exec(ctx, create); err != nil {
		// Race window: two reconciles racing on EnsureDatabase. Filter the
		// duplicate-database error and treat as success.
		if isDuplicateDatabaseError(err) {
			return nil
		}
		return fmt.Errorf("create database %q: %w", spec.Name, err)
	}
	return nil
}

// EnsureExtension installs an extension into the *currently connected*
// database. Caller must dial against the target database, not the
// maintenance database, before calling this.
func (a *Admin) EnsureExtension(ctx context.Context, name string) error {
	if err := sqlquote.ValidateIdentifier("extension", name); err != nil {
		return err
	}
	stmt := fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS %s`, sqlquote.QuoteIdentifier(name))
	if _, err := a.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create extension %q: %w", name, err)
	}
	return nil
}

// DropDatabase drops a database. Caller MUST verify the resource's
// deletionPolicy is Delete before invoking this — there is no second
// safety net inside.
func (a *Admin) DropDatabase(ctx context.Context, name string) error {
	if err := sqlquote.ValidateIdentifier("database", name); err != nil {
		return err
	}
	// Force-disconnect existing sessions so the DROP doesn't block on
	// stale connections from departed pods.
	disconnect := fmt.Sprintf(
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = %s AND pid <> pg_backend_pid()`,
		sqlquote.QuoteString(name),
	)
	if _, err := a.pool.Exec(ctx, disconnect); err != nil {
		return fmt.Errorf("terminate backends on %q: %w", name, err)
	}
	stmt := fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, sqlquote.QuoteIdentifier(name))
	if _, err := a.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("drop database %q: %w", name, err)
	}
	return nil
}

// isDuplicateDatabaseError detects the SQLSTATE 42P04 (duplicate_database)
// error returned when two CREATE DATABASE statements race.
func isDuplicateDatabaseError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42P04"
	}
	return false
}
