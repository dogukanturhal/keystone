// SPDX-License-Identifier: AGPL-3.0-or-later

// Package schemadiff compares two SchemaSnapshot.Status.Structure
// artifacts and emits a human-readable list of structural changes.
// Pure — no database connections, no controller-runtime, no k8s
// clients. The webhook, the CLI, and future notebook tooling all call
// into it.
//
// Change taxonomy mirrors the drift analyzer's vocabulary so existing
// dashboards and alerts can consume both streams:
//
//	TableAdded / TableRemoved           whole table appeared / disappeared
//	TableKindChanged                    same name, base table ⇄ view
//	ViewDefinitionChanged               same view, different SELECT
//	ColumnAdded / ColumnRemoved         column added / dropped
//	ColumnRetyped                       same name, different DataType or UDTName
//	ColumnNullability                   same name, nullable flag flipped
//	ColumnDefaultChanged                same name, default expression differs
//	IndexAdded / IndexRemoved           index created / dropped
//	IndexDefinitionChanged              same name, different CREATE INDEX body
//	ConstraintAdded / ConstraintRemoved constraint created / dropped
//	ConstraintDefinitionChanged         same name, different body
//	RLSEnabledChanged                   table's row-level security toggled
//	RLSForcedChanged                    table's FORCE ROW LEVEL SECURITY toggled
//	PolicyAdded / PolicyRemoved         RLS policy created / dropped
//	PolicyChanged                       same policy, different body or roles
//	EnumAdded / EnumRemoved             enum type created / dropped
//	EnumChanged                         same enum, different label list or order
//	ExtensionAdded / ExtensionRemoved   extension installed / removed
//	ExtensionChanged                    same extension, different schema
//	SequenceAdded / SequenceRemoved     sequence created / dropped
//	SequenceChanged                     same sequence, different type or bounds
//	FunctionAdded / FunctionRemoved     function created / dropped
//	FunctionChanged                     same signature, different body or result
//	TriggerAdded / TriggerRemoved       trigger created / dropped
//	TriggerChanged                      same trigger, different timing/events/WHEN
//	MaterializedViewAdded / …Removed    matview created / dropped
//	MaterializedViewChanged             same matview, different SELECT
//
// Diff is symmetric-reportable but NOT commutative: `Diff(a, b)`
// reports the transformation FROM a TO b. Swap the arguments to get
// the reverse story.
//
// It returns a Report, not a bare []Change, because some comparisons
// cannot be made: each dimension entered the snapshot model at some
// version, snapshots below it carry nothing for that dimension, and
// snapshots are retained for years. Report keeps "no differences" and
// "not examined" distinguishable.
package schemadiff

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// ChangeKind is the discriminator for Change.
type ChangeKind string

const (
	KindTableAdded                  ChangeKind = "TableAdded"
	KindTableRemoved                ChangeKind = "TableRemoved"
	KindColumnAdded                 ChangeKind = "ColumnAdded"
	KindColumnRemoved               ChangeKind = "ColumnRemoved"
	KindColumnRetyped               ChangeKind = "ColumnRetyped"
	KindColumnNullability           ChangeKind = "ColumnNullability"
	KindColumnDefaultChanged        ChangeKind = "ColumnDefaultChanged"
	KindIndexAdded                  ChangeKind = "IndexAdded"
	KindIndexRemoved                ChangeKind = "IndexRemoved"
	KindIndexDefinitionChanged      ChangeKind = "IndexDefinitionChanged"
	KindConstraintAdded             ChangeKind = "ConstraintAdded"
	KindConstraintRemoved           ChangeKind = "ConstraintRemoved"
	KindConstraintDefinitionChanged ChangeKind = "ConstraintDefinitionChanged"
	KindRLSEnabledChanged           ChangeKind = "RLSEnabledChanged"
	KindRLSForcedChanged            ChangeKind = "RLSForcedChanged"
	KindPolicyAdded                 ChangeKind = "PolicyAdded"
	KindPolicyRemoved               ChangeKind = "PolicyRemoved"
	KindPolicyChanged               ChangeKind = "PolicyChanged"
	KindTableKindChanged            ChangeKind = "TableKindChanged"
	KindViewDefinitionChanged       ChangeKind = "ViewDefinitionChanged"
	KindEnumAdded                   ChangeKind = "EnumAdded"
	KindEnumRemoved                 ChangeKind = "EnumRemoved"
	KindEnumChanged                 ChangeKind = "EnumChanged"
	KindExtensionAdded              ChangeKind = "ExtensionAdded"
	KindExtensionRemoved            ChangeKind = "ExtensionRemoved"
	KindExtensionChanged            ChangeKind = "ExtensionChanged"
	KindSequenceAdded               ChangeKind = "SequenceAdded"
	KindSequenceRemoved             ChangeKind = "SequenceRemoved"
	KindSequenceChanged             ChangeKind = "SequenceChanged"
	KindFunctionAdded               ChangeKind = "FunctionAdded"
	KindFunctionRemoved             ChangeKind = "FunctionRemoved"
	KindFunctionChanged             ChangeKind = "FunctionChanged"
	KindTriggerAdded                ChangeKind = "TriggerAdded"
	KindTriggerRemoved              ChangeKind = "TriggerRemoved"
	KindTriggerChanged              ChangeKind = "TriggerChanged"
	KindMaterializedViewAdded       ChangeKind = "MaterializedViewAdded"
	KindMaterializedViewRemoved     ChangeKind = "MaterializedViewRemoved"
	KindMaterializedViewChanged     ChangeKind = "MaterializedViewChanged"
)

