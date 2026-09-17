// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
)

// AuditEntryRetentionController prunes AuditEntry CRs that have aged
// past their retention window. The cluster-wide chain is append-only,
// so without this controller running, etcd grows unbounded — the
// 2026-04-21 incident demonstrated the failure mode at 114K entries
// from a 50-min retry storm.
//
// SAFETY MODEL
// ============
//   - Entries are NEVER deleted unless an Archiver is configured AND
//     it has successfully written the batch off-cluster.
//   - The Archiver's manifest hash is recorded in
//     AuditLog.status.lastArchivedHash so operators can rehydrate
//     and re-verify the chain genesis-to-watermark at any time.
//   - Delete rate is token-bucket-limited (default 100/sec) to
//     bound write amplification on etcd — exactly the watermark
//     observed during the 2026-04-21 recovery.
//   - When Archive is unset on AuditLogSpec, the controller logs
//     once at startup and otherwise stays dormant. No surprise
//     deletes when an operator forgets to wire MinIO.
//
// SHAPE
// =====
// Mirrors DriftController: a manager.Runnable driven by a time.Ticker
// rather than a controller-runtime Reconciler, because the work is
// an off-cluster I/O burst that doesn't fit the per-CR reconcile
// model. Leader-election guarded so only one replica is archiving
// at a time.
type AuditEntryRetentionController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Archiver is the cached backend. Built lazily each tick from
	// AuditLog.Spec.Archive — operators can flip backends or rotate
	// MinIO credentials with kubectl patch and the controller picks
	// it up on the next CheckInterval. Tests may pre-populate this
	// to inject a stub.
	Archiver audit.Archiver

	// archiverConfigHash is the fingerprint of the spec the cached
	// Archiver was built from; on mismatch the controller rebuilds.
	archiverConfigHash string

	// CheckInterval overrides AuditLogSpec.Archive.CheckInterval.
	// Zero = read from spec, fall back to 1h.
	CheckInterval time.Duration

	// SystemNamespace is the namespace where the credentials Secret
	// lives.
	SystemNamespace string

	// ArchiverFactory builds an archiver from a resolved S3 config.
	// Test hook: stub this to return a fake archiver without touching
	// the real minio client. Defaults to audit.NewS3Archiver.
	ArchiverFactory func(ctx context.Context, cfg audit.S3ArchiverConfig) (audit.Archiver, error)

	// nowFn lets tests inject a deterministic clock.
	nowFn func() time.Time

	// loggedDormant tracks whether the "archiver dormant" log line
	// has already fired this process; suppresses repeated noise.
	loggedDormant bool
}

// Start implements manager.Runnable. Runs until the manager's context
// is canceled.
func (c *AuditEntryRetentionController) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithValues("component", "audit-retention")
	if c.ArchiverFactory == nil {
		c.ArchiverFactory = func(ctx context.Context, cfg audit.S3ArchiverConfig) (audit.Archiver, error) {
			return audit.NewS3Archiver(ctx, cfg)
		}
	}
	interval := c.interval(nil)
	logger.Info("starting audit retention loop", "interval", interval)

	t := time.NewTicker(interval)
	defer t.Stop()

	// Run once immediately so the first pass doesn't wait a full
	// interval after the manager comes up. (Match DriftController.)
	c.runOnce(ctx, logger)

	for {
		select {
		case <-ctx.Done():
			logger.Info("audit retention loop stopping")
			return nil
		case <-t.C:
			c.runOnce(ctx, logger)
		}
	}
}

// NeedLeaderElection makes the periodic loop run only on the elected
// leader, matching the rest of the manager.
func (c *AuditEntryRetentionController) NeedLeaderElection() bool { return true }

func (c *AuditEntryRetentionController) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn().UTC()
	}
	return time.Now().UTC()
}

func (c *AuditEntryRetentionController) interval(alog *keystonev1alpha1.AuditLog) time.Duration {
	if c.CheckInterval > 0 {
		return c.CheckInterval
	}
	if alog != nil && alog.Spec.Archive != nil && alog.Spec.Archive.CheckInterval.Duration > 0 {
		return alog.Spec.Archive.CheckInterval.Duration
	}
	return time.Hour
}

