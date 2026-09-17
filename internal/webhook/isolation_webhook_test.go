// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func productDefinition(defaultIso keystonev1alpha1.IsolationMode, dbs ...keystonev1alpha1.ProductDatabase) *keystonev1alpha1.ProductDefinition {
	return &keystonev1alpha1.ProductDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "pd1"},
		Spec: keystonev1alpha1.ProductDefinitionSpec{
			DefaultIsolation: defaultIso,
			Databases:        dbs,
		},
	}
}

func productInstance(override keystonev1alpha1.IsolationMode) *keystonev1alpha1.ProductInstance {
	return &keystonev1alpha1.ProductInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "pi1"},
		Spec: keystonev1alpha1.ProductInstanceSpec{
			IsolationOverride: override,
		},
	}
}

// --- ProductDefinition create ---------------------------------------------

func TestProductDefinitionValidator_RejectsSiloDefault(t *testing.T) {
	v := &ProductDefinitionValidator{}
	_, err := v.ValidateCreate(context.Background(), productDefinition(keystonev1alpha1.IsolationModeSilo))
	if err == nil {
		t.Fatal("expected silo defaultIsolation to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "spec.defaultIsolation") {
		t.Fatalf("error should name the offending field, got: %v", err)
	}
	// The message must say what is actually provisioned, otherwise the
	// reader cannot tell what guarantee they are losing.
	if !strings.Contains(err.Error(), "bridge") {
		t.Fatalf("error should name the mode actually provisioned, got: %v", err)
	}
}

func TestProductDefinitionValidator_RejectsSiloOnDatabase(t *testing.T) {
	v := &ProductDefinitionValidator{}
	pd := productDefinition("", keystonev1alpha1.ProductDatabase{
		Key:       "financials",
		Isolation: keystonev1alpha1.IsolationModeSilo,
	})
	_, err := v.ValidateCreate(context.Background(), pd)
	if err == nil {
		t.Fatal("expected silo on a database to be rejected")
	}
	if !strings.Contains(err.Error(), "spec.databases[financials].isolation") {
		t.Fatalf("error should name the database key, got: %v", err)
	}
}

func TestProductDefinitionValidator_RejectsPool(t *testing.T) {
	// pool is likewise unimplemented. It is a weaker guarantee than
	// bridge, so provisioning bridge would over-deliver rather than
	// under-deliver — but the resource would still not describe reality.
	v := &ProductDefinitionValidator{}
	if _, err := v.ValidateCreate(context.Background(),
		productDefinition(keystonev1alpha1.IsolationModePool)); err == nil {
		t.Fatal("expected pool to be rejected")
	}
}

func TestProductDefinitionValidator_AllowsBridgeAndUnset(t *testing.T) {
	v := &ProductDefinitionValidator{}
	for _, mode := range []keystonev1alpha1.IsolationMode{"", keystonev1alpha1.IsolationModeBridge} {
		pd := productDefinition(mode, keystonev1alpha1.ProductDatabase{Key: "core", Isolation: mode})
		warns, err := v.ValidateCreate(context.Background(), pd)
		if err != nil {
			t.Fatalf("mode %q should be accepted, got: %v", mode, err)
		}
		if len(warns) != 0 {
			t.Fatalf("mode %q should produce no warnings, got: %v", mode, warns)
		}
	}
}

// --- ProductDefinition update ---------------------------------------------

func TestProductDefinitionValidator_RejectsNewlyIntroducedSilo(t *testing.T) {
	v := &ProductDefinitionValidator{}
	oldPD := productDefinition(keystonev1alpha1.IsolationModeBridge)
	newPD := productDefinition(keystonev1alpha1.IsolationModeSilo)
	if _, err := v.ValidateUpdate(context.Background(), oldPD, newPD); err == nil {
		t.Fatal("expected an update introducing silo to be rejected")
	}
}

func TestProductDefinitionValidator_GrandfathersUnchangedSilo(t *testing.T) {
	// A resource already stored with silo must remain editable, or
	// unrelated changes to it — and GitOps reconciliation of it — break.
	// Rejecting the update would not move any tenant's data, so it would
	// cost availability while buying no isolation.
	v := &ProductDefinitionValidator{}
	oldPD := productDefinition(keystonev1alpha1.IsolationModeSilo)
	newPD := productDefinition(keystonev1alpha1.IsolationModeSilo)
	newPD.Spec.Databases = []keystonev1alpha1.ProductDatabase{{Key: "unrelated-edit"}}

	warns, err := v.ValidateUpdate(context.Background(), oldPD, newPD)
	if err != nil {
		t.Fatalf("unchanged pre-existing silo should be allowed, got: %v", err)
	}
	if len(warns) == 0 {
		t.Fatal("grandfathered silo must still warn — silence is how the original defect hid")
	}
	if !strings.Contains(warns[0], "bridge") {
		t.Fatalf("warning should state what is actually provisioned, got: %v", warns[0])
	}
}

func TestProductDefinitionValidator_RejectsChangeBetweenUnimplementedModes(t *testing.T) {
	// silo -> pool is a *different* unimplemented declaration, not the
	// grandfathered one, so it must be rejected rather than warned.
	v := &ProductDefinitionValidator{}
	oldPD := productDefinition(keystonev1alpha1.IsolationModeSilo)
	newPD := productDefinition(keystonev1alpha1.IsolationModePool)
	if _, err := v.ValidateUpdate(context.Background(), oldPD, newPD); err == nil {
		t.Fatal("expected silo->pool to be rejected")
	}
}

func TestProductDefinitionValidator_GrandfatherIsPerField(t *testing.T) {
	// A stored offence on defaultIsolation must not license a new offence
	// on a database entry.
	v := &ProductDefinitionValidator{}
	oldPD := productDefinition(keystonev1alpha1.IsolationModeSilo)
	newPD := productDefinition(keystonev1alpha1.IsolationModeSilo,
		keystonev1alpha1.ProductDatabase{Key: "new", Isolation: keystonev1alpha1.IsolationModeSilo})
	if _, err := v.ValidateUpdate(context.Background(), oldPD, newPD); err == nil {
		t.Fatal("a new offending database must be rejected even when another field is grandfathered")
	}
}

// --- ProductInstance ------------------------------------------------------

func TestProductInstanceValidator_RejectsSiloOverride(t *testing.T) {
	v := &ProductInstanceValidator{}
	_, err := v.ValidateCreate(context.Background(), productInstance(keystonev1alpha1.IsolationModeSilo))
	if err == nil {
		t.Fatal("expected silo isolationOverride to be rejected")
	}
	if !strings.Contains(err.Error(), "spec.isolationOverride") {
		t.Fatalf("error should name the field, got: %v", err)
	}
}

func TestProductInstanceValidator_AllowsUnset(t *testing.T) {
	v := &ProductInstanceValidator{}
	warns, err := v.ValidateCreate(context.Background(), productInstance(""))
	if err != nil || len(warns) != 0 {
		t.Fatalf("unset override should be silently accepted, got warns=%v err=%v", warns, err)
	}
}

func TestProductInstanceValidator_GrandfathersUnchangedOverride(t *testing.T) {
	v := &ProductInstanceValidator{}
	old := productInstance(keystonev1alpha1.IsolationModeSilo)
	updated := productInstance(keystonev1alpha1.IsolationModeSilo)
	warns, err := v.ValidateUpdate(context.Background(), old, updated)
	if err != nil {
		t.Fatalf("unchanged pre-existing override should be allowed, got: %v", err)
	}
	if len(warns) == 0 {
		t.Fatal("grandfathered override must warn")
	}
}

func TestValidators_DeleteAlwaysAllowed(t *testing.T) {
	pdv := &ProductDefinitionValidator{}
	if _, err := pdv.ValidateDelete(context.Background(),
		productDefinition(keystonev1alpha1.IsolationModeSilo)); err != nil {
		t.Fatalf("delete must never be blocked: %v", err)
	}
	piv := &ProductInstanceValidator{}
	if _, err := piv.ValidateDelete(context.Background(),
		productInstance(keystonev1alpha1.IsolationModeSilo)); err != nil {
		t.Fatalf("delete must never be blocked: %v", err)
	}
}