// The first structure model version that records each dimension.
// Snapshots below it carry nothing for that dimension, and their zero
// values must not be read as observations.
const (
	// rlsModelVersion — per-table RLSEnabled/RLSForced and Policies.
	rlsModelVersion int32 = 1

	// objectsModelVersion — enums, extensions, sequences, functions,
	// triggers, materialized views, and per-view definitions.
	objectsModelVersion int32 = 2
)

// Change is one observed difference between two snapshots.
type Change struct {
	// Kind discriminates the change type.
	Kind ChangeKind

	// Table is the parent table when the change is column-level or
	// index/constraint on a specific table. Empty for schema-level
	// objects.
	Table string

	// Object is the identifier that changed — column name, index
	// name, constraint name, or table name when Kind is Table*.
	Object string

	// Before is the "from" state in a concise human form: old column
	// type, old nullability, old index definition, etc. Empty for
	// *Added changes.
	Before string

	// After is the "to" state. Empty for *Removed changes.
	After string
}

// String formats a Change as a single line suitable for CLI output.
func (c Change) String() string {
	prefix := string(c.Kind)
	if c.Table != "" && c.Object != "" && c.Table != c.Object {
		prefix += " " + c.Table + "." + c.Object
	} else if c.Object != "" {
		prefix += " " + c.Object
	}
	switch {
	case c.Before != "" && c.After != "":
		return fmt.Sprintf("%s: %s → %s", prefix, c.Before, c.After)
	case c.Before != "":
		return prefix + " (was " + c.Before + ")"
	case c.After != "":
		return prefix + " (now " + c.After + ")"
	}
	return prefix
}

// Report is the result of comparing two snapshots.
//
// Changes and Caveats answer different questions and must not be
// collapsed: an empty Changes list means "these two snapshots agree on
// everything compared", which is only the same as "nothing changed"
// when Caveats is also empty. Reporting "no differences" for a
// dimension that was never examined is the failure this type exists to
// prevent.
type Report struct {
	// Changes is every observed difference, in deterministic order
	// (sorted by Table, Object, Kind).
	Changes []Change

	// Caveats names each dimension that could not be compared, and
	// why. Callers must surface these alongside Changes.
	Caveats []string

	// FromModelVersion and ToModelVersion echo the inputs' structure
	// model versions so callers can attribute a caveat to a specific
	// snapshot by name.
	FromModelVersion int32
	ToModelVersion   int32
}

