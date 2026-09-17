// SPDX-License-Identifier: AGPL-3.0-or-later

package pgroll

import (
	"reflect"
	"testing"

	keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"
)

func op(table string) keystonev1alpha1.MigrationOperation {
	return keystonev1alpha1.MigrationOperation{
		Kind:  keystonev1alpha1.MigrationOperationAddColumn,
		Table: table,
	}
}

func TestPlanWaves_AllDistinctTables(t *testing.T) {
	// Every op targets a different table → everything fits in one wave.
	ops := []keystonev1alpha1.MigrationOperation{op("a"), op("b"), op("c")}
	got := PlanWaves(ops)
	want := [][]int{{0, 1, 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestPlanWaves_AllSameTable(t *testing.T) {
	// Every op targets the same table → N waves of 1 each.
	ops := []keystonev1alpha1.MigrationOperation{op("users"), op("users"), op("users")}
	got := PlanWaves(ops)
	want := [][]int{{0}, {1}, {2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestPlanWaves_MixedGreedyPacking(t *testing.T) {
	// 5 ops: [users, users, orders, users, orders]
	// Wave 1: users[0], orders[2]
	// Wave 2: users[1], orders[4]
	// Wave 3: users[3]
	ops := []keystonev1alpha1.MigrationOperation{
		op("users"), op("users"), op("orders"), op("users"), op("orders"),
	}
	got := PlanWaves(ops)
	want := [][]int{{0, 2}, {1, 4}, {3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestPlanWaves_SameTableOrderPreserved(t *testing.T) {
	// Critical invariant: among ops on the same table, wave index
	// strictly increases. A=users, B=users, C=users ⇒ A.wave < B.wave < C.wave.
	ops := []keystonev1alpha1.MigrationOperation{
		op("users"), op("orders"), op("users"), op("payments"), op("users"),
	}
	got := PlanWaves(ops)
	// Walk users ops and verify monotonic wave indexes.
	var userWaves []int
	for waveIdx, wave := range got {
		for _, opIdx := range wave {
			if ops[opIdx].Table == "users" {
				userWaves = append(userWaves, waveIdx)
			}
		}
	}
	if len(userWaves) != 3 {
		t.Fatalf("expected 3 users ops, got wave-indexes %v", userWaves)
	}
	for i := 1; i < len(userWaves); i++ {
		if userWaves[i] <= userWaves[i-1] {
			t.Errorf("same-table ops must land in strictly increasing waves; got %v", userWaves)
		}
	}
}

func TestPlanWaves_EmptyInput(t *testing.T) {
	if got := PlanWaves(nil); got != nil {
		t.Errorf("nil input should return nil; got %v", got)
	}
	if got := PlanWaves([]keystonev1alpha1.MigrationOperation{}); got != nil {
		t.Errorf("empty slice should return nil; got %v", got)
	}
}

func TestPlanWaves_SingleOp(t *testing.T) {
	got := PlanWaves([]keystonev1alpha1.MigrationOperation{op("a")})
	want := [][]int{{0}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestEffectiveParallelism(t *testing.T) {
	cases := []struct {
		name      string
		requested int32
		waveSize  int
		want      int
	}{
		{"sequential zero", 0, 5, 1},
		{"sequential one", 1, 5, 1},
		{"cap below wave", 3, 5, 3},
		{"cap above wave", 16, 3, 3},
		{"cap equal wave", 5, 5, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveParallelism(tc.requested, tc.waveSize); got != tc.want {
				t.Errorf("requested=%d wave=%d got=%d want=%d",
					tc.requested, tc.waveSize, got, tc.want)
			}
		})
	}
}
