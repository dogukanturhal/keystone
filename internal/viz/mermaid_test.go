// SPDX-License-Identifier: AGPL-3.0-or-later

package viz

import (
	"strings"
	"testing"

	"github.com/dogukanturhal/keystone-sdk/go/drift"
)

func TestMermaidERD_Nil(t *testing.T) {
	got := MermaidERD(nil)
	if got != "erDiagram" {
		t.Errorf("nil snapshot: got %q, want %q", got, "erDiagram")
	}
}

func TestMermaidERD_Empty(t *testing.T) {
	got := MermaidERD(&drift.Snapshot{Schema: "public"})
	if got != "erDiagram" {
		t.Errorf("empty snapshot: got %q, want %q", got, "erDiagram")
	}
}

func TestMermaidERD_SingleTable(t *testing.T) {
	s := &drift.Snapshot{
		Schema: "billing",
		Tables: []drift.TableShape{
			{
				Name: "invoices",
				Kind: "BASE TABLE",
				Columns: []drift.ColumnShape{
					{Name: "id", Ordinal: 1, DataType: "uuid", Nullable: false},
					{Name: "amount", Ordinal: 2, DataType: "numeric", Nullable: false},
					{Name: "created_at", Ordinal: 3, DataType: "timestamp with time zone", Nullable: false},
				},
			},
		},
		Constraints: []drift.ObjectDDL{
			{Name: "invoices_pkey", Table: "invoices", Type: "PRIMARY KEY", Definition: "PRIMARY KEY (id)"},
		},
	}

	got := MermaidERD(s)

	if !strings.Contains(got, "erDiagram") {
		t.Error("missing erDiagram header")
	}
	if !strings.Contains(got, "invoices {") {
		t.Error("missing invoices entity")
	}
	if !strings.Contains(got, "uuid id PK") {
		t.Errorf("missing PK marker for id column; got:\n%s", got)
	}
	if !strings.Contains(got, "numeric amount") {
		t.Errorf("missing amount column; got:\n%s", got)
	}
	if !strings.Contains(got, "timestamptz created_at") {
		t.Errorf("missing timestamp mapping; got:\n%s", got)
	}
}

func TestMermaidERD_ForeignKeys(t *testing.T) {
	s := &drift.Snapshot{
		Schema: "sales",
		Tables: []drift.TableShape{
			{
				Name: "customers",
				Kind: "BASE TABLE",
				Columns: []drift.ColumnShape{
					{Name: "id", Ordinal: 1, DataType: "uuid"},
					{Name: "name", Ordinal: 2, DataType: "text"},
				},
			},
			{
				Name: "orders",
				Kind: "BASE TABLE",
				Columns: []drift.ColumnShape{
					{Name: "id", Ordinal: 1, DataType: "uuid"},
					{Name: "customer_id", Ordinal: 2, DataType: "uuid"},
					{Name: "total", Ordinal: 3, DataType: "numeric"},
				},
			},
		},
		Constraints: []drift.ObjectDDL{
			{Name: "customers_pkey", Table: "customers", Type: "PRIMARY KEY", Definition: "PRIMARY KEY (id)"},
			{Name: "orders_pkey", Table: "orders", Type: "PRIMARY KEY", Definition: "PRIMARY KEY (id)"},
			{Name: "orders_customer_fk", Table: "orders", Type: "FOREIGN KEY",
				Definition: "FOREIGN KEY (customer_id) REFERENCES customers(id)"},
		},
	}

	got := MermaidERD(s)

	// Relationship line.
	if !strings.Contains(got, `customers ||--o{ orders : "orders_customer_fk"`) {
		t.Errorf("missing FK relationship; got:\n%s", got)
	}
	// FK marker on customer_id.
	if !strings.Contains(got, "uuid customer_id FK") {
		t.Errorf("missing FK marker on customer_id; got:\n%s", got)
	}
}

func TestMermaidERD_SkipsViews(t *testing.T) {
	s := &drift.Snapshot{
		Schema: "app",
		Tables: []drift.TableShape{
			{Name: "users", Kind: "BASE TABLE", Columns: []drift.ColumnShape{
				{Name: "id", Ordinal: 1, DataType: "uuid"},
			}},
			{Name: "active_users", Kind: "VIEW", Columns: []drift.ColumnShape{
				{Name: "id", Ordinal: 1, DataType: "uuid"},
			}},
		},
	}

	got := MermaidERD(s)

	if !strings.Contains(got, "users {") {
		t.Error("missing users entity")
	}
	if strings.Contains(got, "active_users") {
		t.Errorf("views should be excluded; got:\n%s", got)
	}
}

func TestMermaidERD_UniqueConstraint(t *testing.T) {
	s := &drift.Snapshot{
		Schema: "auth",
		Tables: []drift.TableShape{
			{Name: "users", Kind: "BASE TABLE", Columns: []drift.ColumnShape{
				{Name: "id", Ordinal: 1, DataType: "uuid"},
				{Name: "email", Ordinal: 2, DataType: "text"},
			}},
		},
		Constraints: []drift.ObjectDDL{
			{Name: "users_pkey", Table: "users", Type: "PRIMARY KEY", Definition: "PRIMARY KEY (id)"},
			{Name: "users_email_key", Table: "users", Type: "UNIQUE", Definition: "UNIQUE (email)"},
		},
	}

	got := MermaidERD(s)

	if !strings.Contains(got, "text email UK") {
		t.Errorf("missing UK marker; got:\n%s", got)
	}
}

