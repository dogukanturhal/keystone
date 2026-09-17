// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Keystone Manager — operator entrypoint.
//
// Phase 1: scheme registration, controller-runtime manager construction,
// leader election, secure metrics (controller-runtime built-in
// authn/authz; NO kube-rbac-proxy sidecar — that image is EOL 2025),
// healthz/readyz endpoints. Controllers themselves land in Phase 2.

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	zapr "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
	keystonecontroller "github.com/dogukanturhal/keystone/internal/controller"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
	"github.com/dogukanturhal/keystone/internal/tracing"
	keystonewebhook "github.com/dogukanturhal/keystone/internal/webhook"
)

var (
	version = "v0.1.1"
	commit  = "unknown"
	date    = "unknown"

	scheme = runtime.NewScheme()
)

func init() {
	// Register Kubernetes core types (v1.Secret, v1.ConfigMap, etc.) so
	// reconcilers can read admin credentials from Secrets via the
	// controller-runtime client. Without this, LogicalDatabase reconcile
	// fails with `no kind is registered for the type v1.Secret in scheme`
	// when reading the DatabaseProvider.adminCredentialsRef target.
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(keystonev1alpha1.AddToScheme(scheme))
}

type options struct {
	role                 string
	metricsAddr          string
	probeAddr            string
	leaderElect          bool
	leaderElectID        string
	leaderElectNamespace string
	leaderLeaseDuration  time.Duration
	leaderRenewDeadline  time.Duration
	leaderRetryPeriod    time.Duration
	cacheNamespaces      string
	systemNamespace      string
	driftCheckInterval   time.Duration
	otelEndpoint         string
	otelInsecure         bool
	otelSampleRatio      float64
	secureMetrics        bool
	enableHTTP2          bool
	startManager         bool
	showVersion          bool
	enableWebhooks       bool
	enablePprof          bool
	// Audit NATS JetStream exporter (T2 #25 Phase 4).
	// auditNATSURL is the NATS server URL. When empty the NATS exporter is
	// disabled and the existing stdout / noop path is used unchanged.
	auditNATSURL            string
	auditNATSSubjectPrefix  string
	auditNATSPublishTimeout time.Duration
	// auditWriteCR controls whether Append writes AuditEntry CRs to etcd.
	// Default true. Set to false (Phase 5a) after the NATS path has been
	// proven live for 7 days. Must not be false when auditNATSURL is empty.
	auditWriteCR bool

	// CR-TTL controller toggles. Off by default for first release; flip
	// on after one week soak per docs/runbooks/cr-ttl-rollout.md.
	enableCRTTL        bool
	crTTLCheckInterval time.Duration
	crTTLRetainSuccess time.Duration
	crTTLRetainFailure time.Duration
}

