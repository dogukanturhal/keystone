// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

var ownershipSeq atomic.Int32

// ownershipEnv is one isolated database + role set. Roles are cluster-global,
// so every name carries a per-test suffix.
type ownershipEnv struct {
	h                 *pgHarness
	db                string
	admin, own, third string // ks_admin analogue (NOSUPERUSER CREATEROLE), expected owner, third role
	super             *pgxpool.Pool
	adminPool         *pgxpool.Pool
	query             pg.OwnershipQuery
}

func dialAs(t *testing.T, ctx context.Context, h *pgHarness, user, pw, database string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", user, pw, h.host, h.port, database))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 6
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("dial %s: %v", user, err)
	}
	t.Cleanup(p.Close)
	return p
}

func mustExec(t *testing.T, ctx context.Context, p *pgxpool.Pool, sqls ...string) {
	t.Helper()
	for _, s := range sqls {
		if _, err := p.Exec(ctx, s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func newOwnershipEnv(t *testing.T, ctx context.Context) *ownershipEnv {
	t.Helper()
	h := getHarness(t)
	n := ownershipSeq.Add(1)
	e := &ownershipEnv{
		h: h, db: fmt.Sprintf("own_it_%d", n),
		admin: fmt.Sprintf("ks_admin_%d", n), own: fmt.Sprintf("own_%d", n), third: fmt.Sprintf("third_%d", n),
	}
	maint := dialRaw(t, ctx, h, "postgres")
	mustExec(t, ctx, maint,
		fmt.Sprintf(`CREATE ROLE %s NOLOGIN CREATEDB`, e.own),
		fmt.Sprintf(`CREATE ROLE %s NOLOGIN`, e.third),
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'pw' NOSUPERUSER CREATEROLE`, e.admin),
		fmt.Sprintf(`GRANT %s TO %s`, e.own, e.admin),
		fmt.Sprintf(`GRANT %s TO %s`, e.third, e.admin),
		fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, e.db, e.own),
	)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		m := dialRaw(t, c, h, "postgres")
		_, _ = m.Exec(c, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, e.db))
		for _, r := range []string{e.admin, e.third, e.own} {
			_, _ = m.Exec(c, fmt.Sprintf(`DROP ROLE IF EXISTS %s`, r))
		}
	})
	e.super = dialRaw(t, ctx, h, e.db)
	mustExec(t, ctx, e.super,
		fmt.Sprintf(`ALTER SCHEMA public OWNER TO %s`, e.own),
		fmt.Sprintf(`CREATE SCHEMA app AUTHORIZATION %s`, e.admin), // schema drift: owner != own
		fmt.Sprintf(`GRANT CREATE ON SCHEMA app TO %s`, e.own),
		fmt.Sprintf(`CREATE SCHEMA other AUTHORIZATION %s`, e.own),
	)
	e.adminPool = dialAs(t, ctx, h, e.admin, "pw", e.db)
	e.query = pg.OwnershipQuery{
		Schemas:           []pg.SchemaOwner{{Name: "public", Owner: e.own}, {Name: "app", Owner: e.own}},
		ExpectedOwner:     e.own,
		ExcludedRelations: []string{"schema_migrations", "keystone_schema_migrations", "keystone_baselines"},
		SampleSize:        5,
	}
	return e
}

func (e *ownershipEnv) seedFixtures(t *testing.T, ctx context.Context) {
	t.Helper()
	mustExec(t, ctx, e.super, `CREATE EXTENSION IF NOT EXISTS pgcrypto SCHEMA public`) // members must not be reported
	mustExec(t, ctx, e.adminPool,
		`CREATE TABLE public.t_serial (id serial PRIMARY KEY, ident int GENERATED ALWAYS AS IDENTITY, v text)`,
		`CREATE INDEX t_serial_v ON public.t_serial (v)`,
		`CREATE TABLE public.pt (id int, v text) PARTITION BY RANGE (id)`,
		`CREATE TABLE public.pt_1 PARTITION OF public.pt FOR VALUES FROM (0) TO (100)`,
		`CREATE VIEW public.v1 AS SELECT 1 AS x`,
		`CREATE MATERIALIZED VIEW public.mv1 AS SELECT 1 AS x`,
		`CREATE SEQUENCE public.sq1`,
		`CREATE TYPE public.e1 AS ENUM ('a','b')`,
		`CREATE DOMAIN public.d1 AS int CHECK (VALUE > 0)`,
		`CREATE TYPE public.c1 AS (a int, b text)`,
		`CREATE TYPE public.r1 AS RANGE (subtype = int4)`,
		`CREATE FUNCTION public.f1() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		`CREATE PROCEDURE public.p1() LANGUAGE sql AS 'SELECT 1'`,
		`CREATE AGGREGATE public.agg1(int) (sfunc = int4pl, stype = int4)`,
		`CREATE FUNCTION public.f_sd() RETURNS int LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`,
		`CREATE TABLE app.t2 (id int)`,
		`CREATE TABLE public.rls_t (id int)`,
		`ALTER TABLE public.rls_t ENABLE ROW LEVEL SECURITY`,
		// RLS-dependent: views over an RLS table, partition children of RLS ancestors
		`CREATE VIEW public.v_rls AS SELECT id FROM public.rls_t`,
		`CREATE MATERIALIZED VIEW public.mv_rls AS SELECT id FROM public.rls_t`,
		`CREATE TABLE public.prls (id int) PARTITION BY RANGE (id)`,
		`ALTER TABLE public.prls ENABLE ROW LEVEL SECURITY`,
		`CREATE TABLE public.prls_1 PARTITION OF public.prls FOR VALUES FROM (0) TO (100)`,
		// two levels: only the top ancestor has RLS
		`CREATE TABLE public.rls_top (id int) PARTITION BY RANGE (id)`,
		`ALTER TABLE public.rls_top ENABLE ROW LEVEL SECURITY`,
		`CREATE TABLE public.rls_mid PARTITION OF public.rls_top FOR VALUES FROM (0) TO (100) PARTITION BY RANGE (id)`,
		`CREATE TABLE public.rls_leaf PARTITION OF public.rls_mid FOR VALUES FROM (0) TO (50)`,
		// tracking tables: excluded
		`CREATE TABLE public.schema_migrations (version text)`,
		`CREATE TABLE public.keystone_baselines (id int)`,
		// unmanaged schema: ignored
		`CREATE TABLE other.t_other (id int)`,
	)
	mustExec(t, ctx, e.super,
		`CREATE FUNCTION public.f_pg() RETURNS int LANGUAGE sql AS 'SELECT 1'`, // owned by the harness superuser
		fmt.Sprintf(`CREATE TABLE public.ok_t (id int)`),
		fmt.Sprintf(`ALTER TABLE public.ok_t OWNER TO %s`, e.own),
		fmt.Sprintf(`CREATE TABLE public.third_t (id int)`),
		fmt.Sprintf(`ALTER TABLE public.third_t OWNER TO %s`, e.third),
	)
}

// groupKey -> count, for exact assertions.
func groupMap(rep pg.OwnershipReport) map[string]int {
	m := map[string]int{}
	for _, g := range rep.Groups {
		m[g.Kind+"/"+g.Owner] = g.Count
	}
	return m
}

func assertGroups(t *testing.T, rep pg.OwnershipReport, want map[string]int) {
	t.Helper()
	got := groupMap(rep)
	if len(got) != len(want) {
		t.Errorf("groups differ:\n got  %v\n want %v", fmtMap(got), fmtMap(want))
		return
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("groups differ:\n got  %v\n want %v", fmtMap(got), fmtMap(want))
			return
		}
	}
}

func fmtMap(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(out)
	return out
}

func TestIntegration_Ownership_InspectExactCounts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)
	e.seedFixtures(t, ctx)

	rep, err := pg.InspectOwnership(ctx, e.adminPool, e.query)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OwnerRoleMissing || rep.ConnRoleSuper || rep.ConnRole != e.admin {
		t.Fatalf("preflight: %+v", rep)
	}
	super := rep.Groups // keep for diagnostics
	_ = super
	assertGroups(t, rep, map[string]int{
		"table/" + e.admin:             6, // t_serial, pt_1, rls_t, app.t2, prls_1, rls_leaf (tracking tables excluded)
		"table/" + e.third:             1,
		"partitioned_table/" + e.admin: 4, // pt, prls, rls_top, rls_mid
		"view/" + e.admin:              2, // v1, v_rls
		"materialized_view/" + e.admin: 2, // mv1, mv_rls
		"sequence/" + e.admin:          3, // serial + identity (owned) + sq1
		"enum/" + e.admin:              1,
		"domain/" + e.admin:            1,
		"composite_type/" + e.admin:    1,
		"range/" + e.admin:             1,
		"function/" + e.admin:          2, // f1, f_sd
		"function/" + e.h.username:     1, // f_pg
		"procedure/" + e.admin:         1,
		"aggregate/" + e.admin:         1,
		"schema/" + e.admin:            1, // app
	})
	// blocked: rls_t, prls, rls_top (RLS unforced), v_rls, mv_rls, prls_1, rls_mid,
	// rls_leaf (RLS dependent), 2 owned sequences, f_sd
	blocked := 0
	for _, g := range rep.Groups {
		blocked += g.Blocked
	}
	if blocked != 11 {
		t.Errorf("blocked=%d want 11", blocked)
	}
	// remediable: conn-owned minus blocked minus schema
	// tables 3 + pt 1 + view 1 + mv 1 + sq1 1 + enum + domain + composite + range + f1 + p1 + agg = 14
	if got := rep.RemediableCount(); got != 14 {
		t.Errorf("remediable=%d want 13", got)
	}
	if rep.Total() != 6+1+4+2+2+3+1+1+1+1+2+1+1+1+1 {
		t.Errorf("total=%d", rep.Total())
	}
}