// Diff returns every change between `from` and `to`, in deterministic
// order (sorted by Table, Object, Kind). Nil inputs are treated as
// empty snapshots; diffing nil → X reports every object in X as added.
//
// Row-level security is compared only when both snapshots record it
// (structure model version >= 1). When either predates it the RLS
// dimension is skipped and a caveat is returned rather than silently
// treating the missing side as "no policies, not forced" — that
// reading would report every existing policy as newly added and, worse,
// would mask a policy that had actually been dropped.
func Diff(from, to *keystonev1alpha1.StructuralSnapshot) Report {
	fromTables := indexTables(from)
	toTables := indexTables(to)

	fromIdx := indexObjects(snapshotIndexes(from))
	toIdx := indexObjects(snapshotIndexes(to))
	fromCons := indexObjects(snapshotConstraints(from))
	toCons := indexObjects(snapshotConstraints(to))

	var changes []Change

	// -- Tables: added, removed, column-level deltas ------------------
	for name, toT := range toTables {
		fromT, ok := fromTables[name]
		if !ok {
			changes = append(changes, Change{
				Kind: KindTableAdded, Table: name, Object: name,
				After: toT.Kind,
			})
			// Every column of a new table is also implicitly added;
			// skip the per-column ColumnAdded entries to avoid noise.
			continue
		}
		// A name that stays but changes kind is a base table replaced by
		// a view or the reverse. Neither TableAdded nor TableRemoved
		// fires for it — the name is present on both sides — and the
		// column list is usually identical, which is the point of the
		// substitution. Reported on its own.
		if fromT.Kind != toT.Kind {
			changes = append(changes, Change{
				Kind: KindTableKindChanged, Table: name, Object: name,
				Before: fromT.Kind, After: toT.Kind,
			})
		}
		changes = append(changes, diffColumns(name, fromT.Columns, toT.Columns)...)
	}
	for name, fromT := range fromTables {
		if _, ok := toTables[name]; !ok {
			changes = append(changes, Change{
				Kind: KindTableRemoved, Table: name, Object: name,
				Before: fromT.Kind,
			})
		}
	}

	// -- Indexes + constraints ---------------------------------------
	changes = append(changes, diffObjectDDL(fromIdx, toIdx,
		KindIndexAdded, KindIndexRemoved, KindIndexDefinitionChanged)...)
	changes = append(changes, diffObjectDDL(fromCons, toCons,
		KindConstraintAdded, KindConstraintRemoved, KindConstraintDefinitionChanged)...)

	report := Report{
		FromModelVersion: modelVersion(from),
		ToModelVersion:   modelVersion(to),
	}

	// -- Row-level security -------------------------------------------
	if report.FromModelVersion >= rlsModelVersion && report.ToModelVersion >= rlsModelVersion {
		changes = append(changes, diffRLS(fromTables, toTables)...)
		changes = append(changes, diffPolicies(
			indexPolicies(snapshotPolicies(from)),
			indexPolicies(snapshotPolicies(to)))...)
	} else {
		report.Caveats = append(report.Caveats,
			"row-level security was not compared: per-table FORCE/ENABLE and "+
				"policies are recorded only from structure model version "+
				strconv.Itoa(int(rlsModelVersion))+", and at least one of these "+
				"snapshots was captured before that")
	}

	// -- Everything else the inspector reads ---------------------------
	if report.FromModelVersion >= objectsModelVersion && report.ToModelVersion >= objectsModelVersion {
		changes = append(changes, diffViewDefinitions(fromTables, toTables)...)
		changes = append(changes, diffEnums(from, to)...)
		changes = append(changes, diffExtensions(from, to)...)
		changes = append(changes, diffSequences(from, to)...)
		changes = append(changes, diffFunctions(from, to)...)
		changes = append(changes, diffTriggers(from, to)...)
		changes = append(changes, diffMatViews(from, to)...)
	} else {
		report.Caveats = append(report.Caveats,
			"enums, extensions, sequences, functions, triggers, materialized "+
				"views and view definitions were not compared: they are recorded "+
				"only from structure model version "+
				strconv.Itoa(int(objectsModelVersion))+", and at least one of "+
				"these snapshots was captured before that")
	}

	// Deterministic output.
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].Table != changes[j].Table {
			return changes[i].Table < changes[j].Table
		}
		if changes[i].Object != changes[j].Object {
			return changes[i].Object < changes[j].Object
		}
		return changes[i].Kind < changes[j].Kind
	})
	report.Changes = changes
	return report
}

