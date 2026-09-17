package controller

import (
	"testing"

	"github.com/dogukanturhal/keystone-sdk/go/drift"
)

// snapshotModelOnlyChange decides whether a moved hash is a real schema
// change or only the snapshot model growing a field. Getting it wrong in
// one direction spams the whole fleet with drift nobody can clear; getting
// it wrong in the other silently swallows a genuine finding. Neither
// failure is visible from the outside, so both directions are pinned here.

func tbl(name string, rlsEnabled bool, forced *bool) drift.TableShape {
	return drift.TableShape{
		Name:       name,
		Kind:       "BASE TABLE",
		Columns:    []drift.ColumnShape{{Name: "id", Ordinal: 1, DataType: "uuid", UDTName: "uuid"}},
		RLSEnabled: rlsEnabled,
		RLSForced:  forced,
	}
}

func boolPtr(b bool) *bool { return &b }

// legacySnap is what an inspector built before RLSForced existed wrote:
// the FORCE bit is simply absent.
func legacySnap(t *testing.T) (*drift.Snapshot, string) {
	t.Helper()
	s := &drift.Snapshot{
		Schema: "public",
		Tables: []drift.TableShape{
			tbl("accounts", true, nil),
			tbl("messages", true, nil),
		},
	}
	h, err := drift.Hash(s)
	if err != nil {
		t.Fatalf("hash legacy snapshot: %v", err)
	}
	return s, h
}

func TestSnapshotModelOnlyChange_RestatesWhenOnlyTheModelGrew(t *testing.T) {
	prior, baselineHash := legacySnap(t)

	// Same database, re-inspected by a build that records FORCE. The tables
	// were already forced; nothing about the schema moved.
	observed := &drift.Snapshot{
		Schema: "public",
		Tables: []drift.TableShape{
			tbl("accounts", true, boolPtr(true)),
			tbl("messages", true, boolPtr(true)),
		},
	}
	observedHash, err := drift.Hash(observed)
	if err != nil {
		t.Fatalf("hash observed: %v", err)
	}
	if observedHash == baselineHash {
		t.Fatal("precondition failed: adding the field must move the hash, " +
			"otherwise this test proves nothing")
	}

	got, err := snapshotModelOnlyChange(prior, observed, baselineHash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatal("want restate: the hash moved only because the snapshot model grew")
	}
}

func TestSnapshotModelOnlyChange_ReportsRealDriftAlongsideTheModelChange(t *testing.T) {
	prior, baselineHash := legacySnap(t)

	// The model grew AND a table lost RLS. The model change must not
	// provide cover for the real one.
	observed := &drift.Snapshot{
		Schema: "public",
		Tables: []drift.TableShape{
			tbl("accounts", false, boolPtr(false)),
			tbl("messages", true, boolPtr(true)),
		},
	}

	got, err := snapshotModelOnlyChange(prior, observed, baselineHash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatal("want ordinary drift handling: accounts lost RLS, which the " +
			"model-upgrade path would have swallowed")
	}
}

func TestSnapshotModelOnlyChange_IgnoresDroppedTableHiddenByTheUpgrade(t *testing.T) {
	prior, baselineHash := legacySnap(t)

	observed := &drift.Snapshot{
		Schema: "public",
		Tables: []drift.TableShape{
			tbl("accounts", true, boolPtr(true)),
		},
	}

	got, err := snapshotModelOnlyChange(prior, observed, baselineHash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatal("want ordinary drift handling: messages was dropped")
	}
}

func TestSnapshotModelOnlyChange_NotRepeatedOnceTheBaselineCarriesTheField(t *testing.T) {
	// After one restate the stored snapshot has the field, so the projection
	// is no longer meaningful and must not fire again — otherwise a later
	// genuine FORCE loss would be restated away as a model upgrade.
	prior := &drift.Snapshot{
		Schema: "public",
		Tables: []drift.TableShape{
			tbl("accounts", true, boolPtr(true)),
			tbl("messages", true, boolPtr(true)),
		},
	}
	baselineHash, err := drift.Hash(prior)
	if err != nil {
		t.Fatalf("hash prior: %v", err)
	}

	observed := &drift.Snapshot{
		Schema: "public",
		Tables: []drift.TableShape{
			tbl("accounts", true, boolPtr(false)), // FORCE lost — a real, critical finding
			tbl("messages", true, boolPtr(true)),
		},
	}

	got, err := snapshotModelOnlyChange(prior, observed, baselineHash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatal("want ordinary drift handling: a modern baseline is never a model upgrade")
	}
}

func TestSnapshotModelOnlyChange_NoPriorSnapshot(t *testing.T) {
	// ReadSnapshot failing leaves prior nil. There is nothing to project
	// against, so the caller must fall through rather than restate blindly.
	got, err := snapshotModelOnlyChange(nil, &drift.Snapshot{Schema: "public"}, "whatever")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Fatal("want ordinary drift handling when no prior snapshot is on record")
	}
}

func TestSnapshotModelOnlyChange_LeavesReceiversUnmodified(t *testing.T) {
	// WithoutRLSForce must not strip the field from the snapshot the caller
	// is about to persist as the new baseline.
	prior, baselineHash := legacySnap(t)
	observed := &drift.Snapshot{
		Schema: "public",
		Tables: []drift.TableShape{
			tbl("accounts", true, boolPtr(true)),
			tbl("messages", true, boolPtr(true)),
		},
	}
	before, err := drift.Hash(observed)
	if err != nil {
		t.Fatalf("hash observed: %v", err)
	}

	if _, err := snapshotModelOnlyChange(prior, observed, baselineHash); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	after, err := drift.Hash(observed)
	if err != nil {
		t.Fatalf("re-hash observed: %v", err)
	}
	if before != after {
		t.Fatalf("observed snapshot was mutated: %s -> %s", before, after)
	}
}