func TestIntegration_Ownership_RemediateAndSecondCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)
	e.seedFixtures(t, ctx)

	cands, err := pg.ListRemediationCandidates(ctx, e.adminPool, e.query, 0)
	if err != nil {
		t.Fatal(err)
	}
	res := pg.ReassignCandidates(ctx, e.adminPool, cands, e.own, 0)
	if res.StoppedOnLock || res.FirstError != nil || len(res.Failed) != 0 {
		t.Fatalf("pass not clean: %+v", res)
	}
	if res.Total() != 14 {
		t.Errorf("reassigned %d want 14 (%v)", res.Total(), res.Reassigned)
	}
	// RLS table, owned sequences, secdef are refused client-side.
	// rls_t, prls, rls_top are unforced; v_rls, mv_rls, prls_1, rls_mid, rls_leaf depend on RLS tables.
	if res.Skipped[pg.SkipRLS] != 3 || res.Skipped[pg.SkipRLSDependent] != 5 ||
		res.Skipped[pg.SkipSecDef] != 1 || res.Skipped[pg.SkipOwnedSeq] != 2 {
		t.Errorf("skips: %v", res.Skipped)
	}

	var owner string
	// owned sequences follow their table
	for _, seq := range []string{"public.t_serial_id_seq", "public.t_serial_ident_seq"} {
		if err := e.super.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1::regclass`, seq).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != e.own {
			t.Errorf("%s owner %s, want it to follow its table to %s", seq, owner, e.own)
		}
	}
	// untouched
	for _, rel := range []string{"public.rls_t", "public.v_rls", "public.mv_rls", "public.prls_1", "public.rls_mid", "public.rls_leaf"} {
		_ = e.super.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1::regclass`, rel).Scan(&owner)
		if owner != e.admin {
			t.Errorf("%s moved to %s", rel, owner)
		}
	}

	rep, err := pg.InspectOwnership(ctx, e.adminPool, e.query)
	if err != nil {
		t.Fatal(err)
	}
	assertGroups(t, rep, map[string]int{
		"table/" + e.admin:             3, // rls_t, prls_1, rls_leaf
		"partitioned_table/" + e.admin: 3, // prls, rls_top, rls_mid
		"view/" + e.admin:              1, // v_rls
		"materialized_view/" + e.admin: 1, // mv_rls
		"table/" + e.third:             1,
		"function/" + e.admin:          1, // f_sd
		"function/" + e.h.username:     1,
		"schema/" + e.admin:            1,
	})
	if rep.RemediableCount() != 0 {
		t.Errorf("second check remediable=%d want 0", rep.RemediableCount())
	}
}

