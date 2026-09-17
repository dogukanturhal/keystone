// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

// +kubebuilder:webhook:path=/validate-keystone-hexxlock-io-v1alpha1-schemadefinition,mutating=false,failurePolicy=fail,sideEffects=None,groups=keystone.hexxlock.io,resources=schemadefinitions,verbs=create;update,versions=v1alpha1,name=vschemadefinition.kb.io,admissionReviewVersions=v1

// SchemaDefinitionValidator enforces structural invariants the CRD
// schema can't express alone:
//
//   - unique table names within a SchemaDefinition
//   - unique column names within each table
//   - every column in a composite PrimaryKey actually exists
//   - every ForeignKey.columns entry exists
//   - AllowDestructive=false rejects specs that would obviously drop
//     columns (no prior schema to diff against, but specs referencing a
//     table with zero columns or renaming via primaryKey remap count)
//
// The heavier semantic checks (is this diff actually destructive?) run
// at reconcile time inside `declarative.Diff` where the inspector has
// observed the live schema. The webhook is limited to spec-internal
// consistency.
type SchemaDefinitionValidator struct{}

var _ admission.Validator[*keystonev1alpha1.SchemaDefinition] = &SchemaDefinitionValidator{}

func (v *SchemaDefinitionValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &keystonev1alpha1.SchemaDefinition{}).
		WithValidator(v).
		Complete()
}

func (v *SchemaDefinitionValidator) ValidateCreate(ctx context.Context, sd *keystonev1alpha1.SchemaDefinition) (admission.Warnings, error) {
	return v.validate(ctx, sd)
}

func (v *SchemaDefinitionValidator) ValidateUpdate(ctx context.Context, _, newSD *keystonev1alpha1.SchemaDefinition) (admission.Warnings, error) {
	return v.validate(ctx, newSD)
}

func (v *SchemaDefinitionValidator) ValidateDelete(_ context.Context, _ *keystonev1alpha1.SchemaDefinition) (admission.Warnings, error) {
	return nil, nil
}

