// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/drift"
)

// structuralSnapshotFromDrift is the only path by which an observation
// reaches the snapshot registry, so anything it drops is invisible to
// every downstream consumer — `keystonectl snapshot diff`, the
// change-management evidence trail, and any future restore tooling.
// Row-level security was dropped here for the whole life of the type:
// the inspector read RLSEnabled, RLSForced and the policy list, and
// this function copied tables, indexes and constraints only.

func driftSnapshotWithRLS() *drift.Snapshot {
	return &drift.Snapshot{
		Schema: "public",
		Tables: []drift.TableShape{{
			Name: "messages", Kind: "BASE TABLE",
			Columns: []drift.ColumnShape{
				{Name: "id", Ordinal: 1, DataType: "uuid", UDTName: "uuid"},
				{Name: "tenant_id", Ordinal: 2, DataType: "uuid", UDTName: "uuid"},
			},
			RLSEnabled: true,
			RLSForced:  ptrBool(true),
		}},
		Policies: []drift.PolicyShape{{
			Name: "tenant_isolation", Table: "messages", Command: "ALL",
			Permissive: true,
			Roles:      []string{"downstream-service_app"},
			Using:      "tenant_id = current_tenant()",
			WithCheck:  "tenant_id = current_tenant()",
		}},
	}
}

func TestStructuralSnapshotFromDrift_CarriesRLS(t *testing.T) {
	out := structuralSnapshotFromDrift(driftSnapshotWithRLS())
	if out == nil {
		t.Fatal("nil structure")
	}

	if out.ModelVersion != keystonev1alpha1.StructuralSnapshotModelVersion {
		t.Errorf("ModelVersion = %d, want %d — without it every consumer has to guess "+
			"whether an empty policy list is an observation",
			out.ModelVersion, keystonev1alpha1.StructuralSnapshotModelVersion)
	}

	if len(out.Tables) != 1 {
		t.Fatalf("want 1 table, got %d", len(out.Tables))
	}
	tbl := out.Tables[0]
	if !tbl.RLSEnabled {
		t.Error("RLSEnabled lost in translation")
	}
	if tbl.RLSForced == nil {
		t.Fatal("RLSForced lost in translation; NO FORCE would be undetectable")
	}
	if !*tbl.RLSForced {
		t.Error("RLSForced = false, want true")
	}

	if len(out.Policies) != 1 {
		t.Fatalf("want 1 policy, got %d", len(out.Policies))
	}
	p := out.Policies[0]
	want := keystonev1alpha1.SnapshotPolicy{
		Name: "tenant_isolation", Table: "messages", Command: "ALL",
		Permissive: true,
		Roles:      []string{"downstream-service_app"},
		Using:      "tenant_id = current_tenant()",
		WithCheck:  "tenant_id = current_tenant()",
	}
	if p.Name != want.Name || p.Table != want.Table || p.Command != want.Command ||
		p.Permissive != want.Permissive || p.Using != want.Using || p.WithCheck != want.WithCheck {
		t.Errorf("policy = %+v, want %+v", p, want)
	}
	if len(p.Roles) != 1 || p.Roles[0] != "downstream-service_app" {
		t.Errorf("Roles = %v, want [downstream-service_app]", p.Roles)
	}
}

// TestStructuralSnapshotFromDrift_NoAliasing — the drift snapshot is
// still live after this call (the reconciler hashes it and renders the
// ERD from it). A captured SchemaSnapshot is immutable by contract, so
// sharing backing memory with a structure that is still being read
// would make that contract depend on nobody downstream writing to it.
func TestStructuralSnapshotFromDrift_NoAliasing(t *testing.T) {
	src := driftSnapshotWithRLS()
	out := structuralSnapshotFromDrift(src)

	*src.Tables[0].RLSForced = false
	src.Policies[0].Roles[0] = "public"

	if out.Tables[0].RLSForced == nil || !*out.Tables[0].RLSForced {
		t.Error("RLSForced aliases the drift snapshot; a later write reached the captured CR")
	}
	if out.Policies[0].Roles[0] != "downstream-service_app" {
		t.Errorf("policy Roles alias the drift snapshot; got %q", out.Policies[0].Roles[0])
	}
}

