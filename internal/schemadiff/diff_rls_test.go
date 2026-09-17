// SPDX-License-Identifier: AGPL-3.0-or-later

package schemadiff

import (
	"strings"
	"testing"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// The snapshot registry is sold as change-management evidence: "this is
// what state X looked like before we ran migration Y". Until row-level
// security was modelled it could not honour that for the one dimension
// that carries tenant isolation — `DROP POLICY tenant_isolation` and
// `ALTER TABLE … NO FORCE ROW LEVEL SECURITY` both left the captured
// structure byte-identical, so `snapshot diff` printed "no structural
// differences" across a change that opened every tenant's rows to
// every other tenant.
//
// These tests pin both halves of the fix: the changes are now seen,
// and where they cannot be seen the diff says so instead of reporting
// agreement.

func rlsTbl(name string, enabled, forced bool) keystonev1alpha1.SnapshotTable {
	t := tbl(name, col("id", "uuid", false), col("tenant_id", "uuid", false))
	t.RLSEnabled = enabled
	f := forced
	t.RLSForced = &f
	return t
}

func pol(table, name, using string) keystonev1alpha1.SnapshotPolicy {
	return keystonev1alpha1.SnapshotPolicy{
		Name: name, Table: table, Command: "ALL", Permissive: true,
		Roles: []string{"downstream-service_app"}, Using: using,
	}
}

func rlsSnap(tables []keystonev1alpha1.SnapshotTable, policies []keystonev1alpha1.SnapshotPolicy) *keystonev1alpha1.StructuralSnapshot {
	s := snap(tables, nil, nil)
	s.Policies = policies
	return s
}

func findChangeOn(changes []Change, kind ChangeKind, table, object string) *Change {
	for i := range changes {
		if changes[i].Kind == kind && changes[i].Table == table && changes[i].Object == object {
			return &changes[i]
		}
	}
	return nil
}

func kindsOf(changes []Change) []ChangeKind {
	out := make([]ChangeKind, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Kind)
	}
	return out
}

// -- The regression --------------------------------------------------

// TestDiff_RLSForcedFlipIsReported is the case that motivated modelling
// RLS at all. ENABLE without FORCE exempts the table's owner, and the
// application connects as the owner — so NO FORCE silently disables
// every policy for exactly the connection they exist to constrain,
// while leaving the policies themselves in place and visible.
func TestDiff_RLSForcedFlipIsReported(t *testing.T) {
	from := rlsSnap([]keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}, nil)
	to := rlsSnap([]keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, false)}, nil)

	rep := Diff(from, to)
	if len(rep.Caveats) != 0 {
		t.Fatalf("both snapshots are current-model; unexpected caveats %v", rep.Caveats)
	}
	c := findChange(rep.Changes, KindRLSForcedChanged, "messages")
	if c == nil {
		t.Fatalf("NO FORCE ROW LEVEL SECURITY not reported; got %v", kindsOf(rep.Changes))
	}
	if c.Before != "FORCED" {
		t.Errorf("Before = %q, want FORCED", c.Before)
	}
	// The consequence, not just the bit — this line is where an
	// operator is most likely to notice what NO FORCE actually means.
	if !strings.Contains(c.After, "owner exempt") {
		t.Errorf("After = %q, want it to name the owner exemption", c.After)
	}
}

func TestDiff_RLSEnabledFlipIsReported(t *testing.T) {
	from := rlsSnap([]keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}, nil)
	to := rlsSnap([]keystonev1alpha1.SnapshotTable{rlsTbl("messages", false, true)}, nil)

	c := findChange(Diff(from, to).Changes, KindRLSEnabledChanged, "messages")
	if c == nil {
		t.Fatal("DISABLE ROW LEVEL SECURITY not reported")
	}
	if c.Before != "ENABLED" || c.After != "DISABLED" {
		t.Errorf("got %s → %s, want ENABLED → DISABLED", c.Before, c.After)
	}
}

// -- Policies --------------------------------------------------------

func TestDiff_PolicyRemovedIsReported(t *testing.T) {
	tables := []keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}
	from := rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{
		pol("messages", "tenant_isolation", "tenant_id = current_tenant()"),
	})
	to := rlsSnap(tables, nil)

	c := findChange(Diff(from, to).Changes, KindPolicyRemoved, "tenant_isolation")
	if c == nil {
		t.Fatal("DROP POLICY not reported")
	}
	if !strings.Contains(c.Before, "current_tenant()") {
		t.Errorf("Before = %q, want the dropped policy's USING expression", c.Before)
	}
}

