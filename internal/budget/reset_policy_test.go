package budget

import (
	"errors"
	"testing"
	"time"

	"github.com/SuperMarioYL/tokenctl/internal/config"
)

// resetNode builds a single-leaf tree node with a 50ms window and the supplied
// reset policy, disabling the arbiter so rollover is driven deterministically
// by rolloverAll + backdating (mirroring TestTree_CoherentWindowRollover).
func resetNode(t *testing.T, p *config.ResetPolicy) *Tree {
	t.Helper()
	root := &config.GroupConfig{
		Name:        "org",
		Weight:      1,
		Budget:      &config.TokenBudget{Tokens: 1000, Window: "50ms", SoftThrottleAt: 0.999},
		ResetPolicy: p,
	}
	tr := newTestTree(t, root)
	if err := tr.Bind("k", "org"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return tr
}

func backdate(n *node, d time.Duration) {
	n.mu.Lock()
	n.windowStart = time.Now().Add(-d)
	n.mu.Unlock()
}

// TestTree_RolloverPolicyCarriesUnspent is the feat_budget_reset_policy
// headline: a node with rollover_cap_pct: 50 carries min(unspent, 50% of
// ceiling) into the next window, that carry is consumed before the window-2
// ceiling, the carry does not leak past one window, and the budget still
// hard-denies at the ceiling.
func TestTree_RolloverPolicyCarriesUnspent(t *testing.T) {
	tr := resetNode(t, &config.ResetPolicy{Mode: "rollover", RolloverCapPct: 50})
	leaf := tr.leafByPath["org"]

	// Window 1: consume 400, unspent 600 → carry into window 2 = min(600, 500) = 500.
	adm, err := tr.Admit("k", "claude", "")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	adm.AddInput(400)
	adm.Release()

	backdate(leaf, 200*time.Millisecond)
	tr.rolloverAll(time.Now())

	if leaf.carry != 500 {
		t.Fatalf("window-2 carry = %d, want 500 (min(600 unspent, 500 cap))", leaf.carry)
	}

	// Window 2: the carry is consumed before the ceiling. At 500 consumed the
	// effective load is 0 (500 - 500 carry), well under the 1000 ceiling.
	adm2, err := tr.Admit("k", "claude", "")
	if err != nil {
		t.Fatalf("Admit window 2: %v", err)
	}
	adm2.AddInput(500)
	adm2.Release()
	if load, _ := leaf.effectiveLoad(time.Now()); load != 0 {
		t.Fatalf("after 500 carry-consumed, effective load = %d, want 0 (carry consumed before ceiling)", load)
	}

	// Consume another 1000 (the ceiling). Effective load = 1500 - 500 = 1000.
	adm3, err := tr.Admit("k", "claude", "")
	if err != nil {
		t.Fatalf("Admit ceiling: %v", err)
	}
	adm3.AddInput(1000)
	adm3.Release()

	// Next admit hard-denies at the ceiling (effective load 1000 >= 1000).
	if _, err := tr.Admit("k", "claude", ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("post-ceiling Admit err = %v, want ErrDenied", err)
	}

	// Window 3 rollover: ceiling was fully used (trueUsed 1000) → no carry. The
	// window-1 carry must NOT leak forward.
	backdate(leaf, 200*time.Millisecond)
	tr.rolloverAll(time.Now())
	if leaf.carry != 0 {
		t.Fatalf("window-3 carry = %d, want 0 (carry does not leak past one window; ceiling fully used)", leaf.carry)
	}
}

// TestTree_RolloverPolicyCapsAtNPct verifies the carry is capped at N% of the
// ceiling even when unspent exceeds it: a tiny 10-token spend leaves 990
// unspent, but a 50% cap limits the carry to 500, not 990.
func TestTree_RolloverPolicyCapsAtNPct(t *testing.T) {
	tr := resetNode(t, &config.ResetPolicy{Mode: "rollover", RolloverCapPct: 50})
	leaf := tr.leafByPath["org"]

	adm, _ := tr.Admit("k", "claude", "")
	adm.AddInput(10) // unspent 990
	adm.Release()

	backdate(leaf, 200*time.Millisecond)
	tr.rolloverAll(time.Now())

	if leaf.carry != 500 {
		t.Fatalf("carry = %d, want 500 (capped at 50%% of 1000, not 990 unspent)", leaf.carry)
	}
}

// TestTree_HardResetPolicyIsZeroCarry confirms the default hard policy (no
// rollover) zeroes the consumed counter with no carry — today's behaviour
// preserved exactly.
func TestTree_HardResetPolicyIsZeroCarry(t *testing.T) {
	tr := resetNode(t, &config.ResetPolicy{Mode: "hard"})
	leaf := tr.leafByPath["org"]

	adm, _ := tr.Admit("k", "claude", "")
	adm.AddInput(900)
	adm.Release()

	backdate(leaf, 200*time.Millisecond)
	tr.rolloverAll(time.Now())

	if leaf.carry != 0 {
		t.Fatalf("hard policy carry = %d, want 0", leaf.carry)
	}
	if c, _ := leaf.snapshotConsumed(); c != 0 {
		t.Fatalf("hard policy consumed = %d, want 0 (zeroed on rollover)", c)
	}
}

// TestTree_GracePolicyKeepsOldWindowLive asserts a grace_period keeps the old
// window's ceiling live past the nominal rollover instant before flipping:
// inside the grace window the consumed counter is NOT reset, and after it the
// reset fires.
func TestTree_GracePolicyKeepsOldWindowLive(t *testing.T) {
	root := &config.GroupConfig{
		Name:        "org",
		Weight:      1,
		Budget:      &config.TokenBudget{Tokens: 1000, Window: "50ms", SoftThrottleAt: 0.999},
		ResetPolicy: &config.ResetPolicy{Mode: "grace", GracePeriod: "100ms"},
	}
	tr := newTestTree(t, root)
	if err := tr.Bind("k", "org"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	leaf := tr.leafByPath["org"]

	adm, _ := tr.Admit("k", "claude", "")
	adm.AddInput(700)
	adm.Release()

	// Backdate past the 50ms window but NOT past the 50ms+100ms grace boundary:
	// the old window's ceiling must stay live (no reset).
	backdate(leaf, 80*time.Millisecond)
	if load, _ := leaf.effectiveLoad(time.Now()); load != 700 {
		t.Fatalf("inside grace: effective load = %d, want 700 (old window live, no reset)", load)
	}

	// Backdate past the grace boundary: the reset fires.
	backdate(leaf, 200*time.Millisecond)
	if load, _ := leaf.effectiveLoad(time.Now()); load != 0 {
		t.Fatalf("after grace: effective load = %d, want 0 (window reset)", load)
	}
}

// TestTree_RolloverInvariantHoldsAcrossChildren verifies the parent>=sum(child)
// invariant still holds across a rollover when children carry different
// amounts: the carry offsets the admit gate, not the consumed counter, so
// sum(child.consumed) <= parent.consumed holds after the boundary.
func TestTree_RolloverInvariantHoldsAcrossChildren(t *testing.T) {
	root := &config.GroupConfig{
		Name:        "org",
		Weight:      1,
		Budget:      &config.TokenBudget{Tokens: 2000, Window: "50ms", SoftThrottleAt: 0.999},
		ResetPolicy: &config.ResetPolicy{Mode: "hard"},
		Children: []*config.GroupConfig{
			{Name: "a", Weight: 1, Budget: &config.TokenBudget{Tokens: 1000, Window: "50ms", SoftThrottleAt: 0.999},
				ResetPolicy: &config.ResetPolicy{Mode: "rollover", RolloverCapPct: 50}},
			{Name: "b", Weight: 1, Budget: &config.TokenBudget{Tokens: 1000, Window: "50ms", SoftThrottleAt: 0.999},
				ResetPolicy: &config.ResetPolicy{Mode: "rollover", RolloverCapPct: 50}},
		},
	}
	tr := newTestTree(t, root)
	if err := tr.Bind("ka", "org.a"); err != nil {
		t.Fatalf("Bind a: %v", err)
	}
	if err := tr.Bind("kb", "org.b"); err != nil {
		t.Fatalf("Bind b: %v", err)
	}
	leafA := tr.leafByPath["org.a"]
	leafB := tr.leafByPath["org.b"]

	// Window 1: a spends 400 (carry 500), b spends 900 (unspent 100 → carry 100).
	admA, _ := tr.Admit("ka", "claude", "")
	admA.AddInput(400)
	admA.Release()
	admB, _ := tr.Admit("kb", "claude", "")
	admB.AddInput(900)
	admB.Release()

	// Roll the whole tree coherently against one now.
	now := time.Now()
	for _, n := range []*node{tr.root, leafA, leafB} {
		n.mu.Lock()
		n.windowStart = now.Add(-200 * time.Millisecond)
		n.mu.Unlock()
	}
	tr.rolloverAll(now)

	// After rollover the invariant holds: sum(child.consumed) <= parent.consumed
	// (both reset to 0, carry only offsets the admit gate, not the counter).
	a, _ := leafA.snapshotConsumed()
	b, _ := leafB.snapshotConsumed()
	p, _ := tr.root.snapshotConsumed()
	if a+b > p {
		t.Fatalf("invariant broken after rollover: sum(child)=%d > parent=%d (a=%d b=%d)", a+b, p, a, b)
	}

	// Window 2: spend through both children; the parent rolls up the sum.
	admA2, _ := tr.Admit("ka", "claude", "")
	admA2.AddInput(100)
	admA2.Release()
	admB2, _ := tr.Admit("kb", "claude", "")
	admB2.AddInput(50)
	admB2.Release()

	a2, _ := leafA.snapshotConsumed()
	b2, _ := leafB.snapshotConsumed()
	p2, _ := tr.root.snapshotConsumed()
	if a2+b2 > p2 {
		t.Fatalf("invariant broken in window 2: sum(child)=%d > parent=%d", a2+b2, p2)
	}
	if a2+b2 != p2 {
		t.Fatalf("window-2 roll-up mismatch: sum(child)=%d, parent=%d (must equal when no concurrent stragglers)", a2+b2, p2)
	}
}
