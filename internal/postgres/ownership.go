// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dogukanturhal/keystone-sdk/go/sqlquote"
)

// Ownership drift: objects inside a LogicalDatabase's managed schemas whose
// owner is not the expected role (the role the migration runner SET LOCAL
// ROLEs to). Inspection is one read-only catalog query that groups in SQL;
// remediation reassigns only objects owned by the connection role
// (session_user), one transaction per object.
//
// Routines with an internal dependency (range and multirange constructors)
// are generated with their type, cannot be altered on their own and are not
// reported.
//
// The catalog query deliberately avoids pg_relation_size, pg_get_*def and
// casts of user-supplied names: those can take locks on user relations. Plain
// catalog SELECTs only take AccessShareLock on catalogs, so they neither
// block nor get blocked by user DDL or DML.

// Object kinds reported by the ownership query.
const (
	OwnershipKindSchema = "schema"
)

// Remediation tunables.
const (
	ownershipInspectStatementTimeout = "5s"
	ownershipInspectLockTimeout      = "1s"
	ownershipReassignLockTimeout     = "500ms"
	ownershipReassignStmtTimeout     = "10s"

	// DefaultReassignBudget is the total wall-clock budget of one remediation
	// pass. The LogicalDatabase controller reconciles one object at a time,
	// so a hot table must not delay every other reconcile.
	DefaultReassignBudget = 10 * time.Second

	// MaxRemediationCandidates caps one pass; the next cycle continues.
	MaxRemediationCandidates = 500

	maxOwnershipGroups = 64
)

// rlsDependentExpr is true for a pg_class row aliased c that reaches an
// RLS-enabled relation: a view or matview whose rewrite rule references an RLS
// table, or an inheritance/partition child with an RLS-enabled ancestor at any
// depth. Changing the owner of such an object changes whose RLS context (and
// bypass) applies to the data it exposes, so remediation refuses it.
const rlsDependentExpr = `(CASE
  WHEN c.relkind IN ('v','m') THEN EXISTS (
    SELECT 1 FROM pg_rewrite rw
    JOIN pg_depend d ON d.classid='pg_rewrite'::regclass AND d.objid=rw.oid AND d.refclassid='pg_class'::regclass
    JOIN pg_class t ON t.oid=d.refobjid
    WHERE rw.ev_class=c.oid AND t.oid<>c.oid AND t.relrowsecurity)
  WHEN c.relkind IN ('r','p','f') THEN EXISTS (
    WITH RECURSIVE anc(oid) AS (
      SELECT i.inhparent FROM pg_inherits i WHERE i.inhrelid=c.oid
      UNION
      SELECT i.inhparent FROM pg_inherits i JOIN anc a ON i.inhrelid=a.oid)
    SELECT 1 FROM anc JOIN pg_class t ON t.oid=anc.oid WHERE t.relrowsecurity)
  ELSE false END)`

