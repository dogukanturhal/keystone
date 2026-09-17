// SPDX-License-Identifier: AGPL-3.0-or-later

package schemadiff

import (
	"testing"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// The capture step dropped every object below for the whole life of the
// type: the inspector read enums, extensions, sequences, functions,
// triggers, materialized views and view bodies, and
// structuralSnapshotFromDrift copied tables, indexes and constraints.
// A rewritten trigger function, a view that stopped filtering by
// tenant, and a dropped materialized view all produced a byte-identical
// status.structure — so `snapshot diff` printed "no structural
// differences" across each of them.

// objSnap builds a current-model snapshot. Tests for older models must
// set ModelVersion back down explicitly.
func objSnap(mutate func(*keystonev1alpha1.StructuralSnapshot)) *keystonev1alpha1.StructuralSnapshot {
	s := &keystonev1alpha1.StructuralSnapshot{
		ModelVersion: keystonev1alpha1.StructuralSnapshotModelVersion,
		Schema:       "public",
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func TestDiff_ViewDefinitionRewriteIsReported(t *testing.T) {
	tbl := func(def string) keystonev1alpha1.SnapshotTable {
		return keystonev1alpha1.SnapshotTable{
			Name: "my_messages", Kind: "VIEW",
			Columns: []keystonev1alpha1.SnapshotColumn{
				{Name: "id", Ordinal: 1, DataType: "uuid", UDTName: "uuid"},
			},
			ViewDefinition: def,
		}
	}
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Tables = []keystonev1alpha1.SnapshotTable{
			tbl("SELECT id FROM messages WHERE tenant_id = current_tenant()"),
		}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Tables = []keystonev1alpha1.SnapshotTable{tbl("SELECT id FROM messages")}
	})

	c := findChangeOn(Diff(from, to).Changes, KindViewDefinitionChanged, "my_messages", "my_messages")
	if c == nil {
		t.Fatal("a view that stopped filtering by tenant was reported as no change; " +
			"the column list is identical either way")
	}
	if c.After != "SELECT id FROM messages" {
		t.Errorf("After = %q", c.After)
	}
}

// A view's columns are unchanged by a WHERE rewrite, so nothing else in
// the report can stand in for ViewDefinitionChanged.
func TestDiff_ViewRewriteIsTheOnlySignal(t *testing.T) {
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Tables = []keystonev1alpha1.SnapshotTable{{
			Name: "v", Kind: "VIEW",
			ViewDefinition: "SELECT 1 WHERE tenant_id = current_tenant()",
		}}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Tables = []keystonev1alpha1.SnapshotTable{{
			Name: "v", Kind: "VIEW", ViewDefinition: "SELECT 1",
		}}
	})
	kinds := kindsOf(Diff(from, to).Changes)
	if len(kinds) != 1 || kinds[0] != KindViewDefinitionChanged {
		t.Errorf("kinds = %v, want exactly [ViewDefinitionChanged]", kinds)
	}
}

// A base table replaced by a view of the same name is neither an add
// nor a remove, and its column list is typically identical — that being
// the point of the substitution.
func TestDiff_TableToViewIsReported(t *testing.T) {
	cols := []keystonev1alpha1.SnapshotColumn{
		{Name: "id", Ordinal: 1, DataType: "uuid", UDTName: "uuid"},
	}
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Tables = []keystonev1alpha1.SnapshotTable{
			{Name: "messages", Kind: "BASE TABLE", Columns: cols},
		}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Tables = []keystonev1alpha1.SnapshotTable{
			{Name: "messages", Kind: "VIEW", Columns: cols},
		}
	})

	c := findChangeOn(Diff(from, to).Changes, KindTableKindChanged, "messages", "messages")
	if c == nil {
		t.Fatal("a base table replaced by a same-named view was reported as no change")
	}
	if c.Before != "BASE TABLE" || c.After != "VIEW" {
		t.Errorf("Before/After = %q/%q, want BASE TABLE/VIEW", c.Before, c.After)
	}
}

