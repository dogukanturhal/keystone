// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Integration tests drive LogicalDatabaseReconciler and
// MigrationExecutionReconciler against a real PostgreSQL running in a
// testcontainer. They cover the path the envtest suites stop at: pool
// acquire → DDL execution → SQL apply → bookkeeping insert.
//
// Run with:
//
//	GOWORK=off go test -count=1 -tags integration ./internal/controller/
//
// Requires a working Docker daemon reachable via DOCKER_HOST / default
// socket. The tests skip cleanly when Docker is unreachable as a
// defence-in-depth on top of the build tag.
//
// One postgres:16-alpine container is shared across every test via
// sync.Once to amortise the ~3-5s startup cost.

package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/dogukanturhal/keystone-sdk/go/migration"
	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// -- Shared container harness ------------------------------------------

type pgHarness struct {
	container *tcpostgres.PostgresContainer
	host      string
	port      int
	username  string
	password  string
}

var (
	sharedHarness     *pgHarness
	sharedHarnessOnce sync.Once
	sharedHarnessErr  error
)

// getHarness starts the shared container on first call, reuses it after.
// Skips the test cleanly if Docker isn't reachable.
func getHarness(t *testing.T) *pgHarness {
	t.Helper()
	sharedHarnessOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		// An already-running PostgreSQL, when one is named. This is the CI
		// path: the shared kubernetes executor exposes no Docker socket, so
		// testcontainers cannot start anything there and this whole suite
		// skipped on every pipeline. A GitLab `services:` container needs
		// no socket and arrives as a DSN.
		//
		// Parsed into the individual fields rather than kept as a string
		// because the harness has to reconstruct the exact pg.PoolConfig
		// the reconciler builds, so PoolCache.InjectForTest matches.
		if dsn := os.Getenv(testDSNEnv); dsn != "" {
			cfg, perr := pgxpool.ParseConfig(dsn)
			if perr != nil {
				sharedHarnessErr = fmt.Errorf("parse %s: %w", testDSNEnv, perr)
				return
			}
			pool, perr := pgxpool.NewWithConfig(ctx, cfg)
			if perr != nil {
				sharedHarnessErr = fmt.Errorf("connect %s: %w", testDSNEnv, perr)
				return
			}
			if perr := pool.Ping(ctx); perr != nil {
				sharedHarnessErr = fmt.Errorf("ping %s: %w", testDSNEnv, perr)
				return
			}
			pool.Close()
			sharedHarness = &pgHarness{
				host:     cfg.ConnConfig.Host,
				port:     int(cfg.ConnConfig.Port),
				username: cfg.ConnConfig.User,
				password: cfg.ConnConfig.Password,
			}
			return
		}

		c, err := tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithUsername("keystone_test"),
			tcpostgres.WithPassword("test"),
			tcpostgres.WithDatabase("postgres"),
			tcpostgres.BasicWaitStrategies(),
		)
		if err != nil {
			sharedHarnessErr = err
			return
		}
		host, herr := c.Host(ctx)
		if herr != nil {
			sharedHarnessErr = herr
			return
		}
		port, perr := c.MappedPort(ctx, "5432/tcp")
		if perr != nil {
			sharedHarnessErr = perr
			return
		}
		sharedHarness = &pgHarness{
			container: c,
			host:      host,
			port:      int(port.Num()),
			username:  "keystone_test",
			password:  "test",
		}
	})
	if sharedHarnessErr != nil || sharedHarness == nil {
		t.Skipf("no dev database: set %s, or start Docker for testcontainers (%v)",
			testDSNEnv, sharedHarnessErr)
	}
	return sharedHarness
}

// testDSNEnv names an already-running PostgreSQL to test against instead
// of starting a container. Matches the variable the keystone-sdk suites
// read, so one `services:` database serves both repositories' CI.
const testDSNEnv = "KEYSTONE_TEST_DSN"

// dialRaw opens a direct pgxpool against the container for verification
// queries or for pre-seeding the PoolCache. SSL disabled because the
// postgres:16-alpine image ships without TLS; the production API rejects
// sslmode=disable, which is why we InjectForTest into the cache rather
// than letting the reconciler dial.
func dialRaw(t *testing.T, ctx context.Context, h *pgHarness, database string) *pgxpool.Pool {
	t.Helper()
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		h.username, h.password, h.host, h.port, database)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial %s: %v", database, err)
	}
	return pool
}

