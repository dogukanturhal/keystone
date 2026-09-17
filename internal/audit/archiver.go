// SPDX-License-Identifier: AGPL-3.0-or-later

package audit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// Archiver is the off-cluster sink for AuditEntry CRs that have aged
// past their retention window. Implementations consume a batch of
// entries (already filtered + sorted ascending by Sequence) and
// produce a single immutable archive object — the retention
// controller relies on this atomicity to maintain the chain
// invariant: an entry is deleted from etcd ONLY after Archive
// returns nil.
//
// Errors returned by Archive abort the current retention pass
// without deleting any entries. The next pass retries from the same
// watermark; idempotency in the backend is the implementation's
// responsibility (object names include the sequence range so a
// re-upload overwrites itself rather than fragmenting the archive).
type Archiver interface {
	// Archive uploads the given batch and returns the integrity
	// manifest describing what was written. The manifest includes a
	// SHA-256 of the archive object and the highest entry hash
	// (SelfHash) in the batch — operators can rehydrate the chain
	// by reading the archive back and re-computing the hashes.
	Archive(ctx context.Context, batch []keystonev1alpha1.AuditEntry) (ArchiveManifest, error)

	// Kind reports which backend this archiver is. Surfaced in
	// metrics labels and in the AuditEntry archive log line.
	Kind() string
}

// ArchiveManifest describes a single archived batch.
type ArchiveManifest struct {
	// ObjectKey is the key the batch was written under (S3 path
	// component, NOT including the bucket).
	ObjectKey string

	// FirstSequence is the lowest entry sequence in the batch.
	FirstSequence int64

	// LastSequence is the highest entry sequence in the batch.
	// Equals the new AuditLog.status.lastArchivedSequence after the
	// retention pass commits.
	LastSequence int64

	// LastEntryHash is the SelfHash of the entry at LastSequence.
	// Operators can reconcile by re-hashing the archive contents
	// and confirming the final hash matches.
	LastEntryHash string

	// SizeBytes is the on-the-wire size of the archive object
	// (post-gzip compression). Feeds the
	// keystone_audit_archive_bytes_total counter.
	SizeBytes int64

	// SHA256 is the hex-encoded SHA-256 of the archive object body.
	// Surfaced in operator-visible logs so manual integrity checks
	// (`mc cat ... | sha256sum`) can be cross-verified.
	SHA256 string

	// EntryCount is the number of entries in the batch.
	EntryCount int

	// CreatedAt is when the batch was uploaded.
	CreatedAt time.Time
}

// NoopArchiver is the dormant default — used when no Archive block is
// configured on AuditLog. Returns an empty manifest so the retention
// controller's accounting path stays exercised in tests, but does
// NOT mark any entries as archive-eligible (the controller MUST
// short-circuit when archiver.Kind() == "noop"). Embedding this
// guard at the controller layer rather than here lets unit tests
// simulate "configured but no-op" without pulling in S3 dependencies.
type NoopArchiver struct{}

// Archive discards the batch. Always returns ErrArchiverDormant —
// callers must NOT delete CRs based on a noop "success".
func (NoopArchiver) Archive(_ context.Context, _ []keystonev1alpha1.AuditEntry) (ArchiveManifest, error) {
	return ArchiveManifest{}, ErrArchiverDormant
}

// Kind always returns "noop".
func (NoopArchiver) Kind() string { return "noop" }

// ErrArchiverDormant is returned by NoopArchiver to signal "I am
// configured but intentionally inert". The retention controller
// treats this as a benign skip and does NOT delete entries.
var ErrArchiverDormant = errBadArchiver{msg: "archiver dormant: NoopArchiver in use"}

type errBadArchiver struct{ msg string }

func (e errBadArchiver) Error() string { return e.msg }

// IsDormant reports whether err signals an intentionally inert
// archiver (vs a transient backend failure). The controller uses
// this to short-circuit metrics + delete logic without alerting.
func IsDormant(err error) bool {
	_, ok := err.(errBadArchiver)
	return ok
}

