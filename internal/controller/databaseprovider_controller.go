// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/conditions"
	pg "github.com/dogukanturhal/keystone/internal/postgres"
)

// DatabaseProviderReconciler watches the admin-credentials Secret of
// each DatabaseProvider and reports rotation events into
// DatabaseProvider.status. On rotation it also evicts cached pools
// that were opened with the prior credentials — without this, the
// PoolCache would continue handing out the stale pool for the old
// password until operator restart.
//
// What the reconciler does NOT do:
//
//   - Resolve Vault references directly. Vault → Secret sync is ESO
//     / CSI-secret-store's job; Keystone treats the Secret as the
//     only boundary so operators can swap out the sync mechanism
//     (ESO, Reloader, Sealed Secrets, kubectl) without changing
//     Keystone code.
//
//   - Verify the new credentials against the live database. The
//     existing per-consumer resolveCredentials + pool acquire paths
//     do that; reconciliation here is pure observation.
//
// See docs/adrs/0024 for the ESO integration narrative.
type DatabaseProviderReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Pools    *pg.PoolCache

	SystemNamespace string
}

// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=databaseproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=keystone.hexxlock.io,resources=databaseproviders/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *DatabaseProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("databaseprovider", req.Name)

	var provider keystonev1alpha1.DatabaseProvider
	if err := r.Get(ctx, req.NamespacedName, &provider); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Connection-profile change detection: when spec.host / spec.port
	// are repointed (server migration, DNS→IP cutover, local
	// port-forward verification), cached pools still dialing the
	// previous endpoint must be evicted — Acquire keys pools by the
	// full connection string, so without eviction the old-endpoint
	// pools linger until operator restart. Mirrors the credential-
	// rotation eviction below, but keyed on the CR-visible profile
	// instead of the Secret ResourceVersion. The new profile is
	// stamped into status by markCredsReady / markCredsNotReady, so
	// the comparison self-heals even when credential resolution fails
	// (eviction is idempotent regardless).
	profile := connectionProfile(&provider)
	if prev := provider.Status.ObservedConnectionProfile; prev != "" && prev != profile {
		r.evictPoolsForProfile(&provider, prev, profile, logger)
	}

	secretName := provider.Spec.AdminCredentialsRef.SecretName
	if secretName == "" {
		return r.markCredsNotReady(ctx, &provider, "NoSecretName",
			fmt.Errorf("spec.adminCredentialsRef.secretName is empty"))
	}

	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{
		Namespace: r.SystemNamespace, Name: secretName,
	}, &sec)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.markCredsNotReady(ctx, &provider, "SecretNotFound",
				fmt.Errorf("Secret %s/%s not found", r.SystemNamespace, secretName))
		}
		return ctrl.Result{}, err
	}

	userKey := provider.Spec.AdminCredentialsRef.UsernameKey
	if userKey == "" {
		userKey = "username"
	}
	passKey := provider.Spec.AdminCredentialsRef.PasswordKey
	if passKey == "" {
		passKey = "password"
	}
	if _, ok := sec.Data[userKey]; !ok {
		return r.markCredsNotReady(ctx, &provider, "MissingUsernameKey",
			fmt.Errorf("Secret missing key %q", userKey))
	}
	if _, ok := sec.Data[passKey]; !ok {
		return r.markCredsNotReady(ctx, &provider, "MissingPasswordKey",
			fmt.Errorf("Secret missing key %q", passKey))
	}

	// Rotation detection: current Secret ResourceVersion differs
	// from the last-observed version recorded in status. First
	// reconcile (empty status) skips the rotation event and just
	// records the observation.
	rotated := provider.Status.CredentialsObservedVersion != "" &&
		provider.Status.CredentialsObservedVersion != sec.ResourceVersion
	if rotated {
		r.evictStalePools(ctx, &provider, logger)
	}

	return r.markCredsReady(ctx, &provider, &sec, rotated)
}

// markCredsReady writes the success path — updates observed version
// and rotation timestamp, emits CredentialsReady=True condition.
func (r *DatabaseProviderReconciler) markCredsReady(
	ctx context.Context,
	provider *keystonev1alpha1.DatabaseProvider,
	sec *corev1.Secret,
	rotated bool,
) (ctrl.Result, error) {
	patch := client.MergeFrom(provider.DeepCopy())
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.CredentialsObservedVersion = sec.ResourceVersion
	provider.Status.ObservedConnectionProfile = connectionProfile(provider)
	if rotated {
		now := metav1.Now()
		provider.Status.CredentialsRotatedAt = &now
		if r.Recorder != nil {
			r.Recorder.Eventf(provider, corev1.EventTypeNormal, "CredentialsRotated",
				"admin Secret %s/%s rotated; stale pools evicted",
				r.SystemNamespace, sec.Name)
		}
	}
	provider.Status.Conditions = conditions.Set(provider.Status.Conditions,
		keystonev1alpha1.ConditionTypeCredentialsReady,
		metav1.ConditionTrue, "SecretResolved",
		fmt.Sprintf("admin Secret %s/%s observed at resourceVersion %s",
			r.SystemNamespace, sec.Name, sec.ResourceVersion),
		provider.Generation)
	if err := r.Status().Patch(ctx, provider, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch provider status: %w", err)
	}
	return ctrl.Result{}, nil
}