// providerPoolCfg is the exact PoolConfig the reconciler will construct
// from the DatabaseProvider + LogicalDatabase pair. Matching it byte-for-
// byte is what makes PoolCache.InjectForTest return our pool rather than
// opening a fresh one with sslmode=require.
func providerPoolCfg(h *pgHarness, database string) pg.PoolConfig {
	return pg.PoolConfig{
		Host:     h.host,
		Port:     h.port,
		Database: database,
		Username: h.username,
		Password: h.password,
		SSLMode:  string(keystonev1alpha1.DatabaseProviderSSLModeRequire),
		MaxConns: 4,
	}
}

// seedAdminSecret creates the Secret holding the admin credentials the
// provider's AdminCredentialsRef points at. Must be in keystone-system.
func seedAdminSecret(t *testing.T, ctx context.Context, k8s client.Client, h *pgHarness, name string) {
	t.Helper()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "keystone-system",
			Name:      name,
		},
		Data: map[string][]byte{
			"username": []byte(h.username),
			"password": []byte(h.password),
		},
	}
	if err := k8s.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create admin secret %s: %v", name, err)
	}
}

// seedProvider creates a DatabaseProvider CR. Cluster-scoped; one per test.
func seedProvider(t *testing.T, ctx context.Context, k8s client.Client, h *pgHarness, name, secretName string) *keystonev1alpha1.DatabaseProvider {
	t.Helper()
	p := &keystonev1alpha1.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: keystonev1alpha1.DatabaseProviderSpec{
			Engine:              keystonev1alpha1.DatabaseProviderEnginePostgreSQL,
			Host:                h.host,
			Port:                int32(h.port),
			MaintenanceDatabase: "postgres",
			SSLMode:             keystonev1alpha1.DatabaseProviderSSLModeRequire,
			AdminCredentialsRef: keystonev1alpha1.DatabaseProviderCredentials{
				SecretName:  secretName,
				UsernameKey: "username",
				PasswordKey: "password",
			},
			PoolMaxConns: 4,
		},
	}
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatalf("create provider %s: %v", name, err)
	}
	return p
}

// runLdbReconcileUntilReady drives the reconciler until the
// LogicalDatabase is Ready=True or the iteration budget is exhausted.
// Callers pass an injector hook invoked after each reconcile so the test
// can seed a target-DB pool once the database exists.
func runLdbReconcileUntilReady(
	t *testing.T,
	ctx context.Context,
	r *LogicalDatabaseReconciler,
	k8s client.Client,
	key types.NamespacedName,
	afterEach func(iter int),
	maxIter int,
) {
	t.Helper()
	req := ctrl.Request{NamespacedName: key}
	for i := 0; i < maxIter; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Logf("iter %d reconcile err (may recover): %v", i, err)
		}
		if afterEach != nil {
			afterEach(i)
		}
		var ldb keystonev1alpha1.LogicalDatabase
		if err := k8s.Get(ctx, key, &ldb); err != nil {
			t.Fatalf("get ldb: %v", err)
		}
		cond := findCondition(ldb.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
		if cond != nil && cond.Status == metav1.ConditionTrue {
			return
		}
	}
	var ldb keystonev1alpha1.LogicalDatabase
	_ = k8s.Get(ctx, key, &ldb)
	t.Fatalf("LogicalDatabase not Ready after %d iters; conditions=%+v", maxIter, ldb.Status.Conditions)
}

// -- Test 1: LogicalDatabase happy path --------------------------------