func TestDiff_PolicyAddedIsReported(t *testing.T) {
	tables := []keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}
	from := rlsSnap(tables, nil)
	to := rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{
		pol("messages", "tenant_isolation", "tenant_id = current_tenant()"),
	})

	if c := findChange(Diff(from, to).Changes, KindPolicyAdded, "tenant_isolation"); c == nil {
		t.Fatal("CREATE POLICY not reported")
	}
}

// TestDiff_PolicyPredicateChangeIsReported — a policy that keeps its
// name but loosens its predicate is the subtlest way to lose isolation,
// and the one a name-only comparison would miss entirely.
func TestDiff_PolicyPredicateChangeIsReported(t *testing.T) {
	tables := []keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}
	from := rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{
		pol("messages", "tenant_isolation", "tenant_id = current_tenant()"),
	})
	to := rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{
		pol("messages", "tenant_isolation", "true"),
	})

	c := findChange(Diff(from, to).Changes, KindPolicyChanged, "tenant_isolation")
	if c == nil {
		t.Fatal("loosened USING predicate not reported")
	}
	if !strings.Contains(c.Before, "current_tenant()") || !strings.Contains(c.After, "true") {
		t.Errorf("got %q → %q, want the old and new predicates", c.Before, c.After)
	}
}

// TestDiff_PermissiveToRestrictiveIsReported — PERMISSIVE policies
// OR together, RESTRICTIVE ones AND together. Flipping the flag inverts
// how the whole policy set combines without touching a predicate.
func TestDiff_PermissiveToRestrictiveIsReported(t *testing.T) {
	tables := []keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}
	p := pol("messages", "tenant_isolation", "tenant_id = current_tenant()")
	restrictive := p
	restrictive.Permissive = false

	from := rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{p})
	to := rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{restrictive})

	c := findChange(Diff(from, to).Changes, KindPolicyChanged, "tenant_isolation")
	if c == nil {
		t.Fatal("PERMISSIVE → RESTRICTIVE not reported")
	}
	if !strings.HasPrefix(c.Before, "PERMISSIVE") || !strings.HasPrefix(c.After, "RESTRICTIVE") {
		t.Errorf("got %q → %q, want the permissive flag to lead", c.Before, c.After)
	}
}

// TestDiff_PolicyRoleReorderIsNotAChange — PostgreSQL does not promise
// a stable order for a policy's role list. Reporting a reorder as a
// change would make every diff noisy and train operators to skim past
// exactly the lines that matter.
func TestDiff_PolicyRoleReorderIsNotAChange(t *testing.T) {
	tables := []keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}
	a := pol("messages", "tenant_isolation", "tenant_id = current_tenant()")
	a.Roles = []string{"downstream-service_app", "downstream-service_ro"}
	b := a
	b.Roles = []string{"downstream-service_ro", "downstream-service_app"}

	rep := Diff(rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{a}),
		rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{b}))
	if len(rep.Changes) != 0 {
		t.Errorf("role reorder reported as a change: %v", rep.Changes)
	}
}

func TestDiff_PolicyRoleChangeIsReported(t *testing.T) {
	tables := []keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}
	a := pol("messages", "tenant_isolation", "tenant_id = current_tenant()")
	b := a
	b.Roles = []string{"public"}

	if c := findChange(Diff(rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{a}),
		rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{b})).Changes,
		KindPolicyChanged, "tenant_isolation"); c == nil {
		t.Fatal("policy widened to PUBLIC not reported")
	}
}

// TestDiff_PolicyNamesAreScopedToTable — policy names are unique per
// table, not per schema. `tenant_isolation` on two tables is two
// policies; keying the comparison on the bare name would collapse them
// and report a dropped policy as unchanged.
func TestDiff_PolicyNamesAreScopedToTable(t *testing.T) {
	tables := []keystonev1alpha1.SnapshotTable{
		rlsTbl("messages", true, true), rlsTbl("mailboxes", true, true),
	}
	from := rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{
		pol("messages", "tenant_isolation", "tenant_id = current_tenant()"),
		pol("mailboxes", "tenant_isolation", "tenant_id = current_tenant()"),
	})
	to := rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{
		pol("messages", "tenant_isolation", "tenant_id = current_tenant()"),
	})

	changes := Diff(from, to).Changes
	if c := findChangeOn(changes, KindPolicyRemoved, "mailboxes", "tenant_isolation"); c == nil {
		t.Fatalf("removal on mailboxes not reported; got %v", changes)
	}
	if c := findChangeOn(changes, KindPolicyRemoved, "messages", "tenant_isolation"); c != nil {
		t.Error("messages.tenant_isolation reported removed; it is untouched")
	}
	if len(changes) != 1 {
		t.Errorf("want exactly 1 change, got %d: %v", len(changes), changes)
	}
}

