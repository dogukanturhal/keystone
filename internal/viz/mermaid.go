// SPDX-License-Identifier: AGPL-3.0-or-later

// Package viz renders schema visualizations from drift snapshots.
package viz

import (
	"fmt"
	"strings"

	"github.com/dogukanturhal/keystone-sdk/go/drift"
)

// MermaidERD converts a drift.Snapshot into a Mermaid erDiagram string.
// The output is self-contained and can be rendered by any Mermaid-compatible
// frontend (HexxForge UI, GitLab markdown, etc.).
//
// Foreign-key relationships are extracted from the Constraints slice
// (type="FOREIGN KEY"). Columns are rendered with their data type and
// PK/FK/UK markers where applicable.
func MermaidERD(s *drift.Snapshot) string {
	if s == nil || len(s.Tables) == 0 {
		return "erDiagram"
	}

	// Pre-index: which columns are PKs, FKs, unique?
	pkCols := indexPKColumns(s)
	ukCols := indexUniqueColumns(s)
	fkCols := indexFKColumns(s)
	rels := extractRelationships(s)

	var b strings.Builder
	b.WriteString("erDiagram\n")

	// Render relationships first (Mermaid convention).
	for _, rel := range rels {
		fmt.Fprintf(&b, "    %s ||--o{ %s : %q\n",
			sanitizeID(rel.from), sanitizeID(rel.to), rel.label)
	}

	// Render table entities.
	for _, t := range s.Tables {
		if t.Kind != "BASE TABLE" {
			continue
		}
		fmt.Fprintf(&b, "    %s {\n", sanitizeID(t.Name))
		for _, c := range t.Columns {
			marker := columnMarker(t.Name, c.Name, pkCols, fkCols, ukCols)
			fmt.Fprintf(&b, "        %s %s%s\n",
				mermaidType(c.DataType), sanitizeID(c.Name), marker)
		}
		b.WriteString("    }\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// relationship represents one FK edge.
type relationship struct {
	from  string // referenced (parent) table
	to    string // referencing (child) table
	label string // constraint name
}

// extractRelationships parses FK constraints to build edges.
// pg_get_constraintdef output looks like:
//
//	FOREIGN KEY (col) REFERENCES parent_table(col)
//	FOREIGN KEY (col1, col2) REFERENCES parent_table(col1, col2)
func extractRelationships(s *drift.Snapshot) []relationship {
	var rels []relationship
	for _, c := range s.Constraints {
		if c.Type != "FOREIGN KEY" {
			continue
		}
		refTable := parseFKRefTable(c.Definition)
		if refTable == "" {
			continue
		}
		rels = append(rels, relationship{
			from:  refTable,
			to:    c.Table,
			label: c.Name,
		})
	}
	return rels
}

// parseFKRefTable extracts the referenced table from a FOREIGN KEY definition.
// Input: "FOREIGN KEY (order_id) REFERENCES orders(id)"
// Output: "orders"
func parseFKRefTable(def string) string {
	upper := strings.ToUpper(def)
	idx := strings.Index(upper, "REFERENCES ")
	if idx < 0 {
		return ""
	}
	rest := def[idx+len("REFERENCES "):]
	// The next token is the table name, possibly schema-qualified.
	// Strip leading whitespace.
	rest = strings.TrimSpace(rest)
	// Find the opening paren.
	paren := strings.IndexByte(rest, '(')
	if paren < 0 {
		return ""
	}
	tableName := strings.TrimSpace(rest[:paren])
	// Strip optional schema prefix (e.g., "public.orders" → "orders").
	if dot := strings.LastIndexByte(tableName, '.'); dot >= 0 {
		tableName = tableName[dot+1:]
	}
	return tableName
}

// parseFKColumns extracts the referencing column(s) from a FOREIGN KEY definition.
// Input: "FOREIGN KEY (order_id, item_id) REFERENCES orders(id, seq)"
// Output: ["order_id", "item_id"]
func parseFKColumns(def string) []string {
	upper := strings.ToUpper(def)
	fkIdx := strings.Index(upper, "FOREIGN KEY")
	if fkIdx < 0 {
		return nil
	}
	rest := def[fkIdx+len("FOREIGN KEY"):]
	open := strings.IndexByte(rest, '(')
	close := strings.IndexByte(rest, ')')
	if open < 0 || close < 0 || close <= open {
		return nil
	}
	inner := rest[open+1 : close]
	parts := strings.Split(inner, ",")
	cols := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			cols = append(cols, p)
		}
	}
	return cols
}

// indexPKColumns returns a set of "table.column" for primary key columns.
func indexPKColumns(s *drift.Snapshot) map[string]bool {
	m := make(map[string]bool)
	for _, c := range s.Constraints {
		if c.Type != "PRIMARY KEY" {
			continue
		}
		for _, col := range parseConstraintColumns(c.Definition) {
			m[c.Table+"."+col] = true
		}
	}
	return m
}

// indexUniqueColumns returns a set of "table.column" for unique constraints
// that are single-column (multi-column uniques don't get the marker).
func indexUniqueColumns(s *drift.Snapshot) map[string]bool {
	m := make(map[string]bool)
	for _, c := range s.Constraints {
		if c.Type != "UNIQUE" {
			continue
		}
		cols := parseConstraintColumns(c.Definition)
		if len(cols) == 1 {
			m[c.Table+"."+cols[0]] = true
		}
	}
	return m
}

// indexFKColumns returns a set of "table.column" for FK source columns.
func indexFKColumns(s *drift.Snapshot) map[string]bool {
	m := make(map[string]bool)
	for _, c := range s.Constraints {
		if c.Type != "FOREIGN KEY" {
			continue
		}
		for _, col := range parseFKColumns(c.Definition) {
			m[c.Table+"."+col] = true
		}
	}
	return m
}

// parseConstraintColumns extracts column names from a constraint definition.
// Works for PK and UNIQUE: "PRIMARY KEY (id)" or "UNIQUE (email)"
func parseConstraintColumns(def string) []string {
	open := strings.IndexByte(def, '(')
	close := strings.LastIndexByte(def, ')')
	if open < 0 || close < 0 || close <= open {
		return nil
	}
	inner := def[open+1 : close]
	parts := strings.Split(inner, ",")
	cols := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			cols = append(cols, p)
		}
	}
	return cols
}

