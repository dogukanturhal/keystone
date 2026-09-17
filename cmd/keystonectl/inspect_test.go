// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import "testing"

// The Snapshot→SchemaDefinitionSpec conversion and its DDL parsers now
// live in (and are tested by) the keystone-sdk schemaspec package. What
// remains here is the CLI-only flag parsing.

func TestParseSchemaSelector(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{"single", "scope=example-service", map[string]string{"scope": "example-service"}, false},
		{"two", "scope=example-service,tier=tenant", map[string]string{"scope": "example-service", "tier": "tenant"}, false},
		{"whitespace", " scope = example-service , tier = tenant ", map[string]string{"scope": "example-service", "tier": "tenant"}, false},
		{"trailing-comma", "scope=example-service,", map[string]string{"scope": "example-service"}, false},
		{"missing-equals", "scope example-service", nil, true},
		{"empty-key", "=example-service", nil, true},
		{"empty", "", nil, true},
		{"only-commas", ",,,", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSchemaSelector(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if !tt.wantErr {
				for k, v := range tt.want {
					if got[k] != v {
						t.Errorf("got[%q]=%q want %q", k, got[k], v)
					}
				}
			}
		})
	}
}