// diffRLS compares the two per-table row-level-security bits. Only
// called when both snapshots record them.
func diffRLS(from, to map[string]keystonev1alpha1.SnapshotTable) []Change {
	var changes []Change
	for name, toT := range to {
		fromT, ok := from[name]
		if !ok {
			// A new table; TableAdded already covers it.
			continue
		}
		if fromT.RLSEnabled != toT.RLSEnabled {
			changes = append(changes, Change{
				Kind: KindRLSEnabledChanged, Table: name, Object: name,
				Before: rlsEnabledLabel(fromT.RLSEnabled),
				After:  rlsEnabledLabel(toT.RLSEnabled),
			})
		}
		// nil on either side means that table was captured before the
		// field existed. The model-version gate above already rules
		// that out for whole snapshots; this guards a mixed document.
		if fromT.RLSForced == nil || toT.RLSForced == nil {
			continue
		}
		if *fromT.RLSForced != *toT.RLSForced {
			changes = append(changes, Change{
				Kind: KindRLSForcedChanged, Table: name, Object: name,
				Before: rlsForcedLabel(*fromT.RLSForced),
				After:  rlsForcedLabel(*toT.RLSForced),
			})
		}
	}
	return changes
}

// diffPolicies compares two policy sets keyed by (table, name).
func diffPolicies(from, to map[string]keystonev1alpha1.SnapshotPolicy) []Change {
	var changes []Change
	for key, toP := range to {
		fromP, ok := from[key]
		if !ok {
			changes = append(changes, Change{
				Kind: KindPolicyAdded, Table: toP.Table, Object: toP.Name,
				After: policyDescription(toP),
			})
			continue
		}
		if before, after := policyDescription(fromP), policyDescription(toP); before != after {
			changes = append(changes, Change{
				Kind: KindPolicyChanged, Table: toP.Table, Object: toP.Name,
				Before: before, After: after,
			})
		}
	}
	for key, fromP := range from {
		if _, ok := to[key]; !ok {
			changes = append(changes, Change{
				Kind: KindPolicyRemoved, Table: fromP.Table, Object: fromP.Name,
				Before: policyDescription(fromP),
			})
		}
	}
	return changes
}

// diffColumns returns the column-level differences for a single
// table. Preserves ordinal information by keying on column name
// (columns reorder for storage-internal reasons without a semantic
// change).
func diffColumns(table string, from, to []keystonev1alpha1.SnapshotColumn) []Change {
	fromMap := make(map[string]keystonev1alpha1.SnapshotColumn, len(from))
	for _, c := range from {
		fromMap[c.Name] = c
	}
	toMap := make(map[string]keystonev1alpha1.SnapshotColumn, len(to))
	for _, c := range to {
		toMap[c.Name] = c
	}

	var changes []Change
	for name, tc := range toMap {
		fc, ok := fromMap[name]
		if !ok {
			changes = append(changes, Change{
				Kind: KindColumnAdded, Table: table, Object: name,
				After: columnDescription(tc),
			})
			continue
		}
		if fc.DataType != tc.DataType || fc.UDTName != tc.UDTName {
			changes = append(changes, Change{
				Kind: KindColumnRetyped, Table: table, Object: name,
				Before: columnType(fc), After: columnType(tc),
			})
		}
		if fc.Nullable != tc.Nullable {
			changes = append(changes, Change{
				Kind: KindColumnNullability, Table: table, Object: name,
				Before: nullableLabel(fc.Nullable),
				After:  nullableLabel(tc.Nullable),
			})
		}
		if fc.Default != tc.Default {
			changes = append(changes, Change{
				Kind: KindColumnDefaultChanged, Table: table, Object: name,
				Before: fc.Default, After: tc.Default,
			})
		}
	}
	for name, fc := range fromMap {
		if _, ok := toMap[name]; !ok {
			changes = append(changes, Change{
				Kind: KindColumnRemoved, Table: table, Object: name,
				Before: columnDescription(fc),
			})
		}
	}
	return changes
}

// diffObjectDDL handles index + constraint deltas with the same
// algorithm: added / removed / definition-changed.
func diffObjectDDL(
	from, to map[string]keystonev1alpha1.SnapshotObjectDDL,
	addedKind, removedKind, changedKind ChangeKind,
) []Change {
	var changes []Change
	for name, t := range to {
		f, ok := from[name]
		if !ok {
			changes = append(changes, Change{
				Kind: addedKind, Table: t.Table, Object: name,
				After: t.Definition,
			})
			continue
		}
		if f.Definition != t.Definition {
			changes = append(changes, Change{
				Kind: changedKind, Table: t.Table, Object: name,
				Before: f.Definition, After: t.Definition,
			})
		}
	}
	for name, f := range from {
		if _, ok := to[name]; !ok {
			changes = append(changes, Change{
				Kind: removedKind, Table: f.Table, Object: name,
				Before: f.Definition,
			})
		}
	}
	return changes
}