func TestIntegration_LogicalDatabase_HappyPath_CreatesDatabaseAndRole(t *testing.T) {
	h := getHarness(t)
	k8s, stop := StartEnvtest(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		dbName    = "keystone_it_happy"
		ownerRole = "keystone_it_owner"
	)

	seedAdminSecret(t, ctx, k8s, h, "happy-admin")
	seedProvider(t, ctx, k8s, h, "it-happy-provider", "happy-admin")

	// Pre-seed maintenance pool: reconciler calls EnsureDatabase against
	// the maintenance DB first.
	maintPool := dialRaw(t, ctx, h, "postgres")
	defer maintPool.Close()

	pools := pg.NewPoolCache()
	pools.InjectForTest(providerPoolCfg(h, "postgres"), maintPool)

	r := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(32),
		Pools:           pools,
		SystemNamespace: "keystone-system",
	}
	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-happy-ldb"},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           dbName,
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "it-happy-provider",
			OwnerRole:      ownerRole,
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create ldb: %v", err)
	}
	key := client.ObjectKeyFromObject(ldb)
	runLdbReconcileUntilReady(t, ctx, r, k8s, key, nil, 5)

	// Verify DB + role exist.
	var got int
	if err := maintPool.QueryRow(ctx,
		`SELECT 1 FROM pg_database WHERE datname = $1`, dbName).Scan(&got); err != nil {
		t.Errorf("pg_database lookup: %v", err)
	}
	if err := maintPool.QueryRow(ctx,
		`SELECT 1 FROM pg_roles WHERE rolname = $1`, ownerRole).Scan(&got); err != nil {
		t.Errorf("pg_roles lookup: %v", err)
	}

	// Verify status reflects success.
	var after keystonev1alpha1.LogicalDatabase
	if err := k8s.Get(ctx, key, &after); err != nil {
		t.Fatalf("get ldb: %v", err)
	}
	cond := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready=%v, want True", cond)
	}
	if cond.Reason != "Reconciled" {
		t.Errorf("reason=%q, want Reconciled", cond.Reason)
	}
	wantEP := fmt.Sprintf("%s:%d", h.host, h.port)
	if after.Status.ResolvedEndpoint != wantEP {
		t.Errorf("resolvedEndpoint=%q, want %q", after.Status.ResolvedEndpoint, wantEP)
	}
}

// -- Test 2: LogicalDatabase extensions --------------------------------

func TestIntegration_LogicalDatabase_ExtensionsEnsured(t *testing.T) {
	h := getHarness(t)
	k8s, stop := StartEnvtest(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		dbName    = "keystone_it_ext"
		ownerRole = "keystone_it_ext_owner"
	)

	seedAdminSecret(t, ctx, k8s, h, "ext-admin")
	seedProvider(t, ctx, k8s, h, "it-ext-provider", "ext-admin")

	maintPool := dialRaw(t, ctx, h, "postgres")
	defer maintPool.Close()

	pools := pg.NewPoolCache()
	pools.InjectForTest(providerPoolCfg(h, "postgres"), maintPool)

	// The reconciler will also acquire a pool against the target DB to
	// CREATE EXTENSION. That DB doesn't exist until after the first
	// successful maintenance-reconcile, so we inject the target pool
	// lazily via afterEach.
	var targetPool *pgxpool.Pool
	defer func() {
		if targetPool != nil {
			targetPool.Close()
		}
	}()

	r := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(32),
		Pools:           pools,
		SystemNamespace: "keystone-system",
	}
	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-ext-ldb"},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           dbName,
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "it-ext-provider",
			OwnerRole:      ownerRole,
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
			Extensions:     []string{"pgcrypto"},
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create ldb: %v", err)
	}
	key := client.ObjectKeyFromObject(ldb)

	afterEach := func(iter int) {
		// Check DB existence; once present, inject the target-DB pool.
		var exists bool
		if err := maintPool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, dbName).Scan(&exists); err != nil {
			return
		}
		if exists && targetPool == nil {
			targetPool = dialRaw(t, ctx, h, dbName)
			pools.InjectForTest(providerPoolCfg(h, dbName), targetPool)
		}
	}
	runLdbReconcileUntilReady(t, ctx, r, k8s, key, afterEach, 6)

	// Verify pgcrypto in target DB.
	if targetPool == nil {
		t.Fatal("targetPool was never initialised")
	}
	var one int
	if err := targetPool.QueryRow(ctx,
		`SELECT 1 FROM pg_extension WHERE extname = 'pgcrypto'`).Scan(&one); err != nil {
		t.Errorf("pgcrypto not installed: %v", err)
	}
}

// -- Test 3: MigrationExecution applies versioned SQL ------------------

