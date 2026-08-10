package budget

import (
	"testing"
	"time"

	"github.com/SuperMarioYL/tokenctl/internal/config"
)

// TestTree_ResetPolicyTable is the m4 regression consolidating the shipped
// feat_budget_reset_policy contract — hard / rollover / grace — into one
// table-driven pin. For each policy it spends a known amount, forces a window
// rollover by backdating (no sleeping), and asserts the post-rollover (carry,
// consumed) state plus that the next admit's outcome reflects the policy:
//
//   - hard       — consumed zeroed, no carry, fresh window admits.
//   - rollover   — min(unspent, N% of ceiling) carried as headroom, consumed
//     zeroed, fresh window admits (the carry offsets the admit gate, consumed
//     first).
//   - grace (inside the grace_period) — the old window's ceiling stays live:
//     consumed is NOT reset.
//   - grace (past the grace_period) — the reset fires: consumed zeroed.
//
// It reuses the resetNode + backdate helpers from reset_policy_test.go. The
// per-policy depth (carry semantics, one-window-only, grace boundary) is
// covered by the dedicated tests there; this table pins the observable
// post-rollover contract for all three modes in one place.
func TestTree_ResetPolicyTable(t *testing.T) {
	cases := []struct {
		name        string
		policy      *config.ResetPolicy
		preSpend    int64
		backdate    time.Duration // how far past the 50ms window to backdate
		wantCarry   int64
		wantConsume int64 // consumed after rollover
	}{
		{
			name:     "hard_zeroes_consumed_no_carry",
			policy:   &config.ResetPolicy{Mode: "hard"},
			preSpend: 900, backdate: 200 * time.Millisecond,
			wantCarry: 0, wantConsume: 0,
		},
		{
			// 400 spent, 600 unspent, 50% cap -> carry min(600, 500) = 500.
			name:     "rollover_carries_min_unspent_cap",
			policy:   &config.ResetPolicy{Mode: "rollover", RolloverCapPct: 50},
			preSpend: 400, backdate: 200 * time.Millisecond,
			wantCarry: 500, wantConsume: 0,
		},
		{
			// Tiny spend, 990 unspent, but the 50% cap limits the carry to 500
			// (not 990) — the cap, not the unspent total, is the binding value.
			name:     "rollover_cap_not_unspent_total",
			policy:   &config.ResetPolicy{Mode: "rollover", RolloverCapPct: 50},
			preSpend: 10, backdate: 200 * time.Millisecond,
			wantCarry: 500, wantConsume: 0,
		},
		{
			// Inside the grace_period (50ms window + 100ms grace = 150ms
			// boundary): backdate 80ms is past the window but NOT past the
			// grace boundary, so the old window's ceiling stays live.
			name:     "grace_inside_keeps_old_window_live",
			policy:   &config.ResetPolicy{Mode: "grace", GracePeriod: "100ms"},
			preSpend: 700, backdate: 80 * time.Millisecond,
			wantCarry: 0, wantConsume: 700,
		},
		{
			// Past the grace boundary (200ms > 150ms): the reset fires.
			name:     "grace_past_boundary_resets",
			policy:   &config.ResetPolicy{Mode: "grace", GracePeriod: "100ms"},
			preSpend: 700, backdate: 200 * time.Millisecond,
			wantCarry: 0, wantConsume: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := resetNode(t, tc.policy)
			leaf := tr.leafByPath["org"]

			// Pre-spend so the window has a known consumption before rollover.
			adm, err := tr.Admit("k", "claude", "")
			if err != nil {
				t.Fatalf("pre-spend Admit: %v", err)
			}
			adm.AddInput(tc.preSpend)
			adm.Release()

			// Drive a coherent rollover by backdating the leaf past its window
			// (and, for grace, past/inside the grace boundary) then rolling the
			// whole tree against one now.
			backdate(leaf, tc.backdate)
			tr.rolloverAll(time.Now())

			if leaf.carry != tc.wantCarry {
				t.Errorf("post-rollover carry = %d, want %d", leaf.carry, tc.wantCarry)
			}
			if c, _ := leaf.snapshotConsumed(); c != tc.wantConsume {
				t.Errorf("post-rollover consumed = %d, want %d", c, tc.wantConsume)
			}

			// The next admit must succeed: hard/rollover reset to 0 (carry only
			// offsets the gate, it does not block admit), and grace-inside is
			// at 700/1000 (70%) — under the 0.999 soft threshold resetNode
			// uses. This pins that a rollover never leaves the leaf unable to
			// admit fresh traffic.
			adm2, err := tr.Admit("k", "claude", "")
			if err != nil {
				t.Fatalf("post-rollover Admit err = %v, want nil (fresh window must admit)", err)
			}
			adm2.Release()
		})
	}
}
