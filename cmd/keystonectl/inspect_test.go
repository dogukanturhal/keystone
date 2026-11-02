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
		{"single", "scope=falcon-id", map[string]string{"scope": "falcon-id"}, false},
		{"two", "scope=falcon-id,tier=tenant", map[string]string{"scope": "falcon-id", "tier": "tenant"}, false},
		{"whitespace", " scope = falcon-id , tier = tenant ", map[string]string{"scope": "falcon-id", "tier": "tenant"}, false},
		{"trailing-comma", "scope=falcon-id,", map[string]string{"scope": "falcon-id"}, false},
		{"missing-equals", "scope falcon-id", nil, true},
		{"empty-key", "=falcon-id", nil, true},
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