func TestIntegration_Ownership_LockTest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)
	e.seedFixtures(t, ctx)

	// Another session holds ACCESS EXCLUSIVE on one table for the whole test.
	tx, err := e.super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE public.t_serial IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	rep, err := pg.InspectOwnership(ctx, e.adminPool, e.query)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("inspection blocked by lock: %v", err)
	}
	t.Logf("inspect under ACCESS EXCLUSIVE lock: %v", took)
	if took > time.Second {
		t.Errorf("inspection took %v under a table lock, want < 1s", took)
	}
	if rep.Total() == 0 {
		t.Error("expected drift")
	}

	cands, err := pg.ListRemediationCandidates(ctx, e.adminPool, e.query, 0)
	if err != nil {
		t.Fatalf("candidate query blocked: %v", err)
	}
	start = time.Now()
	res := pg.ReassignCandidates(ctx, e.adminPool, cands, e.own, 0)
	t.Logf("remediation pass with locked table: %v", time.Since(start))
	if !res.StoppedOnLock {
		t.Fatalf("expected stop on 55P03, got %+v", res)
	}
	if res.Failed["table"] != 1 {
		t.Errorf("failed=%v, want exactly the locked table", res.Failed)
	}
	var owner string
	_ = e.super.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.t_serial'::regclass`).Scan(&owner)
	if owner != e.admin {
		t.Errorf("locked table moved to %s", owner)
	}
	// Objects ordered before the locked table (partitioned table, app.t2,
	// pt_1) were already moved; the pass stopped at the first lock timeout.
	_ = e.super.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='app.t2'::regclass`).Scan(&owner)
	if owner != e.own {
		t.Errorf("app.t2 owner %s, want %s (moved before the lock)", owner, e.own)
	}
}

func TestIntegration_Ownership_MissingOwnerRoleAndSuperuser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)

	q := e.query
	q.ExpectedOwner = "no_such_role_" + e.db
	rep, err := pg.InspectOwnership(ctx, e.adminPool, q)
	if err != nil || !rep.OwnerRoleMissing || len(rep.Groups) != 0 {
		t.Fatalf("missing owner role: %+v err=%v", rep, err)
	}

	// Superuser connection role: reported, never remediated.
	e.seedFixtures(t, ctx)
	srep, err := pg.InspectOwnership(ctx, e.super, e.query)
	if err != nil {
		t.Fatal(err)
	}
	if !srep.ConnRoleSuper || srep.RemediableCount() != 0 {
		t.Errorf("superuser conn: super=%v remediable=%d", srep.ConnRoleSuper, srep.RemediableCount())
	}
	cands, _ := pg.ListRemediationCandidates(ctx, e.super, e.query, 0)
	if len(cands) == 0 {
		t.Fatal("expected superuser-owned candidates (f_pg)")
	}
	for _, c := range cands {
		if err := pg.ReassignOne(ctx, e.super, c, e.own); err == nil {
			t.Fatalf("ReassignOne succeeded for %s as superuser", c.Ident)
		}
	}
}

func TestIntegration_Ownership_ScaleCheckUnder500ms(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)
	mustExec(t, ctx, e.adminPool, `DO $$
