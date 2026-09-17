// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/migration"
)

// ensureMigrationPlan creates (or returns existing) MigrationPlan CR
// for a (bundle, schema, version) triple. Plans are the Plan-Gate-Apply
// artifact — reviewable snapshots of the DDL that will run against a
// specific (bundle, schema) pair.
//
// This helper CREATES plans only. Approval is owned by
// MigrationPlanReconciler (see migrationplan_controller.go): it
// resolves spec.approved=true via SchemaPolicy.spec.planApproval (Auto
// default) or leaves the gate closed until a reviewer flips it
// (Manual mode). Splitting creation from approval means kubectl
// apply of a plan YAML goes through the same gate as controller-
// generated plans.
//
// Plan name: <bundle>-<version>-<schema>-plan, deterministic so
// retries find the same plan rather than spawning duplicates.
func ensureMigrationPlan(
	ctx context.Context,
	c client.Client,
	bundle *keystonev1alpha1.MigrationBundle,
	schema *keystonev1alpha1.DatabaseSchema,
	src *migration.ResolvedSource,
	scheme *runtime.Scheme,
) (*keystonev1alpha1.MigrationPlan, error) {
	planName := planName(bundle.Name, bundle.Spec.Version, schema.Name)
	var plan keystonev1alpha1.MigrationPlan
	err := c.Get(ctx, types.NamespacedName{Namespace: schema.Namespace, Name: planName}, &plan)
	if err == nil {
		return &plan, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get MigrationPlan %s: %w", planName, err)
	}

	// Build statements list — for versioned strategy, one entry per
	// SQL file; for pgroll, one entry per operation. Truncate to plan's
	// MaxItems (2048 / 4KiB each).
	statements := make([]keystonev1alpha1.PlannedStatement, 0)
	if src != nil {
		idx := int32(1)
		for _, name := range src.Names {
			body := src.Files[name]
			if len(body) > 4096 {
				body = body[:3950] + "… [truncated; see ConfigMap]"
			}
			statements = append(statements, keystonev1alpha1.PlannedStatement{
				File: name, Index: idx, SQL: body,
			})
			idx++
		}
	}
	for i, op := range bundle.Spec.Operations {
		statements = append(statements, keystonev1alpha1.PlannedStatement{
			File:  fmt.Sprintf("operations[%d]", i),
			Index: int32(i + 1),
			SQL:   fmt.Sprintf("kind=%s table=%s", op.Kind, op.Table),
		})
	}

	// `bundle` label uses bundle.Spec.Version (short content-address)
	// rather than bundle.Name to stay under the 63-char Kubernetes label-
	// value limit. Full bundle.Name lives in the annotation below + on
	// spec.bundleRef of the Plan (where MaxLength=253 is permitted).
	plan = keystonev1alpha1.MigrationPlan{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: schema.Namespace,
			Name:      planName,
			Labels: map[string]string{
				"keystone.hexxlock.io/bundle":  bundle.Spec.Version,
				"keystone.hexxlock.io/version": bundle.Spec.Version,
				"keystone.hexxlock.io/schema":  schema.Name,
			},
			Annotations: map[string]string{
				"keystone.hexxlock.io/bundle-name": bundle.Name,
			},
		},
		Spec: keystonev1alpha1.MigrationPlanSpec{
			BundleRef:     bundle.Name,
			BundleVersion: bundle.Spec.Version,
			SchemaRef:     schema.Name,
			Statements:    statements,
			// Findings stays empty in Phase 3.1; squawk wiring is 3.1.1.
		},
	}
	if scheme != nil {
		_ = controllerutil.SetControllerReference(bundle, &plan, scheme)
	}
	createErr := c.Create(ctx, &plan)
	switch {
	case createErr == nil:
		// Happy path: Create populates plan.UID, ResourceVersion,
		// CreationTimestamp in-place via the response body. No
		// re-read required, and re-reading immediately races the
		// controller-runtime cache (the Informer watch event that
		// populates the cache lands AFTER apiserver-side commit
		// returns to us — observed in v0.1.8 e2e as
		// "re-read created MigrationPlan: ... not found").
	case apierrors.IsAlreadyExists(createErr):
		// Lost a create race against another reconciler / replica.
		// Our `plan` struct hasn't been populated by Create — we
		// MUST Get to learn the existing object's UID + the rest.
		// Poll: between AlreadyExists from the apiserver and the
		// Informer watch updating our cache there's a window of
		// up to one watch-event RTT. 30s is generous; the typical
		// observed lag is <1s.
		pollErr := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
			func(ctx context.Context) (bool, error) {
				err := c.Get(ctx, types.NamespacedName{Namespace: plan.Namespace, Name: plan.Name}, &plan)
				if apierrors.IsNotFound(err) {
					// Cache hasn't observed the object yet; keep polling.
					return false, nil
				}
				if err != nil {
					// Real error — abort with it.
					return false, err
				}
				return true, nil
			})
		if pollErr != nil {
			return nil, fmt.Errorf("await cached MigrationPlan %s after AlreadyExists: %w",
				planName, pollErr)
		}
	default:
		return nil, fmt.Errorf("create MigrationPlan %s: %w", planName, createErr)
	}

	// Approval is deliberately NOT set here — MigrationPlanReconciler
	// owns spec.approved + status.approvalMode. Doing it inline would
	// race against the reconciler and hide the Plan-Gate-Apply gate
	// from operators of Manual-mode bundles.

	return &plan, nil
}

// planName composes a deterministic name. Trimmed to K8s 253-char limit.
func planName(bundle, version, schema string) string {
	name := fmt.Sprintf("%s-%s-%s-plan", bundle, version, schema)
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