func parseFlags() *options {
	o := &options{}
	flag.StringVar(&o.role, "role", "hub",
		"operator role: hub | agent")
	flag.StringVar(&o.metricsAddr, "metrics-bind-address", ":8443",
		"address the metrics endpoint binds to (https when secure-metrics)")
	flag.StringVar(&o.probeAddr, "health-probe-bind-address", ":8081",
		"address the healthz/readyz endpoints bind to")
	flag.BoolVar(&o.leaderElect, "leader-elect", false,
		"enable leader election; required for multi-replica deployments")
	flag.StringVar(&o.leaderElectID, "leader-election-id", "keystone-manager.keystone.hexxlock.io",
		"resource lock identifier for leader election")
	flag.StringVar(&o.leaderElectNamespace, "leader-election-namespace", "",
		"namespace for the leader election lease (defaults to pod namespace)")
	// Leader election timings. Zero (the default) leaves controller-runtime's
	// own defaults in place — 15s/10s/2s — so behaviour is unchanged unless a
	// deployment opts in.
	//
	// Worth raising on clusters with a single-member etcd: `etcdctl defrag`
	// blocks the whole API server there (no quorum peer can serve reads
	// during the rewrite), and a stall longer than renew-deadline makes the
	// manager exit with "leader election lost". Observed on the HexxLock hub
	// 2026-08-01: a 13s defrag killed this operator, 209 times cumulatively.
	// OpenShift's library-go uses 137s/107s/26s as its production default for
	// exactly this class of disruption.
	flag.DurationVar(&o.leaderLeaseDuration, "leader-elect-lease-duration", 0,
		"duration non-leaders wait before force-acquiring leadership; "+
			"0 uses the controller-runtime default (15s)")
	flag.DurationVar(&o.leaderRenewDeadline, "leader-elect-renew-deadline", 0,
		"duration the acting leader retries refreshing leadership before giving up; "+
			"must be less than lease-duration; 0 uses the controller-runtime default (10s)")
	flag.DurationVar(&o.leaderRetryPeriod, "leader-elect-retry-period", 0,
		"interval between leadership actions; must be less than renew-deadline; "+
			"0 uses the controller-runtime default (2s)")
	flag.StringVar(&o.cacheNamespaces, "watch-namespaces", "",
		"comma-separated namespaces to watch; empty means cluster-wide")
	flag.StringVar(&o.systemNamespace, "system-namespace", "keystone-system",
		"namespace where DatabaseProvider admin Secrets live; "+
			"cross-namespace Secret refs are rejected for safety")
	flag.DurationVar(&o.driftCheckInterval, "drift-check-interval", time.Hour,
		"interval between DriftController inspection passes; "+
			"shorten for staging or chaos tests")
	flag.StringVar(&o.otelEndpoint, "otel-endpoint", "",
		"OTLP HTTP collector URL (e.g. http://otel-collector:4318); "+
			"empty disables tracing")
	flag.BoolVar(&o.otelInsecure, "otel-insecure", false,
		"skip TLS verification for the OTLP collector")
	flag.Float64Var(&o.otelSampleRatio, "otel-sample-ratio", 0.1,
		"trace sampler ratio (0.0–1.0)")
	flag.BoolVar(&o.secureMetrics, "secure-metrics", true,
		"serve metrics over HTTPS with controller-runtime authn/authz; "+
			"disable only for local development")
	flag.BoolVar(&o.enableHTTP2, "enable-http2", false,
		"enable HTTP/2 on the metrics server (CVE-2023-44487/45288 — leave off)")
	flag.BoolVar(&o.startManager, "start-manager", false,
		"actually start the controller-runtime manager; without this the "+
			"binary prints version info and exits (safe for local smoke tests)")
	flag.BoolVar(&o.showVersion, "version", false, "print version and exit")
	flag.BoolVar(&o.enableWebhooks, "enable-webhooks", true,
		"register validating admission webhooks for MigrationBundle, "+
			"SchemaDefinition, AuditEntry, ProductDefinition and "+
			"ProductInstance; disable only for local integration tests "+
			"without a webhook server (envtest starts without TLS by default)")
	flag.BoolVar(&o.enablePprof, "enable-pprof", false,
		"register Go runtime profiling handlers (/debug/pprof/*) on the "+
			"metrics server. Inherits the same TokenReview + "+
			"SubjectAccessReview authn/authz as /metrics, so a caller's RBAC "+
			"must include nonResourceURLs: [\"/debug/pprof/*\"] (see the "+
			"keystone-pprof-reader ClusterRole the chart renders when "+
			"metrics.pprof.enabled=true). Default off — off-by-default keeps "+
			"the runtime introspection surface inert in production unless an "+
			"operator deliberately turns it on for a leak hunt.")
	// T2 #25 Phase 4 — NATS JetStream audit exporter.
	// When --audit-nats-url is empty (the default) no NATS connection is
	// attempted and behaviour is identical to before this change.
	flag.StringVar(&o.auditNATSURL, "audit-nats-url",
		envOrDefault("AUDIT_NATS_URL", ""),
		"NATS server URL for JetStream audit publishing "+
			"(e.g. nats://nats.messaging-system.svc.cluster.local:4222); "+
			"empty disables the NATS exporter. Overridden by AUDIT_NATS_URL env var.")
	flag.StringVar(&o.auditNATSSubjectPrefix, "audit-nats-subject-prefix",
		envOrDefault("AUDIT_NATS_SUBJECT_PREFIX", "audit.keystone"),
		"NATS subject prefix; published as <prefix>.<verb>. "+
			"Must match the KEYSTONE_AUDIT stream's subject filter.")
	flag.DurationVar(&o.auditNATSPublishTimeout, "audit-nats-publish-timeout",
		5*time.Second,
		"per-publish context timeout for NATS JetStream; "+
			"keep generous (5s default) so transient broker latency "+
			"doesn't surface as false errors in metrics.")
	// T2 #25 Phase 5a — WriteCR toggle.
	// Default true (backward compat). Set false only after Phase 4 NATS path
	// has been confirmed live for 7 days and MinIO WORM is verified. Requires
	// AUDIT_NATS_URL to be set; refuses to start otherwise (no audit sink).
	// CR-TTL controller — garbage collects terminal MigrationBundle and
	// MigrationExecution CRs whose age exceeds the per-SD or operator-
	// wide retention window. Off by default for first release; the
	// 2026-05-08 etcd-bloat post-mortem prescribed soak-then-flip after
	// one week of dual-replica observability.
	flag.BoolVar(&o.enableCRTTL, "enable-cr-ttl",
		envBoolOrDefault("KEYSTONE_ENABLE_CR_TTL", false),
		"enable the CR-TTL controller that GCs terminal MigrationBundle / "+
			"MigrationExecution CRs past their retention window. Default off — "+
			"flip on per-cluster after one-week soak. Overridden by "+
			"KEYSTONE_ENABLE_CR_TTL env var.")
	flag.DurationVar(&o.crTTLCheckInterval, "cr-ttl-check-interval",
		15*time.Minute,
		"interval between CR-TTL controller passes; shorter values keep GC "+
			"closer to real-time at the cost of API quota.")
	flag.DurationVar(&o.crTTLRetainSuccess, "cr-ttl-retain-success",
		24*time.Hour,
		"operator-wide default for retainSuccessFor when the owning "+
			"SchemaDefinition does not pin spec.cleanup.retainSuccessFor. "+
			"Per-SD overrides take precedence.")
	flag.DurationVar(&o.crTTLRetainFailure, "cr-ttl-retain-failure",
		7*24*time.Hour,
		"operator-wide default for retainFailureFor when the owning "+
			"SchemaDefinition does not pin spec.cleanup.retainFailureFor. "+
			"Per-SD overrides take precedence.")

	flag.BoolVar(&o.auditWriteCR, "audit-write-cr",
		envBoolOrDefault("AUDIT_WRITE_CR", true),
		"write AuditEntry CRs to etcd on each Append; "+
			"default true. Set false (Phase 5a) only when AUDIT_NATS_URL is also "+
			"set and the NATS path has been proven live for 7 days. "+
			"Overridden by AUDIT_WRITE_CR env var.")
	flag.Parse()
	return o
}

