// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
)

// stubArchiver records the batches it receives and optionally errors.
type stubArchiver struct {
	mu        sync.Mutex
	calls     [][]keystonev1alpha1.AuditEntry
	failNext  error
	manifests []audit.ArchiveManifest
}

func (s *stubArchiver) Archive(_ context.Context, batch []keystonev1alpha1.AuditEntry) (audit.ArchiveManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return audit.ArchiveManifest{}, err
	}
	cp := make([]keystonev1alpha1.AuditEntry, len(batch))
	copy(cp, batch)
	s.calls = append(s.calls, cp)
	m := audit.ArchiveManifest{
		ObjectKey:     fmt.Sprintf("test/auditentries-%d-%d.jsonl.gz", batch[0].Spec.Sequence, batch[len(batch)-1].Spec.Sequence),
		FirstSequence: batch[0].Spec.Sequence,
		LastSequence:  batch[len(batch)-1].Spec.Sequence,
		LastEntryHash: batch[len(batch)-1].Spec.SelfHash,
		SizeBytes:     int64(len(batch) * 100),
		SHA256:        "deadbeef",
		EntryCount:    len(batch),
		CreatedAt:     time.Now().UTC(),
	}
	s.manifests = append(s.manifests, m)
	return m, nil
}

func (s *stubArchiver) Kind() string { return "stub" }

type discardLogger struct{ buf bytes.Buffer }

func (d *discardLogger) Info(msg string, kv ...any)             {}
func (d *discardLogger) Error(err error, msg string, kv ...any) {}

func newRetentionFakeClient(objs ...runtime.Object) client.Client {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = keystonev1alpha1.AddToScheme(s)
	return fake.NewClientBuilder().
		WithScheme(s).
		WithRuntimeObjects(objs...).
		WithStatusSubresource(&keystonev1alpha1.AuditLog{}, &keystonev1alpha1.AuditEntry{}).
		Build()
}

func mkEntry(seq int64, age time.Duration, prevHash, selfHash string) *keystonev1alpha1.AuditEntry {
	return &keystonev1alpha1.AuditEntry{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("entry-%020d", seq),
			Namespace: "keystone-system",
		},
		Spec: keystonev1alpha1.AuditEntrySpec{
			Sequence:  seq,
			Verb:      "test",
			Outcome:   "success",
			Timestamp: metav1.NewTime(time.Now().Add(-age).UTC()),
			SelfHash:  selfHash,
			PrevHash:  prevHash,
			Actor: keystonev1alpha1.AuditActor{
				Username: "test-actor",
			},
			ResourceRef: keystonev1alpha1.AuditResourceRef{
				APIVersion: "keystone.hexxlock.io/v1alpha1",
				Kind:       "MigrationBundle",
				Name:       "test-bundle",
			},
		},
	}
}

func mkAuditLog(retentionDays int32, archive *keystonev1alpha1.AuditArchiveSpec) *keystonev1alpha1.AuditLog {
	return &keystonev1alpha1.AuditLog{
		ObjectMeta: metav1.ObjectMeta{
			Name: keystonev1alpha1.AuditLogName,
		},
		Spec: keystonev1alpha1.AuditLogSpec{
			RetentionDays: retentionDays,
			Archive:       archive,
		},
	}
}

// TestRetention_DormantWhenNoArchiveBlock — verifies the safe default:
// no Archive spec → controller logs once and does nothing, even when
// expired entries exist. Critical for tier-1 safety: an unconfigured
// archive must NEVER cause data loss.
func TestRetention_DormantWhenNoArchiveBlock(t *testing.T) {
	alog := mkAuditLog(7, nil)
	e := mkEntry(1, 30*24*time.Hour, "", "self1") // 30d old, retention=7d → expired
	c := newRetentionFakeClient(alog, e)

	ctrl := &AuditEntryRetentionController{
		Client:          c,
		SystemNamespace: "keystone-system",
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	// Entry must still exist — no archiver, no delete.
	got := &keystonev1alpha1.AuditEntry{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "keystone-system", Name: e.Name}, got); err != nil {
		t.Fatalf("entry was deleted but controller is dormant: %v", err)
	}

	// Status must NOT have advanced.
	gotLog := &keystonev1alpha1.AuditLog{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: keystonev1alpha1.AuditLogName}, gotLog)
	if gotLog.Status.LastArchivedSequence != 0 {
		t.Fatalf("LastArchivedSequence advanced while dormant: got %d", gotLog.Status.LastArchivedSequence)
	}
}

