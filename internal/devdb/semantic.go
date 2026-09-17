// SPDX-License-Identifier: AGPL-3.0-or-later

package devdb

import (
	"context"
	"fmt"
	"strings"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
	"github.com/dogukanturhal/keystone-sdk/go/drift"
	"github.com/dogukanturhal/keystone-sdk/go/analyze"
)

// SemanticAnalyzer replays migrations against an ephemeral dev database
// and inspects the resulting schema to detect issues that static AST
// analysis cannot catch:
//
//   - SQL syntax errors that only manifest at execution time
//   - Invalid DEFAULT expressions (e.g., calling a non-existent function)
//   - Type mismatches in FK references
//   - Implicit type coercions that silently truncate data
//   - Constraints that reference non-existent columns
//   - CREATE INDEX on non-existent columns
//   - Schema objects left in an inconsistent state
//
// The analyzer implements the analyze.Analyzer interface so it can be
// registered alongside the 51 static analyzers. However, it requires
// a DevDatabasePool, so it's constructed separately and only runs when
// a dev database is configured (via SchemaPolicy.spec.devDatabaseRef).
type SemanticAnalyzer struct {
	pool *Pool
}

// NewSemanticAnalyzer creates an analyzer bound to the given dev pool.
func NewSemanticAnalyzer(pool *Pool) *SemanticAnalyzer {
	return &SemanticAnalyzer{pool: pool}
}

// ID returns the stable rule identifier.
func (a *SemanticAnalyzer) ID() string { return "semantic-replay" }

// Description returns a short explanation.
func (a *SemanticAnalyzer) Description() string {
	return "replays migrations against an ephemeral dev database to detect semantic errors"
}

// Check replays the migration's SQL files against an ephemeral schema
// and reports any execution failures or structural anomalies.
func (a *SemanticAnalyzer) Check(ctx context.Context, m *analyze.Migration) ([]analyze.Finding, error) {
	if a.pool == nil {
		return nil, nil
	}
	if len(m.Files) == 0 {
		return nil, nil
	}

	session, err := a.pool.Acquire(ctx)
	if err != nil {
		return []analyze.Finding{{
			Rule:     a.ID(),
			Severity: keystonev1alpha1.LintLevelWarning,
			Message:  fmt.Sprintf("failed to acquire dev database session: %v", err),
		}}, nil
	}
	defer func() { _ = session.Release(ctx) }()

	var findings []analyze.Finding

	// Replay each migration file in order.
	for _, f := range m.Files {
		if err := session.ReplaySQL(ctx, f.Body); err != nil {
			findings = append(findings, analyze.Finding{
				Rule:     a.ID(),
				Severity: keystonev1alpha1.LintLevelError,
				File:     f.Name,
				Message:  fmt.Sprintf("migration failed on dev database: %v", truncateError(err)),
			})
			// Stop replaying — subsequent files may depend on this one.
			return findings, nil
		}
	}

	// All files replayed successfully. Inspect the resulting schema
	// and run semantic checks on the outcome.
	snapshot, err := session.Inspect(ctx)
	if err != nil {
		findings = append(findings, analyze.Finding{
			Rule:     a.ID(),
			Severity: keystonev1alpha1.LintLevelWarning,
			Message:  fmt.Sprintf("dev database inspection failed after replay: %v", err),
		})
		return findings, nil
	}

	// Semantic checks on the resulting schema.
	findings = append(findings, checkOrphanedFKs(snapshot, m)...)
	findings = append(findings, checkEmptyTables(snapshot, m)...)

	return findings, nil
}

// checkOrphanedFKs detects foreign keys referencing tables or columns
// that don't exist in the resulting schema.
func checkOrphanedFKs(snap *drift.Snapshot, m *analyze.Migration) []analyze.Finding {
	tableNames := make(map[string]bool, len(snap.Tables))
	for _, t := range snap.Tables {
		tableNames[t.Name] = true
	}

	var findings []analyze.Finding
	for _, c := range snap.Constraints {
		if c.Type != "FOREIGN KEY" {
			continue
		}
		// Parse the REFERENCES clause to find the target table.
		upper := strings.ToUpper(c.Definition)
		refIdx := strings.Index(upper, "REFERENCES ")
		if refIdx < 0 {
			continue
		}
		rest := c.Definition[refIdx+len("REFERENCES "):]
		paren := strings.IndexByte(rest, '(')
		if paren < 0 {
			continue
		}
		refTable := strings.TrimSpace(rest[:paren])
		// Strip schema prefix if present.
		if dot := strings.LastIndexByte(refTable, '.'); dot >= 0 {
			refTable = refTable[dot+1:]
		}
		if !tableNames[refTable] {
			findings = append(findings, analyze.Finding{
				Rule:     "semantic-orphaned-fk",
				Severity: keystonev1alpha1.LintLevelError,
				Message: fmt.Sprintf(
					"constraint %s on table %s references non-existent table %s",
					c.Name, c.Table, refTable),
			})
		}
	}
	return findings
}

// checkEmptyTables detects tables with zero columns, which can happen
// from malformed CREATE TABLE statements.
func checkEmptyTables(snap *drift.Snapshot, m *analyze.Migration) []analyze.Finding {
	var findings []analyze.Finding
	for _, t := range snap.Tables {
		if t.Kind == "BASE TABLE" && len(t.Columns) == 0 {
			findings = append(findings, analyze.Finding{
				Rule:     "semantic-empty-table",
				Severity: keystonev1alpha1.LintLevelWarning,
				Message:  fmt.Sprintf("table %s has zero columns after migration replay", t.Name),
			})
		}
	}
	return findings
}

// truncateError limits error messages to avoid bloating CRD status.
func truncateError(err error) string {
	msg := err.Error()
	if len(msg) > 512 {
		return msg[:509] + "..."
	}
	return msg
}
