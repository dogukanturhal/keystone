// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

// The --role flag was previously parsed and logged but never branched
// on, so --role=agent silently produced a second fully-active hub that
// executed migrations. These tests pin the flag closed until the agent
// role actually exists.
func TestOptionsValidate_RoleHubAccepted(t *testing.T) {
	o := &options{role: roleHub}
	if err := o.validate(); err != nil {
		t.Fatalf("hub is the implemented role and must validate, got: %v", err)
	}
}

func TestOptionsValidate_RoleAgentRejected(t *testing.T) {
	o := &options{role: "agent"}
	err := o.validate()
	if err == nil {
		t.Fatal("--role=agent must be rejected: there is no agent code path, " +
			"so it would run every reconciler and execute migrations as a hub")
	}
	if !strings.Contains(err.Error(), "agent") || !strings.Contains(err.Error(), roleHub) {
		t.Fatalf("error should name both the rejected and the supported role, got: %v", err)
	}
}

func TestOptionsValidate_RoleUnknownRejected(t *testing.T) {
	for _, role := range []string{"", "Hub", "worker", "spoke"} {
		o := &options{role: role}
		if err := o.validate(); err == nil {
			t.Errorf("role %q must be rejected", role)
		}
	}
}