func indexTables(s *keystonev1alpha1.StructuralSnapshot) map[string]keystonev1alpha1.SnapshotTable {
	out := map[string]keystonev1alpha1.SnapshotTable{}
	if s == nil {
		return out
	}
	for _, t := range s.Tables {
		out[t.Name] = t
	}
	return out
}

func snapshotIndexes(s *keystonev1alpha1.StructuralSnapshot) []keystonev1alpha1.SnapshotObjectDDL {
	if s == nil {
		return nil
	}
	return s.Indexes
}

func modelVersion(s *keystonev1alpha1.StructuralSnapshot) int32 {
	if s == nil {
		return 0
	}
	return s.ModelVersion
}

func snapshotPolicies(s *keystonev1alpha1.StructuralSnapshot) []keystonev1alpha1.SnapshotPolicy {
	if s == nil {
		return nil
	}
	return s.Policies
}

// indexPolicies keys by table then name. Policy names are unique per
// table, not per schema — two tables may each carry a policy called
// "tenant_isolation", and keying on the bare name would silently
// collapse them into one.
func indexPolicies(in []keystonev1alpha1.SnapshotPolicy) map[string]keystonev1alpha1.SnapshotPolicy {
	out := make(map[string]keystonev1alpha1.SnapshotPolicy, len(in))
	for _, p := range in {
		out[p.Table+"\x00"+p.Name] = p
	}
	return out
}

// policyDescription renders a policy in a form close to the CREATE
// POLICY that would produce it. Comparison is on this rendering, so
// every field it prints is a field the diff can detect a change in —
// keep it total over SnapshotPolicy.
func policyDescription(p keystonev1alpha1.SnapshotPolicy) string {
	var b strings.Builder
	if p.Permissive {
		b.WriteString("PERMISSIVE")
	} else {
		b.WriteString("RESTRICTIVE")
	}
	if p.Command != "" {
		b.WriteString(" FOR ")
		b.WriteString(p.Command)
	}
	if len(p.Roles) > 0 {
		// Sorted: PostgreSQL does not promise a stable role order, and
		// a reordering is not a change.
		roles := append([]string(nil), p.Roles...)
		sort.Strings(roles)
		b.WriteString(" TO ")
		b.WriteString(strings.Join(roles, ", "))
	}
	if p.Using != "" {
		b.WriteString(" USING (")
		b.WriteString(p.Using)
		b.WriteString(")")
	}
	if p.WithCheck != "" {
		b.WriteString(" WITH CHECK (")
		b.WriteString(p.WithCheck)
		b.WriteString(")")
	}
	return b.String()
}

func rlsEnabledLabel(b bool) string {
	if b {
		return "ENABLED"
	}
	return "DISABLED"
}

// rlsForcedLabel spells out the owner-exemption consequence rather
// than printing a bare bool: "NO FORCE" is the state in which policies
// exist but do not apply to the owning role, and a diff line is the
// place an operator is most likely to notice that.
func rlsForcedLabel(b bool) string {
	if b {
		return "FORCED"
	}
	return "NOT FORCED (owner exempt)"
}

func snapshotConstraints(s *keystonev1alpha1.StructuralSnapshot) []keystonev1alpha1.SnapshotObjectDDL {
	if s == nil {
		return nil
	}
	return s.Constraints
}

func indexObjects(in []keystonev1alpha1.SnapshotObjectDDL) map[string]keystonev1alpha1.SnapshotObjectDDL {
	out := make(map[string]keystonev1alpha1.SnapshotObjectDDL, len(in))
	for _, o := range in {
		out[o.Name] = o
	}
	return out
}

// columnDescription formats type + nullability together — used for
// the compact "column added" / "column removed" lines.
func columnDescription(c keystonev1alpha1.SnapshotColumn) string {
	s := columnType(c) + " " + nullableLabel(c.Nullable)
	if c.Default != "" {
		s += " default=" + c.Default
	}
	return s
}

func columnType(c keystonev1alpha1.SnapshotColumn) string {
	if c.UDTName != "" && c.UDTName != c.DataType {
		return c.DataType + " (" + c.UDTName + ")"
	}
	return c.DataType
}

func nullableLabel(b bool) string {
	if b {
		return "NULL"
	}
	return "NOT NULL"
}

// -- Model version 2: the rest of the inspected schema -----------------

