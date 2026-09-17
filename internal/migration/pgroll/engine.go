// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pgroll implements Keystone's expand/contract migration engine
// for zero-downtime column-level changes. Lighter than xataio/pgroll
// (no view-level schema versioning — apps stay on their original
// search_path); covers the three highest-value operations:
//
//	add_column    — Expand: ADD COLUMN; Contract: optional SET NOT NULL
//	drop_column   — Expand: deprecation marker only; Contract: DROP COLUMN
//	rename_column — Expand: ADD new + COPY + dual-write trigger;
//	                Contract: DROP TRIGGER + DROP old
//
// Trade-off vs xataio/pgroll: we don't run two schema versions in
// parallel. Apps that read the renamed column see it during the Expand
// phase via the trigger-mirrored value; apps that drop a column must
// stop referencing it during Expand (controller does not enforce —
// dual-gate policies should). In exchange we don't break apps' search_path.
package pgroll

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/sqlquote"
)

// Engine executes one MigrationOperation in the requested phase against
// a target schema. The pool MUST be connected to the database hosting
// the schema; the engine does not open connections.
type Engine struct {
	pool   *pgxpool.Pool
	schema string
}

// NewEngine binds the engine to a pool + target schema.
func NewEngine(pool *pgxpool.Pool, schema string) (*Engine, error) {
	if err := sqlquote.ValidateIdentifier("schema", schema); err != nil {
		return nil, err
	}
	return &Engine{pool: pool, schema: schema}, nil
}

// Phase identifies which side of the expand/contract pipeline the
// engine is running.
type Phase string

const (
	PhaseExpand   Phase = "expand"
	PhaseContract Phase = "contract"
	// PhaseAbort rolls back whatever Expand created: drops shadow
	// columns, triggers, functions, and NOT VALID constraints. Handlers
	// are idempotent via IF EXISTS semantics — running PhaseAbort on a
	// never-expanded operation is a safe no-op. Invoked by the
	// controller when the operator sets
	// keystone.hexxlock.io/abort=true on a non-terminal execution.
	PhaseAbort Phase = "abort"
)

// Result reports per-statement timings + a human-readable description of
// what the engine did. Mirrors migration.FileResult so the
// MigrationExecutionReconciler can surface a unified status shape.
type Result struct {
	Description string
	Statements  []string
}

// Apply dispatches the operation to the kind-specific handler.
func (e *Engine) Apply(ctx context.Context, op keystonev1alpha1.MigrationOperation, phase Phase) (*Result, error) {
	if err := sqlquote.ValidateIdentifier("table", op.Table); err != nil {
		return nil, err
	}
	switch op.Kind {
	case keystonev1alpha1.MigrationOperationAddColumn:
		if op.AddColumn == nil {
			return nil, fmt.Errorf("kind=add_column requires spec.addColumn")
		}
		return e.applyAddColumn(ctx, op.Table, op.AddColumn, phase)
	case keystonev1alpha1.MigrationOperationDropColumn:
		if op.DropColumn == nil {
			return nil, fmt.Errorf("kind=drop_column requires spec.dropColumn")
		}
		return e.applyDropColumn(ctx, op.Table, op.DropColumn, phase)
	case keystonev1alpha1.MigrationOperationRenameColumn:
		if op.RenameColumn == nil {
			return nil, fmt.Errorf("kind=rename_column requires spec.renameColumn")
		}
		return e.applyRenameColumn(ctx, op.Table, op.RenameColumn, phase)
	case keystonev1alpha1.MigrationOperationAddConstraint:
		if op.AddConstraint == nil {
			return nil, fmt.Errorf("kind=add_constraint requires spec.addConstraint")
		}
		return e.applyAddConstraint(ctx, op.Table, op.AddConstraint, phase)
	case keystonev1alpha1.MigrationOperationAlterColumnType:
		if op.AlterColumnType == nil {
			return nil, fmt.Errorf("kind=alter_column_type requires spec.alterColumnType")
		}
		return e.applyAlterColumnType(ctx, op.Table, op.AlterColumnType, phase)
	case keystonev1alpha1.MigrationOperationSetNotNull:
		if op.SetNotNull == nil {
			return nil, fmt.Errorf("kind=set_not_null requires spec.setNotNull")
		}
		return e.applySetNotNull(ctx, op.Table, op.SetNotNull, phase)
	case keystonev1alpha1.MigrationOperationDropNotNull:
		if op.DropNotNull == nil {
			return nil, fmt.Errorf("kind=drop_not_null requires spec.dropNotNull")
		}
		return e.applyDropNotNull(ctx, op.Table, op.DropNotNull, phase)
	default:
		return nil, fmt.Errorf("unknown operation kind %q", op.Kind)
	}
}

