package budget

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SuperMarioYL/tokenctl/internal/config"
)

// tierTree builds a single-leaf tree whose parent budget is generous (so a tier
// sub-ceiling is the binding constraint, not the node) and installs the given
// model_tiers. Reservation is disabled so attribution is exact.
func tierTree(t *testing.T, tiers []config.ModelTier) *Tree {
	t.Helper()
	root := budgetNode("org", 1, 1_000_000, 0.999)
	tr := newTestTree(t, root)
	if err := tr.Bind("k", "org"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := tr.SetModelTiers(tiers); err != nil {
		t.Fatalf("SetModelTiers: %v", err)
	}
	return tr
}

// opusHaikuTiers is the canonical v0.9.0 fixture: an Opus tier (10x cost
// multiplier + 1000-token sub-ceiling) and a Haiku tier (1x, no sub-ceiling so
// Haiku traffic keeps flowing when Opus is capped).
func opusHaikuTiers() []config.ModelTier {
	return []config.ModelTier{
		{
			Name:           "opus",
			Pattern:        "claude-opus.*",
			CostMultiplier: 10,
			BudgetTokensPerWindow: &config.TokenBudget{
				Tokens: 1000, Window: "1h", SoftThrottleAt: 0.999,
			},
		},
		{
			Name:           "haiku",
			Pattern:        "claude-haiku.*",
			CostMultiplier: 1,
		},
	}
}

// TestTree_TierCapFiresIndependentlyOfNodeBudget is the feat_model_tier_override
// headline regression: an Opus swarm on a leaf whose parent budget has room is
// hard-denied at the tier ceiling while sibling Haiku traffic on the SAME leaf
// keeps flowing. The deny must surface as ErrTierDenied (X-TokenCtl-Reason:
// budget_exceeded_tier) so it is distinguishable from a node-budget deny.
func TestTree_TierCapFiresIndependentlyOfNodeBudget(t *testing.T) {
	tr := tierTree(t, opusHaikuTiers())

	// Window 1: 100 raw Opus tokens * mult 10 = 1000 attributed → exactly the
	// tier ceiling. The parent node is at 1000/1_000_000 (plenty of room).
	adm, err := tr.Admit("k", "claude", "claude-opus-4-20250514")
	if err != nil {
		t.Fatalf("first Opus Admit: %v", err)
	}
	adm.AddInput(100)
	adm.Release()

	// The cost-multiplied attribution must reconcile: 100 raw → 1000 attributed.
	stats := tr.TierStats()
	var opusConsumed int64
	for _, s := range stats {
		if s.Name == "opus" {
			opusConsumed = s.ConsumedTokens
		}
	}
	if opusConsumed != 1000 {
		t.Fatalf("opus tier consumed = %d, want 1000 (100 raw * mult 10)", opusConsumed)
	}
	if c, _ := tr.root.snapshotConsumed(); c != 1000 {
		t.Fatalf("root consumed = %d, want 1000 (cost-multiplied roll-up)", c)
	}

	// Next Opus admit: tier effectiveLoad 1000 >= ceiling 1000 → hard-deny.
	// The node has 999000 tokens of room, so a node-budget deny must NOT fire —
	// the TIER cap is the binding constraint.
	_, err = tr.Admit("k", "claude", "claude-opus-4-20250514")
	if !errors.Is(err, ErrTierDenied) {
		t.Fatalf("second Opus Admit err = %v, want ErrTierDenied (tier ceiling crossed, parent has room)", err)
	}
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("ErrTierDenied must wrap ErrDenied so the proxy's 429 path fires: %v", err)
	}

	// Sibling Haiku traffic on the SAME leaf keeps flowing: its tier has no
	// sub-ceiling, so the Opus cap cannot block it.
	admH, err := tr.Admit("k", "claude", "claude-haiku-3-5")
	if err != nil {
		t.Fatalf("Haiku Admit after Opus cap: %v (sibling tier traffic must keep flowing)", err)
	}
	admH.AddInput(200)
	admH.Release()

	// Haiku is cost-multiplied by 1, so 200 raw == 200 attributed.
	for _, s := range tr.TierStats() {
		if s.Name == "haiku" {
			if s.ConsumedTokens != 200 {
				t.Fatalf("haiku tier consumed = %d, want 200 (mult 1)", s.ConsumedTokens)
			}
		}
	}
}