// keyedObject is what every dimension below reduces to: an identity to
// match old against new, a table to attribute the change to, an object
// name to print, and a rendering that is total over the type so any
// field change shows up as a text difference.
type keyedObject struct {
	key    string
	table  string
	object string
	desc   string
}

// diffKeyed is the added/removed/changed skeleton shared by every
// object dimension. Matching is on key alone; a difference in desc is
// what makes a match a change, so a renderer that omits a field makes
// that field permanently undiffable. Keep every desc total.
func diffKeyed(from, to []keyedObject, added, removed, changed ChangeKind) []Change {
	index := func(in []keyedObject) map[string]keyedObject {
		out := make(map[string]keyedObject, len(in))
		for _, o := range in {
			out[o.key] = o
		}
		return out
	}
	fromIdx, toIdx := index(from), index(to)

	var changes []Change
	for key, t := range toIdx {
		f, ok := fromIdx[key]
		if !ok {
			changes = append(changes, Change{
				Kind: added, Table: t.table, Object: t.object, After: t.desc,
			})
			continue
		}
		if f.desc != t.desc {
			changes = append(changes, Change{
				Kind: changed, Table: t.table, Object: t.object,
				Before: f.desc, After: t.desc,
			})
		}
	}
	for key, f := range fromIdx {
		if _, ok := toIdx[key]; !ok {
			changes = append(changes, Change{
				Kind: removed, Table: f.table, Object: f.object, Before: f.desc,
			})
		}
	}
	return changes
}

// diffViewDefinitions reports views whose defining SELECT changed.
//
// A view's column list survives a rewrite of its WHERE clause intact,
// so this is the only signal that separates a view that still filters
// by tenant from one that no longer does. Views present on one side
// only are already covered by TableAdded/TableRemoved, and a name that
// switched between table and view by TableKindChanged; this reports
// only same-name, same-kind bodies.
func diffViewDefinitions(from, to map[string]keystonev1alpha1.SnapshotTable) []Change {
	var changes []Change
	for name, toT := range to {
		fromT, ok := from[name]
		if !ok || fromT.Kind != toT.Kind {
			continue
		}
		if fromT.ViewDefinition != toT.ViewDefinition {
			changes = append(changes, Change{
				Kind: KindViewDefinitionChanged, Table: name, Object: name,
				Before: fromT.ViewDefinition, After: toT.ViewDefinition,
			})
		}
	}
	return changes
}

func diffEnums(from, to *keystonev1alpha1.StructuralSnapshot) []Change {
	// Label order is PostgreSQL's sort order, not declaration order, so
	// it is compared as given and never sorted: reordering the labels of
	// an enum silently reverses every comparison and ORDER BY on a
	// column of that type. This is the opposite of a policy's role list,
	// where order carries nothing.
	render := func(in []keystonev1alpha1.SnapshotEnum) []keyedObject {
		out := make([]keyedObject, 0, len(in))
		for _, e := range in {
			out = append(out, keyedObject{
				key: e.Name, object: e.Name,
				desc: "(" + strings.Join(e.Labels, ", ") + ")",
			})
		}
		return out
	}
	return diffKeyed(render(enumsOf(from)), render(enumsOf(to)),
		KindEnumAdded, KindEnumRemoved, KindEnumChanged)
}

func diffExtensions(from, to *keystonev1alpha1.StructuralSnapshot) []Change {
	// The inspector reports name and schema only, so an in-place version
	// bump (ALTER EXTENSION … UPDATE TO) is not visible here — it is not
	// captured, so it cannot be diffed.
	render := func(in []keystonev1alpha1.SnapshotExtension) []keyedObject {
		out := make([]keyedObject, 0, len(in))
		for _, e := range in {
			out = append(out, keyedObject{
				key: e.Name, object: e.Name, desc: "schema=" + e.Schema,
			})
		}
		return out
	}
	return diffKeyed(render(extensionsOf(from)), render(extensionsOf(to)),
		KindExtensionAdded, KindExtensionRemoved, KindExtensionChanged)
}

func diffSequences(from, to *keystonev1alpha1.StructuralSnapshot) []Change {
	render := func(in []keystonev1alpha1.SnapshotSequence) []keyedObject {
		out := make([]keyedObject, 0, len(in))
		for _, q := range in {
			out = append(out, keyedObject{
				key: q.Name, object: q.Name,
				desc: fmt.Sprintf("%s INCREMENT BY %d MINVALUE %d MAXVALUE %d START %d",
					q.DataType, q.IncrementBy, q.MinValue, q.MaxValue, q.StartValue),
			})
		}
		return out
	}
	return diffKeyed(render(sequencesOf(from)), render(sequencesOf(to)),
		KindSequenceAdded, KindSequenceRemoved, KindSequenceChanged)
}