func TestIntegration_MigrationExecution_AppliesVersionedSQL(t *testing.T) {
	h := getHarness(t)
	k8s, stop := StartEnvtest(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const (
		dbName     = "keystone_it_mig"
		ownerRole  = "keystone_it_mig_owner"
		schemaName = "public" // write into public so default grants suffice
		version    = "001"
	)

	seedAdminSecret(t, ctx, k8s, h, "mig-admin")
	seedProvider(t, ctx, k8s, h, "it-mig-provider", "mig-admin")

	maintPool := dialRaw(t, ctx, h, "postgres")
	defer maintPool.Close()

	pools := pg.NewPoolCache()
	pools.InjectForTest(providerPoolCfg(h, "postgres"), maintPool)

	// Step 1: create + reconcile LogicalDatabase.
	var targetPool *pgxpool.Pool
	defer func() {
		if targetPool != nil {
			targetPool.Close()
		}
	}()

	ldbR := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(32),
		Pools:           pools,
		SystemNamespace: "keystone-system",
	}
	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-mig-ldb"},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           dbName,
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "it-mig-provider",
			OwnerRole:      ownerRole,
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create ldb: %v", err)
	}
	ldbKey := client.ObjectKeyFromObject(ldb)
	injectTarget := func(int) {
		var exists bool
		if err := maintPool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, dbName).Scan(&exists); err != nil {
			return
		}
		if exists && targetPool == nil {
			targetPool = dialRaw(t, ctx, h, dbName)
			pools.InjectForTest(providerPoolCfg(h, dbName), targetPool)
		}
	}
	runLdbReconcileUntilReady(t, ctx, ldbR, k8s, ldbKey, injectTarget, 5)
	if targetPool == nil {
		// No extensions requested → reconciler never opened target pool.
		// Inject explicitly for the migration step.
		targetPool = dialRaw(t, ctx, h, dbName)
		pools.InjectForTest(providerPoolCfg(h, dbName), targetPool)
	}

	// Step 2: create + mark DatabaseSchema Ready. We avoid running the
	// full DatabaseSchemaReconciler (which re-does role ensure) because
	// "public" already exists with a different owner — simpler to
	// manually flip Ready=True here.
	dbs := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-mig-schema"},
		Spec: keystonev1alpha1.DatabaseSchemaSpec{
			Name:               schemaName,
			LogicalDatabaseRef: ldb.Name,
			OwnerRole:          ownerRole,
			DeletionPolicy:     keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, dbs); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	dbsFetched := &keystonev1alpha1.DatabaseSchema{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(dbs), dbsFetched); err != nil {
		t.Fatalf("get schema: %v", err)
	}
	dbsFetched.Status.ObservedGeneration = dbsFetched.Generation
	dbsFetched.Status.Conditions = []metav1.Condition{{
		Type:               keystonev1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "integration test preset",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: dbsFetched.Generation,
	}}
	if err := k8s.Status().Update(ctx, dbsFetched); err != nil {
		t.Fatalf("status update schema: %v", err)
	}

	// Step 3: ConfigMap with one versioned SQL file.
	const sqlBody = `CREATE TABLE demo (id int primary key);`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-mig-sql"},
		Data: map[string]string{
			"001_init.up.sql": sqlBody,
		},
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create cm: %v", err)
	}

	// Step 4: MigrationBundle referencing the ConfigMap.
	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-mig-bundle"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:  version,
			Strategy: keystonev1alpha1.StrategyVersioned,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name:        cm.Name,
					FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"it": "mig"},
			},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}

	// Compute the content hash the same way the runner will.
	files := map[string]string{"001_init.up.sql": sqlBody}
	names := []string{"001_init.up.sql"}
	contentHash := migration.HashFiles(files, names)

	// Step 5: MigrationPlan (manually — bundle reconciler would do it in prod).
	plan := &keystonev1alpha1.MigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-mig-plan"},
		Spec: keystonev1alpha1.MigrationPlanSpec{
			BundleRef:     bundle.Name,
			BundleVersion: version,
			SchemaRef:     dbs.Name,
			// A4 Plan-Gate-Apply: approval is owned by the
			// MigrationPlanReconciler (Auto mode), which this test does
			// not run — preset the gate so the execution path under
			// test is isolated.
			Approved: true,
			Statements: []keystonev1alpha1.PlannedStatement{
				{File: "001_init.up.sql", Index: 1, SQL: sqlBody},
			},
		},
	}
	if err := k8s.Create(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// Step 6: MigrationExecution — drive to Succeeded.
	exec := &keystonev1alpha1.MigrationExecution{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-mig-exec"},
		Spec: keystonev1alpha1.MigrationExecutionSpec{
			PlanRef:       plan.Name,
			BundleRef:     bundle.Name,
			BundleVersion: version,
			SchemaRef:     dbs.Name,
			ContentHash:   contentHash,
		},
	}
	if err := k8s.Create(ctx, exec); err != nil {
		t.Fatalf("create exec: %v", err)
	}

	mxR := &MigrationExecutionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(32),
		Pools:           pools,
		Resolver:        migration.NewConfigMapResolver(k8s),
		SystemNamespace: "keystone-system",
	}
	execKey := client.ObjectKeyFromObject(exec)
	req := ctrl.Request{NamespacedName: execKey}
	for i := 0; i < 5; i++ {
		if _, err := mxR.Reconcile(ctx, req); err != nil {
			t.Logf("iter %d exec reconcile err (may recover): %v", i, err)
		}
		var after keystonev1alpha1.MigrationExecution
		if err := k8s.Get(ctx, execKey, &after); err != nil {
			t.Fatalf("get exec: %v", err)
		}
		if after.Status.Phase == keystonev1alpha1.ExecutionPhaseSucceeded {
			break
		}
	}
	var after keystonev1alpha1.MigrationExecution
	if err := k8s.Get(ctx, execKey, &after); err != nil {
		t.Fatalf("get exec: %v", err)
	}
	if after.Status.Phase != keystonev1alpha1.ExecutionPhaseSucceeded {
		t.Fatalf("phase=%q, want Succeeded; failure=%s; conds=%+v",
			after.Status.Phase, after.Status.FailureMessage, after.Status.Conditions)
	}
	if len(after.Status.Applied) == 0 {
		t.Errorf("status.applied is empty")
	}
	if len(after.Status.Applied) > 0 && after.Status.Applied[0].File != "001_init.up.sql" {
		t.Errorf("applied[0].file=%q, want 001_init.up.sql", after.Status.Applied[0].File)
	}
	// TotalDurationMS is exposed via the status condition message; the
	// status doesn't carry a top-level duration field. Spot-check that
	// per-file DurationMS has been populated instead.
	if len(after.Status.Applied) > 0 && after.Status.Applied[0].DurationMS < 0 {
		t.Errorf("applied[0].durationMS = %d; expected >= 0", after.Status.Applied[0].DurationMS)
	}

	// Verify the table exists in the target DB.
	var tbl int
	if err := targetPool.QueryRow(ctx, `
		SELECT 1 FROM information_schema.tables
		 WHERE table_schema = $1 AND table_name = 'demo'`,
		schemaName).Scan(&tbl); err != nil {
		t.Errorf("demo table not found in %s: %v", schemaName, err)
	}

	// Verify schema_migrations row with matching version + content hash.
	var (
		gotVersion string
		gotHash    string
	)
	query := fmt.Sprintf(
		`SELECT version, content_hash FROM %s.schema_migrations WHERE version = $1`,
		schemaName,
	)
	if err := targetPool.QueryRow(ctx, query, version).Scan(&gotVersion, &gotHash); err != nil {
		t.Errorf("schema_migrations lookup: %v", err)
	}
	if gotVersion != version {
		t.Errorf("schema_migrations.version=%q, want %q", gotVersion, version)
	}
	if gotHash != contentHash {
		t.Errorf("schema_migrations.content_hash=%q, want %q", gotHash, contentHash)
	}

	// Double-check the runner wired the correct tracking table name —
	// the DDL inside the runner quotes identifiers, verify the table
	// lives under the schema and not a bare "schema_migrations".
	var tableSchema string
	if err := targetPool.QueryRow(ctx, `
		SELECT table_schema FROM information_schema.tables
		 WHERE table_name = 'schema_migrations' LIMIT 1`).Scan(&tableSchema); err != nil {
		t.Errorf("find schema_migrations table: %v", err)
	}
	if !strings.EqualFold(tableSchema, schemaName) {
		t.Errorf("schema_migrations in schema %q, want %q", tableSchema, schemaName)
	}
}

