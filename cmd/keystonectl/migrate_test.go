// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dogukanturhal/keystone-sdk/go/authoring"
)

func TestExistingUpFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"001_a.up.sql", "001_a.down.sql", "002_b.up.sql", "keystone.sum", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := existingUpFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"001_a.up.sql", "002_b.up.sql"}
	if len(got) != len(want) {
		t.Fatalf("existingUpFiles=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("existingUpFiles[%d]=%q want %q", i, got[i], want[i])
		}
	}

	// Missing dir is not an error (first migration).
	missing, err := existingUpFiles(filepath.Join(dir, "nope"))
	if err != nil || missing != nil {
		t.Errorf("missing dir: got %v, %v; want nil, nil", missing, err)
	}
}

func TestLintGate_DestructiveExemption(t *testing.T) {
	// A forward migration that drops a column — trips no-drop-column
	// (error severity).
	bundle := authoring.Bundle{
		Version: "002",
		UpFile:  "002_drop_legacy.up.sql",
		Up: "-- drop_legacy (up)\n" +
			"SET LOCAL statement_timeout = '30s';\n\n" +
			`ALTER TABLE "users" DROP COLUMN IF EXISTS "legacy";` + "\n",
	}

	// Without --allow-destructive, lint=error must block.
	blocked := &migrateDiffOpts{lint: "error", schema: "public", allowDestr: false, name: "drop_legacy"}
	if err := blocked.lintGate(context.Background(), bundle); err == nil {
		t.Error("expected lint=error to block a destructive migration without --allow-destructive")
	}

	// With --allow-destructive, the destructive-class finding is exempted.
	allowed := &migrateDiffOpts{lint: "error", schema: "public", allowDestr: true, name: "drop_legacy"}
	if err := allowed.lintGate(context.Background(), bundle); err != nil {
		t.Errorf("--allow-destructive should exempt destructive-class findings, got: %v", err)
	}

	// lint=off never blocks.
	off := &migrateDiffOpts{lint: "off", schema: "public", name: "drop_legacy"}
	if err := off.lintGate(context.Background(), bundle); err != nil {
		t.Errorf("lint=off should never block, got: %v", err)
	}

	// lint=warn never blocks.
	warn := &migrateDiffOpts{lint: "warn", schema: "public", name: "drop_legacy"}
	if err := warn.lintGate(context.Background(), bundle); err != nil {
		t.Errorf("lint=warn should never block, got: %v", err)
	}
}