// ownershipCTE is shared by the summary and the candidate query.
//
//	$1 text[] managed schema names
//	$2 text[] expected schema owners, parallel to $1
//	$3 text   expected object owner
//	$4 text[] excluded relnames (tracking tables)
var ownershipCTE = `
WITH want AS (
  SELECT (SELECT oid FROM pg_roles WHERE rolname = $3)           AS obj_owner,
         (SELECT oid FROM pg_roles WHERE rolname = session_user) AS conn_role
), nsp AS (
  SELECT n.oid, n.nspname, n.nspowner,
         (SELECT oid FROM pg_roles WHERE rolname = m.owner) AS want_owner
  FROM unnest($1::text[], $2::text[]) AS m(name, owner)
  JOIN pg_namespace n ON n.nspname = m.name
  WHERE n.nspname NOT IN ('pg_catalog','information_schema','pg_toast')
    AND n.nspname !~ '^pg_(toast_)?temp_'
), objs AS (
  SELECT 'pg_class'::regclass AS classid, c.oid AS objid,
         CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'partitioned_table' WHEN 'v' THEN 'view'
              WHEN 'm' THEN 'materialized_view' WHEN 'f' THEN 'foreign_table' WHEN 'S' THEN 'sequence' END AS kind,
         format('%I.%I', n.nspname, c.relname) AS ident, c.relowner AS owner, w.obj_owner AS want_owner,
         (c.relkind = 'S' AND EXISTS (SELECT 1 FROM pg_depend d
            WHERE d.classid='pg_class'::regclass AND d.objid=c.oid
              AND d.refclassid='pg_class'::regclass AND d.deptype IN ('a','i'))) AS follows_parent,
         (c.relrowsecurity AND NOT c.relforcerowsecurity) AS rls_unforced,
         ` + rlsDependentExpr + ` AS rls_dep,
         false AS secdef
  FROM pg_class c JOIN nsp n ON n.oid = c.relnamespace CROSS JOIN want w
  WHERE c.relkind IN ('r','p','v','m','f','S')
    AND NOT (c.relkind IN ('r','p') AND c.relname = ANY($4::text[]))
  UNION ALL
  SELECT 'pg_proc'::regclass, p.oid,
         CASE p.prokind WHEN 'f' THEN 'function' WHEN 'p' THEN 'procedure'
              WHEN 'a' THEN 'aggregate' WHEN 'w' THEN 'window_function' END,
         p.oid::regprocedure::text, p.proowner, w.obj_owner, false, false, false, p.prosecdef
  FROM pg_proc p JOIN nsp n ON n.oid = p.pronamespace CROSS JOIN want w
  WHERE NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_proc'::regclass AND d.objid=p.oid AND d.deptype='i')
  UNION ALL
  SELECT 'pg_type'::regclass, t.oid,
         CASE t.typtype WHEN 'e' THEN 'enum' WHEN 'd' THEN 'domain' WHEN 'c' THEN 'composite_type'
              WHEN 'r' THEN 'range' WHEN 'b' THEN 'base_type' END,
         format('%I.%I', n.nspname, t.typname), t.typowner, w.obj_owner, false, false, false, false
  FROM pg_type t JOIN nsp n ON n.oid = t.typnamespace CROSS JOIN want w
  WHERE t.typtype IN ('e','d','c','r','b')
    AND (t.typtype <> 'c' OR EXISTS (SELECT 1 FROM pg_class rc WHERE rc.oid=t.typrelid AND rc.relkind='c'))
    AND NOT EXISTS (SELECT 1 FROM pg_type e WHERE e.oid=t.typelem AND e.typarray=t.oid)
  UNION ALL
  SELECT 'pg_namespace'::regclass, n.oid, 'schema', format('%I', n.nspname), n.nspowner, n.want_owner, false, false, false, false
  FROM nsp n WHERE n.want_owner IS NOT NULL
), drift AS (
  SELECT o.*, pg_get_userbyid(o.owner) AS owner_name, o.owner = w.conn_role AS conn_owned
  FROM objs o CROSS JOIN want w
  WHERE o.owner IS DISTINCT FROM o.want_owner
    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid=o.classid AND d.objid=o.objid AND d.deptype='e')
)`

var ownershipSummarySQL = ownershipCTE + `
SELECT kind, owner_name, conn_owned, count(*)::int AS n,
       count(*) FILTER (WHERE follows_parent OR rls_unforced OR rls_dep OR secdef)::int AS n_blocked,
       (array_agg(ident ORDER BY ident))[1:$5] AS sample,
       (sum(count(*)) OVER ())::int AS total
FROM drift GROUP BY 1,2,3 ORDER BY n DESC, 1, 2
LIMIT ` + strconv.Itoa(maxOwnershipGroups)