func diffFunctions(from, to *keystonev1alpha1.StructuralSnapshot) []Change {
	// Functions are overloadable, so the identity is (name, args). Keying
	// on the name alone collapses an overload set and can report a
	// dropped overload as unchanged — the same failure as keying a policy
	// on its bare name.
	render := func(in []keystonev1alpha1.SnapshotFunction) []keyedObject {
		out := make([]keyedObject, 0, len(in))
		for _, f := range in {
			out = append(out, keyedObject{
				key:    f.Name + "\x00" + f.Args,
				object: f.Name + "(" + f.Args + ")",
				desc: "RETURNS " + f.Returns + " LANGUAGE " + f.Language +
					" AS " + f.Definition,
			})
		}
		return out
	}
	return diffKeyed(render(functionsOf(from)), render(functionsOf(to)),
		KindFunctionAdded, KindFunctionRemoved, KindFunctionChanged)
}

func diffTriggers(from, to *keystonev1alpha1.StructuralSnapshot) []Change {
	// Trigger names are unique per table, not per schema.
	render := func(in []keystonev1alpha1.SnapshotTrigger) []keyedObject {
		out := make([]keyedObject, 0, len(in))
		for _, t := range in {
			desc := t.Timing + " " + strings.Join(t.Events, " OR ") + " " +
				forEachLabel(t.ForEachRow) + " EXECUTE " + t.Function
			if t.When != "" {
				desc += " WHEN (" + t.When + ")"
			}
			out = append(out, keyedObject{
				key:   t.Table + "\x00" + t.Name,
				table: t.Table, object: t.Name, desc: desc,
			})
		}
		return out
	}
	return diffKeyed(render(triggersOf(from)), render(triggersOf(to)),
		KindTriggerAdded, KindTriggerRemoved, KindTriggerChanged)
}

func diffMatViews(from, to *keystonev1alpha1.StructuralSnapshot) []Change {
	render := func(in []keystonev1alpha1.SnapshotMaterializedView) []keyedObject {
		out := make([]keyedObject, 0, len(in))
		for _, mv := range in {
			out = append(out, keyedObject{
				key: mv.Name, table: mv.Name, object: mv.Name, desc: mv.Definition,
			})
		}
		return out
	}
	return diffKeyed(render(matViewsOf(from)), render(matViewsOf(to)),
		KindMaterializedViewAdded, KindMaterializedViewRemoved, KindMaterializedViewChanged)
}

// forEachLabel spells out the row/statement distinction, which decides
// how many times the function runs.
func forEachLabel(row bool) string {
	if row {
		return "FOR EACH ROW"
	}
	return "FOR EACH STATEMENT"
}

// Nil-safe accessors. Diff accepts nil snapshots — an unpopulated
// status.structure on either side — and the gate above only proves the
// model version, not that the pointer is non-nil.

func enumsOf(s *keystonev1alpha1.StructuralSnapshot) []keystonev1alpha1.SnapshotEnum {
	if s == nil {
		return nil
	}
	return s.Enums
}

func extensionsOf(s *keystonev1alpha1.StructuralSnapshot) []keystonev1alpha1.SnapshotExtension {
	if s == nil {
		return nil
	}
	return s.Extensions
}

func sequencesOf(s *keystonev1alpha1.StructuralSnapshot) []keystonev1alpha1.SnapshotSequence {
	if s == nil {
		return nil
	}
	return s.Sequences
}

func functionsOf(s *keystonev1alpha1.StructuralSnapshot) []keystonev1alpha1.SnapshotFunction {
	if s == nil {
		return nil
	}
	return s.Functions
}

func triggersOf(s *keystonev1alpha1.StructuralSnapshot) []keystonev1alpha1.SnapshotTrigger {
	if s == nil {
		return nil
	}
	return s.Triggers
}

func matViewsOf(s *keystonev1alpha1.StructuralSnapshot) []keystonev1alpha1.SnapshotMaterializedView {
	if s == nil {
		return nil
	}
	return s.MaterializedViews
}