// roleHub is the only operator role that exists. The hub-and-spoke split
// described in the architecture docs is not implemented: there is no
// agent code path, and a manager started with --role=agent would run the
// full set of reconcilers and dispatch migrations itself.
//
// For a component that executes schema migrations against production
// databases, an operator believing it deployed a passive agent while a
// second active hub runs is a genuinely dangerous outcome, so the flag
// is validated rather than logged and ignored.
const roleHub = "hub"

// validate checks flag combinations that the flag package cannot express.
// It returns an error rather than exiting so main() controls reporting.
func (o *options) validate() error {
	if o.role != roleHub {
		return fmt.Errorf(
			"--role=%q is not supported: only %q is implemented. Cross-cluster "+
				"agent dispatch does not exist — a manager started with any other "+
				"role would still run every reconciler and execute migrations "+
				"itself, contrary to what the flag implies",
			o.role, roleHub)
	}
	return nil
}

func newLogger() *zap.Logger {
	cfg := zap.NewProductionConfig()
	cfg.EncoderConfig.TimeKey = "ts"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.EncoderConfig.EncodeDuration = zapcore.StringDurationEncoder
	logger, err := cfg.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to build logger: %v\n", err)
		os.Exit(1)
	}
	return logger
}

// envOrDefault returns the value of the environment variable named key,
// or fallback when the variable is unset or empty.
func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envBoolOrDefault returns the bool value of the environment variable named
// key, or fallback when the variable is unset, empty, or not a recognised
// bool literal. Recognised false literals: "0", "false", "no", "off".
// Everything else (including non-empty non-bool values) is treated as true.
func envBoolOrDefault(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// stdoutAuditEnabled reports whether the audit logger should
// stream each committed entry to os.Stdout as newline-delimited
// JSON (Loki / Alloy / Promtail scraping). Default ON; set
// KEYSTONE_AUDIT_STDOUT=false to silence — useful for envtest
// runs where the extra stdout noise interferes with test output.
func stdoutAuditEnabled() bool {
	v := strings.TrimSpace(os.Getenv("KEYSTONE_AUDIT_STDOUT"))
	if v == "" {
		return true
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

func main() {
	o := parseFlags()

	if o.showVersion {
		fmt.Printf("keystone-manager %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	// Validated before the logger exists so a misconfigured role fails
	// loudly on stderr at startup rather than being buried in JSON logs.
	if err := o.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		os.Exit(2)
	}

	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	// Hand controller-runtime its own logger that wraps zap so all log output
	// flows through a single sink with consistent formatting.
	ctrllog.SetLogger(zapr.New(zapr.UseDevMode(false)))

	logger.Info("keystone-manager starting",
		zap.String("version", version),
		zap.String("commit", commit),
		zap.String("role", o.role),
		zap.Bool("leaderElect", o.leaderElect),
		zap.Bool("secureMetrics", o.secureMetrics),
	)

	if !o.startManager {
		logger.Info("manager not started (--start-manager=false); exiting cleanly",
			zap.String("hint", "pass --start-manager to wire controller-runtime "+
				"against the cluster (requires a kubeconfig)"))
		return
	}

	if err := run(o, logger); err != nil {
		logger.Fatal("manager exited with error", zap.Error(err))
	}
}

func run(o *options, logger *zap.Logger) error {
	// Set up signal-aware context. We use signal.NotifyContext directly
	// instead of ctrl.SetupSignalHandler() to avoid the "close of closed
	// channel" panic in controller-runtime v0.23.x when the Go runtime
	// delivers SIGTERM during init.
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tracingCtx := signalCtx
	_, shutdownTracing, err := tracing.Init(tracingCtx, tracing.ProviderConfig{
		Endpoint:       o.otelEndpoint,
		Insecure:       o.otelInsecure,
		ServiceVersion: version,
		SampleRatio:    o.otelSampleRatio,
	})
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shutdownTracing(ctx)
	}()
	logger.Info("tracing initialised",
		zap.String("endpoint", o.otelEndpoint),
		zap.Float64("sampleRatio", o.otelSampleRatio),
	)
	// TLS opt-in for HTTP/2 — disabled by default per CVE-2023-44487 and
	// CVE-2023-45288 (HTTP/2 rapid reset). Enable explicitly only after
	// validating the controller-runtime version's mitigations.
	disableHTTP2 := func(c *tls.Config) {
		if !o.enableHTTP2 {
			c.NextProtos = []string{"http/1.1"}
		}
	}

	// Fail-closed against the secure-metrics + pprof-enabled mismatch.
	// The metrics-server FilterProvider wraps ExtraHandlers only when
	// SecureServing is on (controller-runtime v0.23 metrics/server.go
	// L223-244 — FilterProvider is set on the same branch as TLS). If
	// an operator deliberately disables secure-metrics (e.g. for a
	// local debug session) AND leaves the chart's pprof flag on, the
	// runtime profiling endpoints would serve plaintext HTTP with no
	// TokenReview / SubjectAccessReview at all — every CPU sample,
	// goroutine dump, and binary path leaks to anyone who can reach
	// the pod's metrics port. Refuse to start instead of silently
	// downgrading the auth posture. Operators wanting local pprof
	// turn off secure-metrics AND off pprof, then poke the binary
	// over a separate localhost listener (or use `kubectl debug`).
	if o.enablePprof && !o.secureMetrics {
		return fmt.Errorf("--enable-pprof requires --secure-metrics=true: refusing to expose " +
			"/debug/pprof/* without TokenReview + SubjectAccessReview authn/authz; " +
			"either turn pprof off or leave secure-metrics on (default)")
	}

	// T2 #25 Phase 5a — fail-fast: AUDIT_WRITE_CR=false requires AUDIT_NATS_URL.
	// Without a durable NATS sink, every audit event would be silently dropped —
	// no CR write, no NATS publish, no SIEM record. This violates the 7-year WORM
	// mandate. Refuse to start rather
	// than create a silent compliance hole. Checked here, before ctrl.NewManager,
	// so the test in main_test.go is hermetic (no kubeconfig required).
	if !o.auditWriteCR && o.auditNATSURL == "" {
		return fmt.Errorf(
			"audit: AUDIT_WRITE_CR=false requires AUDIT_NATS_URL to be set — " +
				"disabling CR writes without a durable NATS sink leaves no audit " +
				"record at all; set AUDIT_NATS_URL or revert AUDIT_WRITE_CR to true " +
				"(T2 #25 Phase 5a prerequisite: Phase 4 NATS path proven live for 7 days)")
	}

	metricsOpts := metricsserver.Options{
		BindAddress: o.metricsAddr,
	}
	if o.secureMetrics {
		// controller-runtime v0.18+ — replaces the deprecated
		// kube-rbac-proxy sidecar pattern. Authentication via
		// TokenReview, authorization via SubjectAccessReview against
		// the metrics endpoint.
		metricsOpts.SecureServing = true
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
		metricsOpts.TLSOpts = []func(*tls.Config){disableHTTP2}
	}
	if o.enablePprof {
		// Go runtime profiling endpoints. Same bind address as /metrics
		// so the existing TLS + TokenReview + SubjectAccessReview chain
		// covers them — the metrics-server FilterProvider wraps every
		// ExtraHandler. /debug/pprof/* is a separate nonResourceURL in
		// SAR, so the caller's RBAC must grant it explicitly (chart
		// renders keystone-pprof-reader when metrics.pprof.enabled=true;
		// operators bind it to a debugging SA temporarily).
		//
		// Why this exists: without these handlers a memory or goroutine
		// leak in the operator can only be diagnosed indirectly via
		// Prometheus go_memstats_* metrics, which are aggregate counts
		// only. heap_objects=14M doesn't tell you *which* type retains
		// the objects; pprof inuse_objects/inuse_space does. The
		// 2026-05-07 OOM cycle (28× in 11h) had to ship a GOMEMLIMIT
		// bandage without root-cause data because pprof wasn't exposed.
		//
		// Off by default — enabling adds no per-request overhead until
		// /debug/pprof/profile or /debug/pprof/trace is hit (those are
		// the only handlers that engage active CPU/trace sampling), but
		// keeps the runtime introspection surface inert until an
		// operator opts in for an investigation.
		mux := pprofHandlers()
		metricsOpts.ExtraHandlers = mux
	}

	// Per-type cache options. Two entries today, both reducing the
	// cache footprint without changing reconciler semantics:
	//
	//   AuditEntry — Transform strips Spec.Before/After at cache-entry
	//   time. The largest type by count (104K+ on a single live cluster
	//   vs. <100 of every other namespaced CR), and its two snapshot
	//   fields are read by NO reconciler. Frees hundreds of MiB of
	//   resident memory. Full justification in
	//   internal/controller/cache_transform.go::StripAuditEntryPayload.
	//
	//   ConfigMap — Label-existence selector on
	//   keystone.hexxlock.io/schemadefinition narrows the cache from
	//   "every CM in the cluster" (hundreds, MB-sized due to Argo's
	//   last-applied-configuration annotations) to "only Keystone-
	//   emitted bundle CMs" (the SD reconciler stamps the label on
	//   every emit). Cache size becomes O(active SDs) — currently 3-5.
	//   User-authored ConfigMap-sourced bundles fall outside the cache
	//   scope; both reconciler and webhook construct configMapResolver
	//   against mgr.GetAPIReader() (uncached) so those still resolve.
	cacheOpts := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&keystonev1alpha1.AuditEntry{}: {
				Transform: keystonecontroller.StripAuditEntryPayload,
			},
			&corev1.ConfigMap{}: {
				Label: cacheManagedConfigMapsSelector(),
			},
		},
	}
	if o.cacheNamespaces != "" {
		cacheOpts.DefaultNamespaces = parseNamespaces(o.cacheNamespaces)
	}

	mgrOpts := ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsOpts,
		HealthProbeBindAddress:  o.probeAddr,
		LeaderElection:          o.leaderElect,
		LeaderElectionID:        o.leaderElectID,
		LeaderElectionNamespace: o.leaderElectNamespace,
		Cache:                   cacheOpts,
	}

	// Leader election timings are only overridden when explicitly set, so a
	// zero value keeps controller-runtime's defaults. Validate the ordering
	// here rather than letting it surface later as an opaque leader-election
	// failure at manager start.
	if o.leaderLeaseDuration > 0 || o.leaderRenewDeadline > 0 || o.leaderRetryPeriod > 0 {
		if o.leaderLeaseDuration <= 0 || o.leaderRenewDeadline <= 0 || o.leaderRetryPeriod <= 0 {
			return fmt.Errorf(
				"leader election timings must be set together: "+
					"lease-duration=%s renew-deadline=%s retry-period=%s",
				o.leaderLeaseDuration, o.leaderRenewDeadline, o.leaderRetryPeriod)
		}
		if o.leaderRenewDeadline >= o.leaderLeaseDuration {
			return fmt.Errorf(
				"leader-elect-renew-deadline (%s) must be less than "+
					"leader-elect-lease-duration (%s)",
				o.leaderRenewDeadline, o.leaderLeaseDuration)
		}
		if o.leaderRetryPeriod >= o.leaderRenewDeadline {
			return fmt.Errorf(
				"leader-elect-retry-period (%s) must be less than "+
					"leader-elect-renew-deadline (%s)",
				o.leaderRetryPeriod, o.leaderRenewDeadline)
		}
		mgrOpts.LeaseDuration = &o.leaderLeaseDuration
		mgrOpts.RenewDeadline = &o.leaderRenewDeadline
		mgrOpts.RetryPeriod = &o.leaderRetryPeriod
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("register healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("register readyz: %w", err)
	}

	// Phase 2: SchemaController wires LogicalDatabase + DatabaseSchema
	// reconcilers. DatabaseProvider reconciliation lands in Phase 3 — for
	// now, the controllers read the provider and resolve credentials
	// passively.
	pools := pg.NewPoolCache()

	auditLogger := audit.NewLogger(mgr.GetClient())

	// T2 #25 Phase 4: select the exporter based on config priority:
	//   nats > stdout > noop
	//
	// NATS is the durable path (JetStream PUB-ACK, Phase 3 archiver drains
	// to MinIO WORM). Stdout is the Loki/Promtail scraping path (C1). Both
	// can be active simultaneously via MultiExporter when both are enabled.
	//
	// The CR is always written first and remains authoritative; a dropped
	// export is best-effort and recoverable via keystone-backfill.
	var exporters []audit.Exporter

	if o.auditNATSURL != "" {
		natsExp, err := audit.New(audit.NATSExporterOptions{
			URL:           o.auditNATSURL,
			SubjectPrefix: o.auditNATSSubjectPrefix,
			Timeout:       o.auditNATSPublishTimeout,
			Log:           logger,
		})
		if err != nil {
			// Non-fatal: log and continue with the fallback path. The operator
			// CRs are still written; a broken NATS connection is recoverable via
			// keystone-backfill once the broker is reachable.
			logger.Warn("nats audit exporter connect failed; falling back to stdout/noop",
				zap.String("url", o.auditNATSURL),
				zap.Error(err),
			)
		} else {
			exporters = append(exporters, natsExp)
			// Drain+close on graceful shutdown so in-flight publishes flush.
			defer func() { _ = natsExp.Close() }()
			logger.Info("nats audit exporter enabled",
				zap.String("url", o.auditNATSURL),
				zap.String("subjectPrefix", o.auditNATSSubjectPrefix),
			)
		}
	}

	// C1 — optional stdout JSON export for Loki / Alloy / Promtail
	// scraping. Default ON so operators get SIEM coverage without a
	// config change; set KEYSTONE_AUDIT_STDOUT=false in envtest or
	// local dev where the extra stdout noise is unhelpful.
	if stdoutAuditEnabled() {
		exporters = append(exporters, audit.NewStdoutJSONExporter())
	}

	auditLogger.Exporter = audit.NewMultiExporter(exporters...)
	auditLogger.ExportErrorFn = func(err error, spec keystonev1alpha1.AuditEntrySpec) {
		ctrl.Log.WithName("audit-exporter").
			WithValues("sequence", spec.Sequence, "verb", spec.Verb).
			Error(err, "audit export failed (CR remains authoritative)")
	}
	if len(exporters) == 0 {
		logger.Info("audit exporter: noop (no AUDIT_NATS_URL and KEYSTONE_AUDIT_STDOUT=false)")
	} else {
		logger.Info("audit exporters wired", zap.Int("count", len(exporters)))
	}

	// T2 #25 Phase 5a — wire the WriteCR toggle and emit boot-time mode log.
	auditLogger.WriteCR = o.auditWriteCR

	// Boot-time audit mode log — describes the active pipeline so operators
	// can confirm the expected mode from container logs at startup.
	switch {
	case o.auditWriteCR && o.auditNATSURL == "":
		logger.Info("audit: CR-only (legacy) — AuditEntry CRs written to etcd; no NATS exporter")
	case o.auditWriteCR && o.auditNATSURL != "":
		logger.Info("audit: dual-write (Phase 4) — CR authoritative + NATS shadow",
			zap.String("natsURL", o.auditNATSURL))
	default: // !auditWriteCR && auditNATSURL != "" (guard above ensures this is the only remaining case)
		logger.Info("audit: NATS-only (Phase 5a) — chain-head in MinIO; etcd CR writes disabled",
			zap.String("natsURL", o.auditNATSURL))
	}

	managerIdentity := audit.ActorFromServiceAccount(o.systemNamespace, "keystone-manager")

	if err := (&keystonecontroller.LogicalDatabaseReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Pools:           pools,
		SystemNamespace: o.systemNamespace,
		AuditLogger:     auditLogger,
		ManagerIdentity: managerIdentity,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup LogicalDatabaseReconciler: %w", err)
	}
	if err := (&keystonecontroller.DatabaseSchemaReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Pools:           pools,
		SystemNamespace: o.systemNamespace,
		AuditLogger:     auditLogger,
		ManagerIdentity: managerIdentity,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup DatabaseSchemaReconciler: %w", err)
	}

	if err := (&keystonecontroller.MigrationBundleReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		AuditLogger:     auditLogger,
		ManagerIdentity: managerIdentity,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup MigrationBundleReconciler: %w", err)
	}
	if err := (&keystonecontroller.MigrationPlanReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup MigrationPlanReconciler: %w", err)
	}
	if err := (&keystonecontroller.DatabaseProviderReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Pools:           pools,
		SystemNamespace: o.systemNamespace,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup DatabaseProviderReconciler: %w", err)
	}
	if err := (&keystonecontroller.MigrationExecutionReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Pools:           pools,
		AuditLogger:     auditLogger,
		ManagerIdentity: managerIdentity,
		SystemNamespace: o.systemNamespace,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup MigrationExecutionReconciler: %w", err)
	}

	if err := (&keystonecontroller.ProductInstanceReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		AuditLogger:     auditLogger,
		ManagerIdentity: managerIdentity,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup ProductInstanceReconciler: %w", err)
	}

	driftController := &keystonecontroller.DriftController{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Pools:           pools,
		CheckInterval:   o.driftCheckInterval,
		SystemNamespace: o.systemNamespace,
		AuditLogger:     auditLogger,
		ManagerIdentity: managerIdentity,
	}
	if err := driftController.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup DriftController: %w", err)
	}

	// Shares the DriftController rather than owning a second one: it
	// delegates to reconcileSchema, so the annotated path and the swept
	// path run identical logic against the same pool cache.
	if err := (&keystonecontroller.DriftAcceptanceReconciler{
		Client: mgr.GetClient(),
		Drift:  driftController,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup DriftAcceptanceReconciler: %w", err)
	}

	if err := (&keystonecontroller.SchemaDefinitionReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Pools:           pools,
		SystemNamespace: o.systemNamespace,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup SchemaDefinitionReconciler: %w", err)
	}

	if err := (&keystonecontroller.SchemaSnapshotReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Pools:           pools,
		SystemNamespace: o.systemNamespace,
		AuditLogger:     auditLogger,
		ManagerIdentity: managerIdentity,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup SchemaSnapshotReconciler: %w", err)
	}

	// Defence-in-depth partner for audit.Logger — heals
	// AuditLog.status drift on a 30s tick. Prevents the
	// 2026-04-21 etcd 409-retry storm pattern from re-emerging.
	if err := (&keystonecontroller.AuditLogReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup AuditLogReconciler: %w", err)
	}

	// Phase 12 — AuditEntry retention. Stays dormant until an operator
	// configures spec.archive on the AuditLog CR (and provisions the
	// MinIO bucket with object-lock + WORM retention). The controller
	// reads its own spec on every tick, so flipping the switch is a
	// `kubectl patch auditlog keystone --type=merge -p '...'` away
	// from active archival; no manager restart needed.
	if err := (&keystonecontroller.AuditEntryRetentionController{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Archiver:        nil, // resolved on demand from AuditLog spec
		SystemNamespace: o.systemNamespace,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup AuditEntryRetentionController: %w", err)
	}

	// CR-TTL controller — garbage-collects terminal MigrationBundle +
	// MigrationExecution CRs whose age exceeds the SD-pinned (or
	// operator-wide-default) retention window. Off by default; flip on
	// only after the one-week soak prescribed by the 2026-05-08
	// etcd-bloat post-mortem (see docs/runbooks/cr-ttl-rollout.md when
	// it lands). The controller still starts on the runnable list when
	// disabled — every tick records a "disabled" pass-duration sample
	// so operators can see the loop is alive without an explicit
	// readiness probe.
	if err := (&keystonecontroller.CRTTLController{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Enabled:              o.enableCRTTL,
		CheckInterval:        o.crTTLCheckInterval,
		DefaultRetainSuccess: o.crTTLRetainSuccess,
		DefaultRetainFailure: o.crTTLRetainFailure,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup CRTTLController: %w", err)
	}

	if o.enableWebhooks {
		if err := (&keystonewebhook.MigrationBundleValidator{}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup MigrationBundleValidator webhook: %w", err)
		}
		if err := (&keystonewebhook.SchemaDefinitionValidator{}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup SchemaDefinitionValidator webhook: %w", err)
		}
		if err := (&keystonewebhook.AuditEntryValidator{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup AuditEntryValidator webhook: %w", err)
		}
		if err := (&keystonewebhook.ProductDefinitionValidator{}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup ProductDefinitionValidator webhook: %w", err)
		}
		if err := (&keystonewebhook.ProductInstanceValidator{}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setup ProductInstanceValidator webhook: %w", err)
		}
	}

	logger.Info("controllers registered",
		zap.String("metricsAddr", o.metricsAddr),
		zap.String("probeAddr", o.probeAddr),
		zap.String("systemNamespace", o.systemNamespace),
		zap.Duration("driftCheckInterval", o.driftCheckInterval),
		zap.Strings("controllers", []string{
			"logicaldatabase", "databaseschema",
			"migrationbundle", "migrationexecution",
			"productinstance", "drift (periodic)",
		}),
	)

	if err := mgr.Start(tracingCtx); err != nil {
		return fmt.Errorf("manager exited: %w", err)
	}
	return nil
}

// pprofHandlers returns the standard net/http/pprof handler set keyed
// by the path each is registered under in the metrics server's mux.
// pprof.Index dispatches every named profile (heap, goroutine, allocs,
// block, mutex, threadcreate, …) by stripping the /debug/pprof/ prefix,
// so registering the trailing-slash subtree root catches anything new
// the runtime ships in future Go versions without code changes here.
// cmdline / profile / symbol / trace are registered explicitly because
// they are direct net/http.HandlerFuncs (not dispatched through Index)
// and the controller-runtime metrics server's mux does longest-prefix
// match, so an exact-path registration takes precedence.
func pprofHandlers() map[string]http.Handler {
	return map[string]http.Handler{
		"/debug/pprof/":        http.HandlerFunc(pprof.Index),
		"/debug/pprof/cmdline": http.HandlerFunc(pprof.Cmdline),
		"/debug/pprof/profile": http.HandlerFunc(pprof.Profile),
		"/debug/pprof/symbol":  http.HandlerFunc(pprof.Symbol),
		"/debug/pprof/trace":   http.HandlerFunc(pprof.Trace),
	}
}

// cacheManagedConfigMapsSelector returns the label-existence
// selector the controller-runtime cache uses to scope its ConfigMap
// watch. Selects every ConfigMap that carries the
// `keystone.hexxlock.io/schemadefinition` label key (any non-empty
// value), which the SchemaDefinitionReconciler stamps on every
// auto-emitted SQL-bundle CM (see internal/controller/
// schemadefinition_controller.go::emitBundle).
//
// Tied to the api package's LabelSchemaDefinition constant rather
// than the literal string so that a future rename catches at
// compile time.
//
// Errors from labels.NewRequirement on a fresh, hard-coded constant
// + selection.Exists predicate are not reachable at runtime — the
// selector helper accepts only invalid keys (illegal characters or
// >63-byte segment), and LabelSchemaDefinition is a known-valid
// constant. We still log+panic on the impossible path because a
// silent fall-through (no selector) would degrade to the
// pre-Layer-2 cluster-wide ConfigMap cache.
func cacheManagedConfigMapsSelector() labels.Selector {
	req, err := labels.NewRequirement(
		keystonev1alpha1.LabelSchemaDefinition, selection.Exists, nil,
	)
	if err != nil {
		// Compile-time-stable invariant violated — bail loudly.
		panic(fmt.Sprintf("cache selector: build %q-exists requirement: %v",
			keystonev1alpha1.LabelSchemaDefinition, err))
	}
	return labels.NewSelector().Add(*req)
}

// parseNamespaces converts a comma-separated namespace list into the
// controller-runtime cache scope map. Empty entries are ignored.
func parseNamespaces(in string) map[string]cache.Config {
	out := map[string]cache.Config{}
	start := 0
	for i := 0; i <= len(in); i++ {
		if i == len(in) || in[i] == ',' {
			ns := trim(in[start:i])
			if ns != "" {
				out[ns] = cache.Config{}
			}
			start = i + 1
		}
	}
	return out
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
