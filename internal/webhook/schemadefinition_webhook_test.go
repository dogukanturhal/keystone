// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func TestSchemaDefinitionValidator_DuplicateTable(t *testing.T) {
	v := &SchemaDefinitionValidator{}
	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "sd1"},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef:     "sch",
			ApplyStrategy: "versioned",
			Tables: []keystonev1alpha1.DesiredTable{
				{
					Name: "users",
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "id", Type: "uuid", PrimaryKey: true},
					},
				},
				{
					Name: "users", // duplicate
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "id", Type: "uuid", PrimaryKey: true},
					},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), sd)
	if err == nil || !strings.Contains(err.Error(), "duplicate table name") {
		t.Errorf("expected duplicate-table rejection; got %v", err)
	}
}

func TestSchemaDefinitionValidator_DuplicateColumn(t *testing.T) {
	v := &SchemaDefinitionValidator{}
	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "sd2"},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef:     "sch",
			ApplyStrategy: "versioned",
			Tables: []keystonev1alpha1.DesiredTable{
				{
					Name: "users",
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "id", Type: "uuid", PrimaryKey: true},
						{Name: "id", Type: "text"}, // duplicate
					},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), sd)
	if err == nil || !strings.Contains(err.Error(), "duplicate column") {
		t.Errorf("expected duplicate-column rejection; got %v", err)
	}
}

func TestSchemaDefinitionValidator_CompositePKWithColumnPK(t *testing.T) {
	v := &SchemaDefinitionValidator{}
	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "sd3"},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef:     "sch",
			ApplyStrategy: "versioned",
			Tables: []keystonev1alpha1.DesiredTable{
				{
					Name: "t",
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "a", Type: "uuid", PrimaryKey: true},
						{Name: "b", Type: "text"},
					},
					PrimaryKey: []string{"a", "b"}, // and column-level already marked PK
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), sd)
	if err == nil || !strings.Contains(err.Error(), "both column-level PrimaryKey and table-level PrimaryKey") {
		t.Errorf("expected mixed-PK rejection; got %v", err)
	}
}

func TestSchemaDefinitionValidator_IndexColumnMissing(t *testing.T) {
	v := &SchemaDefinitionValidator{}
	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "sd4"},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef:     "sch",
			ApplyStrategy: "versioned",
			Tables: []keystonev1alpha1.DesiredTable{
				{
					Name: "t",
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "id", Type: "uuid", PrimaryKey: true},
					},
					Indexes: []keystonev1alpha1.DesiredIndex{
						{Name: "idx_t_missing", Columns: []string{"nonexistent"}, Method: "btree"},
					},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), sd)
	if err == nil || !strings.Contains(err.Error(), "does not exist on table") {
		t.Errorf("expected index-column-missing rejection; got %v", err)
	}
}

func TestSchemaDefinitionValidator_FKColumnMismatch(t *testing.T) {
	v := &SchemaDefinitionValidator{}
	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "sd5"},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef:     "sch",
			ApplyStrategy: "versioned",
			Tables: []keystonev1alpha1.DesiredTable{
				{
					Name: "orders",
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "id", Type: "uuid", PrimaryKey: true},
						{Name: "user_id", Type: "uuid"},
					},
					ForeignKeys: []keystonev1alpha1.DesiredForeignKey{
						{
							Name:              "fk_orders_user",
							Columns:           []string{"user_id"},
							ReferencesTable:   "users",
							ReferencesColumns: []string{"id", "tenant_id"}, // cardinality mismatch
						},
					},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), sd)
	if err == nil || !strings.Contains(err.Error(), "mismatched cardinality") {
		t.Errorf("expected FK-cardinality rejection; got %v", err)
	}
}

func TestSchemaDefinitionValidator_CleanSpecAdmitted(t *testing.T) {
	v := &SchemaDefinitionValidator{}
	sd := &keystonev1alpha1.SchemaDefinition{
		ObjectMeta: metav1.ObjectMeta{Namespace: "keystone-system", Name: "sd6"},
		Spec: keystonev1alpha1.SchemaDefinitionSpec{
			SchemaRef:     "sch",
			ApplyStrategy: "versioned",
			Tables: []keystonev1alpha1.DesiredTable{
				{
					Name: "users",
					Columns: []keystonev1alpha1.DesiredColumn{
						{Name: "id", Type: "uuid", PrimaryKey: true},
						{Name: "email", Type: "text"},
					},
					Indexes: []keystonev1alpha1.DesiredIndex{
						{Name: "idx_users_email", Columns: []string{"email"}, Method: "btree", Unique: true},
					},
				},
			},
		},
	}
	_, err := v.ValidateCreate(context.Background(), sd)
	if err != nil {
		t.Errorf("clean spec should admit; got %v", err)
	}
}
