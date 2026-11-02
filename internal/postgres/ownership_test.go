// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"strings"
	"testing"
)

func TestAlterOwnerStatement_KeywordPerKind(t *testing.T) {
	cases := map[string]string{
		"table":             "ALTER TABLE public.t OWNER TO \"own\"",
		"partitioned_table": "ALTER TABLE public.t OWNER TO \"own\"",
		"view":              "ALTER VIEW public.t OWNER TO \"own\"",
		"materialized_view": "ALTER MATERIALIZED VIEW public.t OWNER TO \"own\"",
		"foreign_table":     "ALTER FOREIGN TABLE public.t OWNER TO \"own\"",
		"sequence":          "ALTER SEQUENCE public.t OWNER TO \"own\"",
		"function":          "ALTER ROUTINE public.t OWNER TO \"own\"",
		"procedure":         "ALTER ROUTINE public.t OWNER TO \"own\"",
		"aggregate":         "ALTER ROUTINE public.t OWNER TO \"own\"",
		"window_function":   "ALTER ROUTINE public.t OWNER TO \"own\"",
		"domain":            "ALTER DOMAIN public.t OWNER TO \"own\"",
		"enum":              "ALTER TYPE public.t OWNER TO \"own\"",
		"composite_type":    "ALTER TYPE public.t OWNER TO \"own\"",
		"range":             "ALTER TYPE public.t OWNER TO \"own\"",
		"base_type":         "ALTER TYPE public.t OWNER TO \"own\"",
	}
	for kind, want := range cases {
		got, err := alterOwnerStatement(kind, "public.t", "own")
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", kind, got, err, want)
		}
	}
	if _, err := alterOwnerStatement("schema", "x", "own"); err == nil {
		t.Error("schema must have no ALTER statement")
	}
}

func TestAlterOwnerStatement_QuotesOwner(t *testing.T) {
	got, _ := alterOwnerStatement("table", "public.t", `o"; DROP TABLE x; --`)
	if strings.Contains(got, `o"; DROP`) || !strings.Contains(got, `"o""; DROP TABLE x; --"`) {
		t.Errorf("owner not quoted: %s", got)
	}
}

func TestCandidateRefusal(t *testing.T) {
	cases := []struct {
		name  string
		c     Candidate
		super bool
		want  string
	}{
		{"plain table", Candidate{Kind: "table"}, false, ""},
		{"function", Candidate{Kind: "function"}, false, ""},
		{"schema", Candidate{Kind: "schema"}, false, SkipSchema},
		{"owned sequence", Candidate{Kind: "sequence", FollowsParent: true}, false, SkipOwnedSeq},
		{"standalone sequence", Candidate{Kind: "sequence"}, false, ""},
		{"rls unforced", Candidate{Kind: "table", RLSUnforced: true}, false, SkipRLS},
		{"security definer", Candidate{Kind: "function", SecDef: true}, false, SkipSecDef},
		{"rls dependent", Candidate{Kind: "view", RLSDependent: true}, false, SkipRLSDependent},
		{"rls unforced wins over dependent", Candidate{Kind: "table", RLSUnforced: true, RLSDependent: true}, false, SkipRLS},
		{"superuser connection", Candidate{Kind: "table"}, true, SkipSuperuser},
	}
	for _, tc := range cases {
		if got := tc.c.Refusal(tc.super); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestReportCounts(t *testing.T) {
	rep := OwnershipReport{Groups: []OwnershipGroup{
		{Kind: "table", Owner: "ks", ConnOwned: true, Count: 4, Blocked: 1},
		{Kind: "function", Owner: "postgres", Count: 64},
		{Kind: "schema", Owner: "ks", ConnOwned: true, Count: 1},
	}}
	if got := rep.Total(); got != 69 {
		t.Errorf("Total=%d want 69", got)
	}
	if got := rep.RemediableCount(); got != 3 {
		t.Errorf("RemediableCount=%d want 3 (schema and foreign-owned never count)", got)
	}
	rep.ConnRoleSuper = true
	if got := rep.RemediableCount(); got != 0 {
		t.Errorf("superuser connection role must report 0 remediable, got %d", got)
	}
}

func TestReportTotal_ExactBeyondGroupCap(t *testing.T) {
	rep := OwnershipReport{TotalDrifted: 500, Groups: []OwnershipGroup{{Kind: "table", Count: 3}}}
	if got := rep.Total(); got != 500 {
		t.Errorf("Total=%d want exact 500 beyond the capped group list", got)
	}
}

func TestRelkindOf(t *testing.T) {
	for kind, want := range map[string]string{
		"table": "r", "partitioned_table": "p", "view": "v", "materialized_view": "m",
		"foreign_table": "f", "sequence": "S", "function": "", "enum": "",
	} {
		if got := relkindOf(kind); got != want {
			t.Errorf("relkindOf(%q)=%q want %q", kind, got, want)
		}
	}
}

func TestOwnershipQueryArgs_NeverNil(t *testing.T) {
	a := OwnershipQuery{ExpectedOwner: "own"}.args(int32(5))
	if a[0] == nil || a[1] == nil || a[3] == nil {
		t.Errorf("array params must be non-nil empty slices: %#v", a)
	}
}