// TestTree_TierCostMultiplierReconcilesAgainstSnapshot checks the
// cost-multiplied attribution reconciles against the windowed snapshot
// (tokenctl top): the leaf + root consumed equal the sum of raw deltas scaled
// by the tier multiplier, and the tier counter matches.
func TestTree_TierCostMultiplierReconcilesAgainstSnapshot(t *testing.T) {
	tr := tierTree(t, opusHaikuTiers())

	adm, err := tr.Admit("k", "claude", "claude-opus-4-20250514")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	adm.AddInput(100)
	adm.AddOutput(50)
	adm.Release()

	// 100 + 50 = 150 raw, * mult 10 = 1500 attributed.
	if c, _ := tr.root.snapshotConsumed(); c != 1500 {
		t.Errorf("root consumed = %d, want 1500 (150 raw * mult 10)", c)
	}
	if c, _ := tr.leafByPath["org"].snapshotConsumed(); c != 1500 {
		t.Errorf("leaf consumed = %d, want 1500", c)
	}
	var opusConsumed int64
	for _, s := range tr.TierStats() {
		if s.Name == "opus" {
			opusConsumed = s.ConsumedTokens
		}
	}
	if opusConsumed != 1500 {
		t.Errorf("opus tier consumed = %d, want 1500", opusConsumed)
	}
}

// TestTree_NoTierWhenModelAbsent verifies that passing an empty model (the
// pre-v0.9.0 path, e.g. a provider whose body we could not peek) applies no
// tier and behaves identically to the flat-budget path.
func TestTree_NoTierWhenModelAbsent(t *testing.T) {
	tr := tierTree(t, opusHaikuTiers())

	adm, err := tr.Admit("k", "claude", "")
	if err != nil {
		t.Fatalf("Admit (no model): %v", err)
	}
	adm.AddInput(100)
	adm.Release()

	// No tier resolved → no multiplier → 100 raw == 100 attributed.
	if c, _ := tr.root.snapshotConsumed(); c != 100 {
		t.Errorf("root consumed = %d, want 100 (no tier, no multiplier)", c)
	}
	for _, s := range tr.TierStats() {
		if s.ConsumedTokens != 0 {
			t.Errorf("tier %q consumed = %d, want 0 (no model resolved → no tier match)", s.Name, s.ConsumedTokens)
		}
	}
}

// TestTree_TierCapWithConcurrentReservation asserts the tier in-flight
// reservation bounds a concurrent Opus swarm against the tier ceiling, mirroring
// the node-level swarm fix. With a non-zero reserve, concurrent admits cannot
// collectively overshoot the tier ceiling before their tokens are credited.
func TestTree_TierCapWithConcurrentReservation(t *testing.T) {
	root := budgetNode("org", 1, 1_000_000_000, 0.999)
	tr, err := NewTree(root, nil)
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	tr.arb.shutdown()
	t.Cleanup(func() { _ = tr.Close() })
	if err := tr.Bind("k", "org"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// Opus mult 10 + a 200-token reserve → each admit reserves 2000 attributed
	// tokens against a 100000-token tier ceiling, so ~50 admits fit before the
	// reserved tier load reaches the ceiling (well below the 500-attempt
	// unbounded case).
	if err := tr.SetModelTiers([]config.ModelTier{
		{
			Name:           "opus",
			Pattern:        "claude-opus.*",
			CostMultiplier: 10,
			BudgetTokensPerWindow: &config.TokenBudget{
				Tokens: 100_000, Window: "1h", SoftThrottleAt: 0.999,
			},
		},
	}); err != nil {
		t.Fatalf("SetModelTiers: %v", err)
	}
	tr.SetReserveEstimate(200)

	const attempts = 500
	var (
		admitted int64
		mu       sync.Mutex
		held     []*Admission
	)
	done := make(chan struct{})
	for i := 0; i < attempts; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			a, err := tr.Admit("k", "claude", "claude-opus-4-20250514")
			if err != nil {
				return
			}
			atomic.AddInt64(&admitted, 1)
			mu.Lock()
			held = append(held, a.(*Admission))
			mu.Unlock()
		}()
	}
	for i := 0; i < attempts; i++ {
		<-done
	}

	got := atomic.LoadInt64(&admitted)
	// The reserved tier load must have bounded admits far below the unbounded
	// 500 case. Theoretical max ~50 (100000/2000); allow generous slack for the
	// check-then-act burst under contention while staying well below 500.
	if got == 0 {
		t.Fatal("expected at least one Opus admission past the tier gate")
	}
	if got > 250 {
		t.Fatalf("admitted %d concurrent Opus requests against a 100000-token tier ceiling with 200-token reservations — the tier gate's check-then-act overshoot is not bounded", got)
	}
	for _, a := range held {
		a.Release()
	}
}