// S3Archiver writes archive batches to an S3-compatible endpoint
// (MinIO, AWS S3, etc). One archive object per batch, named by
// sequence range so re-uploads are idempotent.
//
// Object key shape:
//
//	<pathPrefix>/<year>/<month>/<day>/auditentries-<firstSeq>-<lastSeq>.jsonl.gz
//
// Body format: newline-delimited ECS JSON (matching
// StdoutJSONExporter), gzip-compressed. Re-hydration:
//
//	mc cat <bucket>/<key> | gunzip | jq -c
type S3Archiver struct {
	client     *minio.Client
	bucket     string
	pathPrefix string
}

// S3ArchiverConfig is the constructor input — populated from
// AuditArchiveS3Spec + the resolved Secret credentials.
type S3ArchiverConfig struct {
	Endpoint        string
	Bucket          string
	Region          string
	UseTLS          bool
	AccessKeyID     string
	SecretAccessKey string
	PathPrefix      string
}

// NewS3Archiver returns a configured S3Archiver. The constructor
// validates the bucket exists; failure here surfaces fast at manager
// startup rather than masking misconfiguration as transient errors
// during reconcile.
func NewS3Archiver(ctx context.Context, cfg S3ArchiverConfig) (*S3Archiver, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("S3Archiver: endpoint and bucket are required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("S3Archiver: accessKeyID and secretAccessKey are required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseTLS,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("S3Archiver: minio client: %w", err)
	}

	exists, err := mc.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("S3Archiver: probe bucket %q: %w", cfg.Bucket, err)
	}
	if !exists {
		return nil, fmt.Errorf("S3Archiver: bucket %q does not exist (provision via Crossplane S3Bucket with object-lock + WORM retention before enabling archive)", cfg.Bucket)
	}

	return &S3Archiver{
		client:     mc,
		bucket:     cfg.Bucket,
		pathPrefix: cfg.PathPrefix,
	}, nil
}

// Kind returns "s3".
func (a *S3Archiver) Kind() string { return "s3" }

// Archive serialises the batch as gzipped JSON Lines and uploads to
// the configured bucket. The body is buffered in memory so a single
// archive batch's working-set must fit in BatchSize * average-entry
// size; defaults (1000 entries × ~5KB) keep this under 5MB which is
// well within the 128Mi pod memory limit.
func (a *S3Archiver) Archive(ctx context.Context, batch []keystonev1alpha1.AuditEntry) (ArchiveManifest, error) {
	if len(batch) == 0 {
		return ArchiveManifest{}, fmt.Errorf("S3Archiver.Archive: empty batch")
	}

	// Defensive: caller should pre-sort ascending by sequence, but
	// re-sort here so chain integrity doesn't depend on caller
	// hygiene.
	sorted := make([]keystonev1alpha1.AuditEntry, len(batch))
	copy(sorted, batch)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Spec.Sequence < sorted[j].Spec.Sequence
	})

	first := sorted[0].Spec.Sequence
	last := sorted[len(sorted)-1].Spec.Sequence
	lastHash := sorted[len(sorted)-1].Spec.SelfHash

	// Build the gzip JSON Lines body.
	body := &bytes.Buffer{}
	gz := gzip.NewWriter(body)
	enc := json.NewEncoder(gz)
	for i := range sorted {
		// Match the StdoutJSONExporter ECS shape so re-hydrated
		// archives are interchangeable with the live SIEM stream.
		record := toECSRecord(sorted[i].Spec)
		if err := enc.Encode(record); err != nil {
			return ArchiveManifest{}, fmt.Errorf("encode entry seq=%d: %w", sorted[i].Spec.Sequence, err)
		}
	}
	if err := gz.Close(); err != nil {
		return ArchiveManifest{}, fmt.Errorf("gzip close: %w", err)
	}

	bodyBytes := body.Bytes()
	hash := sha256.Sum256(bodyBytes)
	hexHash := hex.EncodeToString(hash[:])
	now := time.Now().UTC()

	key := a.objectKey(now, first, last)

	// Upload with the SHA-256 surfaced as both the standard ETag
	// hint AND a user metadata field so manual integrity checks
	// from `mc stat` or `aws s3api head-object` succeed without
	// re-downloading the body.
	_, err := a.client.PutObject(ctx, a.bucket, key, bytes.NewReader(bodyBytes), int64(len(bodyBytes)),
		minio.PutObjectOptions{
			ContentType:     "application/gzip",
			ContentEncoding: "gzip",
			UserMetadata: map[string]string{
				"sha256":         hexHash,
				"first-sequence": fmt.Sprintf("%d", first),
				"last-sequence":  fmt.Sprintf("%d", last),
				"last-hash":      lastHash,
				"entry-count":    fmt.Sprintf("%d", len(sorted)),
				"keystone-archive-schema": "ecs.v1.gzip.jsonl",
			},
		})
	if err != nil {
		return ArchiveManifest{}, fmt.Errorf("S3 PutObject %q: %w", key, err)
	}

	return ArchiveManifest{
		ObjectKey:     key,
		FirstSequence: first,
		LastSequence:  last,
		LastEntryHash: lastHash,
		SizeBytes:     int64(len(bodyBytes)),
		SHA256:        hexHash,
		EntryCount:    len(sorted),
		CreatedAt:     now,
	}, nil
}