// setNotNullConstraintName is the deterministic name used for the
// NOT VALID CHECK that set_not_null installs during Expand. Exposed so
// the abort handler names the same constraint.
func setNotNullConstraintName(column string) string {
	return "keystone_notnull_" + column
}

// applyAddConstraint — Expand: ADD CONSTRAINT … NOT VALID (no
// AccessExclusive lock). Contract: VALIDATE CONSTRAINT (SHARE UPDATE
// EXCLUSIVE — readers + writers continue).
//
// Constraint definition is hand-validated here as a charset whitelist
// because it lands directly in DDL (no parameter binding). Admission
// catches the obvious cases first; this is defence in depth.
func (e *Engine) applyAddConstraint(ctx context.Context, table string, op *keystonev1alpha1.AddConstraintOp, phase Phase) (*Result, error) {
	if err := sqlquote.ValidateIdentifier("constraint", op.Name); err != nil {
		return nil, err
	}
	if err := validateConstraintBody(op.Definition); err != nil {
		return nil, err
	}
	res := &Result{}
	switch phase {
	case PhaseExpand:
		var clause string
		switch op.Type {
		case "check":
			clause = fmt.Sprintf("CHECK %s NOT VALID", op.Definition)
		case "foreign_key":
			// Definition must already start with FOREIGN KEY (...)
			// REFERENCES ... — admission verifies.
			clause = fmt.Sprintf("%s NOT VALID", op.Definition)
		default:
			return nil, fmt.Errorf("unsupported constraint type %q", op.Type)
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s",
			e.qualified(table), sqlquote.QuoteIdentifier(op.Name), clause)
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("expand add_constraint: %w", err)
		}
		res.Description = fmt.Sprintf("added constraint %s on %s NOT VALID", op.Name, table)
		res.Statements = []string{stmt}
		return res, nil
	case PhaseContract:
		stmt := fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s",
			e.qualified(table), sqlquote.QuoteIdentifier(op.Name))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("contract validate constraint: %w", err)
		}
		res.Description = fmt.Sprintf("validated constraint %s on %s", op.Name, table)
		res.Statements = []string{stmt}
		return res, nil
	case PhaseAbort:
		// Drop the NOT VALID constraint installed during Expand. IF
		// EXISTS makes the abort safe if Expand never ran.
		stmt := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s",
			e.qualified(table), sqlquote.QuoteIdentifier(op.Name))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("abort drop constraint: %w", err)
		}
		res.Description = fmt.Sprintf("aborted add_constraint %s on %s (constraint dropped)", op.Name, table)
		res.Statements = []string{stmt}
		return res, nil
	}
	return nil, fmt.Errorf("unknown phase %q", phase)
}