DECLARE i int;
BEGIN
  FOR i IN 1..300 LOOP
    EXECUTE format('CREATE TABLE public.s_%s (id int PRIMARY KEY, a int, b int, c int, d int)', i);
    EXECUTE format('CREATE INDEX s_%s_a ON public.s_%s (a)', i, i);
    EXECUTE format('CREATE INDEX s_%s_b ON public.s_%s (b)', i, i);
    EXECUTE format('CREATE INDEX s_%s_c ON public.s_%s (c)', i, i);
    EXECUTE format('CREATE INDEX s_%s_d ON public.s_%s (d)', i, i);
  END LOOP;
  FOR i IN 1..500 LOOP
    EXECUTE format('CREATE FUNCTION public.fn_%s() RETURNS int LANGUAGE sql AS ''SELECT %s''', i, i);
  END LOOP;
END $$`)
	var idx int
	_ = e.super.QueryRow(ctx, `SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indrelid WHERE c.relname LIKE 's\_%'`).Scan(&idx)
	if idx != 1500 {
		t.Fatalf("fixture indexes=%d want 1500", idx)
	}
	// warm the connection, then time the check
	if _, err := pg.InspectOwnership(ctx, e.adminPool, e.query); err != nil {
		t.Fatal(err)
	}
	var worst time.Duration
	for i := 0; i < 5; i++ {
		start := time.Now()
		rep, err := pg.InspectOwnership(ctx, e.adminPool, e.query)
		d := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if d > worst {
			worst = d
		}
		if i == 0 {
			assertGroups(t, rep, map[string]int{"table/" + e.admin: 300, "function/" + e.admin: 500, "schema/" + e.admin: 1})
		}
	}
	t.Logf("scale: 300 tables / 1500 indexes / 500 functions, worst of 5 checks = %v", worst)
	if worst > 500*time.Millisecond {
		t.Errorf("check took %v, want < 500ms", worst)
	}
}

// TestIntegration_Ownership_ControllerEndToEnd drives the real reconcile
// through checkOwnership: condition True without gating Ready, then the
// opt-in remediation flips it.
func TestIntegration_Ownership_ControllerEndToEnd(t *testing.T) {
	k8s, stop := StartEnvtest(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)
	e.seedFixtures(t, ctx)

	const ns = "keystone-system"
	secName, provName, ldbName := "own-e2e-admin", "own-e2e-provider", "own-e2e-ldb"
	h := *e.h
	h.username, h.password = e.admin, "pw" // connect as the non-superuser connection role
	seedAdminSecret(t, ctx, k8s, &h, secName)
	prov := seedProvider(t, ctx, k8s, &h, provName, secName)

	pools := pg.NewPoolCache()
	pools.InjectForTest(providerPoolCfg(&h, "postgres"), dialAs(t, ctx, &h, e.admin, "pw", "postgres"))
	pools.InjectForTest(providerPoolCfg(&h, e.db), e.adminPool)

	for _, s := range []struct{ name, owner string }{{"public", e.own}, {"app", e.own}} {
		ds := &keystonev1alpha1.DatabaseSchema{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "own-e2e-" + s.name},
			Spec: keystonev1alpha1.DatabaseSchemaSpec{
				Name: s.name, LogicalDatabaseRef: ldbName, OwnerRole: s.owner,
			},
		}
		if err := k8s.Create(ctx, ds); err != nil {
			t.Fatalf("create schema %s: %v", s.name, err)
		}
	}
	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: ldbName},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name: e.db, ClusterRef: "c", ProviderRef: provName, OwnerRole: e.own,
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatal(err)
	}
	rec := record.NewFakeRecorder(64)
	r := &LogicalDatabaseReconciler{
		Client: k8s, Scheme: clientgoscheme.Scheme, Recorder: rec, Pools: pools,
		SystemNamespace: ns, OwnershipCheckInterval: time.Hour,
	}
	key := client.ObjectKeyFromObject(ldb)
	runLdbReconcileUntilReady(t, ctx, r, k8s, key, nil, 6)

	var got keystonev1alpha1.LogicalDatabase
	if err := k8s.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	c := findCondition(got.Status.Conditions, keystonev1alpha1.ConditionTypeOwnershipDrift)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != keystonev1alpha1.ReasonOwnershipDriftDetected {
		t.Fatalf("OwnershipDrift=%+v, want True/DriftDetected", c)
	}
	t.Logf("condition message (%d bytes): %s", len(c.Message), c.Message)
	if got.Status.OwnershipDrift == nil || got.Status.OwnershipDrift.DriftedObjects != 28 {
		t.Fatalf("summary %+v", got.Status.OwnershipDrift)
	}
	if rdy := findCondition(got.Status.Conditions, keystonev1alpha1.ConditionTypeReady); rdy == nil || rdy.Status != metav1.ConditionTrue {
		t.Fatalf("Ready gated by drift: %+v", rdy)
	}
	// Report policy moved nothing.
	var owner string
	_ = e.super.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.t_serial'::regclass`).Scan(&owner)
	if owner != e.admin {
		t.Fatalf("Report policy changed an owner: %s", owner)
	}

	// Opt in on the provider; the policy change bypasses the throttle.
	prov.Spec.OwnershipRemediation = keystonev1alpha1.OwnershipRemediationReassignFromConnectionRole
	if err := k8s.Update(ctx, prov); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	c = findCondition(got.Status.Conditions, keystonev1alpha1.ConditionTypeOwnershipDrift)
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("after remediation: %+v", c)
	}
	if got.Status.OwnershipDrift.Remediable != 0 || got.Status.OwnershipDrift.DriftedObjects != 12 {
		t.Errorf("after remediation summary %+v", got.Status.OwnershipDrift)
	}
	_ = e.super.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.t_serial'::regclass`).Scan(&owner)
	if owner != e.own {
		t.Errorf("t_serial owner %s after remediation, want %s", owner, e.own)
	}
	sawRemediated := false
	for len(rec.Events) > 0 {
		if ev := <-rec.Events; strings.Contains(ev, "OwnershipRemediated") {
			sawRemediated = true
		}
	}
	if !sawRemediated {
		t.Error("no OwnershipRemediated event")
	}

}

