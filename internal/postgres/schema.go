// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/dogukanturhal/keystone-sdk/go/sqlquote"
)

// SchemaSpec describes the desired state of one schema inside a database.
// The Admin must be connected to the target database, not the maintenance
// DB, when EnsureSchema is called.
type SchemaSpec struct {
	Name            string
	Owner           string
	SearchPathHints []string
	DefaultPrivs    []DefaultPriv
}

// DefaultPriv mirrors keystonev1alpha1.SchemaPrivilegeDefault but stays in
// the postgres package so this layer doesn't import the Kubernetes API
// types — keeps the testability surface clean.
type DefaultPriv struct {
	Role       string
	ObjectType string // tables | sequences | functions | types | schemas
	Privileges []string
}

// EnsureSchema creates the schema (if missing), aligns ownership, and
// re-applies ALTER DEFAULT PRIVILEGES. Idempotent.
func (a *Admin) EnsureSchema(ctx context.Context, spec SchemaSpec) error {
	if err := sqlquote.ValidateIdentifier("schema", spec.Name); err != nil {
		return err
	}
	if err := sqlquote.ValidateIdentifier("role", spec.Owner); err != nil {
		return err
	}

	// CREATE SCHEMA IF NOT EXISTS — single transaction so ownership and
	// schema arrive together. AUTHORIZATION ensures the role owns it
	// from the start (rather than transient ownership by the connecting
	// admin user).
	stmt := fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s AUTHORIZATION %s`,
		sqlquote.QuoteIdentifier(spec.Name), sqlquote.QuoteIdentifier(spec.Owner))
	if _, err := a.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create schema %q: %w", spec.Name, err)
	}

	// Re-apply ownership in case the schema pre-existed (CNPG-managed-roles
	// landmine — a schema created by the cluster admin needs explicit
	// REASSIGN before the new role can take ownership).
	alter := fmt.Sprintf(`ALTER SCHEMA %s OWNER TO %s`,
		sqlquote.QuoteIdentifier(spec.Name), sqlquote.QuoteIdentifier(spec.Owner))
	if _, err := a.pool.Exec(ctx, alter); err != nil {
		return fmt.Errorf("alter schema owner %q: %w", spec.Name, err)
	}

	// Default privileges. ALTER DEFAULT PRIVILEGES applies to objects
	// created in the schema FROM THIS POINT FORWARD by the granting role.
	// Existing objects are unaffected; that's intentional — controllers
	// must not retroactively re-grant a tenant's tables, that would be a
	// security regression.
	for _, p := range spec.DefaultPrivs {
		if err := a.applyDefaultPriv(ctx, spec.Name, spec.Owner, p); err != nil {
			return err
		}
	}

	// Search path hints — best effort. If the role has no LOGIN attribute
	// the ALTER ROLE will fail; we accept that and emit no error to keep
	// the reconcile non-flaky for service roles.
	if len(spec.SearchPathHints) > 0 {
		path := []string{spec.Name}
		path = append(path, spec.SearchPathHints...)
		// Deduplicate while preserving order.
		seen := make(map[string]struct{}, len(path))
		dedup := path[:0]
		for _, p := range path {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			dedup = append(dedup, p)
		}
		quoted := make([]string, 0, len(dedup))
		for _, p := range dedup {
			quoted = append(quoted, sqlquote.QuoteIdentifier(p))
		}
		alter := fmt.Sprintf(`ALTER ROLE %s SET search_path = %s`,
			sqlquote.QuoteIdentifier(spec.Owner), strings.Join(quoted, ","))
		if _, err := a.pool.Exec(ctx, alter); err != nil {
			// Best-effort; downgrade to debug logging at the controller layer.
			return fmt.Errorf("set search_path on %q: %w", spec.Owner, err)
		}
	}

	return nil
}

// validObjectTypes constrains the ALTER DEFAULT PRIVILEGES object class
// so we never substring-inject. Mirror of the API enum.
var validObjectTypes = map[string]bool{
	"tables": true, "sequences": true, "functions": true,
	"types": true, "schemas": true,
}

// validPrivileges constrains the privilege keywords. PostgreSQL accepts
// more (TRUNCATE, REFERENCES, …) but Keystone narrows the surface to the
// CRUD-shaped set most tenant workloads need. New privileges land via API
// validation review.
var validPrivileges = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"USAGE": true, "EXECUTE": true, "ALL": true,
}

func (a *Admin) applyDefaultPriv(ctx context.Context, schema, owner string, p DefaultPriv) error {
	if err := sqlquote.ValidateIdentifier("role", p.Role); err != nil {
		return err
	}
	if !validObjectTypes[p.ObjectType] {
		return fmt.Errorf("invalid object type %q (allowed: tables sequences functions types schemas)", p.ObjectType)
	}
	if len(p.Privileges) == 0 {
		// Empty privileges means "revoke all defaults" — explicit
		// distinction from omitting the entry. Translate to REVOKE.
		stmt := fmt.Sprintf(
			`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s REVOKE ALL ON %s FROM %s`,
			sqlquote.QuoteIdentifier(owner), sqlquote.QuoteIdentifier(schema),
			strings.ToUpper(p.ObjectType), sqlquote.QuoteIdentifier(p.Role),
		)
		if _, err := a.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("revoke default privs on %s: %w", p.ObjectType, err)
		}
		return nil
	}
	for _, priv := range p.Privileges {
		if !validPrivileges[strings.ToUpper(priv)] {
			return fmt.Errorf("invalid privilege %q", priv)
		}
	}
	stmt := fmt.Sprintf(
		`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA %s GRANT %s ON %s TO %s`,
		sqlquote.QuoteIdentifier(owner), sqlquote.QuoteIdentifier(schema),
		strings.Join(uppercase(p.Privileges), ","),
		strings.ToUpper(p.ObjectType),
		sqlquote.QuoteIdentifier(p.Role),
	)
	if _, err := a.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("grant default privs on %s: %w", p.ObjectType, err)
	}
	return nil
}

// DropSchema drops a schema. Caller MUST verify deletion policy.
// Uses CASCADE so all owned objects (tables, sequences, functions, …)
// are removed. This is destructive; the controller gates it behind
// spec.deletionPolicy = Delete + a finalizer-controlled flow.
func (a *Admin) DropSchema(ctx context.Context, name string) error {
	if err := sqlquote.ValidateIdentifier("schema", name); err != nil {
		return err
	}
	stmt := fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, sqlquote.QuoteIdentifier(name))
	if _, err := a.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("drop schema %q: %w", name, err)
	}
	return nil
}

func uppercase(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}