// applyAlterColumnType — Expand: ADD shadow column with new type;
// COPY converted data via cast expression; INSTALL trigger that
// mirrors writes both ways. Contract: DROP TRIGGER + DROP old column +
// RENAME shadow → original.
//
// Names: shadow column is "<column>_keystone_alter"; trigger is
// "keystone_alter_<table>_<column>"; trigger function is
// "<trigger>_fn".
func (e *Engine) applyAlterColumnType(ctx context.Context, table string, op *keystonev1alpha1.AlterColumnTypeOp, phase Phase) (*Result, error) {
	if err := sqlquote.ValidateIdentifier("column", op.Column); err != nil {
		return nil, err
	}
	if err := validateColumnType(op.NewType); err != nil {
		return nil, err
	}
	shadow := op.Column + "_keystone_alter"
	cast := op.CastExpression
	if cast == "" {
		cast = fmt.Sprintf("%s::%s", sqlquote.QuoteIdentifier(op.Column), op.NewType)
	}
	triggerName := fmt.Sprintf("keystone_alter_%s_%s", table, op.Column)
	functionName := triggerName + "_fn"

	res := &Result{}
	switch phase {
	case PhaseExpand:
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		stmts := []string{
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
				e.qualified(table), sqlquote.QuoteIdentifier(shadow), op.NewType),
			fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s IS DISTINCT FROM (%s)",
				e.qualified(table),
				sqlquote.QuoteIdentifier(shadow),
				cast,
				sqlquote.QuoteIdentifier(shadow),
				cast),
			fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.%s() RETURNS trigger AS $body$
BEGIN
	IF NEW.%s IS DISTINCT FROM OLD.%s THEN
		NEW.%s := %s;
	ELSIF NEW.%s IS DISTINCT FROM OLD.%s THEN
		NEW.%s := NEW.%s;
	END IF;
	RETURN NEW;
END $body$ LANGUAGE plpgsql`,
				sqlquote.QuoteIdentifier(e.schema), sqlquote.QuoteIdentifier(functionName),
				sqlquote.QuoteIdentifier(op.Column), sqlquote.QuoteIdentifier(op.Column),
				sqlquote.QuoteIdentifier(shadow), cast,
				sqlquote.QuoteIdentifier(shadow), sqlquote.QuoteIdentifier(shadow),
				sqlquote.QuoteIdentifier(op.Column), sqlquote.QuoteIdentifier(shadow),
			),
			fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s",
				sqlquote.QuoteIdentifier(triggerName), e.qualified(table)),
			fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s.%s()",
				sqlquote.QuoteIdentifier(triggerName), e.qualified(table),
				sqlquote.QuoteIdentifier(e.schema), sqlquote.QuoteIdentifier(functionName)),
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("alter_column_type expand: %w (sql: %s)", err, s)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		res.Description = fmt.Sprintf("expanded alter_column_type %s.%s → %s (shadow %s installed)",
			table, op.Column, op.NewType, shadow)
		res.Statements = stmts
		return res, nil

	case PhaseContract:
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		stmts := []string{
			fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s",
				sqlquote.QuoteIdentifier(triggerName), e.qualified(table)),
			fmt.Sprintf("DROP FUNCTION IF EXISTS %s.%s()",
				sqlquote.QuoteIdentifier(e.schema), sqlquote.QuoteIdentifier(functionName)),
			fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s",
				e.qualified(table), sqlquote.QuoteIdentifier(op.Column)),
			fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
				e.qualified(table), sqlquote.QuoteIdentifier(shadow), sqlquote.QuoteIdentifier(op.Column)),
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("alter_column_type contract: %w (sql: %s)", err, s)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		res.Description = fmt.Sprintf("contracted alter_column_type %s.%s (shadow promoted to original)",
			table, op.Column)
		res.Statements = stmts
		return res, nil
	case PhaseAbort:
		// Drop shadow column + trigger + function, leaving the original
		// column untouched. Each stmt uses IF EXISTS so repeat aborts
		// are safe; no transaction wrapper because independent DROPs
		// can succeed individually.
		stmts := []string{
			fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s",
				sqlquote.QuoteIdentifier(triggerName), e.qualified(table)),
			fmt.Sprintf("DROP FUNCTION IF EXISTS %s.%s()",
				sqlquote.QuoteIdentifier(e.schema), sqlquote.QuoteIdentifier(functionName)),
			fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s",
				e.qualified(table), sqlquote.QuoteIdentifier(shadow)),
		}
		for _, s := range stmts {
			if _, err := e.pool.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("abort alter_column_type: %w (sql: %s)", err, s)
			}
		}
		res.Description = fmt.Sprintf("aborted alter_column_type %s.%s (shadow %s torn down)",
			table, op.Column, shadow)
		res.Statements = stmts
		return res, nil
	}
	return nil, fmt.Errorf("unknown phase %q", phase)
}

// validateConstraintBody applies a defence-in-depth charset whitelist
// to constraint definitions. Permits letters, digits, parens, comma,
// space, period, underscore, comparison operators, single quotes
// (string literals in CHECK clauses are common), and the FK-required
// keywords. Refuses semicolons, double quotes (would let attackers
// break out of identifier quoting elsewhere if reused), and NUL.
func validateConstraintBody(s string) error {
	if s == "" {
		return fmt.Errorf("constraint definition required")
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '(' || c == ')' || c == ',' || c == ' ',
			c == '.' || c == '_' || c == '\'',
			c == '<' || c == '>' || c == '=' || c == '!',
			c == '+' || c == '-' || c == '*' || c == '/' || c == '%',
			c == '\n' || c == '\t':
			continue
		default:
			return fmt.Errorf("constraint definition contains illegal character %q", c)
		}
	}
	return nil
}

// qualified returns "schema"."table".
func (e *Engine) qualified(table string) string {
	return sqlquote.QuoteIdentifier(e.schema) + "." + sqlquote.QuoteIdentifier(table)
}

// validateColumnType is a defence-in-depth check against the
// admission-time max-length validation. Rejects suspicious bytes.
func validateColumnType(t string) error {
	if t == "" {
		return fmt.Errorf("type required")
	}
	for _, c := range t {
		// Allow letters, digits, parens, comma, space, period, underscore.
		// Refuse anything else — covers semicolon, quote, NUL.
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '(' || c == ')',
			c == ',' || c == ' ',
			c == '.' || c == '_':
			continue
		default:
			return fmt.Errorf("type %q contains illegal character %q", t, c)
		}
	}
	return nil
}

// applyAddColumn — Expand adds the column nullable (with optional
// default that backfills existing rows under PG 11+ instant fast-path
// when default is constant). Contract optionally sets NOT NULL.
func (e *Engine) applyAddColumn(ctx context.Context, table string, op *keystonev1alpha1.AddColumnOp, phase Phase) (*Result, error) {
	if err := sqlquote.ValidateIdentifier("column", op.Name); err != nil {
		return nil, err
	}
	if err := validateColumnType(op.Type); err != nil {
		return nil, err
	}

	res := &Result{}
	switch phase {
	case PhaseExpand:
		clause := fmt.Sprintf("ADD COLUMN IF NOT EXISTS %s %s",
			sqlquote.QuoteIdentifier(op.Name), op.Type)
		if op.Default != "" {
			clause += " DEFAULT " + op.Default
		}
		// During Expand we always allow NULL — even if EnforceNotNullInContract
		// is true. That makes the Expand step non-blocking.
		stmt := fmt.Sprintf("ALTER TABLE %s %s", e.qualified(table), clause)
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("expand add_column: %w", err)
		}
		res.Description = fmt.Sprintf("added column %s.%s %s (nullable, expand phase)", table, op.Name, op.Type)
		res.Statements = []string{stmt}
		return res, nil

	case PhaseContract:
		if op.Nullable && !op.EnforceNotNullInContract {
			res.Description = "no contract action (column stays nullable per spec)"
			return res, nil
		}
		// Set NOT NULL; will fail if backfill is incomplete — operator
		// must verify first.
		stmt := fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL",
			e.qualified(table), sqlquote.QuoteIdentifier(op.Name))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("contract add_column SET NOT NULL: %w", err)
		}
		res.Description = fmt.Sprintf("set %s.%s NOT NULL (contract phase)", table, op.Name)
		res.Statements = []string{stmt}
		return res, nil
	case PhaseAbort:
		stmt := fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s",
			e.qualified(table), sqlquote.QuoteIdentifier(op.Name))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("abort add_column: %w", err)
		}
		res.Description = fmt.Sprintf("aborted add_column %s.%s (column dropped)", table, op.Name)
		res.Statements = []string{stmt}
		return res, nil
	}
	return nil, fmt.Errorf("unknown phase %q", phase)
}

// applyDropColumn — Expand records intent only (no schema mutation, so
// readers continue to work). Contract issues DROP COLUMN.
func (e *Engine) applyDropColumn(ctx context.Context, table string, op *keystonev1alpha1.DropColumnOp, phase Phase) (*Result, error) {
	if err := sqlquote.ValidateIdentifier("column", op.Name); err != nil {
		return nil, err
	}
	res := &Result{}
	switch phase {
	case PhaseExpand:
		// Expand is intentionally a no-op. Comment the column to leave
		// breadcrumbs for any operator inspecting the schema directly.
		stmt := fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s",
			e.qualified(table), sqlquote.QuoteIdentifier(op.Name),
			sqlquote.QuoteString("[keystone] scheduled for drop in contract phase"))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("expand drop_column comment: %w", err)
		}
		res.Description = fmt.Sprintf("marked %s.%s as scheduled-for-drop (no schema change yet)", table, op.Name)
		res.Statements = []string{stmt}
		return res, nil

	case PhaseContract:
		stmt := fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s",
			e.qualified(table), sqlquote.QuoteIdentifier(op.Name))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("contract drop_column: %w", err)
		}
		res.Description = fmt.Sprintf("dropped column %s.%s", table, op.Name)
		res.Statements = []string{stmt}
		return res, nil
	case PhaseAbort:
		// Expand was a metadata-only COMMENT; abort restores the
		// original (empty) comment. Apps were never aware of the
		// scheduled drop so nothing else to undo.
		stmt := fmt.Sprintf("COMMENT ON COLUMN %s.%s IS NULL",
			e.qualified(table), sqlquote.QuoteIdentifier(op.Name))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("abort drop_column comment: %w", err)
		}
		res.Description = fmt.Sprintf("aborted drop_column %s.%s (deprecation comment cleared)", table, op.Name)
		res.Statements = []string{stmt}
		return res, nil
	}
	return nil, fmt.Errorf("unknown phase %q", phase)
}

// applyRenameColumn — Expand:
//   1. ADD new column with same type as old
//   2. UPDATE new = old to backfill
//   3. CREATE TRIGGER mirroring writes between old and new
// Contract:
//   1. DROP TRIGGER
//   2. DROP old column
//
// The trigger is intentionally bidirectional during Expand so apps can
// be in mid-rollout reading either name. Single-statement DDL inside a
// transaction throughout to avoid mid-flight inconsistency.
func (e *Engine) applyRenameColumn(ctx context.Context, table string, op *keystonev1alpha1.RenameColumnOp, phase Phase) (*Result, error) {
	if err := sqlquote.ValidateIdentifier("column", op.From); err != nil {
		return nil, err
	}
	if err := sqlquote.ValidateIdentifier("column", op.To); err != nil {
		return nil, err
	}
	if op.From == op.To {
		return nil, fmt.Errorf("rename_column: from and to are identical")
	}

	res := &Result{}
	triggerName := fmt.Sprintf("keystone_rename_%s_%s_%s", table, op.From, op.To)
	functionName := triggerName + "_fn"

	switch phase {
	case PhaseExpand:
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		// 1. Read existing column type from information_schema so the
		// new column matches exactly.
		var dataType, udtName string
		err = tx.QueryRow(ctx, `
			SELECT data_type, udt_name
			  FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
		`, e.schema, table, op.From).Scan(&dataType, &udtName)
		if err != nil {
			return nil, fmt.Errorf("look up column type for %s.%s: %w", table, op.From, err)
		}
		colType := dataType
		// USER-DEFINED falls back to udt_name (enums, custom domains).
		if strings.EqualFold(dataType, "USER-DEFINED") {
			colType = udtName
		}

		stmts := []string{
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
				e.qualified(table), sqlquote.QuoteIdentifier(op.To), colType),
			fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s IS DISTINCT FROM %s",
				e.qualified(table),
				sqlquote.QuoteIdentifier(op.To), sqlquote.QuoteIdentifier(op.From),
				sqlquote.QuoteIdentifier(op.To), sqlquote.QuoteIdentifier(op.From)),
			// Trigger function: mirror NEW.from→to AND NEW.to→from on
			// every write so apps reading either name see fresh data.
			fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s.%s() RETURNS trigger AS $body$
BEGIN
	IF NEW.%s IS DISTINCT FROM OLD.%s THEN
		NEW.%s := NEW.%s;
	ELSIF NEW.%s IS DISTINCT FROM OLD.%s THEN
		NEW.%s := NEW.%s;
	END IF;
	RETURN NEW;
END $body$ LANGUAGE plpgsql`,
				sqlquote.QuoteIdentifier(e.schema), sqlquote.QuoteIdentifier(functionName),
				sqlquote.QuoteIdentifier(op.From), sqlquote.QuoteIdentifier(op.From),
				sqlquote.QuoteIdentifier(op.To), sqlquote.QuoteIdentifier(op.From),
				sqlquote.QuoteIdentifier(op.To), sqlquote.QuoteIdentifier(op.To),
				sqlquote.QuoteIdentifier(op.From), sqlquote.QuoteIdentifier(op.To),
			),
			fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s",
				sqlquote.QuoteIdentifier(triggerName), e.qualified(table)),
			fmt.Sprintf("CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION %s.%s()",
				sqlquote.QuoteIdentifier(triggerName), e.qualified(table),
				sqlquote.QuoteIdentifier(e.schema), sqlquote.QuoteIdentifier(functionName)),
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("rename_column expand stmt: %w (sql: %s)", err, s)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		res.Description = fmt.Sprintf("expanded rename %s.%s → %s (trigger %s installed)",
			table, op.From, op.To, triggerName)
		res.Statements = stmts
		return res, nil

	case PhaseContract:
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		stmts := []string{
			fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s",
				sqlquote.QuoteIdentifier(triggerName), e.qualified(table)),
			fmt.Sprintf("DROP FUNCTION IF EXISTS %s.%s()",
				sqlquote.QuoteIdentifier(e.schema), sqlquote.QuoteIdentifier(functionName)),
			fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s",
				e.qualified(table), sqlquote.QuoteIdentifier(op.From)),
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("rename_column contract stmt: %w (sql: %s)", err, s)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		res.Description = fmt.Sprintf("contracted rename %s.%s → %s (old column dropped)",
			table, op.From, op.To)
		res.Statements = stmts
		return res, nil
	case PhaseAbort:
		// Rollback: drop the new column + trigger + function, keeping
		// the original column untouched so apps still see the source
		// of truth. IF EXISTS keeps this safe under repeat aborts.
		stmts := []string{
			fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s",
				sqlquote.QuoteIdentifier(triggerName), e.qualified(table)),
			fmt.Sprintf("DROP FUNCTION IF EXISTS %s.%s()",
				sqlquote.QuoteIdentifier(e.schema), sqlquote.QuoteIdentifier(functionName)),
			fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s",
				e.qualified(table), sqlquote.QuoteIdentifier(op.To)),
		}
		for _, s := range stmts {
			if _, err := e.pool.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("abort rename_column: %w (sql: %s)", err, s)
			}
		}
		res.Description = fmt.Sprintf("aborted rename %s.%s → %s (new column + trigger torn down)",
			table, op.From, op.To)
		res.Statements = stmts
		return res, nil
	}
	return nil, fmt.Errorf("unknown phase %q", phase)
}