// TestRetention_DormantWhenBackendIsNoop — same safety, but explicit
// backend=noop.
func TestRetention_DormantWhenBackendIsNoop(t *testing.T) {
	alog := mkAuditLog(7, &keystonev1alpha1.AuditArchiveSpec{Backend: "noop"})
	e := mkEntry(1, 30*24*time.Hour, "", "self1")
	c := newRetentionFakeClient(alog, e)

	ctrl := &AuditEntryRetentionController{
		Client:          c,
		SystemNamespace: "keystone-system",
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	got := &keystonev1alpha1.AuditEntry{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "keystone-system", Name: e.Name}, got); err != nil {
		t.Fatalf("noop backend should not delete entries: %v", err)
	}
}

// TestRetention_ArchivesAndDeletesExpired — happy path: spec.archive
// configured, expired entries get archived and deleted, watermark advances.
func TestRetention_ArchivesAndDeletesExpired(t *testing.T) {
	stub := &stubArchiver{}
	cred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "minio-creds"},
		Data: map[string][]byte{
			"accessKeyID":     []byte("test-ak"),
			"secretAccessKey": []byte("test-sk"),
		},
	}
	alog := mkAuditLog(7, &keystonev1alpha1.AuditArchiveSpec{
		Backend: "s3",
		S3: &keystonev1alpha1.AuditArchiveS3Spec{
			Endpoint: "minio.example:9000",
			Bucket:   "keystone-audit-archive",
			CredentialsRef: keystonev1alpha1.AuditArchiveCredsRef{
				SecretName: "minio-creds",
			},
		},
		BatchSize:       2,
		DeleteRateLimit: 100,
	})
	// 4 expired (30d > 7d retention), 1 fresh (1h < 7d).
	expired := []runtime.Object{
		mkEntry(1, 30*24*time.Hour, "", "h1"),
		mkEntry(2, 30*24*time.Hour, "h1", "h2"),
		mkEntry(3, 30*24*time.Hour, "h2", "h3"),
		mkEntry(4, 30*24*time.Hour, "h3", "h4"),
	}
	fresh := mkEntry(5, time.Hour, "h4", "h5")
	objs := append([]runtime.Object{alog, cred}, expired...)
	objs = append(objs, fresh)
	c := newRetentionFakeClient(objs...)

	ctrl := &AuditEntryRetentionController{
		Client:          c,
		SystemNamespace: "keystone-system",
		ArchiverFactory: func(_ context.Context, _ audit.S3ArchiverConfig) (audit.Archiver, error) {
			return stub, nil
		},
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	// Stub got 2 batches (BatchSize=2 → ceil(4/2)=2 calls).
	if len(stub.calls) != 2 {
		t.Fatalf("expected 2 archive calls, got %d", len(stub.calls))
	}
	// Each batch sorted ascending by sequence.
	for i, batch := range stub.calls {
		for j := 1; j < len(batch); j++ {
			if batch[j-1].Spec.Sequence > batch[j].Spec.Sequence {
				t.Errorf("batch %d not sorted: %d before %d", i, batch[j-1].Spec.Sequence, batch[j].Spec.Sequence)
			}
		}
	}

	// All 4 expired entries deleted; fresh entry untouched.
	remaining := &keystonev1alpha1.AuditEntryList{}
	_ = c.List(context.Background(), remaining)
	if len(remaining.Items) != 1 || remaining.Items[0].Spec.Sequence != 5 {
		t.Fatalf("expected only the fresh entry to remain; got %d entries", len(remaining.Items))
	}

	// Watermark advanced to highest archived sequence (4).
	gotLog := &keystonev1alpha1.AuditLog{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: keystonev1alpha1.AuditLogName}, gotLog)
	if gotLog.Status.LastArchivedSequence != 4 {
		t.Errorf("LastArchivedSequence = %d, want 4", gotLog.Status.LastArchivedSequence)
	}
	if gotLog.Status.LastArchivedHash != "h4" {
		t.Errorf("LastArchivedHash = %q, want h4", gotLog.Status.LastArchivedHash)
	}
	if gotLog.Status.LastArchivedTime == nil {
		t.Errorf("LastArchivedTime nil after archive")
	}
}