// runOnce is the per-tick body. Logs and updates metrics on every
// pass, even no-op (so operators can observe the controller is
// alive). Returns nothing — errors are logged and counted but never
// fail the loop.
func (c *AuditEntryRetentionController) runOnce(ctx context.Context, logger logrLike) {
	start := time.Now()

	alog := &keystonev1alpha1.AuditLog{}
	if err := c.Get(ctx, types.NamespacedName{Name: keystonev1alpha1.AuditLogName}, alog); err != nil {
		if apierrors.IsNotFound(err) {
			// No AuditLog yet — nothing to do.
			return
		}
		logger.Error(err, "load AuditLog")
		auditArchiveErrorsTotal.WithLabelValues("load_auditlog").Inc()
		return
	}

	// 1. Resolve the archiver from spec. This both decides if we're
	// dormant AND rebuilds when the operator changes credentials or
	// the bucket name without a manager restart.
	if err := c.resolveArchiver(ctx, alog, logger); err != nil {
		logger.Error(err, "resolve archiver")
		auditArchiveErrorsTotal.WithLabelValues("resolve_archiver").Inc()
		c.recordPassDuration(start, "error")
		return
	}
	if c.Archiver == nil || c.Archiver.Kind() == "noop" {
		if !c.loggedDormant {
			logger.Info("audit retention dormant: no Archive backend configured on AuditLog spec",
				"hint", "set spec.archive.s3 + provision keystone-audit-archive bucket to enable")
			c.loggedDormant = true
		}
		c.recordPassDuration(start, "dormant")
		return
	}
	// Re-arm the dormant log line if we ever go inactive again.
	c.loggedDormant = false

	// The admission webhook refuses to delete an entry that is not yet
	// expired, so it and this loop must compute the identical cutoff —
	// a disagreement of one second rejects this controller's own
	// deletes and entries pile up in etcd. Both call RetentionCutoff.
	retention := alog.Spec.RetentionDays
	cutoff := keystonev1alpha1.RetentionCutoff(retention, c.now())

	// 2. List candidates. Cluster-wide; AuditEntries are namespaced
	// (in the system namespace) but we list-all so a future change
	// of namespace doesn't break the retention loop.
	entries := &keystonev1alpha1.AuditEntryList{}
	if err := c.List(ctx, entries); err != nil {
		logger.Error(err, "list AuditEntries")
		auditArchiveErrorsTotal.WithLabelValues("list").Inc()
		c.recordPassDuration(start, "error")
		return
	}

	expired := make([]keystonev1alpha1.AuditEntry, 0)
	for i := range entries.Items {
		e := &entries.Items[i]
		if e.Spec.Timestamp.Time.Before(cutoff) {
			expired = append(expired, *e)
		}
	}

	c.updateLagGauge(alog, expired, cutoff)

	if len(expired) == 0 {
		// Liveness log — emit on every empty pass so operators can grep
		// "audit retention pass complete" and confirm the loop is ticking
		// without forcing an action. Same key shape as the archived-path
		// log at the bottom of this function so log queries match both.
		// Without this, the loop is silent for the entire window where
		// no entries are aged past retention (often weeks at a time on
		// a 90-day default), which made Phase 5a verification ambiguous
		// on 2026-05-08 — couldn't tell "loop is fine, no candidates"
		// from "loop is dead" without a metric scrape.
		logger.Info("audit retention pass complete",
			"candidates", len(entries.Items),
			"expired", 0,
			"archived", 0,
			"deleted", 0)
		c.recordPassDuration(start, "no_expired")
		return
	}

	// 3. Sort ascending by sequence; archive in batches.
	sort.SliceStable(expired, func(i, j int) bool {
		return expired[i].Spec.Sequence < expired[j].Spec.Sequence
	})

	batchSize := defaultBatchSize
	if alog.Spec.Archive.BatchSize > 0 {
		batchSize = int(alog.Spec.Archive.BatchSize)
	}
	rateLimit := defaultDeleteRateLimit
	if alog.Spec.Archive.DeleteRateLimit > 0 {
		rateLimit = int(alog.Spec.Archive.DeleteRateLimit)
	}

	rl := flowcontrol.NewTokenBucketRateLimiter(float32(rateLimit), rateLimit)

	batched := 0
	deleted := 0
	for offset := 0; offset < len(expired); offset += batchSize {
		end := offset + batchSize
		if end > len(expired) {
			end = len(expired)
		}
		batch := expired[offset:end]

		manifest, err := c.Archiver.Archive(ctx, batch)
		if err != nil {
			if audit.IsDormant(err) {
				// Defensive — already filtered above, but if a future
				// archiver returns dormant from Archive (e.g. backend
				// auth failure escalated to dormant) we silently skip.
				return
			}
			logger.Error(err, "archive batch failed",
				"first_seq", batch[0].Spec.Sequence,
				"last_seq", batch[len(batch)-1].Spec.Sequence,
				"count", len(batch))
			auditArchiveErrorsTotal.WithLabelValues("upload").Inc()
			c.recordPassDuration(start, "error")
			return
		}

		auditArchiveEntriesTotal.WithLabelValues("success").Add(float64(manifest.EntryCount))
		auditArchiveBytesTotal.Add(float64(manifest.SizeBytes))
		batched += manifest.EntryCount

		// 4. Delete the archived entries from etcd, rate-limited.
		batchDeleted, derr := c.deleteBatch(ctx, batch, rl, logger)
		deleted += batchDeleted
		if derr != nil {
			logger.Error(derr, "delete batch failed (archive succeeded; entries remain in etcd until next pass)",
				"archive_object", manifest.ObjectKey,
				"deleted_in_batch", batchDeleted,
				"batch_size", len(batch))
			auditArchiveErrorsTotal.WithLabelValues("delete").Inc()
			// Don't return — update status with what we DID archive.
			// Next pass will re-archive any entries that didn't get
			// deleted; the S3 PutObject is idempotent on the same
			// sequence range.
			break
		}

		// 5. Update AuditLog.status watermark after each batch.
		if perr := c.patchStatus(ctx, alog, manifest); perr != nil {
			logger.Error(perr, "patch AuditLog status")
			auditArchiveErrorsTotal.WithLabelValues("patch_status").Inc()
		}

		logger.Info("archived + deleted AuditEntry batch",
			"archive_object", manifest.ObjectKey,
			"first_seq", manifest.FirstSequence,
			"last_seq", manifest.LastSequence,
			"count", manifest.EntryCount,
			"size_bytes", manifest.SizeBytes,
			"sha256", manifest.SHA256)

		if c.Recorder != nil {
			c.Recorder.Eventf(alog, corev1.EventTypeNormal, "AuditArchive",
				"archived seq %d-%d (%d entries, %d bytes) → %s",
				manifest.FirstSequence, manifest.LastSequence,
				manifest.EntryCount, manifest.SizeBytes, manifest.ObjectKey)
		}
	}

	c.recordPassDuration(start, "archived")
	logger.Info("audit retention pass complete",
		"expired", len(expired),
		"archived", batched,
		"deleted", deleted)
}