// applySetNotNull implements the lock-light NOT NULL transition:
//   Expand: ADD CONSTRAINT chk_<col>_notnull CHECK (col IS NOT NULL) NOT VALID
//   Contract: VALIDATE CONSTRAINT, SET NOT NULL, DROP CONSTRAINT
//   Abort:   DROP CONSTRAINT IF EXISTS
//
// Contract is safe to retry — if VALIDATE fails on existing NULL rows,
// the operator fixes the data and the next reconcile retries without
// needing to redo Expand.
func (e *Engine) applySetNotNull(ctx context.Context, table string, op *keystonev1alpha1.SetNotNullOp, phase Phase) (*Result, error) {
	if err := sqlquote.ValidateIdentifier("column", op.Column); err != nil {
		return nil, err
	}
	name := setNotNullConstraintName(op.Column)
	res := &Result{}
	switch phase {
	case PhaseExpand:
		stmt := fmt.Sprintf(
			"ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID",
			e.qualified(table), sqlquote.QuoteIdentifier(name), sqlquote.QuoteIdentifier(op.Column))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("expand set_not_null: %w", err)
		}
		res.Description = fmt.Sprintf("added NOT VALID check %s on %s.%s", name, table, op.Column)
		res.Statements = []string{stmt}
		return res, nil
	case PhaseContract:
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		stmts := []string{
			// VALIDATE takes SHARE UPDATE EXCLUSIVE only — readers +
			// writers continue while the scan runs.
			fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s",
				e.qualified(table), sqlquote.QuoteIdentifier(name)),
			// SET NOT NULL is near-instant because PG recognises the
			// proven CHECK — no full-table scan.
			fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL",
				e.qualified(table), sqlquote.QuoteIdentifier(op.Column)),
			// Drop the now-redundant check so future schemas don't
			// carry the noise.
			fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s",
				e.qualified(table), sqlquote.QuoteIdentifier(name)),
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("contract set_not_null: %w (sql: %s)", err, s)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		res.Description = fmt.Sprintf("set %s.%s NOT NULL via validated CHECK", table, op.Column)
		res.Statements = stmts
		return res, nil
	case PhaseAbort:
		stmt := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s",
			e.qualified(table), sqlquote.QuoteIdentifier(name))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("abort set_not_null: %w", err)
		}
		res.Description = fmt.Sprintf("aborted set_not_null on %s.%s (check dropped)", table, op.Column)
		res.Statements = []string{stmt}
		return res, nil
	}
	return nil, fmt.Errorf("unknown phase %q", phase)
}