// A kind flip must not also be reported as a view-body change: there is
// no old body to compare against, and printing one implies a rewrite
// that did not happen.
func TestDiff_KindFlipIsNotAlsoAViewRewrite(t *testing.T) {
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Tables = []keystonev1alpha1.SnapshotTable{{Name: "t", Kind: "BASE TABLE"}}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Tables = []keystonev1alpha1.SnapshotTable{
			{Name: "t", Kind: "VIEW", ViewDefinition: "SELECT 1"},
		}
	})
	for _, c := range Diff(from, to).Changes {
		if c.Kind == KindViewDefinitionChanged {
			t.Errorf("kind flip also reported %s", c.Kind)
		}
	}
}

func TestDiff_EnumLabelAddedIsReported(t *testing.T) {
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Enums = []keystonev1alpha1.SnapshotEnum{
			{Name: "mail_state", Labels: []string{"queued", "sent"}},
		}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Enums = []keystonev1alpha1.SnapshotEnum{
			{Name: "mail_state", Labels: []string{"queued", "sent", "bounced"}},
		}
	})

	c := findChangeOn(Diff(from, to).Changes, KindEnumChanged, "", "mail_state")
	if c == nil {
		t.Fatal("enum label addition not reported")
	}
	if c.After != "(queued, sent, bounced)" {
		t.Errorf("After = %q", c.After)
	}
}

// Enum label order is PostgreSQL's SORT order, not declaration order —
// it is what `<` and ORDER BY compare on. Reordering is a real change,
// unlike reordering a policy's role list.
func TestDiff_EnumLabelReorderIsAChange(t *testing.T) {
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Enums = []keystonev1alpha1.SnapshotEnum{
			{Name: "sev", Labels: []string{"low", "high"}},
		}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Enums = []keystonev1alpha1.SnapshotEnum{
			{Name: "sev", Labels: []string{"high", "low"}},
		}
	})
	if findChangeOn(Diff(from, to).Changes, KindEnumChanged, "", "sev") == nil {
		t.Error("reordered enum labels reported as unchanged; every comparison " +
			"on that type just reversed")
	}
}

func TestDiff_EnumAddedAndRemoved(t *testing.T) {
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Enums = []keystonev1alpha1.SnapshotEnum{{Name: "gone", Labels: []string{"a"}}}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Enums = []keystonev1alpha1.SnapshotEnum{{Name: "fresh", Labels: []string{"b"}}}
	})
	changes := Diff(from, to).Changes
	if findChangeOn(changes, KindEnumRemoved, "", "gone") == nil {
		t.Error("dropped enum not reported")
	}
	if findChangeOn(changes, KindEnumAdded, "", "fresh") == nil {
		t.Error("new enum not reported")
	}
}

func TestDiff_ExtensionInstalledIsReported(t *testing.T) {
	from := objSnap(nil)
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Extensions = []keystonev1alpha1.SnapshotExtension{
			{Name: "pgcrypto", Schema: "public"},
		}
	})
	if findChangeOn(Diff(from, to).Changes, KindExtensionAdded, "", "pgcrypto") == nil {
		t.Fatal("newly installed extension not reported")
	}
}

func TestDiff_ExtensionSchemaMoveIsReported(t *testing.T) {
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Extensions = []keystonev1alpha1.SnapshotExtension{
			{Name: "pgcrypto", Schema: "public"},
		}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Extensions = []keystonev1alpha1.SnapshotExtension{
			{Name: "pgcrypto", Schema: "ext"},
		}
	})
	c := findChangeOn(Diff(from, to).Changes, KindExtensionChanged, "", "pgcrypto")
	if c == nil {
		t.Fatal("extension moved to another schema not reported")
	}
	if c.Before != "schema=public" || c.After != "schema=ext" {
		t.Errorf("Before/After = %q/%q", c.Before, c.After)
	}
}

func TestDiff_SequenceBoundChangeIsReported(t *testing.T) {
	seq := func(dt string, max int64) keystonev1alpha1.SnapshotSequence {
		return keystonev1alpha1.SnapshotSequence{
			Name: "messages_id_seq", DataType: dt,
			IncrementBy: 1, MinValue: 1, MaxValue: max, StartValue: 1,
		}
	}
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Sequences = []keystonev1alpha1.SnapshotSequence{seq("bigint", 9223372036854775807)}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Sequences = []keystonev1alpha1.SnapshotSequence{seq("integer", 2147483647)}
	})

	c := findChangeOn(Diff(from, to).Changes, KindSequenceChanged, "", "messages_id_seq")
	if c == nil {
		t.Fatal("a sequence narrowed from bigint to integer was reported as no change")
	}
	if c.Before == c.After {
		t.Error("Before and After render identically; the change is invisible in the report")
	}
}