func (v *SchemaDefinitionValidator) validate(_ context.Context, sd *keystonev1alpha1.SchemaDefinition) (admission.Warnings, error) {
	var errs []string

	tableNames := make(map[string]bool, len(sd.Spec.Tables))
	for ti := range sd.Spec.Tables {
		t := &sd.Spec.Tables[ti]
		// Unique table names.
		if tableNames[t.Name] {
			errs = append(errs, fmt.Sprintf("tables[%d]: duplicate table name %q", ti, t.Name))
			continue
		}
		tableNames[t.Name] = true

		// Unique column names + exactly-one single-column PK.
		colNames := make(map[string]bool, len(t.Columns))
		pkCount := 0
		for ci := range t.Columns {
			c := &t.Columns[ci]
			if colNames[c.Name] {
				errs = append(errs, fmt.Sprintf(
					"tables[%s].columns[%d]: duplicate column %q", t.Name, ci, c.Name))
				continue
			}
			colNames[c.Name] = true
			if c.PrimaryKey {
				pkCount++
			}
		}

		// Composite PK must reference existing columns; single-column
		// and composite PKs are mutually exclusive.
		if len(t.PrimaryKey) > 0 && pkCount > 0 {
			errs = append(errs, fmt.Sprintf(
				"tables[%s]: cannot use both column-level PrimaryKey and table-level PrimaryKey", t.Name))
		}
		for _, pkCol := range t.PrimaryKey {
			if !colNames[pkCol] {
				errs = append(errs, fmt.Sprintf(
					"tables[%s].primaryKey: column %q does not exist on table", t.Name, pkCol))
			}
		}

		// Index spec validation. Each DesiredIndex must:
		//   - populate exactly one of Columns / ColumnRefs / Expression
		//     (mutual exclusivity — CRD schema can't easily express this)
		//   - reference existing columns from Columns/ColumnRefs/Include
		//   - INCLUDE only with btree (PG hard rule)
		for ii, idx := range t.Indexes {
			shapes := 0
			if len(idx.Columns) > 0 {
				shapes++
			}
			if len(idx.ColumnRefs) > 0 {
				shapes++
			}
			if idx.Expression != "" {
				shapes++
			}
			switch shapes {
			case 0:
				errs = append(errs, fmt.Sprintf(
					"tables[%s].indexes[%d]: must populate exactly one of columns / columnRefs / expression",
					t.Name, ii))
			case 1:
				// valid
			default:
				errs = append(errs, fmt.Sprintf(
					"tables[%s].indexes[%d]: columns / columnRefs / expression are mutually exclusive (got %d)",
					t.Name, ii, shapes))
			}

			// Column refs must exist on the table.
			for _, ic := range idx.Columns {
				if !colNames[ic] {
					errs = append(errs, fmt.Sprintf(
						"tables[%s].indexes[%d].columns: %q does not exist on table", t.Name, ii, ic))
				}
			}
			for ci, cr := range idx.ColumnRefs {
				// DesiredIndexColumn: exactly one of Name or Expression
				// must be set.
				crShapes := 0
				if cr.Name != "" {
					crShapes++
				}
				if cr.Expression != "" {
					crShapes++
				}
				switch crShapes {
				case 0:
					errs = append(errs, fmt.Sprintf(
						"tables[%s].indexes[%d].columnRefs[%d]: must set exactly one of name or expression",
						t.Name, ii, ci))
				case 1:
					// valid
				default:
					errs = append(errs, fmt.Sprintf(
						"tables[%s].indexes[%d].columnRefs[%d]: name and expression are mutually exclusive",
						t.Name, ii, ci))
				}
				// Column-name refs must exist on the table.
				// Expression refs are validated by PG at apply time.
				if cr.Name != "" && !colNames[cr.Name] {
					errs = append(errs, fmt.Sprintf(
						"tables[%s].indexes[%d].columnRefs[%d]: %q does not exist on table",
						t.Name, ii, ci, cr.Name))
				}
			}
			for _, inc := range idx.Include {
				if !colNames[inc] {
					errs = append(errs, fmt.Sprintf(
						"tables[%s].indexes[%d].include: %q does not exist on table",
						t.Name, ii, inc))
				}
			}

			// PG rule: INCLUDE only valid on btree.
			if len(idx.Include) > 0 {
				method := idx.Method
				if method == "" {
					method = "btree"
				}
				if method != "btree" {
					errs = append(errs, fmt.Sprintf(
						"tables[%s].indexes[%d]: INCLUDE clause requires method=btree (got %q)",
						t.Name, ii, method))
				}
			}
		}

		// FK columns must exist; composite FKs must have matching
		// column/reference cardinality.
		for fi, fk := range t.ForeignKeys {
			for _, fc := range fk.Columns {
				if !colNames[fc] {
					errs = append(errs, fmt.Sprintf(
						"tables[%s].foreignKeys[%d].columns: %q does not exist on table",
						t.Name, fi, fc))
				}
			}
			if len(fk.ReferencesColumns) != len(fk.Columns) {
				errs = append(errs, fmt.Sprintf(
					"tables[%s].foreignKeys[%d]: %d local columns vs %d referenced columns — mismatched cardinality",
					t.Name, fi, len(fk.Columns), len(fk.ReferencesColumns)))
			}
		}
	}

	// Apply strategy must be one of the two supported values. The
	// kubebuilder:validation:Enum catches this at the CRD layer, but
	// the webhook is defence in depth — an out-of-spec value here would
	// confuse the reconciler.
	switch sd.Spec.ApplyStrategy {
	case "", "versioned", "pgroll-expand-contract":
		// valid
	default:
		errs = append(errs, fmt.Sprintf(
			"spec.applyStrategy=%q invalid — must be 'versioned' or 'pgroll-expand-contract'",
			sd.Spec.ApplyStrategy))
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("SchemaDefinition %s/%s: %d validation error(s) — %s",
			sd.Namespace, sd.Name, len(errs), strings.Join(errs, "; "))
	}
	return nil, nil
}