// TestIntegration_Ownership_CheckOwnershipOutcomes covers the Unknown paths
// and the "no managed schemas" path of checkOwnership, which must never fail
// the reconcile.
func TestIntegration_Ownership_CheckOwnershipOutcomes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = keystonev1alpha1.AddToScheme(scheme)
	mk := func(owner string, withSchema bool) (*LogicalDatabaseReconciler, *keystonev1alpha1.LogicalDatabase) {
		ldb := &keystonev1alpha1.LogicalDatabase{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "x", UID: "uid-" + types.UID(owner)},
			Spec:       keystonev1alpha1.LogicalDatabaseSpec{Name: e.db, OwnerRole: owner},
		}
		b := fake.NewClientBuilder().WithScheme(scheme)
		if withSchema {
			b = b.WithObjects(&keystonev1alpha1.DatabaseSchema{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s"},
				Spec:       keystonev1alpha1.DatabaseSchemaSpec{Name: "public", LogicalDatabaseRef: "x", OwnerRole: e.own},
			})
		}
		return &LogicalDatabaseReconciler{Client: b.Build(), Recorder: record.NewFakeRecorder(8), OwnershipCheckInterval: time.Hour}, ldb
	}
	prov := &keystonev1alpha1.DatabaseProvider{}

	r, ldb := mk(e.own, false)
	if o := r.checkOwnership(ctx, ldb, prov, e.adminPool, nil); o.Status != metav1.ConditionFalse ||
		o.Reason != keystonev1alpha1.ReasonOwnershipNoManagedSchemas {
		t.Errorf("no schemas: %+v", o)
	}
	r, ldb = mk("no_such_role_"+e.db, true)
	if o := r.checkOwnership(ctx, ldb, prov, e.adminPool, nil); o.Status != metav1.ConditionUnknown ||
		o.Reason != keystonev1alpha1.ReasonOwnershipOwnerRoleMissing {
		t.Errorf("missing owner: %+v", o)
	}
	r, ldb = mk(e.own, true)
	if o := r.checkOwnership(ctx, ldb, prov, nil, fmt.Errorf("dial failed")); o.Status != metav1.ConditionUnknown ||
		o.Reason != keystonev1alpha1.ReasonOwnershipCheckFailed {
		t.Errorf("no pool: %+v", o)
	}
	// Throttle: a second call within the interval is a no-op outcome.
	if o := r.checkOwnership(ctx, ldb, prov, e.adminPool, nil); !o.Ran {
		t.Fatalf("first real check must run: %+v", o)
	}
	if o := r.checkOwnership(ctx, ldb, prov, e.adminPool, nil); o.Ran {
		t.Error("second check within interval must be throttled")
	}
}