// A function body is where a security-relevant change hides: the
// current_tenant() a policy's USING clause calls can be rewritten
// without touching its signature.
func TestDiff_FunctionBodyRewriteIsReported(t *testing.T) {
	fn := func(body string) keystonev1alpha1.SnapshotFunction {
		return keystonev1alpha1.SnapshotFunction{
			Name: "current_tenant", Args: "", Returns: "uuid",
			Language: "sql", Definition: body,
		}
	}
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Functions = []keystonev1alpha1.SnapshotFunction{
			fn("SELECT current_setting('app.tenant_id')::uuid"),
		}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Functions = []keystonev1alpha1.SnapshotFunction{
			fn("SELECT '00000000-0000-0000-0000-000000000000'::uuid"),
		}
	})

	changes := Diff(from, to).Changes
	var found *Change
	for i := range changes {
		if changes[i].Kind == KindFunctionChanged {
			found = &changes[i]
		}
	}
	if found == nil {
		t.Fatal("a rewritten function body was reported as no change; every policy " +
			"calling it now matches every row")
	}
	if found.Object != "current_tenant()" {
		t.Errorf("Object = %q, want current_tenant()", found.Object)
	}
}

// Functions are overloadable. Keying on the bare name collapses an
// overload set and reports a dropped overload as unchanged — the same
// failure mode as keying a policy on its bare name.
func TestDiff_FunctionOverloadsAreDistinct(t *testing.T) {
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Functions = []keystonev1alpha1.SnapshotFunction{
			{Name: "audit", Args: "text", Returns: "void", Language: "plpgsql", Definition: "a"},
			{Name: "audit", Args: "text, uuid", Returns: "void", Language: "plpgsql", Definition: "b"},
		}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Functions = []keystonev1alpha1.SnapshotFunction{
			{Name: "audit", Args: "text", Returns: "void", Language: "plpgsql", Definition: "a"},
		}
	})

	changes := Diff(from, to).Changes
	var removed []string
	for _, c := range changes {
		if c.Kind == KindFunctionRemoved {
			removed = append(removed, c.Object)
		}
	}
	if len(removed) != 1 || removed[0] != "audit(text, uuid)" {
		t.Fatalf("removed = %v, want exactly [audit(text, uuid)]", removed)
	}
	for _, c := range changes {
		if c.Kind == KindFunctionChanged {
			t.Errorf("surviving overload reported as changed: %+v", c)
		}
	}
}

func TestDiff_TriggerRemovedIsReported(t *testing.T) {
	trg := keystonev1alpha1.SnapshotTrigger{
		Name: "audit_messages", Table: "messages", Timing: "AFTER",
		Events: []string{"INSERT", "UPDATE"}, ForEachRow: true, Function: "audit_fn",
	}
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Triggers = []keystonev1alpha1.SnapshotTrigger{trg}
	})
	to := objSnap(nil)

	c := findChangeOn(Diff(from, to).Changes, KindTriggerRemoved, "messages", "audit_messages")
	if c == nil {
		t.Fatal("a dropped audit trigger was reported as no change")
	}
	if c.Object != "audit_messages" {
		t.Errorf("Object = %q", c.Object)
	}
}

func TestDiff_TriggerRowToStatementIsReported(t *testing.T) {
	trg := func(row bool) keystonev1alpha1.SnapshotTrigger {
		return keystonev1alpha1.SnapshotTrigger{
			Name: "t", Table: "messages", Timing: "AFTER",
			Events: []string{"INSERT"}, ForEachRow: row, Function: "f",
		}
	}
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Triggers = []keystonev1alpha1.SnapshotTrigger{trg(true)}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Triggers = []keystonev1alpha1.SnapshotTrigger{trg(false)}
	})

	c := findChangeOn(Diff(from, to).Changes, KindTriggerChanged, "messages", "t")
	if c == nil {
		t.Fatal("row trigger downgraded to a statement trigger not reported; " +
			"it now fires once per statement instead of once per row")
	}
	if c.Before != "AFTER INSERT FOR EACH ROW EXECUTE f" {
		t.Errorf("Before = %q", c.Before)
	}
	if c.After != "AFTER INSERT FOR EACH STATEMENT EXECUTE f" {
		t.Errorf("After = %q", c.After)
	}
}