// columnMarker returns a Mermaid comment suffix like " PK", " FK", " UK".
func columnMarker(table, col string, pk, fk, uk map[string]bool) string {
	key := table + "." + col
	if pk[key] {
		return " PK"
	}
	if fk[key] {
		return " FK"
	}
	if uk[key] {
		return " UK"
	}
	return ""
}

// mermaidType maps PG data_type strings to Mermaid-friendly type names.
// Mermaid erDiagram types must not contain spaces.
func mermaidType(dt string) string {
	dt = strings.TrimSpace(dt)
	switch {
	case strings.Contains(dt, "character varying"):
		return "varchar"
	case dt == "character":
		return "char"
	case dt == "integer":
		return "int"
	case dt == "bigint":
		return "bigint"
	case dt == "smallint":
		return "smallint"
	case dt == "boolean":
		return "bool"
	case dt == "text":
		return "text"
	case dt == "uuid":
		return "uuid"
	case strings.HasPrefix(dt, "timestamp"):
		return "timestamptz"
	case dt == "date":
		return "date"
	case dt == "jsonb":
		return "jsonb"
	case dt == "json":
		return "json"
	case strings.HasPrefix(dt, "numeric"):
		return "numeric"
	case dt == "bytea":
		return "bytea"
	case dt == "double precision":
		return "float8"
	case dt == "real":
		return "float4"
	case dt == "ARRAY":
		return "array"
	case dt == "USER-DEFINED":
		return "custom"
	case dt == "":
		return "unknown"
	default:
		// Replace spaces with underscores for Mermaid compatibility.
		return strings.ReplaceAll(dt, " ", "_")
	}
}

// sanitizeID ensures a Mermaid entity/attribute name is valid.
// Mermaid erDiagram identifiers must be alphanumeric + underscores.
func sanitizeID(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == '-':
			b.WriteByte('_')
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		return "_"
	}
	return s
}
