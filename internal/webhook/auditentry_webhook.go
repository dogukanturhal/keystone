// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone/internal/audit"
)

// +kubebuilder:webhook:path=/validate-keystone-hexxlock-io-v1alpha1-auditentry,mutating=false,failurePolicy=fail,sideEffects=None,groups=keystone.hexxlock.io,resources=auditentries,verbs=create;update;delete,versions=v1alpha1,name=vauditentry.kb.io,admissionReviewVersions=v1

// AuditEntryValidator enforces append-only semantics on the audit
// chain. Updates are always rejected; creates are accepted only if the
// SelfHash matches the canonical encoding of the spec; deletes are
// accepted only once the entry is past its retention window (see
// ValidateDelete).
//
// This is the integrity guarantee that backs every compliance claim in
// `docs/compliance-mappings.md`. Without this webhook, an attacker with
// apiserver access could rewrite history.
type AuditEntryValidator struct {
	// Client reads the singleton AuditLog to resolve the retention
	// window. A cached reader is fine: retention changes are rare and a
	// stale window only shifts eligibility by the cache's resync
	// interval, in the conservative direction if the window grew.
	Client client.Reader

	// nowFn is injectable for tests. Nil means time.Now.
	nowFn func() time.Time
}

func (v *AuditEntryValidator) now() time.Time {
	if v.nowFn != nil {
		return v.nowFn()
	}
	return time.Now()
}

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
			"entry with verb=update and reason explaining the correction.")
}

// ValidateDelete permits a delete only when the entry is past the
// configured retention window, and refuses every other delete.
//
// The rule is deliberately a property of the entry rather than of the
// caller. An earlier design note in this file described exempting the
// retention pruner by identity — "a separate controller with its own
// signing ServiceAccount" bypassing admission "via a matchExpressions
// selector on the ValidatingWebhookConfiguration". Neither existed: no
// selector was ever configured in either install path, and the pruner
// runs in-process in the manager under the manager's own ServiceAccount.
// An identity exemption would also have been the weaker guarantee,
// since anything running as that ServiceAccount could then erase any
// entry at will.
//
// Checking expiry instead gives the property the compliance mappings
// actually claim: no principal, including the operator itself, can
// remove an audit entry before its retention period has elapsed.
//
// Fails closed. If the retention window cannot be determined the delete
// is refused, because the alternative is deleting evidence on the
// strength of a failed lookup.
func (v *AuditEntryValidator) ValidateDelete(ctx context.Context, entry *keystonev1alpha1.AuditEntry) (admission.Warnings, error) {
	if v.Client == nil {
		return nil, fmt.Errorf(
			"AuditEntry deletion refused: validator has no client configured, " +
				"so the retention window cannot be verified")
	}

	alog := &keystonev1alpha1.AuditLog{}
	if err := v.Client.Get(ctx, types.NamespacedName{Name: keystonev1alpha1.AuditLogName}, alog); err != nil {
		return nil, fmt.Errorf(
			"AuditEntry deletion refused: cannot read AuditLog %q to determine "+
				"the retention window: %w", keystonev1alpha1.AuditLogName, err)
	}

	retention := alog.Spec.RetentionDays
	if entry.IsRetentionExpired(retention, v.now()) {
		return nil, nil
	}

	cutoff := keystonev1alpha1.RetentionCutoff(retention, v.now())
	return nil, fmt.Errorf(
		"AuditEntry %q (sequence %d, timestamp %s) is within the %d-day "+
			"retention window and cannot be deleted; it becomes eligible after %s. "+
			"Audit entries are append-only until retention expires",
		entry.Name, entry.Spec.Sequence,
		entry.Spec.Timestamp.Time.UTC().Format(time.RFC3339),
		keystonev1alpha1.EffectiveRetentionDays(retention),
		cutoff.UTC().Format(time.RFC3339))
}
