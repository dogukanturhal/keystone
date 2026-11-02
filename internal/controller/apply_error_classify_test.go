// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestClassifyApplyFailure locks in the SQLSTATE -> condition-reason mapping
// used by markFailed. The two positive cases (42P07, 42501) are exactly the
// error codes observed on the six stuck MigrationBundles investigated in
// t-XXXX (relation already exists / must be owner of relation); the
// wrapped-error case proves the classifier still works after the error has
// been through the "apply %s: %w" / "begin tx: %w" wrapping chain used
// throughout migration.Runner. The control case (a plain, non-pg error, and
// an unrelated SQLSTATE) must NOT be classified — if it were, the test
// would be meaningless.
func TestClassifyApplyFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "already exists (unwrapped)",
			err:  &pgconn.PgError{Code: "42P07", Message: `relation "overage_charges" already exists`},
			want: "ApplyFailedObjectAlreadyExists",
		},
		{
			name: "already exists (wrapped through Apply's error chain)",
			err: fmt.Errorf("apply %s: %w", "001_declarative_diff.up.sql",
				&pgconn.PgError{Code: "42P07", Message: `relation "overage_charges" already exists`}),
			want: "ApplyFailedObjectAlreadyExists",
		},
		{
			name: "insufficient privilege (unwrapped)",
			err:  &pgconn.PgError{Code: "42501", Message: "must be owner of relation iam_app_installations"},
			want: "ApplyFailedInsufficientPrivilege",
		},
		{
			name: "insufficient privilege (wrapped)",
			err: fmt.Errorf("apply %s: %w", "001_declarative_diff.up.sql",
				&pgconn.PgError{Code: "42501", Message: "must be owner of relation iam_app_installations"}),
			want: "ApplyFailedInsufficientPrivilege",
		},
		// Control: an unrelated SQLSTATE must fall through to the generic
		// reason. If this ever reports a specific reason, the switch below
		// is matching too broadly.
		{
			name: "unrelated SQLSTATE falls through to generic reason",
			err:  &pgconn.PgError{Code: "23505", Message: "duplicate key value"},
			want: "ApplyFailed",
		},
		// Control: a non-pg error (no SQLSTATE at all) must also fall
		// through, proving the classifier isn't just returning the "want"
		// value unconditionally.
		{
			name: "plain non-pg error falls through to generic reason",
			err:  errors.New("begin tx: connection refused"),
			want: "ApplyFailed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyApplyFailure(tc.err); got != tc.want {
				t.Fatalf("classifyApplyFailure(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