func counts(c interface {
	Collect(chan<- prometheus.Metric)
}) int {
	ch := make(chan prometheus.Metric, 1024)
	c.Collect(ch)
	close(ch)
	return len(ch)
}

// TestIntegration_Ownership_ExhaustedPoolDoesNotHang: a target pool whose only
// connection is held must yield Unknown(CheckFailed) within the bound, not a
// hung reconcile, and the failed check must back off.
func TestIntegration_Ownership_ExhaustedPoolDoesNotHang(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)

	cfg, err := pgxpool.ParseConfig(fmt.Sprintf("postgres://%s:pw@%s:%d/%s?sslmode=disable", e.admin, e.h.host, e.h.port, e.db))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	tight, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tight.Close)
	held, err := tight.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	old := ownershipInspectTimeout
	ownershipInspectTimeout = 300 * time.Millisecond
	t.Cleanup(func() { ownershipInspectTimeout = old })

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = keystonev1alpha1.AddToScheme(scheme)
	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "x", UID: "uid-exhausted"},
		Spec:       keystonev1alpha1.LogicalDatabaseSpec{Name: e.db, OwnerRole: e.own},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s"},
		Spec:       keystonev1alpha1.DatabaseSchemaSpec{Name: "public", LogicalDatabaseRef: "x", OwnerRole: e.own},
	}).Build()
	r := &LogicalDatabaseReconciler{Client: c, Recorder: record.NewFakeRecorder(8), OwnershipCheckInterval: time.Hour}
	prov := &keystonev1alpha1.DatabaseProvider{}

	start := time.Now()
	o := r.checkOwnership(ctx, ldb, prov, tight, nil)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("check took %v with the pool exhausted", d)
	}
	if !o.Ran || o.Status != metav1.ConditionUnknown || o.Reason != keystonev1alpha1.ReasonOwnershipCheckFailed {
		t.Fatalf("outcome %+v", o)
	}
	if o2 := r.checkOwnership(ctx, ldb, prov, tight, nil); o2.Ran {
		t.Error("failed check must back off, not retry on the next reconcile")
	}
}

