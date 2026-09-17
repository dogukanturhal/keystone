# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Unit tests for the MigrationBundle policy. Run with:
#   conftest verify --policy policy/conftest

package keystone.migrationbundle_test

import data.keystone.migrationbundle

# Happy path: minimal valid bundle.
test_valid_bundle if {
	count(deny_for(_valid_bundle)) == 0
}

# Missing version is rejected.
test_missing_version if {
	bundle := object.remove(_valid_bundle, ["spec"])
	bundle_with_spec := object.union(bundle, {"spec": {
		"source": {"type": "ConfigMap", "configMapRef": {"name": "x"}},
		"schemaSelector": {"matchLabels": {"module": "crm"}},
	}})
	count(deny_for(bundle_with_spec)) > 0
}

# Bad version pattern is rejected.
test_bad_version_pattern if {
	bundle := object.union(_valid_bundle, {"spec": object.union(
		_valid_bundle.spec,
		{"version": "v 1.0.0"},
	)})
	count(deny_for(bundle)) > 0
}

# Empty selector is rejected.
test_empty_selector if {
	bundle := object.union(_valid_bundle, {"spec": object.union(
		_valid_bundle.spec,
		{"schemaSelector": {}},
	)})
	count(deny_for(bundle)) > 0
}

# Paid tier requires pgroll.
test_paid_tier_requires_pgroll if {
	bundle := object.union(_valid_bundle, {"spec": object.union(
		_valid_bundle.spec,
		{
			"strategy": "versioned",
			"schemaSelector": {"matchLabels": {"tier": "paid"}},
		},
	)})
	count(deny_for(bundle)) > 0
}

deny_for(input) := violations if {
	violations := migrationbundle.deny with input as input
}

_valid_bundle := {
	"apiVersion": "keystone.hexxlock.io/v1alpha1",
	"kind": "MigrationBundle",
	"metadata": {"name": "crm-v42", "namespace": "keystone-system"},
	"spec": {
		"version": "v42",
		"strategy": "versioned",
		"source": {"type": "ConfigMap", "configMapRef": {"name": "crm-sql"}},
		"schemaSelector": {"matchLabels": {"module": "crm"}},
	},
}