var ownershipCandidatesSQL = ownershipCTE + `
SELECT classid::oid, objid, kind, ident, follows_parent, rls_unforced, rls_dep, secdef FROM drift
WHERE conn_owned AND kind <> 'schema'
ORDER BY (kind IN ('table','partitioned_table','foreign_table')) DESC, ident
LIMIT $5`

// SchemaOwner pairs a managed schema with the owner its DatabaseSchema expects.
type SchemaOwner struct {
	Name  string
	Owner string
}

// OwnershipQuery parameterises InspectOwnership.
type OwnershipQuery struct {
	Schemas []SchemaOwner
	// ExpectedOwner is LogicalDatabase.spec.ownerRole.
	ExpectedOwner string
	// ExcludedRelations are tracking-table names never reported.
	ExcludedRelations []string
	// SampleSize bounds the identifiers sampled per group.
	SampleSize int
}

func (q OwnershipQuery) args(last any) []any {
	names := make([]string, len(q.Schemas))
	owners := make([]string, len(q.Schemas))
	for i, s := range q.Schemas {
		names[i], owners[i] = s.Name, s.Owner
	}
	excl := q.ExcludedRelations
	if excl == nil {
		excl = []string{}
	}
	return []any{names, owners, q.ExpectedOwner, excl, last}
}

// OwnershipGroup is one (kind, owner) bucket of drifted objects.
type OwnershipGroup struct {
	Kind      string
	Owner     string
	ConnOwned bool
	Count     int
	// Blocked counts objects remediation would refuse (owned sequence,
	// RLS-unforced table, SECURITY DEFINER routine).
	Blocked int
	Sample  []string
}

// OwnershipReport is the result of one inspection.
type OwnershipReport struct {
	// OwnerRoleMissing: the expected owner role does not exist (nothing else
	// is computed; a NULL owner would mark everything as drift).
	OwnerRoleMissing bool
	// ConnRoleSuper: the connection role is a superuser; remediation is refused.
	ConnRoleSuper bool
	// ConnRole is session_user, the role remediation acts for.
	ConnRole string
	// Groups is capped at maxOwnershipGroups (largest first).
	Groups []OwnershipGroup
	// TotalDrifted is the exact number of drifted objects, including groups
	// beyond the cap.
	TotalDrifted int
}

// Total is the number of drifted objects, exact even when Groups is capped.
func (r OwnershipReport) Total() int {
	if r.TotalDrifted > 0 {
		return r.TotalDrifted
	}
	n := 0
	for _, g := range r.Groups {
		n += g.Count
	}
	return n
}

// RemediableCount is the number of drifted objects a remediation pass could
// move: owned by the connection role, not blocked, connection role not a
// superuser. Schemas are never remediable.
func (r OwnershipReport) RemediableCount() int {
	if r.ConnRoleSuper {
		return 0
	}
	n := 0
	for _, g := range r.Groups {
		if g.Remediable() > 0 {
			n += g.Remediable()
		}
	}
	return n
}

// Remediable is the count in this group a remediation pass could move.
func (g OwnershipGroup) Remediable() int {
	if !g.ConnOwned || g.Kind == OwnershipKindSchema {
		return 0
	}
	return g.Count - g.Blocked
}

func beginInspect(ctx context.Context, pool *pgxpool.Pool, stmt, lock string) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	// search_path=pg_catalog forces regprocedure/regclass text to come out
	// schema-qualified.
	for _, s := range []string{
		"SET LOCAL statement_timeout = '" + stmt + "'",
		"SET LOCAL lock_timeout = '" + lock + "'",
		"SET LOCAL search_path = pg_catalog",
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("%s: %w", s, err)
		}
	}
	return tx, nil
}

// connRole returns session_user and whether it is a superuser.
func connRole(ctx context.Context, tx pgx.Tx) (name string, super bool, err error) {
	err = tx.QueryRow(ctx, `SELECT session_user::text, COALESCE((SELECT rolsuper FROM pg_roles WHERE rolname = session_user), false)`).Scan(&name, &super)
	return name, super, err
}