// deleteBatch deletes the entries from etcd, applying the token-
// bucket rate limiter. Returns the count actually deleted plus any
// error that aborted the batch.
func (c *AuditEntryRetentionController) deleteBatch(
	ctx context.Context,
	batch []keystonev1alpha1.AuditEntry,
	rl flowcontrol.RateLimiter,
	logger logrLike,
) (int, error) {
	deleted := 0
	for i := range batch {
		entry := &batch[i]
		// Wait for a token. Block on the rate limiter; abort on
		// context cancellation so a manager shutdown doesn't hang.
		if err := rl.Wait(ctx); err != nil {
			return deleted, fmt.Errorf("rate limiter: %w", err)
		}

		err := c.Delete(ctx, entry)
		if apierrors.IsNotFound(err) {
			// Already gone (race with another reconcile or operator
			// kubectl delete). Treat as deleted-by-someone-else.
			deleted++
			continue
		}
		if err != nil {
			return deleted, fmt.Errorf("delete %s: %w", entry.Name, err)
		}
		deleted++
	}
	return deleted, nil
}

// patchStatus updates AuditLog.status watermark fields after a
// successful archive batch.
func (c *AuditEntryRetentionController) patchStatus(
	ctx context.Context,
	current *keystonev1alpha1.AuditLog,
	manifest audit.ArchiveManifest,
) error {
	updated := current.DeepCopy()
	updated.Status.LastArchivedSequence = manifest.LastSequence
	now := metav1.NewTime(manifest.CreatedAt)
	updated.Status.LastArchivedTime = &now
	updated.Status.LastArchivedHash = manifest.LastEntryHash
	if err := c.Status().Patch(ctx, updated, client.MergeFrom(current)); err != nil {
		return err
	}
	// Mutate caller's copy so subsequent batches in the same pass
	// see the new watermark.
	current.Status = updated.Status
	return nil
}

// updateLagGauge records the age (seconds) of the oldest expired
// entry that hasn't been archived yet. The gauge feeds the
// "archive lag > 24h" alert in the PrometheusRule.
func (c *AuditEntryRetentionController) updateLagGauge(
	alog *keystonev1alpha1.AuditLog,
	expired []keystonev1alpha1.AuditEntry,
	cutoff time.Time,
) {
	if len(expired) == 0 {
		auditArchiveLagSeconds.Set(0)
		return
	}
	oldest := expired[0].Spec.Timestamp.Time
	for i := range expired {
		if expired[i].Spec.Timestamp.Time.Before(oldest) {
			oldest = expired[i].Spec.Timestamp.Time
		}
	}
	auditArchiveLagSeconds.Set(cutoff.Sub(oldest).Seconds())
}