// TestRetention_NoExpiredEntries — when nothing is past retention,
// the controller is a no-op but lag gauge should be zero.
func TestRetention_NoExpiredEntries(t *testing.T) {
	stub := &stubArchiver{}
	cred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "minio-creds"},
		Data:       map[string][]byte{"accessKeyID": []byte("ak"), "secretAccessKey": []byte("sk")},
	}
	alog := mkAuditLog(7, &keystonev1alpha1.AuditArchiveSpec{
		Backend: "s3",
		S3: &keystonev1alpha1.AuditArchiveS3Spec{
			Endpoint: "minio.example:9000", Bucket: "b",
			CredentialsRef: keystonev1alpha1.AuditArchiveCredsRef{SecretName: "minio-creds"},
		},
	})
	fresh := mkEntry(1, time.Hour, "", "h1")
	c := newRetentionFakeClient(alog, cred, fresh)

	ctrl := &AuditEntryRetentionController{
		Client:          c,
		SystemNamespace: "keystone-system",
		ArchiverFactory: func(_ context.Context, _ audit.S3ArchiverConfig) (audit.Archiver, error) {
			return stub, nil
		},
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	if len(stub.calls) != 0 {
		t.Errorf("expected zero archive calls; got %d", len(stub.calls))
	}
	got := &keystonev1alpha1.AuditEntry{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "keystone-system", Name: fresh.Name}, got); err != nil {
		t.Fatalf("fresh entry deleted: %v", err)
	}
}

// TestRetention_ArchiveFailureSkipsDelete — if the archiver errors on
// upload, the controller MUST NOT delete any entries from etcd. The
// chain integrity guarantee depends on this exact ordering.
func TestRetention_ArchiveFailureSkipsDelete(t *testing.T) {
	stub := &stubArchiver{failNext: errors.New("S3 503: SlowDown")}
	cred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "minio-creds"},
		Data:       map[string][]byte{"accessKeyID": []byte("ak"), "secretAccessKey": []byte("sk")},
	}
	alog := mkAuditLog(7, &keystonev1alpha1.AuditArchiveSpec{
		Backend: "s3",
		S3: &keystonev1alpha1.AuditArchiveS3Spec{
			Endpoint: "minio.example:9000", Bucket: "b",
			CredentialsRef: keystonev1alpha1.AuditArchiveCredsRef{SecretName: "minio-creds"},
		},
	})
	expired := mkEntry(1, 30*24*time.Hour, "", "h1")
	c := newRetentionFakeClient(alog, cred, expired)

	ctrl := &AuditEntryRetentionController{
		Client:          c,
		SystemNamespace: "keystone-system",
		ArchiverFactory: func(_ context.Context, _ audit.S3ArchiverConfig) (audit.Archiver, error) {
			return stub, nil
		},
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	got := &keystonev1alpha1.AuditEntry{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "keystone-system", Name: expired.Name}, got); err != nil {
		t.Fatalf("entry deleted despite archive failure: %v", err)
	}

	// Watermark must not advance.
	gotLog := &keystonev1alpha1.AuditLog{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: keystonev1alpha1.AuditLogName}, gotLog)
	if gotLog.Status.LastArchivedSequence != 0 {
		t.Fatalf("watermark advanced after archive failure: %d", gotLog.Status.LastArchivedSequence)
	}
}