// markCredsNotReady flips CredentialsReady=False with the given
// reason + error. Does NOT stamp CredentialsObservedVersion — the
// status retains the last-known-good version so rotation detection
// on recovery works.
func (r *DatabaseProviderReconciler) markCredsNotReady(
	ctx context.Context,
	provider *keystonev1alpha1.DatabaseProvider,
	reason string,
	cause error,
) (ctrl.Result, error) {
	patch := client.MergeFrom(provider.DeepCopy())
	provider.Status.ObservedGeneration = provider.Generation
	// The connection profile is CR-visible state, independent of
	// credential health — stamp it on the failure path too so a
	// provider stuck on a Secret problem doesn't re-trigger
	// profile-change eviction every reconcile.
	provider.Status.ObservedConnectionProfile = connectionProfile(provider)
	provider.Status.Conditions = conditions.Set(provider.Status.Conditions,
		keystonev1alpha1.ConditionTypeCredentialsReady,
		metav1.ConditionFalse, reason, cause.Error(),
		provider.Generation)
	if err := r.Status().Patch(ctx, provider, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch provider status: %w", err)
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(provider, corev1.EventTypeWarning, reason, "%s", cause.Error())
	}
	return ctrl.Result{}, nil
}

// evictStalePools drops every cached pool whose host+port+database
// match the provider. Does NOT compare passwords — that field isn't
// in CR-visible state. The predicate is deliberately coarse:
// matching host+port is enough, since a provider's Secret only
// governs ITS host+port pair.
func (r *DatabaseProviderReconciler) evictStalePools(
	ctx context.Context,
	provider *keystonev1alpha1.DatabaseProvider,
	logger interface {
		Info(msg string, keysAndValues ...interface{})
	},
) {
	if r.Pools == nil {
		return
	}
	host := provider.Spec.Host
	port := int(provider.Spec.Port)
	dropped := r.Pools.DropMatching(func(cfg pg.PoolConfig) bool {
		return cfg.Host == host && cfg.Port == port
	})
	logger.Info("dropped stale pools after credential rotation",
		"provider", provider.Name, "count", dropped)
}

// connectionProfile renders the CR-visible connection endpoint as a
// stable "host:port" string for status bookkeeping and change
// comparison. net.JoinHostPort keeps IPv6 literals unambiguous.
func connectionProfile(provider *keystonev1alpha1.DatabaseProvider) string {
	return net.JoinHostPort(provider.Spec.Host,
		strconv.Itoa(int(provider.Spec.Port)))
}

// evictPoolsForProfile drops every cached pool dialing the provider's
// PREVIOUS host:port endpoint. Called when the CR-visible connection
// profile changes; the predicate matches on the old endpoint, so
// new-profile pools (none exist yet — Acquire keys by the full
// connection string) are untouched. Complements evictStalePools,
// which handles the credential-rotation case but matches on the NEW
// host+port and therefore cannot reach pools left behind by a
// host/port edit.
func (r *DatabaseProviderReconciler) evictPoolsForProfile(
	provider *keystonev1alpha1.DatabaseProvider,
	prev, next string,
	logger interface {
		Info(msg string, keysAndValues ...interface{})
	},
) {
	if r.Pools == nil {
		return
	}
	host, portStr, err := net.SplitHostPort(prev)
	if err != nil {
		logger.Info("unparseable observed connection profile; skipping pool eviction",
			"provider", provider.Name, "profile", prev, "err", err.Error())
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		logger.Info("unparseable port in observed connection profile; skipping pool eviction",
			"provider", provider.Name, "profile", prev, "err", err.Error())
		return
	}
	dropped := r.Pools.DropMatching(func(cfg pg.PoolConfig) bool {
		return cfg.Host == host && cfg.Port == port
	})
	logger.Info("connection profile changed; dropped stale pools",
		"provider", provider.Name, "from", prev, "to", next, "count", dropped)
	if r.Recorder != nil {
		r.Recorder.Eventf(provider, corev1.EventTypeNormal, "ConnectionProfileChanged",
			"connection profile changed from %s to %s; %d stale pool(s) evicted",
			prev, next, dropped)
	}
}

// SetupWithManager wires the reconciler. The Secret watch is the
// load-bearing piece: it enqueues the provider whenever its admin
// Secret changes (including ESO-driven rotations), so rotation
// propagates within one reconcile tick without polling.
func (r *DatabaseProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.SystemNamespace == "" {
		r.SystemNamespace = "keystone-system"
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("keystone-databaseprovider")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keystonev1alpha1.DatabaseProvider{},
			builder.WithPredicates(ignoreStatusOnlyUpdates())).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.providersForSecret)).
		Named("databaseprovider").
		Complete(r)
}

// providersForSecret maps a Secret event to the DatabaseProviders
// that reference it. Only watches Secrets in SystemNamespace (the
// only place admin Secrets live per the existing design).
func (r *DatabaseProviderReconciler) providersForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	sec, ok := obj.(*corev1.Secret)
	if !ok || sec.Namespace != r.SystemNamespace {
		return nil
	}
	var providers keystonev1alpha1.DatabaseProviderList
	if err := r.List(ctx, &providers); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range providers.Items {
		p := &providers.Items[i]
		if p.Spec.AdminCredentialsRef.SecretName == sec.Name {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
		}
	}
	return out
}