// TestStructuralSnapshotFromDrift_PreservesNilForced — the SDK leaves
// RLSForced nil for snapshots decoded from a pre-RLS baseline. That nil
// must survive rather than becoming false, which would assert "not
// forced" about a table nobody looked at.
func TestStructuralSnapshotFromDrift_PreservesNilForced(t *testing.T) {
	src := driftSnapshotWithRLS()
	src.Tables[0].RLSForced = nil

	out := structuralSnapshotFromDrift(src)
	if out.Tables[0].RLSForced != nil {
		t.Errorf("nil RLSForced became %v; absence of an observation is not an observation",
			*out.Tables[0].RLSForced)
	}
}

// driftSnapshotFull adds the dimensions the inspector reads beyond
// tables and RLS. All of them were dropped by the capture step for the
// whole life of the type.
func driftSnapshotFull() *drift.Snapshot {
	s := driftSnapshotWithRLS()
	s.Tables = append(s.Tables, drift.TableShape{
		Name: "my_messages", Kind: "VIEW",
		Columns: []drift.ColumnShape{
			{Name: "id", Ordinal: 1, DataType: "uuid", UDTName: "uuid"},
		},
		ViewDefinition: "SELECT id FROM messages WHERE tenant_id = current_tenant()",
	})
	s.Enums = []drift.EnumShape{{Name: "mail_state", Labels: []string{"queued", "sent"}}}
	s.Extensions = []drift.ExtShape{{Name: "pgcrypto", Schema: "public"}}
	s.Sequences = []drift.SeqShape{{
		Name: "messages_id_seq", DataType: "bigint",
		IncrementBy: 1, MinValue: 1, MaxValue: 9223372036854775807, StartValue: 1,
	}}
	s.Functions = []drift.FuncShape{{
		Name: "current_tenant", Args: "", Returns: "uuid", Language: "sql",
		Definition: "SELECT current_setting('app.tenant_id')::uuid",
	}}
	s.Triggers = []drift.TriggerShape{{
		Name: "audit_messages", Table: "messages", Timing: "AFTER",
		Events: []string{"INSERT", "UPDATE"}, ForEachRow: true,
		Function: "audit_fn", When: "OLD.tenant_id <> NEW.tenant_id",
	}}
	s.MaterializedViews = []drift.MatViewShape{{
		Name: "mv_stats", Definition: "SELECT count(*) FROM messages",
	}}
	return s
}

