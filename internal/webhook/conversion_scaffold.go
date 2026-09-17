// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

// Conversion-webhook scaffold.
//
// v0.1.x ships `v1alpha1` only, so no kind actually needs conversion.
// This file exists to:
//
//  1. Hold the contract that every `v1alpha1` kind implements the
//     conversion hub (ConvertTo / ConvertFrom) even though the
//     conversion is identity today.
//  2. Make the `v1alpha1` → `v1beta1` upgrade in v0.3 a diff-sized
//     change: add the v1beta1 package, implement the per-kind
//     ConvertTo / ConvertFrom methods, flip the CRD's
//     `spec.conversion.strategy` from None → Webhook.
//  3. Document the round-trip-test expectation so nobody merges a
//     new version without the tests alongside.
//
// See API_STABILITY.md for the contract this scaffold prepares.
//
// Implementation note: controller-runtime's `conversion.Convertible`
// interface is the standard shape. Each kind's v1alpha1 type will
// gain `Hub()` (no-op for the hub type) and each non-hub version
// will gain `ConvertTo(dst conversion.Hub) error` +
// `ConvertFrom(src conversion.Hub) error`. We designate v1alpha1 as
// the hub; conversions fan out from it.
//
// Until v1beta1 ships, this file has no exported symbols — it's a
// placeholder + documentation anchor. Adding exported stubs today
// would be dead code that goroutine linters complain about; the
// file's value is in the comments and in existing at the expected
// path so grep-for-conversion_ finds it.
