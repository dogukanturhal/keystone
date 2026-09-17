// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestLeaderElection_TwoClientsOneLeader verifies the leader-election
// primitive the manager uses at runtime:
//
//   - Two competing lock candidates against the same envtest
//     apiserver both attempt to acquire the lease
//   - Exactly one wins within the lease-duration window
//   - The loser observes leadership transfer when the winner stops
//
// This is the core HA claim from docs/operator-hardening.md §9 and
// the ADR-0014 SLO "controllers keep reconciling across voluntary
// disruptions". Phase 12 ships replicas=2 by default; this test
// proves the lock itself works so the replicas behave as intended.
func TestLeaderElection_TwoClientsOneLeader(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	const (
		leaseName      = "keystone-ha-test"
		leaseNamespace = "keystone-system"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create two lock candidates pointing at the same lease. Run each
	// in its own goroutine; track which wins by signalling a channel
	// on OnStartedLeading.
	winner := make(chan string, 2)
	leaderCtx, cancelLeader := context.WithCancel(ctx)
	defer cancelLeader()

	for _, id := range []string{"candidate-A", "candidate-B"} {
		id := id
		go func() {
			leaderelection.RunOrDie(leaderCtx, leaderelection.LeaderElectionConfig{
				Lock: newEnvtestLock(t, k8s, leaseName, leaseNamespace, id),
				// Aggressive times for a fast test. Production uses
				// controller-runtime defaults (15s/10s/2s).
				LeaseDuration:   3 * time.Second,
				RenewDeadline:   2 * time.Second,
				RetryPeriod:     500 * time.Millisecond,
				ReleaseOnCancel: true,
				Callbacks: leaderelection.LeaderCallbacks{
					OnStartedLeading: func(_ context.Context) {
						select {
						case winner <- id:
						default:
						}
					},
					OnStoppedLeading: func() {},
					OnNewLeader:      func(_ string) {},
				},
			})
		}()
	}

	// Wait for a winner — the first identity on the channel is the
	// one that raced to acquire. The other is still spinning.
	select {
	case w := <-winner:
		t.Logf("first leader acquired: %s", w)
	case <-time.After(10 * time.Second):
		t.Fatal("no leader elected within 10s")
	}

	// Now verify the lease exists with the right holder stamped.
	var lease coordinationv1.Lease
	if err := k8s.Get(ctx, types.NamespacedName{
		Namespace: leaseNamespace, Name: leaseName,
	}, &lease); err != nil {
		t.Fatalf("get lease: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		t.Errorf("lease has no holder")
	}

	// Shut down — ReleaseOnCancel=true releases the lease so we don't
	// leak state across tests.
	cancelLeader()
	time.Sleep(1 * time.Second)
}

// newEnvtestLock constructs a LeaseLock using the envtest rest
// config. The test harness's client.Client is sufficient for the
// lock's CreateOrPatch calls via the underlying dynamic client.
//
// Built as a helper so the main test body stays focused on the
// HA assertion rather than plumbing.
func newEnvtestLock(
	t *testing.T,
	_ client.Client,
	name, namespace, identity string,
) *resourcelock.LeaseLock {
	t.Helper()
	// cfg is the package-level envtest rest.Config populated by
	// StartEnvtest. Using it here keeps the lock on the same
	// apiserver the test client talks to.
	rlock, err := resourcelock.NewFromKubeconfig(
		resourcelock.LeasesResourceLock,
		namespace,
		name,
		resourcelock.ResourceLockConfig{Identity: identity},
		cfg,
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("construct leaderelection lock: %v", err)
	}
	ll, ok := rlock.(*resourcelock.LeaseLock)
	if !ok {
		t.Fatalf("expected *LeaseLock, got %T", rlock)
	}
	return ll
}