func TestStructuralSnapshotFromDrift_CarriesObjects(t *testing.T) {
	out := structuralSnapshotFromDrift(driftSnapshotFull())

	if len(out.Enums) != 1 || out.Enums[0].Name != "mail_state" ||
		len(out.Enums[0].Labels) != 2 || out.Enums[0].Labels[0] != "queued" {
		t.Errorf("Enums = %+v", out.Enums)
	}
	if len(out.Extensions) != 1 || out.Extensions[0].Name != "pgcrypto" ||
		out.Extensions[0].Schema != "public" {
		t.Errorf("Extensions = %+v", out.Extensions)
	}
	if len(out.Sequences) != 1 {
		t.Fatalf("Sequences = %+v", out.Sequences)
	}
	if q := out.Sequences[0]; q.Name != "messages_id_seq" || q.DataType != "bigint" ||
		q.IncrementBy != 1 || q.MinValue != 1 || q.MaxValue != 9223372036854775807 ||
		q.StartValue != 1 {
		t.Errorf("Sequences[0] = %+v", q)
	}
	if len(out.Functions) != 1 {
		t.Fatalf("Functions = %+v", out.Functions)
	}
	if f := out.Functions[0]; f.Name != "current_tenant" || f.Returns != "uuid" ||
		f.Language != "sql" ||
		f.Definition != "SELECT current_setting('app.tenant_id')::uuid" {
		t.Errorf("Functions[0] = %+v — the body is where a rewrite of the "+
			"function every policy calls would hide", f)
	}
	if len(out.Triggers) != 1 {
		t.Fatalf("Triggers = %+v", out.Triggers)
	}
	if tr := out.Triggers[0]; tr.Name != "audit_messages" || tr.Table != "messages" ||
		tr.Timing != "AFTER" || !tr.ForEachRow || tr.Function != "audit_fn" ||
		tr.When != "OLD.tenant_id <> NEW.tenant_id" ||
		len(tr.Events) != 2 || tr.Events[0] != "INSERT" || tr.Events[1] != "UPDATE" {
		t.Errorf("Triggers[0] = %+v", tr)
	}
	if len(out.MaterializedViews) != 1 || out.MaterializedViews[0].Name != "mv_stats" ||
		out.MaterializedViews[0].Definition != "SELECT count(*) FROM messages" {
		t.Errorf("MaterializedViews = %+v", out.MaterializedViews)
	}

	var view *keystonev1alpha1.SnapshotTable
	for i := range out.Tables {
		if out.Tables[i].Name == "my_messages" {
			view = &out.Tables[i]
		}
	}
	if view == nil {
		t.Fatal("view missing from tables")
	}
	if view.ViewDefinition != "SELECT id FROM messages WHERE tenant_id = current_tenant()" {
		t.Errorf("ViewDefinition = %q; a view's columns survive a WHERE rewrite "+
			"intact, so without the body nothing distinguishes a view that "+
			"filters by tenant from one that stopped", view.ViewDefinition)
	}
}

func TestStructuralSnapshotFromDrift_ObjectsCarryCurrentModelVersion(t *testing.T) {
	out := structuralSnapshotFromDrift(driftSnapshotFull())
	if out.ModelVersion != keystonev1alpha1.StructuralSnapshotModelVersion {
		t.Errorf("ModelVersion = %d, want %d", out.ModelVersion,
			keystonev1alpha1.StructuralSnapshotModelVersion)
	}
	if keystonev1alpha1.StructuralSnapshotModelVersion < 2 {
		t.Error("the model version must be bumped when a dimension is added, " +
			"or a reader cannot tell an empty list from an unrecorded one")
	}
}

// Same contract as the RLS pointer: the drift snapshot stays live after
// the call, so every slice must be copied rather than aliased.
func TestStructuralSnapshotFromDrift_NoAliasingObjects(t *testing.T) {
	src := driftSnapshotFull()
	out := structuralSnapshotFromDrift(src)

	src.Enums[0].Labels[0] = "clobbered"
	src.Triggers[0].Events[0] = "TRUNCATE"

	if out.Enums[0].Labels[0] != "queued" {
		t.Errorf("enum labels alias the drift snapshot; got %q", out.Enums[0].Labels[0])
	}
	if out.Triggers[0].Events[0] != "INSERT" {
		t.Errorf("trigger events alias the drift snapshot; got %q", out.Triggers[0].Events[0])
	}
}

func TestStructuralSnapshotFromDrift_NilIn(t *testing.T) {
	if out := structuralSnapshotFromDrift(nil); out != nil {
		t.Errorf("want nil for a nil snapshot, got %+v", out)
	}
}

