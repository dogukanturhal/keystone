// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// Isolation admission control.
//
// ProductDefinition and ProductInstance both carry isolation-mode fields
// whose CRD enum accepts pool, bridge and silo, but the TenancyController
// implements only bridge. Without admission control the API accepts a
// declaration it cannot honour: a ProductDefinition asking for silo
// (database-per-tenant) is provisioned onto a shared database, with no
// error raised and no status condition set. The resource says the tenant
// is isolated; the cluster says otherwise.
//
// That is a data-segregation failure, not a cosmetic one, so these
// validators reject unimplemented modes at admission rather than letting
// the controller silently downgrade them.
//
// Existing resources are grandfathered. An update that leaves an already
// stored unimplemented mode untouched is allowed with a warning, because
// rejecting it would block unrelated edits to resources that are already
// deployed — and would break GitOps reconciliation of them — without
// making any tenant safer. Introducing or changing an unimplemented mode
// is always rejected.

// isolationOffence is a single field declaring a mode the controller
// cannot provision.
type isolationOffence struct {
	path string
	mode keystonev1alpha1.IsolationMode
}

func (o isolationOffence) String() string {
	return fmt.Sprintf("%s=%q", o.path, o.mode)
}

// explain renders the rejection message. It names the mode actually
// provisioned so the reader learns what they are really getting.
func explainOffences(kind, namespace, name string, offences []isolationOffence) error {
	parts := make([]string, 0, len(offences))
	for _, o := range offences {
		parts = append(parts, o.String())
	}
	return fmt.Errorf(
		"%s %s/%s: isolation mode not implemented — %s. Keystone provisions every "+
			"database as %q (shared database, schema-per-tenant) regardless of the "+
			"declared mode, so accepting this would advertise data isolation the "+
			"controller does not provide. Supported modes: %s",
		kind, namespace, name,
		strings.Join(parts, ", "),
		keystonev1alpha1.IsolationModeBridge,
		strings.Join(keystonev1alpha1.ImplementedIsolationModes(), ", "),
	)
}

// grandfatherWarning renders the non-fatal notice for a stored offence
// that this update did not introduce.
func grandfatherWarning(offences []isolationOffence) admission.Warnings {
	parts := make([]string, 0, len(offences))
	for _, o := range offences {
		parts = append(parts, o.String())
	}
	return admission.Warnings{fmt.Sprintf(
		"isolation mode not implemented (%s): this resource is provisioned as %q "+
			"(shared database, schema-per-tenant), NOT the mode declared. Pre-existing "+
			"declaration left unchanged by this update; it provides no data isolation "+
			"guarantee.",
		strings.Join(parts, ", "), keystonev1alpha1.IsolationModeBridge,
	)}
}

// diffOffences splits the offences on an incoming object into those that
// already existed unchanged (grandfathered) and those this write
// introduces or alters (rejected).
func diffOffences(oldOff, newOff []isolationOffence) (grandfathered, introduced []isolationOffence) {
	prior := make(map[string]keystonev1alpha1.IsolationMode, len(oldOff))
	for _, o := range oldOff {
		prior[o.path] = o.mode
	}
	for _, o := range newOff {
		if mode, ok := prior[o.path]; ok && mode == o.mode {
			grandfathered = append(grandfathered, o)
			continue
		}
		introduced = append(introduced, o)
	}
	return grandfathered, introduced
}

// --- ProductDefinition ----------------------------------------------------

// +kubebuilder:webhook:path=/validate-keystone-hexxlock-io-v1alpha1-productdefinition,mutating=false,failurePolicy=fail,sideEffects=None,groups=keystone.hexxlock.io,resources=productdefinitions,verbs=create;update,versions=v1alpha1,name=vproductdefinition.kb.io,admissionReviewVersions=v1

// ProductDefinitionValidator rejects unimplemented isolation modes on
// spec.defaultIsolation and on each spec.databases[].isolation.
type ProductDefinitionValidator struct{}

var _ admission.Validator[*keystonev1alpha1.ProductDefinition] = &ProductDefinitionValidator{}