// InspectOwnership runs the ownership catalog query in a read-only
// transaction and returns the bounded, grouped report.
func InspectOwnership(ctx context.Context, pool *pgxpool.Pool, q OwnershipQuery) (OwnershipReport, error) {
	var rep OwnershipReport
	if q.SampleSize <= 0 {
		q.SampleSize = 5
	}
	tx, err := beginInspect(ctx, pool, ownershipInspectStatementTimeout, ownershipInspectLockTimeout)
	if err != nil {
		return rep, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)`, q.ExpectedOwner).Scan(&exists); err != nil {
		return rep, fmt.Errorf("owner role preflight: %w", err)
	}
	if !exists {
		rep.OwnerRoleMissing = true
		return rep, nil
	}
	if rep.ConnRole, rep.ConnRoleSuper, err = connRole(ctx, tx); err != nil {
		return rep, fmt.Errorf("superuser preflight: %w", err)
	}

	rows, err := tx.Query(ctx, ownershipSummarySQL, q.args(int32(q.SampleSize))...)
	if err != nil {
		return rep, fmt.Errorf("ownership query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g OwnershipGroup
		if err := rows.Scan(&g.Kind, &g.Owner, &g.ConnOwned, &g.Count, &g.Blocked, &g.Sample, &rep.TotalDrifted); err != nil {
			return rep, fmt.Errorf("scan ownership row: %w", err)
		}
		rep.Groups = append(rep.Groups, g)
	}
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("ownership rows: %w", err)
	}
	return rep, nil
}

// Candidate is one drifted object owned by the connection role.
type Candidate struct {
	ClassID       uint32
	ObjID         uint32
	Kind          string
	Ident         string
	FollowsParent bool
	RLSUnforced   bool
	RLSDependent  bool
	SecDef        bool
}

// Skip reasons returned by Candidate.Refusal.
const (
	SkipSchema   = "skipped_schema"
	SkipOwnedSeq = "skipped_owned_sequence"
	SkipRLS      = "skipped_rls"
	// SkipRLSDependent: a view/matview over an RLS table, or a child of an
	// RLS-enabled inheritance/partition ancestor.
	SkipRLSDependent = "skipped_rls_dependent"
	SkipSecDef       = "skipped_secdef"
	SkipSuperuser    = "skipped_superuser"
)

// Refusal returns a non-empty skip reason when the candidate must not be
// reassigned: schemas, owned sequences (they follow their table),
// RLS-enabled-but-unforced tables (an owner bypasses non-FORCE RLS), views and
// inheritance/partition children that reach an RLS table, and
// SECURITY DEFINER routines (an owner change alters whose rights they run
// with). A superuser connection role refuses everything.
func (c Candidate) Refusal(connIsSuper bool) string {
	switch {
	case connIsSuper:
		return SkipSuperuser
	case c.Kind == OwnershipKindSchema:
		return SkipSchema
	case c.FollowsParent:
		return SkipOwnedSeq
	case c.RLSUnforced:
		return SkipRLS
	case c.RLSDependent:
		return SkipRLSDependent
	case c.SecDef:
		return SkipSecDef
	}
	return ""
}

// alterKeyword maps an object kind to the ALTER target keyword.
func alterKeyword(kind string) (string, error) {
	switch kind {
	case "table", "partitioned_table":
		return "TABLE", nil
	case "view":
		return "VIEW", nil
	case "materialized_view":
		return "MATERIALIZED VIEW", nil
	case "foreign_table":
		return "FOREIGN TABLE", nil
	case "sequence":
		return "SEQUENCE", nil
	case "function", "procedure", "aggregate", "window_function":
		return "ROUTINE", nil
	case "domain":
		return "DOMAIN", nil
	case "enum", "composite_type", "range", "base_type":
		return "TYPE", nil
	}
	return "", fmt.Errorf("no ALTER ... OWNER for kind %q", kind)
}

// alterOwnerStatement builds the statement. target is regclass/regprocedure/
// regtype text (already quoted by PostgreSQL); the owner is quoted here.
func alterOwnerStatement(kind, target, newOwner string) (string, error) {
	kw, err := alterKeyword(kind)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ALTER %s %s OWNER TO %s", kw, target, sqlquote.QuoteIdentifier(newOwner)), nil
}

// ListRemediationCandidates returns drifted objects owned by the connection
// role (superset of what may be reassigned; Candidate.Refusal filters).
func ListRemediationCandidates(ctx context.Context, pool *pgxpool.Pool, q OwnershipQuery, limit int) ([]Candidate, error) {
	if limit <= 0 || limit > MaxRemediationCandidates {
		limit = MaxRemediationCandidates
	}
	tx, err := beginInspect(ctx, pool, ownershipInspectStatementTimeout, ownershipInspectLockTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, ownershipCandidatesSQL, q.args(int32(limit))...)
	if err != nil {
		return nil, fmt.Errorf("candidate query: %w", err)
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.ClassID, &c.ObjID, &c.Kind, &c.Ident, &c.FollowsParent, &c.RLSUnforced, &c.RLSDependent, &c.SecDef); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ErrOwnershipChanged means the object vanished or its owner changed between
// listing and reassigning; the next cycle re-evaluates it.
var ErrOwnershipChanged = errors.New("object no longer owned by the connection role")

// IsLockTimeout reports a PostgreSQL lock_timeout cancellation (55P03).
func IsLockTimeout(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "55P03"
}

// liveLookup re-reads the object inside the reassign transaction, keyed by
// oid, so a rename or an ownership change in between is detected.
const (
	liveClassSQL = `SELECT c.relowner = r.oid, c.relkind::text, (c.relrowsecurity AND NOT c.relforcerowsecurity),
	  (c.relkind = 'S' AND EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=c.oid
	     AND d.refclassid='pg_class'::regclass AND d.deptype IN ('a','i'))),
	  ` + rlsDependentExpr + `, false, c.oid::regclass::text
	  FROM pg_class c, pg_roles r WHERE c.oid = $1 AND r.rolname = session_user`
	liveProcSQL = `SELECT p.proowner = r.oid, '', false, false, false, p.prosecdef, p.oid::regprocedure::text
	  FROM pg_proc p, pg_roles r WHERE p.oid = $1 AND r.rolname = session_user`
	liveTypeSQL = `SELECT t.typowner = r.oid, '', false, false, false, false, t.oid::regtype::text
	  FROM pg_type t, pg_roles r WHERE t.oid = $1 AND r.rolname = session_user`
)

// ReassignOne moves one object to newOwner in its own transaction
// (lock_timeout 500ms, statement_timeout 10s). Inside the transaction it
// re-resolves the object by oid and re-checks that the connection role still
// owns it and that it is still remediable, so a rename or concurrent change
// is never acted on blindly. Returns the refusal as an error wrapping
// ErrOwnershipChanged-free *RefusedError when the live state forbids it.
func ReassignOne(ctx context.Context, pool *pgxpool.Pool, c Candidate, newOwner string) error {
	var liveSQL string
	switch c.Kind {
	case "table", "partitioned_table", "view", "materialized_view", "foreign_table", "sequence":
		liveSQL = liveClassSQL
	case "function", "procedure", "aggregate", "window_function":
		liveSQL = liveProcSQL
	case "enum", "domain", "composite_type", "range", "base_type":
		liveSQL = liveTypeSQL
	default:
		return &RefusedError{Reason: SkipSchema}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range []string{
		"SET LOCAL lock_timeout = '" + ownershipReassignLockTimeout + "'",
		"SET LOCAL statement_timeout = '" + ownershipReassignStmtTimeout + "'",
		"SET LOCAL search_path = pg_catalog",
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	_, super, err := connRole(ctx, tx)
	if err != nil {
		return fmt.Errorf("superuser check: %w", err)
	}

	var owned bool
	var relkind, target string // relkind is '' for proc/type lookups
	live := Candidate{ClassID: c.ClassID, ObjID: c.ObjID, Kind: c.Kind}
	if err := tx.QueryRow(ctx, liveSQL, c.ObjID).Scan(&owned, &relkind, &live.RLSUnforced, &live.FollowsParent, &live.RLSDependent, &live.SecDef, &target); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOwnershipChanged
		}
		return fmt.Errorf("re-read object: %w", err)
	}
	if !owned {
		return ErrOwnershipChanged
	}
	// An oid can be reused by a different kind of relation between listing and
	// reassigning; never act on a mismatch.
	if relkind != "" && relkind != relkindOf(c.Kind) {
		return ErrOwnershipChanged
	}
	if reason := live.Refusal(super); reason != "" {
		return &RefusedError{Reason: reason}
	}
	stmt, err := alterOwnerStatement(c.Kind, target, newOwner)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// relkindOf maps a pg_class object kind to its relkind letter ("" if none).
func relkindOf(kind string) string {
	switch kind {
	case "table":
		return "r"
	case "partitioned_table":
		return "p"
	case "view":
		return "v"
	case "materialized_view":
		return "m"
	case "foreign_table":
		return "f"
	case "sequence":
		return "S"
	}
	return ""
}

// RefusedError is returned by ReassignOne when the live object may not be
// reassigned (RLS, SECURITY DEFINER, owned sequence, superuser, schema).
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "reassign refused: " + e.Reason }

// ReassignResult tallies one remediation pass.
type ReassignResult struct {
	// Reassigned/Failed/Skipped are counted per kind, keyed by kind then outcome.
	Reassigned map[string]int
	Failed     map[string]int
	// Skipped is keyed by skip reason (Skip* constants).
	Skipped map[string]int
	// StoppedOnLock: the pass stopped at the first 55P03.
	StoppedOnLock bool
	// BudgetExhausted: the time budget ran out before every candidate ran.
	BudgetExhausted bool
	// FirstError is the first non-lock failure, for the Warning event.
	FirstError error
}

// Total reassigned objects.
func (r ReassignResult) Total() int {
	n := 0
	for _, v := range r.Reassigned {
		n += v
	}
	return n
}

// ReassignCandidates reassigns candidates one transaction each, in order,
// stopping at the first lock timeout or when the budget elapses. The caller
// is responsible for refusing a superuser connection role up front; each
// ReassignOne re-checks it too.
func ReassignCandidates(ctx context.Context, pool *pgxpool.Pool, cands []Candidate, newOwner string, budget time.Duration) ReassignResult {
	res := ReassignResult{Reassigned: map[string]int{}, Failed: map[string]int{}, Skipped: map[string]int{}}
	if budget <= 0 {
		budget = DefaultReassignBudget
	}
	deadline := time.Now().Add(budget)
	for _, c := range cands {
		if reason := c.Refusal(false); reason != "" {
			res.Skipped[reason]++
			continue
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			res.BudgetExhausted = true
			break
		}
		err := ReassignOne(ctx, pool, c, newOwner)
		var refused *RefusedError
		switch {
		case err == nil:
			res.Reassigned[c.Kind]++
		case errors.Is(err, ErrOwnershipChanged):
			res.Skipped["skipped_changed"]++
		case errors.As(err, &refused):
			res.Skipped[refused.Reason]++
		case IsLockTimeout(err):
			res.Failed[c.Kind]++
			res.StoppedOnLock = true
			if res.FirstError == nil {
				res.FirstError = fmt.Errorf("%s: %w", c.Ident, err)
			}
			return res
		default:
			res.Failed[c.Kind]++
			if res.FirstError == nil {
				res.FirstError = fmt.Errorf("%s: %w", c.Ident, err)
			}
		}
	}
	return res
}
