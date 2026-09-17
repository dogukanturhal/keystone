// SPDX-License-Identifier: AGPL-3.0-or-later

package approval

import (
	"testing"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func TestResult_LiftsLintErrors(t *testing.T) {
	cases := []struct {
		name string
		res  Result
		want bool
	}{
		{
			name: "no-policies",
			res:  Result{Satisfied: true},
			want: false,
		},
		{
			name: "applied-satisfied-but-no-min-severity",
			res: Result{
				Policies: []PolicyResult{
					{Applied: true, Satisfied: true, MinLintSeverity: ""},
				},
			},
			want: false,
		},
		{
			name: "applied-satisfied-with-min-severity-error",
			res: Result{
				Policies: []PolicyResult{
					{Applied: true, Satisfied: true, MinLintSeverity: keystonev1alpha1.LintLevelError},
				},
			},
			want: true,
		},
		{
			name: "applied-satisfied-with-min-severity-warning-also-covers-error",
			res: Result{
				Policies: []PolicyResult{
					{Applied: true, Satisfied: true, MinLintSeverity: keystonev1alpha1.LintLevelWarning},
				},
			},
			want: true,
		},
		{
			name: "applied-but-unsatisfied",
			res: Result{
				Policies: []PolicyResult{
					{Applied: true, Satisfied: false, MinLintSeverity: keystonev1alpha1.LintLevelError},
				},
			},
			want: false,
		},
		{
			name: "not-applied",
			res: Result{
				Policies: []PolicyResult{
					{Applied: false, Satisfied: true, MinLintSeverity: keystonev1alpha1.LintLevelError},
				},
			},
			want: false,
		},
		{
			name: "mixed-one-lift-one-no",
			res: Result{
				Policies: []PolicyResult{
					{Applied: true, Satisfied: true, MinLintSeverity: ""},
					{Applied: true, Satisfied: true, MinLintSeverity: keystonev1alpha1.LintLevelError},
				},
			},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.res.LiftsLintErrors(); got != c.want {
				t.Errorf("LiftsLintErrors() = %v, want %v", got, c.want)
			}
		})
	}
}