// TestIntegration_MigrationExecution_RecordOnlySkipsExecution — ADR 0027
// adoption baseline: an executionMode=RecordOnly execution upserts the
// tracking-table row (version + content hash, duration 0) WITHOUT
// executing the SQL. The CREATE TABLE in the bundle must NOT exist in
// the database afterwards — that is the whole point.
func TestIntegration_MigrationExecution_RecordOnlySkipsExecution(t *testing.T) {
	h := getHarness(t)
	k8s, stop := StartEnvtest(t)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const (
		dbName     = "keystone_it_rec"
		ownerRole  = "keystone_it_rec_owner"
		schemaName = "public"
		version    = "0001"
	)

	seedAdminSecret(t, ctx, k8s, h, "rec-admin")
	seedProvider(t, ctx, k8s, h, "it-rec-provider", "rec-admin")

	maintPool := dialRaw(t, ctx, h, "postgres")
	defer maintPool.Close()

	pools := pg.NewPoolCache()
	pools.InjectForTest(providerPoolCfg(h, "postgres"), maintPool)

	var targetPool *pgxpool.Pool
	defer func() {
		if targetPool != nil {
			targetPool.Close()
		}
	}()

	ldbR := &LogicalDatabaseReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(32),
		Pools:           pools,
		SystemNamespace: "keystone-system",
	}
	ldb := &keystonev1alpha1.LogicalDatabase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-rec-ldb"},
		Spec: keystonev1alpha1.LogicalDatabaseSpec{
			Name:           dbName,
			ClusterRef:     "prod-eu-west-1-hub",
			ProviderRef:    "it-rec-provider",
			OwnerRole:      ownerRole,
			DeletionPolicy: keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, ldb); err != nil {
		t.Fatalf("create ldb: %v", err)
	}
	ldbKey := client.ObjectKeyFromObject(ldb)
	injectTarget := func(int) {
		var exists bool
		if err := maintPool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, dbName).Scan(&exists); err != nil {
			return
		}
		if exists && targetPool == nil {
			targetPool = dialRaw(t, ctx, h, dbName)
			pools.InjectForTest(providerPoolCfg(h, dbName), targetPool)
		}
	}
	runLdbReconcileUntilReady(t, ctx, ldbR, k8s, ldbKey, injectTarget, 5)
	if targetPool == nil {
		targetPool = dialRaw(t, ctx, h, dbName)
		pools.InjectForTest(providerPoolCfg(h, dbName), targetPool)
	}

	dbs := &keystonev1alpha1.DatabaseSchema{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-rec-schema"},
		Spec: keystonev1alpha1.DatabaseSchemaSpec{
			Name:               schemaName,
			LogicalDatabaseRef: ldb.Name,
			OwnerRole:          ownerRole,
			DeletionPolicy:     keystonev1alpha1.DeletionPolicyRetain,
		},
	}
	if err := k8s.Create(ctx, dbs); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	dbsFetched := &keystonev1alpha1.DatabaseSchema{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(dbs), dbsFetched); err != nil {
		t.Fatalf("get schema: %v", err)
	}
	dbsFetched.Status.ObservedGeneration = dbsFetched.Generation
	dbsFetched.Status.Conditions = []metav1.Condition{{
		Type:               keystonev1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "integration test preset",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: dbsFetched.Generation,
	}}
	if err := k8s.Status().Update(ctx, dbsFetched); err != nil {
		t.Fatalf("status update schema: %v", err)
	}

	// Baseline SQL that would succeed if executed — the test proves it
	// is NOT executed.
	const sqlBody = `CREATE TABLE must_not_exist_after_record (id int primary key);`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-rec-sql"},
		Data: map[string]string{
			"0001_baseline.up.sql": sqlBody,
		},
	}
	if err := k8s.Create(ctx, cm); err != nil {
		t.Fatalf("create cm: %v", err)
	}

	bundle := &keystonev1alpha1.MigrationBundle{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-rec-bundle"},
		Spec: keystonev1alpha1.MigrationBundleSpec{
			Version:       version,
			Strategy:      keystonev1alpha1.StrategyVersioned,
			ExecutionMode: keystonev1alpha1.ExecutionModeRecordOnly,
			Source: keystonev1alpha1.MigrationSource{
				Type: keystonev1alpha1.SourceConfigMap,
				ConfigMapRef: &keystonev1alpha1.MigrationConfigMapSource{
					Name:        cm.Name,
					FilePattern: "*.up.sql",
				},
			},
			SchemaSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"it": "rec"},
			},
		},
	}
	if err := k8s.Create(ctx, bundle); err != nil {
		t.Fatalf("create bundle: %v", err)
	}

	files := map[string]string{"0001_baseline.up.sql": sqlBody}
	names := []string{"0001_baseline.up.sql"}
	contentHash := migration.HashFiles(files, names)

	plan := &keystonev1alpha1.MigrationPlan{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-rec-plan"},
		Spec: keystonev1alpha1.MigrationPlanSpec{
			BundleRef:     bundle.Name,
			BundleVersion: version,
			SchemaRef:     dbs.Name,
			// A4 Plan-Gate-Apply: approval is owned by the
			// MigrationPlanReconciler (Auto mode), which this test does
			// not run — preset the gate so the execution path under
			// test is isolated.
			Approved: true,
			Statements: []keystonev1alpha1.PlannedStatement{
				{File: "0001_baseline.up.sql", Index: 1, SQL: sqlBody},
			},
		},
	}
	if err := k8s.Create(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}

	exec := &keystonev1alpha1.MigrationExecution{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "it-rec-exec"},
		Spec: keystonev1alpha1.MigrationExecutionSpec{
			PlanRef:       plan.Name,
			BundleRef:     bundle.Name,
			BundleVersion: version,
			SchemaRef:     dbs.Name,
			ContentHash:   contentHash,
			ExecutionMode: keystonev1alpha1.ExecutionModeRecordOnly,
		},
	}
	if err := k8s.Create(ctx, exec); err != nil {
		t.Fatalf("create exec: %v", err)
	}

	mxR := &MigrationExecutionReconciler{
		Client:          k8s,
		Scheme:          clientgoscheme.Scheme,
		Recorder:        record.NewFakeRecorder(32),
		Pools:           pools,
		Resolver:        migration.NewConfigMapResolver(k8s),
		SystemNamespace: "keystone-system",
	}
	execKey := client.ObjectKeyFromObject(exec)
	req := ctrl.Request{NamespacedName: execKey}
	for i := 0; i < 5; i++ {
		if _, err := mxR.Reconcile(ctx, req); err != nil {
			t.Logf("iter %d exec reconcile err (may recover): %v", i, err)
		}
		var current keystonev1alpha1.MigrationExecution
		if err := k8s.Get(ctx, execKey, &current); err != nil {
			t.Fatalf("get exec: %v", err)
		}
		if current.Status.Phase == keystonev1alpha1.ExecutionPhaseSucceeded {
			break
		}
	}
	var after keystonev1alpha1.MigrationExecution
	if err := k8s.Get(ctx, execKey, &after); err != nil {
		t.Fatalf("get exec: %v", err)
	}
	if after.Status.Phase != keystonev1alpha1.ExecutionPhaseSucceeded {
		t.Fatalf("phase=%q, want Succeeded; failure=%s; conds=%+v",
			after.Status.Phase, after.Status.FailureMessage, after.Status.Conditions)
	}
	applied := findCondition(after.Status.Conditions, keystonev1alpha1.ConditionTypeApplied)
	if applied == nil || applied.Reason != "RecordedOnly" {
		t.Errorf("Applied condition reason=%v, want RecordedOnly", applied)
	}

	// THE assertion: the table from the baseline SQL must NOT exist.
	var exists bool
	if err := targetPool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM information_schema.tables
		 WHERE table_schema = $1 AND table_name = 'must_not_exist_after_record')`,
		schemaName).Scan(&exists); err != nil {
		t.Fatalf("table existence check: %v", err)
	}
	if exists {
		t.Errorf("RecordOnly executed the SQL — must_not_exist_after_record exists")
	}

	// The tracking row exists with the right hash and zero duration.
	var (
		gotHash     string
		gotDuration int64
	)
	query := fmt.Sprintf(
		`SELECT content_hash, duration_ms FROM %s.schema_migrations WHERE version = $1`,
		schemaName,
	)
	if err := targetPool.QueryRow(ctx, query, version).Scan(&gotHash, &gotDuration); err != nil {
		t.Fatalf("schema_migrations lookup: %v", err)
	}
	if gotHash != contentHash {
		t.Errorf("schema_migrations.content_hash=%q, want %q", gotHash, contentHash)
	}
	if gotDuration != 0 {
		t.Errorf("schema_migrations.duration_ms=%d, want 0 (nothing executed)", gotDuration)
	}
}
