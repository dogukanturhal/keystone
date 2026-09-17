// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"testing"
)

func TestIsolationMode_IsImplemented(t *testing.T) {
	// Only bridge is provisionable today. If this test starts failing
	// because a mode was implemented, update ImplementedIsolationModes
	// and the admission tests together — the webhook reads this.
	cases := map[IsolationMode]bool{
		IsolationModeBridge:       true,
		IsolationModeSilo:         false,
		IsolationModePool:         false,
		IsolationMode(""):         false,
		IsolationMode("nonsense"): false,
	}
	for mode, want := range cases {
		if got := mode.IsImplemented(); got != want {
			t.Errorf("IsolationMode(%q).IsImplemented() = %v, want %v", mode, got, want)
		}
	}
}

func TestImplementedIsolationModes_MatchesIsImplemented(t *testing.T) {
	// The advertised list and the predicate must not drift apart; the
	// list is what users are told to use in rejection messages.
	for _, name := range ImplementedIsolationModes() {
		if !IsolationMode(name).IsImplemented() {
			t.Errorf("ImplementedIsolationModes() advertises %q but IsImplemented() rejects it", name)
		}
	}
	for _, mode := range []IsolationMode{IsolationModePool, IsolationModeBridge, IsolationModeSilo} {
		if !mode.IsImplemented() {
			continue
		}
		var listed bool
		for _, name := range ImplementedIsolationModes() {
			if IsolationMode(name) == mode {
				listed = true
			}
		}
		if !listed {
			t.Errorf("%q is implemented but missing from ImplementedIsolationModes()", mode)
		}
	}
}

func TestResolveIsolation_Precedence(t *testing.T) {
	cases := []struct {
		name                            string
		override, perDB, productDefault IsolationMode
		want                            IsolationMode
	}{
		{"all unset falls back to bridge", "", "", "", IsolationModeBridge},
		{"product default used when nothing else set", "", "", IsolationModeSilo, IsolationModeSilo},
		{"per-database beats product default", "", IsolationModePool, IsolationModeSilo, IsolationModePool},
		{"override beats everything", IsolationModeBridge, IsolationModePool, IsolationModeSilo, IsolationModeBridge},
		{"empty override falls through to per-database", "", IsolationModeSilo, IsolationModeBridge, IsolationModeSilo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveIsolation(tc.override, tc.perDB, tc.productDefault); got != tc.want {
				t.Errorf("ResolveIsolation(%q, %q, %q) = %q, want %q",
					tc.override, tc.perDB, tc.productDefault, got, tc.want)
			}
		})
	}
}
