# Ownership drift

**Symptom.** A LogicalDatabase has condition `OwnershipDrift=True`, or the
`KeystoneOwnershipDrift` alert fires. Objects inside the database's managed
schemas (tables, views, sequences, functions, types, schemas) are owned by a
role other than `spec.ownerRole`. Migrations that run as `ownerRole`
(`SET LOCAL ROLE`) then fail with `42501 insufficient_privilege` on those
objects (`ApplyFailedInsufficientPrivilege`).

The check is report-only and never affects `Ready`.

## Before you start

- `kubectl` read access to LogicalDatabases in the namespace.
- For the fix itself: `psql` as a **superuser** (or the owner of the objects)
  connected to the affected database.

## 1. Read the condition

```sh
kubectl -n <ns> get logicaldatabase <name> \
  -o jsonpath='{.status.conditions[?(@.type=="OwnershipDrift")].message}{"\n"}'
kubectl -n <ns> get logicaldatabase <name> -o jsonpath='{.status.ownershipDrift}' | jq
```

| Reason | Meaning |
|---|---|
| `DriftDetected` (True) | objects with an unexpected owner exist; the message groups them as `kind/owner=count` |
| `NoDrift` / `NoManagedSchemas` (False) | clean, or no DatabaseSchema references this LogicalDatabase |
| `CheckFailed` (Unknown) | the catalog query or pool failed; see manager logs |
| `OwnerRoleMissing` (Unknown) | `spec.ownerRole` does not exist as a PostgreSQL role |

## 2. Classify before changing anything

* Objects owned by the connection role (`keystone_admin`, listed under
  "Remediable"): safe for the operator to move. Enable it, see step 3.
* Objects owned by `postgres` (or any other role): the operator will never
  move these. Look at them first. Functions with `prolang`/`probin` pointing
  at an extension library may be broken extension members; the right fix can
  be `ALTER EXTENSION ... ADD FUNCTION`, not an owner change. SECURITY DEFINER
  routines change whose rights they run with when the owner changes; review
  them as a security question.
* Tables with `relrowsecurity AND NOT relforcerowsecurity`: the owner (and
  members of the owner role with INHERIT) bypass non-FORCE RLS. Moving them
  can switch RLS off for the app. Run `ALTER TABLE ... FORCE ROW LEVEL SECURITY`
  deliberately, then move.

## 3. Let the operator move its own objects (opt-in)

Set on the DatabaseProvider (default `Report`) or override on one
LogicalDatabase:

```yaml
spec:
  ownershipRemediation: ReassignFromConnectionRole
```

The operator then runs `ALTER ... OWNER TO <ownerRole>` for drifted objects
owned by its connection role only, one transaction per object
(`lock_timeout` 500ms, 10s total budget, stops at the first lock timeout and
retries after 5 minutes; non-lock failures wait the full check interval). It
skips schemas, owned sequences (they follow their table), RLS-unforced tables,
views and materialized views that read an RLS-enabled table, inheritance and
partition children of an RLS-enabled ancestor at any depth (all three are
counted as blocked; the last two appear as `skipped_rls_dependent` in
`keystone_ownership_remediations_total`), and SECURITY DEFINER routines, and
refuses entirely if the connection role is a superuser.

**Rollout note.** Flip the provider-level `ownershipRemediation` only in a
quiet window. The LogicalDatabase controller runs a single worker, so the
first sweep across every LogicalDatabase (each up to the 10s budget plus
catalog queries) can delay realm provisioning by minutes. Prefer one
LogicalDatabase first, then the provider. Results appear as Events
`OwnershipRemediated` / `OwnershipRemediationFailed` and in
`keystone_ownership_remediations_total`.

## 4. Fix the rest as a superuser

Generate the statements, **read them**, then run them. Run in the affected
database; set `:owner` to the LogicalDatabase `spec.ownerRole` and `:schemas`
to the managed schema names. Never use `REASSIGN OWNED BY postgres`: it would
move every extension and system-adjacent object too.

```sql
-- psql -v owner=hexxlock_falcon_id_owner -v schemas='{public,app}' -d <db>
-- (run `SET search_path = pg_catalog;` first so names come out schema-qualified)
SELECT stmt FROM (
  SELECT format('ALTER %s %s OWNER TO %I;',
           CASE c.relkind WHEN 'r' THEN 'TABLE' WHEN 'p' THEN 'TABLE' WHEN 'v' THEN 'VIEW'
                WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'f' THEN 'FOREIGN TABLE' END,
           c.oid::regclass, :'owner') AS stmt, 1 AS ord
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = ANY (:'schemas'::text[])
    AND c.relkind IN ('r','p','v','m','f')
    AND c.relowner <> (SELECT oid FROM pg_roles WHERE rolname = :'owner')
    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass
                    AND d.objid=c.oid AND d.deptype='e')
  UNION ALL
  SELECT format('ALTER ROUTINE %s OWNER TO %I;', p.oid::regprocedure, :'owner'), 2
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = ANY (:'schemas'::text[])
    AND p.proowner <> (SELECT oid FROM pg_roles WHERE rolname = :'owner')
    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_proc'::regclass
                    AND d.objid=p.oid AND d.deptype='e')
  UNION ALL
  SELECT format('ALTER TYPE %s OWNER TO %I;', t.oid::regtype, :'owner'), 3
  FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
  WHERE n.nspname = ANY (:'schemas'::text[])
    AND t.typtype IN ('e','r','b') AND t.typowner <> (SELECT oid FROM pg_roles WHERE rolname = :'owner')
    AND NOT EXISTS (SELECT 1 FROM pg_type e WHERE e.oid=t.typelem AND e.typarray=t.oid)
    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_type'::regclass
                    AND d.objid=t.oid AND d.deptype='e')
) s ORDER BY ord, stmt;
```

Domains need `ALTER DOMAIN`, standalone composite types `ALTER TYPE`, and
schemas `ALTER SCHEMA ... OWNER TO`; the generator leaves them to the
operator output (`kind/owner` groups in the condition) so you decide each.
Owned sequences follow their table.

## Verification

The next reconcile after the throttle (default 30 min; any spec change
re-checks immediately) should show `OwnershipDrift=False`, reason `NoDrift`,
and `keystone_ownership_drift_objects` for the database drops to no series.
