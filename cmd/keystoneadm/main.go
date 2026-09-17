// SPDX-License-Identifier: AGPL-3.0-or-later
//
// keystoneadm — administrative tooling for the Keystone operator.
//
// Subcommands:
//
//	backfill   Replay existing AuditEntry CRs to NATS JetStream (T2 #25 Phase 4).

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"

	"k8s.io/apimachinery/pkg/runtime"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(keystonev1alpha1.AddToScheme(scheme))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: keystoneadm <subcommand> [flags]")
		fmt.Fprintln(os.Stderr, "  backfill  replay AuditEntry CRs to NATS JetStream")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "backfill":
		if err := runBackfill(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "backfill: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(1)
	}
}

func runBackfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)

	var (
		kubeconfig    string
		natsURL       string
		natsPrefix    string
		fromSequence  int64
		dryRun        bool
		publishTimeout time.Duration
	)

	fs.StringVar(&kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"),
		"path to kubeconfig; defaults to KUBECONFIG env or in-cluster config")
	fs.StringVar(&natsURL, "nats-url", "",
		"NATS server URL (required, e.g. nats://nats.messaging-system.svc.cluster.local:4222)")
	fs.StringVar(&natsPrefix, "subject-prefix", "audit.keystone",
		"NATS subject prefix; must match stream's subject filter")
	fs.Int64Var(&fromSequence, "from-sequence", 0,
		"replay from this sequence number inclusive (0 = start from the beginning); "+
			"use to resume after an interrupted backfill")
	fs.BoolVar(&dryRun, "dry-run", false,
		"list entries without publishing; useful for counting work before committing")
	fs.DurationVar(&publishTimeout, "publish-timeout", 5*time.Second,
		"per-message NATS publish timeout")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if natsURL == "" {
		return fmt.Errorf("--nats-url is required")
	}

	logger := newLogger()
	defer func() { _ = logger.Sync() }()

	// Build controller-runtime client.
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return fmt.Errorf("build kubeconfig: %w", err)
	}

	k8sClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create kube client: %w", err)
	}

	// List all AuditEntry CRs. Note: the cache transform in the manager
	// strips Before/After from the informer cache, but here we use a direct
	// client (no cache) so we get the full spec.
	ctx := context.Background()
	list := &keystonev1alpha1.AuditEntryList{}
	if err := k8sClient.List(ctx, list); err != nil {
		return fmt.Errorf("list AuditEntries: %w", err)
	}

	// Sort ascending by sequence so the archiver sees entries in order.
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].Spec.Sequence < list.Items[j].Spec.Sequence
	})

	// Apply --from-sequence filter.
	var filtered []keystonev1alpha1.AuditEntry
	for _, e := range list.Items {
		if e.Spec.Sequence >= fromSequence {
			filtered = append(filtered, e)
		}
	}

	logger.Info("backfill starting",
		zap.Int("total", len(list.Items)),
		zap.Int("toPublish", len(filtered)),
		zap.Int64("fromSequence", fromSequence),
		zap.Bool("dryRun", dryRun),
	)

	if dryRun {
		logger.Info("dry-run complete; no messages published",
			zap.Int("wouldPublish", len(filtered)))
		return nil
	}

	// Connect the NATS exporter.
	exp, err := audit.New(audit.NATSExporterOptions{
		URL:           natsURL,
		SubjectPrefix: natsPrefix,
		Timeout:       publishTimeout,
		Log:           logger,
	})
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer func() { _ = exp.Close() }()

	const progressEvery = 1000
	var published, skipped int

	for i := range filtered {
		spec := filtered[i].Spec

		if err := exp.Export(spec); err != nil {
			// On publish error, log and continue. The Nats-Msg-Id dedupe
			// means re-running backfill is safe — already-published entries
			// will be silently deduped by the server.
			logger.Error("publish failed; continuing (re-run to retry)",
				zap.Int64("sequence", spec.Sequence),
				zap.Error(err),
			)
			skipped++
			continue
		}
		published++

		if published%progressEvery == 0 {
			logger.Info("backfill progress",
				zap.Int("published", published),
				zap.Int("remaining", len(filtered)-published-skipped),
				zap.Int64("lastSequence", spec.Sequence),
			)
		}
	}

	logger.Info("backfill complete",
		zap.Int("published", published),
		zap.Int("skipped", skipped),
		zap.Int("total", len(filtered)),
	)

	if skipped > 0 {
		return fmt.Errorf("%d entries failed to publish; re-run with --from-sequence to retry", skipped)
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

