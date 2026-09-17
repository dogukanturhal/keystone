// SPDX-License-Identifier: AGPL-3.0-or-later

package devdb

import (
	"strings"
	"testing"
)

func TestRandomSchemaName(t *testing.T) {
	name1, err := randomSchemaName()
	if err != nil {
		t.Fatalf("randomSchemaName: %v", err)
	}
	if !strings.HasPrefix(name1, "ks_dev_") {
		t.Errorf("expected ks_dev_ prefix, got %q", name1)
	}
	if len(name1) != len("ks_dev_")+16 {
		t.Errorf("expected 23 chars, got %d: %q", len(name1), name1)
	}

	// Two calls should produce different names.
	name2, err := randomSchemaName()
	if err != nil {
		t.Fatalf("randomSchemaName: %v", err)
	}
	if name1 == name2 {
		t.Errorf("two calls returned same name: %q", name1)
	}
}