// Trigger names are unique per table, not per schema.
func TestDiff_TriggerNamesAreScopedToTable(t *testing.T) {
	trg := func(table, fn string) keystonev1alpha1.SnapshotTrigger {
		return keystonev1alpha1.SnapshotTrigger{
			Name: "audit", Table: table, Timing: "AFTER",
			Events: []string{"INSERT"}, ForEachRow: true, Function: fn,
		}
	}
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Triggers = []keystonev1alpha1.SnapshotTrigger{
			trg("messages", "f"), trg("mailboxes", "f"),
		}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Triggers = []keystonev1alpha1.SnapshotTrigger{trg("messages", "f")}
	})

	c := findChangeOn(Diff(from, to).Changes, KindTriggerRemoved, "mailboxes", "audit")
	if c == nil {
		t.Fatal("a trigger dropped from one table was hidden by the same-named " +
			"trigger on another")
	}
}

func TestDiff_TriggerWhenClauseChangeIsReported(t *testing.T) {
	trg := func(when string) keystonev1alpha1.SnapshotTrigger {
		return keystonev1alpha1.SnapshotTrigger{
			Name: "t", Table: "messages", Timing: "BEFORE",
			Events: []string{"UPDATE"}, ForEachRow: true, Function: "f", When: when,
		}
	}
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Triggers = []keystonev1alpha1.SnapshotTrigger{trg("OLD.tenant_id <> NEW.tenant_id")}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Triggers = []keystonev1alpha1.SnapshotTrigger{trg("")}
	})
	if findChangeOn(Diff(from, to).Changes, KindTriggerChanged, "messages", "t") == nil {
		t.Error("a dropped WHEN clause was reported as no change")
	}
}

func TestDiff_MaterializedViewChangesAreReported(t *testing.T) {
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.MaterializedViews = []keystonev1alpha1.SnapshotMaterializedView{
			{Name: "mv_stats", Definition: "SELECT count(*) FROM messages WHERE tenant_id = current_tenant()"},
			{Name: "mv_gone", Definition: "SELECT 1"},
		}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.MaterializedViews = []keystonev1alpha1.SnapshotMaterializedView{
			{Name: "mv_stats", Definition: "SELECT count(*) FROM messages"},
		}
	})

	changes := Diff(from, to).Changes
	if findChangeOn(changes, KindMaterializedViewChanged, "mv_stats", "mv_stats") == nil {
		t.Error("a rewritten materialized view was reported as no change")
	}
	if findChangeOn(changes, KindMaterializedViewRemoved, "mv_gone", "mv_gone") == nil {
		t.Error("a dropped materialized view was reported as no change")
	}
}

// The whole point of the model version: an older snapshot records none
// of these dimensions, and reading its empty lists as observations would
// report every enum, sequence, function and trigger in the schema as
// newly created.
func TestDiff_PreObjectsModelIsCaveatedNotGuessed(t *testing.T) {
	old := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.ModelVersion = rlsModelVersion // RLS was modelled; the rest was not.
		s.Tables = []keystonev1alpha1.SnapshotTable{{Name: "messages", Kind: "BASE TABLE"}}
	})
	current := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Tables = []keystonev1alpha1.SnapshotTable{{Name: "messages", Kind: "BASE TABLE"}}
		s.Enums = []keystonev1alpha1.SnapshotEnum{{Name: "mail_state", Labels: []string{"queued"}}}
		s.Sequences = []keystonev1alpha1.SnapshotSequence{{Name: "s", DataType: "bigint"}}
		s.Functions = []keystonev1alpha1.SnapshotFunction{{Name: "f", Returns: "void"}}
		s.Triggers = []keystonev1alpha1.SnapshotTrigger{
			{Name: "t", Table: "messages", Timing: "AFTER", Function: "f"},
		}
		s.MaterializedViews = []keystonev1alpha1.SnapshotMaterializedView{{Name: "mv"}}
	})

	report := Diff(old, current)
	for _, c := range report.Changes {
		switch c.Kind {
		case KindEnumAdded, KindSequenceAdded, KindFunctionAdded,
			KindTriggerAdded, KindMaterializedViewAdded, KindExtensionAdded,
			KindViewDefinitionChanged:
			t.Errorf("guessed %s on a snapshot that never recorded that dimension: %+v",
				c.Kind, c)
		}
	}
	if len(report.Caveats) == 0 {
		t.Fatal("no caveat: an empty change list here reads as 'nothing changed' " +
			"when the truth is 'most of the schema was not compared'")
	}
	if report.FromModelVersion != rlsModelVersion ||
		report.ToModelVersion != keystonev1alpha1.StructuralSnapshotModelVersion {
		t.Errorf("model versions = %d/%d", report.FromModelVersion, report.ToModelVersion)
	}
}