func (v *ProductDefinitionValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &keystonev1alpha1.ProductDefinition{}).
		WithValidator(v).
		Complete()
}

func productDefinitionOffences(pd *keystonev1alpha1.ProductDefinition) []isolationOffence {
	var out []isolationOffence
	if m := pd.Spec.DefaultIsolation; m != "" && !m.IsImplemented() {
		out = append(out, isolationOffence{path: "spec.defaultIsolation", mode: m})
	}
	for _, db := range pd.Spec.Databases {
		if m := db.Isolation; m != "" && !m.IsImplemented() {
			out = append(out, isolationOffence{
				path: fmt.Sprintf("spec.databases[%s].isolation", db.Key),
				mode: m,
			})
		}
	}
	return out
}

func (v *ProductDefinitionValidator) ValidateCreate(_ context.Context, pd *keystonev1alpha1.ProductDefinition) (admission.Warnings, error) {
	if off := productDefinitionOffences(pd); len(off) > 0 {
		return nil, explainOffences("ProductDefinition", pd.Namespace, pd.Name, off)
	}
	return nil, nil
}

func (v *ProductDefinitionValidator) ValidateUpdate(_ context.Context, oldPD, newPD *keystonev1alpha1.ProductDefinition) (admission.Warnings, error) {
	grandfathered, introduced := diffOffences(
		productDefinitionOffences(oldPD), productDefinitionOffences(newPD))
	if len(introduced) > 0 {
		return nil, explainOffences("ProductDefinition", newPD.Namespace, newPD.Name, introduced)
	}
	if len(grandfathered) > 0 {
		return grandfatherWarning(grandfathered), nil
	}
	return nil, nil
}

func (v *ProductDefinitionValidator) ValidateDelete(_ context.Context, _ *keystonev1alpha1.ProductDefinition) (admission.Warnings, error) {
	return nil, nil
}

// --- ProductInstance ------------------------------------------------------

// +kubebuilder:webhook:path=/validate-keystone-hexxlock-io-v1alpha1-productinstance,mutating=false,failurePolicy=fail,sideEffects=None,groups=keystone.hexxlock.io,resources=productinstances,verbs=create;update,versions=v1alpha1,name=vproductinstance.kb.io,admissionReviewVersions=v1

// ProductInstanceValidator rejects an unimplemented
// spec.isolationOverride. The override has the highest precedence of the
// three isolation fields, so an unimplemented value here silently
// overrides an otherwise-valid ProductDefinition.
type ProductInstanceValidator struct{}

var _ admission.Validator[*keystonev1alpha1.ProductInstance] = &ProductInstanceValidator{}

func (v *ProductInstanceValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &keystonev1alpha1.ProductInstance{}).
		WithValidator(v).
		Complete()
}

func productInstanceOffences(pi *keystonev1alpha1.ProductInstance) []isolationOffence {
	if m := pi.Spec.IsolationOverride; m != "" && !m.IsImplemented() {
		return []isolationOffence{{path: "spec.isolationOverride", mode: m}}
	}
	return nil
}

func (v *ProductInstanceValidator) ValidateCreate(_ context.Context, pi *keystonev1alpha1.ProductInstance) (admission.Warnings, error) {
	if off := productInstanceOffences(pi); len(off) > 0 {
		return nil, explainOffences("ProductInstance", pi.Namespace, pi.Name, off)
	}
	return nil, nil
}

func (v *ProductInstanceValidator) ValidateUpdate(_ context.Context, oldPI, newPI *keystonev1alpha1.ProductInstance) (admission.Warnings, error) {
	grandfathered, introduced := diffOffences(
		productInstanceOffences(oldPI), productInstanceOffences(newPI))
	if len(introduced) > 0 {
		return nil, explainOffences("ProductInstance", newPI.Namespace, newPI.Name, introduced)
	}
	if len(grandfathered) > 0 {
		return grandfatherWarning(grandfathered), nil
	}
	return nil, nil
}

func (v *ProductInstanceValidator) ValidateDelete(_ context.Context, _ *keystonev1alpha1.ProductInstance) (admission.Warnings, error) {
	return nil, nil
}