// TestRetention_HotReloadArchiver — operator flips backend in spec
// without manager restart; controller picks up new config on next pass.
func TestRetention_HotReloadArchiver(t *testing.T) {
	stub := &stubArchiver{}
	cred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "minio-creds"},
		Data:       map[string][]byte{"accessKeyID": []byte("ak"), "secretAccessKey": []byte("sk")},
	}
	alog := mkAuditLog(7, nil) // start dormant
	expired := mkEntry(1, 30*24*time.Hour, "", "h1")
	c := newRetentionFakeClient(alog, cred, expired)

	ctrl := &AuditEntryRetentionController{
		Client:          c,
		SystemNamespace: "keystone-system",
		ArchiverFactory: func(_ context.Context, _ audit.S3ArchiverConfig) (audit.Archiver, error) {
			return stub, nil
		},
	}

	// Pass 1: dormant — entry survives.
	ctrl.runOnce(context.Background(), &discardLogger{})
	if len(stub.calls) != 0 {
		t.Fatalf("pass 1 should be dormant; got %d calls", len(stub.calls))
	}

	// Operator patches spec to enable archive.
	gotLog := &keystonev1alpha1.AuditLog{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: keystonev1alpha1.AuditLogName}, gotLog)
	gotLog.Spec.Archive = &keystonev1alpha1.AuditArchiveSpec{
		Backend: "s3",
		S3: &keystonev1alpha1.AuditArchiveS3Spec{
			Endpoint: "minio.example:9000", Bucket: "b",
			CredentialsRef: keystonev1alpha1.AuditArchiveCredsRef{SecretName: "minio-creds"},
		},
	}
	if err := c.Update(context.Background(), gotLog); err != nil {
		t.Fatalf("update auditlog: %v", err)
	}

	// Pass 2: archive active — entry archived + deleted.
	ctrl.runOnce(context.Background(), &discardLogger{})
	if len(stub.calls) != 1 {
		t.Fatalf("pass 2 should have archived 1 batch; got %d", len(stub.calls))
	}
	got := &keystonev1alpha1.AuditEntry{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "keystone-system", Name: expired.Name}, got)
	if err == nil {
		t.Fatalf("expired entry not deleted after hot-reload to active")
	}
}

// TestRetention_MissingSecretIsError — bad config should not cause
// silent no-op (operator wouldn't notice). Should error-counter and
// stay dormant for that pass.
func TestRetention_MissingSecretIsError(t *testing.T) {
	stub := &stubArchiver{}
	alog := mkAuditLog(7, &keystonev1alpha1.AuditArchiveSpec{
		Backend: "s3",
		S3: &keystonev1alpha1.AuditArchiveS3Spec{
			Endpoint: "minio.example:9000", Bucket: "b",
			CredentialsRef: keystonev1alpha1.AuditArchiveCredsRef{SecretName: "does-not-exist"},
		},
	})
	expired := mkEntry(1, 30*24*time.Hour, "", "h1")
	c := newRetentionFakeClient(alog, expired)

	ctrl := &AuditEntryRetentionController{
		Client:          c,
		SystemNamespace: "keystone-system",
		ArchiverFactory: func(_ context.Context, _ audit.S3ArchiverConfig) (audit.Archiver, error) {
			return stub, nil
		},
	}
	ctrl.runOnce(context.Background(), &discardLogger{})

	// Entry NOT deleted; no archive call.
	if len(stub.calls) != 0 {
		t.Errorf("archived %d batches with missing secret; expected 0", len(stub.calls))
	}
	got := &keystonev1alpha1.AuditEntry{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "keystone-system", Name: expired.Name}, got); err != nil {
		t.Fatalf("entry deleted despite secret error: %v", err)
	}
}