func (c *AuditEntryRetentionController) recordPassDuration(start time.Time, outcome string) {
	auditArchivePassDurationSeconds.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
}

// resolveArchiver builds (or rebuilds) c.Archiver based on the
// AuditLog's current spec.archive block. Re-resolves on every tick
// but only re-instantiates when the config fingerprint changes — so
// hot-rotating MinIO credentials works without manager restarts but
// happy-path ticks don't pay a Secret-read on every iteration.
func (c *AuditEntryRetentionController) resolveArchiver(
	ctx context.Context,
	alog *keystonev1alpha1.AuditLog,
	logger logrLike,
) error {
	if alog.Spec.Archive == nil {
		c.Archiver = audit.NoopArchiver{}
		c.archiverConfigHash = ""
		return nil
	}
	spec := alog.Spec.Archive

	// Backend=noop short-circuits without resolving creds.
	if spec.Backend == "noop" {
		c.Archiver = audit.NoopArchiver{}
		c.archiverConfigHash = "noop"
		return nil
	}

	if spec.S3 == nil {
		return fmt.Errorf("archive.backend=s3 but archive.s3 is nil")
	}

	// Resolve creds Secret.
	secretName := spec.S3.CredentialsRef.SecretName
	if secretName == "" {
		return fmt.Errorf("archive.s3.credentialsRef.secretName is required")
	}
	akKey := spec.S3.CredentialsRef.AccessKeyIDKey
	if akKey == "" {
		akKey = "accessKeyID"
	}
	skKey := spec.S3.CredentialsRef.SecretAccessKeyKey
	if skKey == "" {
		skKey = "secretAccessKey"
	}

	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: c.SystemNamespace, Name: secretName,
	}, sec); err != nil {
		return fmt.Errorf("read S3 credentials Secret %s/%s: %w",
			c.SystemNamespace, secretName, err)
	}
	ak, ok1 := sec.Data[akKey]
	sk, ok2 := sec.Data[skKey]
	if !ok1 || !ok2 || len(ak) == 0 || len(sk) == 0 {
		return fmt.Errorf("Secret %s/%s missing key %q or %q",
			c.SystemNamespace, secretName, akKey, skKey)
	}

	// Compute fingerprint to decide rebuild-vs-reuse.
	fp := fmt.Sprintf("s3|%s|%s|%s|%v|%s|%d:%d",
		spec.S3.Endpoint, spec.S3.Bucket, spec.S3.Region, spec.S3.UseTLS,
		spec.S3.PathPrefix, len(ak), len(sk))
	if c.Archiver != nil && c.archiverConfigHash == fp {
		return nil
	}

	cfg := audit.S3ArchiverConfig{
		Endpoint:        spec.S3.Endpoint,
		Bucket:          spec.S3.Bucket,
		Region:          spec.S3.Region,
		UseTLS:          spec.S3.UseTLS,
		AccessKeyID:     string(ak),
		SecretAccessKey: string(sk),
		PathPrefix:      spec.S3.PathPrefix,
	}
	a, err := c.ArchiverFactory(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build S3Archiver: %w", err)
	}
	c.Archiver = a
	c.archiverConfigHash = fp
	logger.Info("audit retention archiver active",
		"backend", a.Kind(),
		"endpoint", spec.S3.Endpoint,
		"bucket", spec.S3.Bucket,
		"region", spec.S3.Region)
	return nil
}

// SetupWithManager registers the periodic checker with the manager.
// Mirrors DriftController.SetupWithManager.
func (c *AuditEntryRetentionController) SetupWithManager(mgr ctrl.Manager) error {
	if c.SystemNamespace == "" {
		c.SystemNamespace = "keystone-system"
	}
	if c.Recorder == nil {
		c.Recorder = mgr.GetEventRecorderFor("keystone-audit-retention")
	}
	return mgr.Add(manager.RunnableFunc(c.Start))
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=auditentries,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=auditlogs,verbs=get;list;watch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=auditlogs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// ^ Secret access required to resolve S3 credentials at manager startup.
// ^ AuditEntry delete is the entire reason this controller exists.

const (
	defaultBatchSize       = 1000
	defaultDeleteRateLimit = 100
)

// logrLike is the minimal logger surface this controller uses.
// Sigs.k8s.io's logr.Logger satisfies it; tests can inject a stub.
type logrLike interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}