// TestSchemaSnapshot_PreRLSArchiveStillAdmissible — the registry's
// stated purpose is that an archived snapshot can be brought back:
// replayed as restore evidence, or re-applied into a rebuilt cluster.
// Snapshots written before RLS was modelled carry no modelVersion and
// no per-table rlsEnabled, so marking either field required in the CRD
// would make every one of them fail admission against the new schema —
// breaking exactly the artifact the registry exists to keep usable.
//
// This writes the old shape through the unstructured client, which is
// the only way to omit a field the Go type always serialises.
func TestSchemaSnapshot_PreRLSArchiveStillAdmissible(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "keystone.hexxlock.io/v1alpha1",
		"kind":       "SchemaSnapshot",
		"metadata": map[string]interface{}{
			"namespace": "keystone-system", "name": "archived-pre-rls",
		},
		"spec": map[string]interface{}{"schemaRef": "downstream-service-public"},
	}}
	if err := k8s.Create(ctx, obj); err != nil {
		t.Fatalf("create archived snapshot: %v", err)
	}

	// The pre-RLS structure model: no modelVersion, and a table with no
	// rlsEnabled key at all.
	obj.Object["status"] = map[string]interface{}{
		"phase": "Captured",
		"structure": map[string]interface{}{
			"schema": "public",
			"tables": []interface{}{map[string]interface{}{
				"name": "messages",
				"kind": "BASE TABLE",
				"columns": []interface{}{map[string]interface{}{
					"name": "id", "ordinal": int64(1),
					"dataType": "uuid", "udtName": "uuid", "nullable": false,
				}},
			}},
		},
	}
	if err := k8s.Status().Update(ctx, obj); err != nil {
		t.Fatalf("pre-RLS snapshot rejected by the CRD schema: %v", err)
	}

	// And it must read back as model 0 — not defaulted up to the
	// current model, which would relabel an un-inspected snapshot as
	// RLS-aware and let the diff compare fields nobody captured.
	var back unstructured.Unstructured
	back.SetGroupVersionKind(obj.GroupVersionKind())
	if err := k8s.Get(ctx, types.NamespacedName{
		Namespace: "keystone-system", Name: "archived-pre-rls",
	}, &back); err != nil {
		t.Fatalf("get: %v", err)
	}
	structure, found, err := unstructured.NestedMap(back.Object, "status", "structure")
	if err != nil || !found {
		t.Fatalf("structure not persisted (found=%v err=%v)", found, err)
	}
	if v, ok := structure["modelVersion"]; ok {
		t.Errorf("modelVersion defaulted to %v on read; pre-RLS snapshots must stay at 0", v)
	}
	tables, _, _ := unstructured.NestedSlice(structure, "tables")
	if len(tables) != 1 {
		t.Fatalf("want 1 table, got %d", len(tables))
	}
	if v, ok := tables[0].(map[string]interface{})["rlsEnabled"]; ok {
		t.Errorf("rlsEnabled defaulted to %v on read; absence must survive", v)
	}
}