// TestIntegration_Ownership_NoManagedSchemasDropsGauges: a database that stops
// being checked must not keep a stale last-check gauge.
func TestIntegration_Ownership_NoManagedSchemasDropsGauges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = keystonev1alpha1.AddToScheme(scheme)
	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-gauge", Name: "x", UID: "uid-gauge"},
		Spec:       keystonev1alpha1.LogicalDatabaseSpec{Name: e.db, OwnerRole: e.own},
	}
	r := &LogicalDatabaseReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Recorder: record.NewFakeRecorder(8),
		OwnershipCheckInterval: time.Hour,
	}
	base := counts(ownershipDriftLastCheck)
	publishOwnershipMetrics(ldb, pg.OwnershipReport{}, time.Now())
	if counts(ownershipDriftLastCheck) != base+1 {
		t.Fatal("last-check gauge not published")
	}
	if o := r.checkOwnership(ctx, ldb, &keystonev1alpha1.DatabaseProvider{}, e.adminPool, nil); o.Reason != keystonev1alpha1.ReasonOwnershipNoManagedSchemas {
		t.Fatalf("outcome %+v", o)
	}
	if got := counts(ownershipDriftLastCheck); got != base {
		t.Errorf("last-check gauge series left: %d", got-base)
	}
}

// TestIntegration_Ownership_ReassignOneOidKindChanged: the candidate's kind no
// longer matches the relkind behind its oid (oid reuse); nothing is changed.
func TestIntegration_Ownership_ReassignOneOidKindChanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	e := newOwnershipEnv(t, ctx)
	mustExec(t, ctx, e.adminPool, `CREATE TABLE public.kc (id int)`)
	var oid uint32
	if err := e.adminPool.QueryRow(ctx, `SELECT 'public.kc'::regclass::oid`).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	err := pg.ReassignOne(ctx, e.adminPool, pg.Candidate{ObjID: oid, Kind: "view", Ident: "public.kc"}, e.own)
	if !errors.Is(err, pg.ErrOwnershipChanged) {
		t.Fatalf("err=%v want ErrOwnershipChanged", err)
	}
	var owner string
	if err := e.super.QueryRow(ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = $1`, oid).Scan(&owner); err != nil || owner != e.admin {
		t.Fatalf("owner=%q err=%v, must be untouched", owner, err)
	}
}