// applyDropNotNull removes a NOT NULL constraint. Single-phase by
// design: DROP NOT NULL is metadata-only (no scan, brief lock) and
// rolling it forward is the only useful direction. Abort is
// best-effort — once NULL rows exist, re-applying NOT NULL would
// require data cleanup we can't do blindly.
func (e *Engine) applyDropNotNull(ctx context.Context, table string, op *keystonev1alpha1.DropNotNullOp, phase Phase) (*Result, error) {
	if err := sqlquote.ValidateIdentifier("column", op.Column); err != nil {
		return nil, err
	}
	res := &Result{}
	switch phase {
	case PhaseExpand:
		stmt := fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL",
			e.qualified(table), sqlquote.QuoteIdentifier(op.Column))
		if _, err := e.pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("expand drop_not_null: %w", err)
		}
		res.Description = fmt.Sprintf("dropped NOT NULL from %s.%s", table, op.Column)
		res.Statements = []string{stmt}
		return res, nil
	case PhaseContract:
		res.Description = "no contract action for drop_not_null (single-phase)"
		return res, nil
	case PhaseAbort:
		// Best-effort: don't re-apply NOT NULL. NULL rows may exist and
		// blocking the abort on data cleanup is worse than leaving the
		// column nullable. Record a notice so operators know.
		res.Description = fmt.Sprintf(
			"drop_not_null on %s.%s cannot be aborted cleanly — NULL rows may exist. "+
				"Column remains nullable; re-establish NOT NULL via a new set_not_null bundle after data cleanup.",
			table, op.Column)
		return res, nil
	}
	return nil, fmt.Errorf("unknown phase %q", phase)
}
