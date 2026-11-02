// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5/pgxpool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dogukanturhal/keystone-sdk/go/declarative"
	"github.com/dogukanturhal/keystone-sdk/go/drift"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

type replayFixture struct {
	pool   *pgxpool.Pool
	schema string
	sd     *keystonev1alpha1.SchemaDefinition
	bundle *keystonev1alpha1.MigrationBundle
	runner *migration.Runner
	src    *migration.ResolvedSource
	pc     *applyPrecheck
}

func newReplayFixture(t *testing.T, schema string) *replayFixture {
	t.Helper()
	ctx := context.Background()
	pool := dialRaw(t, ctx, getHarness(t), "postgres")
	t.Cleanup(pool.Close)
	mustExec := func(q string) {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
	mustExec(fmt.Sprintf("CREATE SCHEMA %s", schema))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
	})

	sd := mkSD("billing", "default", nil, nil)
	snap, err := drift.NewInspector(pool).Inspect(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := declarative.Diff(snap, &sd.Spec)
	if err != nil || len(plan.Statements) == 0 {
		t.Fatalf("plan: %v %v", plan, err)
	}
	b := declBundle(sd, "billing-"+planContentHash(plan), metav1.ConditionUnknown, "")
	b.Annotations = map[string]string{keystonev1alpha1.AnnotationSDFingerprint: specFingerprint(&sd.Spec)}
	body := ""
	for _, s := range plan.Statements {
		body += s + ";\n"
	}
	runner, err := migration.NewRunner(pool, schema, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.EnsureBookkeeping(ctx); err != nil {
		t.Fatal(err)
	}
	f := &replayFixture{
		pool: pool, schema: schema, sd: sd, bundle: b, runner: runner,
		src: &migration.ResolvedSource{Names: []string{"001.sql"}, Files: map[string]string{"001.sql": body}},
	}
	f.pc = &applyPrecheck{
		r:    &MigrationExecutionReconciler{Client: newCRTTLFakeClient(sd)},
		pool: pool, bundle: b, schema: schema, inspect: true,
	}
	runner.Precheck = f.pc.run
	return f
}

func (f *replayFixture) trackingRows(t *testing.T) int {
	var n int
	if err := f.pool.QueryRow(context.Background(),
		fmt.Sprintf(`SELECT count(*) FROM %s.schema_migrations`, f.schema)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The 42P07 scenario: a hand-written bundle created the table after the
// declarative plan was computed. The replayed CREATE TABLE must not run.
func TestIntegration_DeclarativeExecWhoseTableExistsAborts(t *testing.T) {
	f := newReplayFixture(t, "replay_exists")
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s.t (id integer PRIMARY KEY)", f.schema)); err != nil {
		t.Fatal(err)
	}
	// A marker proves no SQL ran: the stale plan would have CREATEd t and failed.
	_, err := f.runner.Apply(ctx, "v-decl", "h", f.src)
	var stale *stalePlanError
	if !errors.As(err, &stale) || !errors.Is(err, migration.ErrPrecheckFailed) {
		t.Fatalf("want stale precheck abort, got %v", err)
	}
	if n := f.trackingRows(t); n != 0 {
		t.Fatalf("tracking rows = %d, want 0", n)
	}
}

func TestIntegration_DeclarativeFingerprintMismatchAborts(t *testing.T) {
	f := newReplayFixture(t, "replay_fp")
	f.bundle.Annotations[keystonev1alpha1.AnnotationSDFingerprint] = "stale"
	_, err := f.runner.Apply(context.Background(), "v-decl", "h", f.src)
	var stale *stalePlanError
	if !errors.As(err, &stale) {
		t.Fatalf("want stale, got %v", err)
	}
	var n int
	_ = f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name='t'`, f.schema).Scan(&n)
	if n != 0 || f.trackingRows(t) != 0 {
		t.Fatalf("SQL/row leaked (tables=%d)", n)
	}
}

func baselineSetup(t *testing.T, f *replayFixture, hash string) {
	ctx := context.Background()
	if err := drift.EnsureBaselineTable(ctx, f.pool, f.schema); err != nil {
		t.Fatal(err)
	}
	if err := drift.EnsureSnapshotColumn(ctx, f.pool, f.schema); err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		snap, _ := drift.NewInspector(f.pool).Inspect(ctx, f.schema)
		hash, _ = drift.Hash(snap)
	}
	if err := drift.WriteBaseline(ctx, f.pool, f.schema, drift.BaselineKindStructure, hash, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_BaselineMovesAfterCleanApply(t *testing.T) {
	f := newReplayFixture(t, "replay_base_clean")
	ctx := context.Background()
	baselineSetup(t, f, "")
	if _, err := f.runner.Apply(ctx, "v-decl", "h", f.src); err != nil {
		t.Fatal(err)
	}
	f.pc.advanceBaseline(ctx, "v-decl", logr.Discard())
	snap, _ := drift.NewInspector(f.pool).Inspect(ctx, f.schema)
	want, _ := drift.Hash(snap)
	base, err := drift.ReadBaseline(ctx, f.pool, f.schema, drift.BaselineKindStructure)
	if err != nil || base == nil || base.Hash != want || base.Source != "migration:v-decl" {
		t.Fatalf("baseline = %+v err=%v, want hash %s", base, err, want)
	}
}

func TestIntegration_BaselineStaysWhenPreExistingDrift(t *testing.T) {
	f := newReplayFixture(t, "replay_base_drift")
	ctx := context.Background()
	baselineSetup(t, f, "deadbeef-not-the-live-hash")
	if _, err := f.runner.Apply(ctx, "v-decl", "h", f.src); err != nil {
		t.Fatal(err)
	}
	f.pc.advanceBaseline(ctx, "v-decl", logr.Discard())
	base, _ := drift.ReadBaseline(ctx, f.pool, f.schema, drift.BaselineKindStructure)
	if base == nil || base.Hash != "deadbeef-not-the-live-hash" || base.Source != "test" {
		t.Fatalf("baseline moved despite pre-existing drift: %+v", base)
	}
	_ = time.Now
}