// TestSchemaSnapshot_RLSEraArchiveGainsNoObjects — the sibling of the
// pre-RLS case, one model version up. A snapshot captured while the
// structure model was at 1 is RLS-aware but carries no enums,
// extensions, sequences, functions, triggers, materialized views or
// view definitions, because the controller did not copy them yet.
//
// Two things have to hold for those archives. They must still pass
// admission — none of the model-2 fields may be required. And they must
// read back exactly as written: if the CRD defaulted any of the new
// lists to `[]`, an archive that never looked at triggers would come
// back claiming it had looked and found none, and `snapshot diff` would
// then report every trigger in the newer snapshot as freshly added.
// That is the same false-certainty failure the caveat mechanism exists
// to prevent, arriving through the schema instead of the diff.
func TestSchemaSnapshot_RLSEraArchiveGainsNoObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "keystone.hexxlock.io/v1alpha1",
		"kind":       "SchemaSnapshot",
		"metadata": map[string]interface{}{
			"namespace": "keystone-system", "name": "archived-rls-era",
		},
		"spec": map[string]interface{}{"schemaRef": "downstream-service-public"},
	}}
	if err := k8s.Create(ctx, obj); err != nil {
		t.Fatalf("create archived snapshot: %v", err)
	}

	// The model-1 structure: RLS present, everything model 2 added absent.
	obj.Object["status"] = map[string]interface{}{
		"phase": "Captured",
		"structure": map[string]interface{}{
			"schema":       "public",
			"modelVersion": int64(1),
			"tables": []interface{}{map[string]interface{}{
				"name": "messages", "kind": "BASE TABLE",
				"rlsEnabled": true, "rlsForced": true,
				"columns": []interface{}{map[string]interface{}{
					"name": "id", "ordinal": int64(1),
					"dataType": "uuid", "udtName": "uuid", "nullable": false,
				}},
			}},
			"policies": []interface{}{map[string]interface{}{
				"name": "tenant_isolation", "table": "messages",
				"permissive": true, "command": "ALL",
				"using": "tenant_id = current_tenant()",
			}},
		},
	}
	if err := k8s.Status().Update(ctx, obj); err != nil {
		t.Fatalf("model-1 snapshot rejected by the CRD schema: %v", err)
	}

	var back unstructured.Unstructured
	back.SetGroupVersionKind(obj.GroupVersionKind())
	if err := k8s.Get(ctx, types.NamespacedName{
		Namespace: "keystone-system", Name: "archived-rls-era",
	}, &back); err != nil {
		t.Fatalf("get: %v", err)
	}
	structure, found, err := unstructured.NestedMap(back.Object, "status", "structure")
	if err != nil || !found {
		t.Fatalf("structure not persisted (found=%v err=%v)", found, err)
	}
	if v, _, _ := unstructured.NestedInt64(structure, "modelVersion"); v != 1 {
		t.Errorf("modelVersion read back as %d; a model-1 archive must stay at 1", v)
	}
	for _, key := range []string{
		"enums", "extensions", "sequences",
		"functions", "triggers", "materializedViews",
	} {
		if v, ok := structure[key]; ok {
			t.Errorf("%s defaulted to %v on read; a model-1 archive never captured it", key, v)
		}
	}
	tables, _, _ := unstructured.NestedSlice(structure, "tables")
	if len(tables) != 1 {
		t.Fatalf("want 1 table, got %d", len(tables))
	}
	if v, ok := tables[0].(map[string]interface{})["viewDefinition"]; ok {
		t.Errorf("viewDefinition defaulted to %v on read; absence must survive", v)
	}
}

// TestSchemaSnapshot_TriggerWithoutForEachRowIsRejected — the other half
// of the back-compat argument. forEachRow is required precisely because
// no archive predates it, so a snapshot that omits it is not an old
// artifact being tolerated, it is a new one that lost the field. Letting
// it through would silently mean FOR EACH STATEMENT, quietly turning a
// per-row audit trigger into a per-statement one in the diff output.
//
// If the required marker is ever dropped from the type, this fails.
func TestSchemaSnapshot_TriggerWithoutForEachRowIsRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("envtest is slow; skipping in -short mode")
	}
	k8s, stop := StartEnvtest(t)
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "keystone.hexxlock.io/v1alpha1",
		"kind":       "SchemaSnapshot",
		"metadata": map[string]interface{}{
			"namespace": "keystone-system", "name": "trigger-missing-scope",
		},
		"spec": map[string]interface{}{"schemaRef": "downstream-service-public"},
	}}
	if err := k8s.Create(ctx, obj); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	obj.Object["status"] = map[string]interface{}{
		"phase": "Captured",
		"structure": map[string]interface{}{
			"schema":       "public",
			"modelVersion": int64(2),
			"triggers": []interface{}{map[string]interface{}{
				"name": "audit_messages", "table": "messages",
				"timing":   "AFTER",
				"events":   []interface{}{"INSERT"},
				"function": "audit_row()",
				// forEachRow deliberately omitted.
			}},
		},
	}
	if err := k8s.Status().Update(ctx, obj); err == nil {
		t.Fatal("a trigger with no forEachRow was admitted; the field must stay required")
	}
}
