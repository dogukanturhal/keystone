// SPDX-License-Identifier: AGPL-3.0-or-later

package pgroll

import keystonev1alpha1 "github.com/dogukanturhal/keystone/api/v1alpha1"

// PlanWaves groups pgroll operations into execution waves where each
// wave's operations target distinct Tables and can therefore run in
// parallel without PostgreSQL lock contention on the same relation.
//
// Algorithm: a greedy earliest-placement. Walk operations in bundle
// order; for each op, find the first wave that does not already
// contain another op on the same Table. Placing ops in the earliest
// safe wave keeps the critical path short without constructing the
// full dependency DAG (which would add code for no measurable win at
// bundle sizes ≤ the CRD's MaxItems=16 cap).
//
// Ordering guarantees:
//
//   - For any two ops A and B where A precedes B in the input and
//     both target the same Table, A's wave index is strictly less
//     than B's wave index. This preserves the authoring intent:
//     "add column then backfill then add NOT NULL" stays ordered
//     even when other unrelated ops are parallelised around it.
//   - Within a single wave, the relative order of ops is the
//     original bundle order. Concurrent execution observes the
//     order only up to commit ordering; the invariant is that ops
//     in the same wave are independent (different Tables), so
//     commit order doesn't matter.
//
// Returns a slice of waves; each wave is a slice of op indexes into
// the original ops list. For an empty input PlanWaves returns nil.
//
// Complexity: O(n·k) where n is the number of ops and k is the
// average wave count. With the MaxItems=16 bundle cap, n ≤ 16 so
// this is trivially bounded.
func PlanWaves(ops []keystonev1alpha1.MigrationOperation) [][]int {
	if len(ops) == 0 {
		return nil
	}
	var waves [][]int
	// tablesByWave[i] is the set of tables already occupied in wave i.
	var tablesByWave []map[string]bool

	for i, op := range ops {
		placed := false
		for w := range waves {
			if !tablesByWave[w][op.Table] {
				waves[w] = append(waves[w], i)
				tablesByWave[w][op.Table] = true
				placed = true
				break
			}
		}
		if !placed {
			// None of the existing waves accommodate this op — open a
			// new wave.
			waves = append(waves, []int{i})
			tablesByWave = append(tablesByWave, map[string]bool{op.Table: true})
		}
	}
	return waves
}

// EffectiveParallelism collapses the CRD-level knob into the actual
// goroutine cap the controller will use. Rules:
//
//   - 0 or 1 → sequential (no errgroup, straight for-loop).
//   - 2+    → that many concurrent goroutines within a wave.
//
// Returned value is capped at the wave's size so we never spawn
// idle goroutines. Matches the intuition that "parallelism=16 with a
// 3-op wave" is 3 goroutines, not 16.
func EffectiveParallelism(requested int32, waveSize int) int {
	if requested <= 1 {
		return 1
	}
	cap := int(requested)
	if cap > waveSize {
		return waveSize
	}
	return cap
}
