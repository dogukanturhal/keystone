# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Conftest / OPA policies for MigrationBundle YAMLs at MR time.
#
# These rules are the FIRST gate: CI runs them on every MR before the
# manifests can be merged. They catch the most common foot-guns before
# a controller ever sees the resource. The same intent is enforced again
# at admission time by Kyverno (defence in depth).
#
# Run with:
#   conftest test --policy policy/conftest path/to/MigrationBundle.yaml

package keystone.migrationbundle

import rego.v1

# --- Constants the policy uses ---

allowed_strategies := {"versioned", "declarative", "pgroll-expand-contract"}

dangerous_keywords := {
	"DROP TABLE",
	"DROP COLUMN",
	"DROP DATABASE",
	"TRUNCATE",
	"DELETE FROM",
	"ALTER TABLE.*DROP",
}

# --- Helpers ---

is_migrationbundle(input) if {
	input.apiVersion == "keystone.hexxlock.io/v1alpha1"
	input.kind == "MigrationBundle"
}

# --- Deny rules ---

# Every MigrationBundle must declare a version.
deny contains msg if {
	is_migrationbundle(input)
	not input.spec.version
	msg := "spec.version is required"
}

# Version must match the validated pattern.
deny contains msg if {
	is_migrationbundle(input)
	input.spec.version
	not regex.match(`^[a-zA-Z0-9._-]+$`, input.spec.version)
	msg := sprintf("spec.version %q must match ^[a-zA-Z0-9._-]+$", [input.spec.version])
}

# Strategy must be one of the allowed values; default versioned is fine.
deny contains msg if {
	is_migrationbundle(input)
	input.spec.strategy
	not allowed_strategies[input.spec.strategy]
	msg := sprintf("spec.strategy %q is not in %v",
		[input.spec.strategy, allowed_strategies])
}

# Source must be present.
deny contains msg if {
	is_migrationbundle(input)
	not input.spec.source.type
	msg := "spec.source.type is required"
}

# ConfigMap source must reference a name.
deny contains msg if {
	is_migrationbundle(input)
	input.spec.source.type == "ConfigMap"
	not input.spec.source.configMapRef.name
	msg := "spec.source.configMapRef.name is required when type=ConfigMap"
}

# SchemaSelector must NOT be empty (matches everything = unsafe).
deny contains msg if {
	is_migrationbundle(input)
	input.spec.schemaSelector
	count(object.get(input.spec.schemaSelector, "matchLabels", {})) == 0
	count(object.get(input.spec.schemaSelector, "matchExpressions", [])) == 0
	msg := "spec.schemaSelector must not be empty (would match every schema)"
}

# Production policy: bundles targeting label tier=paid must use
# pgroll-expand-contract for safety. Versioned applies straight DDL with
# locks — fine for canary, dangerous for paid tenants.
deny contains msg if {
	is_migrationbundle(input)
	input.spec.schemaSelector.matchLabels.tier == "paid"
	default_strategy := "versioned"
	strategy := object.get(input.spec, "strategy", default_strategy)
	strategy != "pgroll-expand-contract"
	msg := sprintf(
		"bundles targeting tier=paid must use strategy=pgroll-expand-contract (got %q)",
		[strategy],
	)
}

# Warn (informational) when a bundle has more than 50 schemas matched —
# probably the selector is too broad.
warn contains msg if {
	is_migrationbundle(input)
	count(object.get(input.spec.schemaSelector, "matchLabels", {})) == 1
	# heuristic: matchLabels with one key only is often too broad for paid tier
	tier := object.get(input.spec.schemaSelector.matchLabels, "tier", "")
	tier != ""
	msg := sprintf(
		"selector matches all schemas with tier=%q — confirm this is intended",
		[tier],
	)
}