func (a *S3Archiver) objectKey(t time.Time, first, last int64) string {
	prefix := a.pathPrefix
	if prefix != "" && prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	return fmt.Sprintf("%s%04d/%02d/%02d/auditentries-%020d-%020d.jsonl.gz",
		prefix, t.Year(), int(t.Month()), t.Day(), first, last)
}

// readAllArchive is a test helper exported lowercase for use in the
// archiver_test.go suite — given an io.Reader of an archive object's
// body, decode it back to []keystonev1alpha1.AuditEntrySpec for chain
// re-verification. NOT intended for production use; the retention
// controller writes archives but does not re-read them.
func readAllArchive(r io.Reader) ([]keystonev1alpha1.AuditEntrySpec, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip open: %w", err)
	}
	defer gr.Close()
	dec := json.NewDecoder(gr)
	var out []keystonev1alpha1.AuditEntrySpec
	for dec.More() {
		var record ecsRecord
		if err := dec.Decode(&record); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		out = append(out, fromECSRecord(record))
	}
	return out, nil
}

// fromECSRecord is the inverse of toECSRecord — partial round-trip
// for archive verification. Not all fields are recoverable (Before /
// After are intentionally not in ECS shape), but Sequence + SelfHash
// + PrevHash are, which is what chain re-validation needs.
func fromECSRecord(r ecsRecord) keystonev1alpha1.AuditEntrySpec {
	spec := keystonev1alpha1.AuditEntrySpec{
		Sequence: r.Event.Sequence,
		Verb:     r.Keystone.Verb,
		Outcome:  r.Event.Outcome,
		Reason:   r.Event.Reason,
		SelfHash: r.Keystone.Chain.SelfHash,
		PrevHash: r.Keystone.Chain.PrevHash,
		Actor: keystonev1alpha1.AuditActor{
			Username: r.User.Name,
			UID:      r.User.ID,
			Groups:   r.User.Groups,
		},
		ResourceRef: keystonev1alpha1.AuditResourceRef{
			APIVersion: r.Keystone.Resource.APIVersion,
			Kind:       r.Keystone.Resource.Kind,
			Namespace:  r.Keystone.Resource.Namespace,
			Name:       r.Keystone.Resource.Name,
			UID:        r.Keystone.Resource.UID,
		},
	}
	if r.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil {
			spec.Timestamp.Time = t
		}
	}
	return spec
}