func TestMermaidType(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"character varying", "varchar"},
		{"integer", "int"},
		{"bigint", "bigint"},
		{"boolean", "bool"},
		{"double precision", "float8"},
		{"timestamp without time zone", "timestamptz"},
		{"ARRAY", "array"},
		{"USER-DEFINED", "custom"},
		{"", "unknown"},
		{"some other type", "some_other_type"},
	}
	for _, tt := range tests {
		got := mermaidType(tt.input)
		if got != tt.want {
			t.Errorf("mermaidType(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSanitizeID(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"users", "users"},
		{"user-roles", "user_roles"},
		{"table.name", "table_name"},
		{"", "_"},
	}
	for _, tt := range tests {
		got := sanitizeID(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseFKRefTable(t *testing.T) {
	tests := []struct {
		def, want string
	}{
		{"FOREIGN KEY (customer_id) REFERENCES customers(id)", "customers"},
		{"FOREIGN KEY (a, b) REFERENCES public.orders(c, d)", "orders"},
		{"CHECK (amount > 0)", ""},
	}
	for _, tt := range tests {
		got := parseFKRefTable(tt.def)
		if got != tt.want {
			t.Errorf("parseFKRefTable(%q) = %q, want %q", tt.def, got, tt.want)
		}
	}
}

func TestParseFKColumns(t *testing.T) {
	tests := []struct {
		def  string
		want []string
	}{
		{"FOREIGN KEY (customer_id) REFERENCES customers(id)", []string{"customer_id"}},
		{"FOREIGN KEY (a, b) REFERENCES orders(c, d)", []string{"a", "b"}},
	}
	for _, tt := range tests {
		got := parseFKColumns(tt.def)
		if len(got) != len(tt.want) {
			t.Errorf("parseFKColumns(%q) len = %d, want %d", tt.def, len(got), len(tt.want))
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseFKColumns(%q)[%d] = %q, want %q", tt.def, i, got[i], tt.want[i])
			}
		}
	}
}

func TestMermaidERD_MultiTableWithRelationships(t *testing.T) {
	s := &drift.Snapshot{
		Schema: "erp",
		Tables: []drift.TableShape{
			{Name: "tenants", Kind: "BASE TABLE", Columns: []drift.ColumnShape{
				{Name: "id", Ordinal: 1, DataType: "uuid"},
				{Name: "name", Ordinal: 2, DataType: "text"},
			}},
			{Name: "products", Kind: "BASE TABLE", Columns: []drift.ColumnShape{
				{Name: "id", Ordinal: 1, DataType: "uuid"},
				{Name: "tenant_id", Ordinal: 2, DataType: "uuid"},
				{Name: "sku", Ordinal: 3, DataType: "character varying"},
				{Name: "price", Ordinal: 4, DataType: "numeric"},
			}},
			{Name: "order_items", Kind: "BASE TABLE", Columns: []drift.ColumnShape{
				{Name: "id", Ordinal: 1, DataType: "uuid"},
				{Name: "product_id", Ordinal: 2, DataType: "uuid"},
				{Name: "quantity", Ordinal: 3, DataType: "integer"},
			}},
		},
		Constraints: []drift.ObjectDDL{
			{Name: "tenants_pkey", Table: "tenants", Type: "PRIMARY KEY", Definition: "PRIMARY KEY (id)"},
			{Name: "products_pkey", Table: "products", Type: "PRIMARY KEY", Definition: "PRIMARY KEY (id)"},
			{Name: "order_items_pkey", Table: "order_items", Type: "PRIMARY KEY", Definition: "PRIMARY KEY (id)"},
			{Name: "products_tenant_fk", Table: "products", Type: "FOREIGN KEY",
				Definition: "FOREIGN KEY (tenant_id) REFERENCES tenants(id)"},
			{Name: "order_items_product_fk", Table: "order_items", Type: "FOREIGN KEY",
				Definition: "FOREIGN KEY (product_id) REFERENCES products(id)"},
			{Name: "products_sku_key", Table: "products", Type: "UNIQUE", Definition: "UNIQUE (sku)"},
		},
	}

	got := MermaidERD(s)

	// Verify all three relationships.
	if !strings.Contains(got, `tenants ||--o{ products`) {
		t.Errorf("missing tenants→products relationship; got:\n%s", got)
	}
	if !strings.Contains(got, `products ||--o{ order_items`) {
		t.Errorf("missing products→order_items relationship; got:\n%s", got)
	}
	// Verify markers.
	if !strings.Contains(got, "varchar sku UK") {
		t.Errorf("missing UK on sku; got:\n%s", got)
	}
	if !strings.Contains(got, "int quantity") {
		t.Errorf("missing quantity column; got:\n%s", got)
	}

	// Verify well-formed output (no trailing newlines, proper structure).
	lines := strings.Split(got, "\n")
	if lines[0] != "erDiagram" {
		t.Errorf("first line should be erDiagram, got %q", lines[0])
	}
}
