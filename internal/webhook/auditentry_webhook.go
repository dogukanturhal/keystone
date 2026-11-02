// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
)

// +kubebuilder:webhook:path=/validate-keystone-hexxlock-io-v1alpha1-auditentry,mutating=false,failurePolicy=fail,sideEffects=None,groups=keystone.hexxlock.io,resources=auditentries,verbs=create;update;delete,versions=v1alpha1,name=vauditentry.kb.io,admissionReviewVersions=v1

// AuditEntryValidator enforces append-only semantics on the audit
// chain. Updates and deletes are always rejected; creates are accepted
// only if the SelfHash matches the canonical encoding of the spec.
//
// This is the integrity guarantee that backs every compliance claim
// in `docs/compliance-mappings.md`. Without this webhook, an attacker
// with apiserver access could rewrite history.
type AuditEntryValidator struct{}

var _ admission.Validator[*keystonev1alpha1.AuditEntry] = &AuditEntryValidator{}

// SetupWithManager registers the validator.
func (v *AuditEntryValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &keystonev1alpha1.AuditEntry{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate accepts a new entry only if its SelfHash matches the
// canonical encoding. Callers (the audit Logger) must compute the
// hash correctly; admission is the tripwire catching replay or
// tampering attempts.
func (v *AuditEntryValidator) ValidateCreate(_ context.Context, entry *keystonev1alpha1.AuditEntry) (admission.Warnings, error) {
	if err := audit.VerifyEntry(entry.Spec); err != nil {
		return nil, fmt.Errorf("AuditEntry admission rejected: %w", err)
	}
	// PrevHash "genesis" is allowed exactly once — the first entry.
	// A second genesis claim would split the chain; we can't check
	// that here without a list call (which is expensive on every
	// admission), so the VerifyChain tool catches it post-hoc.
	return nil, nil
}

// ValidateUpdate always rejects. Audit entries are immutable.
func (v *AuditEntryValidator) ValidateUpdate(_ context.Context, _, _ *keystonev1alpha1.AuditEntry) (admission.Warnings, error) {
	return nil, fmt.Errorf(
		"AuditEntry is append-only: updates are never permitted. " +
			"If the entry contains an error, append a new correcting " +
			"entry with verb=update and reason explaining the correction")
}

// ValidateDelete always rejects. Audit entries are immutable.
//
// The only legitimate path to remove an entry is the retention-pruner
// (Phase 12.3), which runs as a separate controller with its own
// signing ServiceAccount and bypasses this webhook via a
// matchExpressions selector on the ValidatingWebhookConfiguration.
// Until that lands, no entry is ever deleted.
func (v *AuditEntryValidator) ValidateDelete(_ context.Context, _ *keystonev1alpha1.AuditEntry) (admission.Warnings, error) {
	return nil, fmt.Errorf(
		"AuditEntry deletion is disabled. The retention pruner " +
			"(Phase 12.3) is the only authorised deleter and it runs " +
			"with a bypass-signed ServiceAccount")
}