// The caveat must not swallow the dimensions that ARE comparable at the
// older model.
func TestDiff_PreObjectsModelStillComparesRLS(t *testing.T) {
	tbl := func(forced bool) keystonev1alpha1.SnapshotTable {
		return keystonev1alpha1.SnapshotTable{
			Name: "messages", Kind: "BASE TABLE", RLSEnabled: true, RLSForced: &forced,
		}
	}
	from := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.ModelVersion = rlsModelVersion
		s.Tables = []keystonev1alpha1.SnapshotTable{tbl(true)}
	})
	to := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.ModelVersion = rlsModelVersion
		s.Tables = []keystonev1alpha1.SnapshotTable{tbl(false)}
	})

	report := Diff(from, to)
	if findChangeOn(report.Changes, KindRLSForcedChanged, "messages", "messages") == nil {
		t.Error("RLS FORCE flip lost; it is recorded at model 1 and must still compare")
	}
	if len(report.Caveats) != 1 {
		t.Errorf("want exactly the objects caveat, got %d: %v",
			len(report.Caveats), report.Caveats)
	}
}

func TestDiff_IdenticalObjectsHaveNoChangesOrCaveats(t *testing.T) {
	build := func() *keystonev1alpha1.StructuralSnapshot {
		return objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
			s.Tables = []keystonev1alpha1.SnapshotTable{
				{Name: "v", Kind: "VIEW", ViewDefinition: "SELECT 1"},
			}
			s.Enums = []keystonev1alpha1.SnapshotEnum{{Name: "e", Labels: []string{"a", "b"}}}
			s.Extensions = []keystonev1alpha1.SnapshotExtension{{Name: "pgcrypto", Schema: "public"}}
			s.Sequences = []keystonev1alpha1.SnapshotSequence{
				{Name: "s", DataType: "bigint", IncrementBy: 1, MinValue: 1, MaxValue: 99, StartValue: 1},
			}
			s.Functions = []keystonev1alpha1.SnapshotFunction{
				{Name: "f", Args: "text", Returns: "void", Language: "sql", Definition: "SELECT 1"},
			}
			s.Triggers = []keystonev1alpha1.SnapshotTrigger{
				{Name: "t", Table: "v", Timing: "AFTER", Events: []string{"INSERT"},
					ForEachRow: true, Function: "f"},
			}
			s.MaterializedViews = []keystonev1alpha1.SnapshotMaterializedView{
				{Name: "mv", Definition: "SELECT 2"},
			}
		})
	}
	report := Diff(build(), build())
	if len(report.Changes) != 0 {
		t.Errorf("identical snapshots reported %d change(s): %+v",
			len(report.Changes), report.Changes)
	}
	if len(report.Caveats) != 0 {
		t.Errorf("caveats on two current-model snapshots: %v", report.Caveats)
	}
}

// A nil structure on one side is a snapshot whose capture never
// populated it. The model-version gate says nothing about the pointer,
// so the accessors have to be nil-safe.
func TestDiff_NilStructureDoesNotPanic(t *testing.T) {
	current := objSnap(func(s *keystonev1alpha1.StructuralSnapshot) {
		s.Enums = []keystonev1alpha1.SnapshotEnum{{Name: "e", Labels: []string{"a"}}}
		s.Triggers = []keystonev1alpha1.SnapshotTrigger{
			{Name: "t", Table: "x", Timing: "AFTER", Function: "f"},
		}
	})
	if got := Diff(nil, current); len(got.Caveats) == 0 {
		t.Error("a nil side has model version 0 and must be caveated")
	}
	if got := Diff(current, nil); len(got.Caveats) == 0 {
		t.Error("a nil side has model version 0 and must be caveated")
	}
	Diff(nil, nil)
}