// -- Old-model snapshots ---------------------------------------------

// TestDiff_PreRLSModelIsCaveatedNotGuessed — snapshots are immutable
// and retained for at least a year, so the registry permanently holds
// captures from before RLS was modelled. Reading their absent policy
// list as "this schema had no policies" would report every existing
// policy as newly added, and would report a schema that had lost a
// policy as having gained several. The honest answer is that the
// question cannot be asked of this pair.
func TestDiff_PreRLSModelIsCaveatedNotGuessed(t *testing.T) {
	tables := []keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}

	old := rlsSnap(tables, nil)
	old.ModelVersion = 0
	for i := range old.Tables {
		old.Tables[i].RLSForced = nil
		old.Tables[i].RLSEnabled = false
	}

	current := rlsSnap(tables, []keystonev1alpha1.SnapshotPolicy{
		pol("messages", "tenant_isolation", "tenant_id = current_tenant()"),
	})

	rep := Diff(old, current)
	if len(rep.Caveats) == 0 {
		t.Fatal("no caveat: the diff claims to have compared RLS across a snapshot that has none")
	}
	for _, c := range rep.Changes {
		switch c.Kind {
		case KindPolicyAdded, KindPolicyRemoved, KindPolicyChanged,
			KindRLSEnabledChanged, KindRLSForcedChanged:
			t.Errorf("fabricated RLS change from an old-model snapshot: %s", c)
		}
	}
	if rep.FromModelVersion != 0 || rep.ToModelVersion != keystonev1alpha1.StructuralSnapshotModelVersion {
		t.Errorf("model versions %d → %d, want 0 → %d",
			rep.FromModelVersion, rep.ToModelVersion,
			keystonev1alpha1.StructuralSnapshotModelVersion)
	}
}

// TestDiff_PreRLSStillComparesStructure — the caveat is scoped to RLS.
// Everything the old snapshot does record must still be compared, or
// the upgrade would blind the tool to ordinary schema changes too.
func TestDiff_PreRLSStillComparesStructure(t *testing.T) {
	old := snap([]keystonev1alpha1.SnapshotTable{tbl("messages", col("id", "uuid", false))}, nil, nil)
	old.ModelVersion = 0
	current := snap([]keystonev1alpha1.SnapshotTable{
		tbl("messages", col("id", "uuid", false)), tbl("mailboxes", col("id", "uuid", false)),
	}, nil, nil)

	rep := Diff(old, current)
	if c := findChange(rep.Changes, KindTableAdded, "mailboxes"); c == nil {
		t.Fatalf("structural change lost behind the RLS caveat; got %v", kindsOf(rep.Changes))
	}
	if len(rep.Caveats) == 0 {
		t.Error("expected the RLS caveat to still be present")
	}
}

// TestDiff_IdenticalCurrentModelHasNoCaveat guards the other direction:
// the caveat must not fire for the ordinary case, or it becomes noise
// that operators learn to ignore.
func TestDiff_IdenticalCurrentModelHasNoCaveat(t *testing.T) {
	s := rlsSnap([]keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)},
		[]keystonev1alpha1.SnapshotPolicy{
			pol("messages", "tenant_isolation", "tenant_id = current_tenant()"),
		})
	rep := Diff(s, s)
	if len(rep.Changes) != 0 || len(rep.Caveats) != 0 {
		t.Errorf("identical snapshots reported %v / %v", rep.Changes, rep.Caveats)
	}
}

// TestDiff_NewTableDoesNotAlsoReportRLS — TableAdded already says the
// whole table is new. Emitting RLSForcedChanged beside it would imply
// someone changed a setting on an existing table.
func TestDiff_NewTableDoesNotAlsoReportRLS(t *testing.T) {
	from := rlsSnap(nil, nil)
	to := rlsSnap([]keystonev1alpha1.SnapshotTable{rlsTbl("messages", true, true)}, nil)

	changes := Diff(from, to).Changes
	if c := findChange(changes, KindTableAdded, "messages"); c == nil {
		t.Fatal("TableAdded missing")
	}
	for _, c := range changes {
		if c.Kind == KindRLSEnabledChanged || c.Kind == KindRLSForcedChanged {
			t.Errorf("RLS change reported for a brand-new table: %s", c)
		}
	}
}
