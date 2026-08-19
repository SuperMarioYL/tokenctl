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

// ---------------------------------------------------------------------------
// fix-tier-soft-throttle-at-silently-ignored
// ---------------------------------------------------------------------------

// TestTree_TierSoftThrottleHonoured is the regression for
// fix-tier-soft-throttle-at-silently-ignored. soft_throttle_at on a tier's
// budget_tokens_per_window was validated + defaulted (0.8) at config load but
// SetModelTiers dropped it and Admit's tier branch only hard-denied — so an
// operator who set soft_throttle_at: 0.5 on an Opus tier had it accepted,
// validated and discarded; sibling agents ran straight to the 100% hard cap
// with no Retry-After the node + wallet paths would have emitted at 50%. The
// tier must now soft-throttle at its configured fraction, mirroring the node
// loop (tree.go's node soft-throttle path). Fails on the unfixed code, which
// would admit at 50%/75% and only ErrTierDenied at 100%.
func TestTree_TierSoftThrottleHonoured(t *testing.T) {
	cases := []struct {
		name       string
		preConsume int64
		wantErr    error
		wantErrIs  bool // whether wantErr is meaningful
	}{
		{name: "under_threshold_49pct", preConsume: 499, wantErr: nil, wantErrIs: false},
		{name: "exactly_at_threshold_50pct", preConsume: 500, wantErr: ErrThrottled, wantErrIs: true},
		{name: "above_threshold_75pct", preConsume: 750, wantErr: ErrThrottled, wantErrIs: true},
		// At the 100% ceiling the tier hard-deny must still win over
		// soft-throttle (hard wins over soft), surfacing ErrTierDenied.
		{name: "at_ceiling_hard_deny_wins", preConsume: 1000, wantErr: ErrTierDenied, wantErrIs: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// mult=1 keeps the math transparent: raw tokens == attributed
			// tokens, so preConsume maps directly to the tier fraction.
			tiers := []config.ModelTier{
				{
					Name:           "opus",
					Pattern:        "claude-opus.*",
					CostMultiplier: 1,
					BudgetTokensPerWindow: &config.TokenBudget{
						Tokens: 1000, Window: "1h", SoftThrottleAt: 0.5,
					},
				},
			}
			tr := tierTree(t, tiers)

			// Pre-consume on a throwaway admission so the next Admit sees the
			// expected fraction. The parent node is at 1_000_000 tokens so the
			// node never throttles/denies — the tier is the sole binding
			// constraint.
			if tc.preConsume > 0 {
				adm, err := tr.Admit("k", "claude", "claude-opus-4-20250514")
				if err != nil {
					t.Fatalf("setup Admit: %v", err)
				}
				adm.AddInput(tc.preConsume)
				adm.Release()
			}

			before := tr.throttlesTotal.Load()
			adm, err := tr.Admit("k", "claude", "claude-opus-4-20250514")
			if tc.wantErrIs {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Admit err = %v, want %v (tier soft_throttle_at=0.5 must throttle at 50%% before the 100%% hard cap; hard-deny must still win at the ceiling)", err, tc.wantErr)
				}
				if adm != nil {
					t.Fatal("expected nil admission on tier throttle/deny")
				}
				// A throttled admit must bump the tree throttle counter so
				// the operator-facing metric surfaces the tier soft-throttle.
				if errors.Is(err, ErrThrottled) && tr.throttlesTotal.Load() != before+1 {
					t.Fatalf("throttlesTotal = %d, want %d (tier soft-throttle must bump the tree throttle counter)", tr.throttlesTotal.Load(), before+1)
				}
			} else {
				if err != nil {
					t.Fatalf("Admit err = %v, want nil (under the 50%% tier soft-throttle threshold)", err)
				}
				if adm == nil {
					t.Fatal("expected non-nil admission")
				}
				adm.Release()
			}
		})
	}
}

// TestTree_TierSoftThrottleDefaultZeroSkips mirrors the softThrottleAt==0 guard:
// a tier whose budget omits soft_throttle_at (and is constructed without the
// 0.8 config default) must NOT always-throttle at frac>=0 — it falls through
// to hard-deny only, so the unfixed footgun (always-throttle) cannot regress.
func TestTree_TierSoftThrottleDefaultZeroSkips(t *testing.T) {
	tiers := []config.ModelTier{
		{
			Name:           "opus",
			Pattern:        "claude-opus.*",
			CostMultiplier: 1,
			BudgetTokensPerWindow: &config.TokenBudget{
				Tokens: 1000, Window: "1h", SoftThrottleAt: 0, // unset: no soft-throttle
			},
		},
	}
	tr := tierTree(t, tiers)

	// Pre-consume 500 (50%): with softThrottleAt==0 the soft-throttle path is
	// skipped, so the admit must succeed (hard-deny only at 100%).
	adm, err := tr.Admit("k", "claude", "claude-opus-4-20250514")
	if err != nil {
		t.Fatalf("setup Admit: %v", err)
	}
	adm.AddInput(500)
	adm.Release()

	adm2, err := tr.Admit("k", "claude", "claude-opus-4-20250514")
	if err != nil {
		t.Fatalf("Admit at 50%% with softThrottleAt=0 err = %v, want nil (unset soft_throttle_at must not always-throttle)", err)
	}
	adm2.Release()
}

// ---------------------------------------------------------------------------
// fix-tier-counter-not-persisted-across-restart
// ---------------------------------------------------------------------------

// tierTreeWithState builds a single-leaf tier tree backed by st (so SaveCounter
// / LoadCounter round-trip through it), with the arbiter stopped and the
// reservation disabled so attribution is exact. Mirrors newWalletTree for the
// tier path; used by the restart-persistence regression.
func tierTreeWithState(t *testing.T, st State, tiers []config.ModelTier) *Tree {
	t.Helper()
	root := budgetNode("org", 1, 1_000_000, 0.999)
	tr, err := NewTree(root, st)
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	tr.SetReserveEstimate(0)
	tr.arb.shutdown()
	t.Cleanup(func() { _ = tr.Close() })
	if err := tr.Bind("k", "org"); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := tr.SetModelTiers(tiers); err != nil {
		t.Fatalf("SetModelTiers: %v", err)
	}
	return tr
}

// opusTierConsumed reads the current-window consumed for the named tier via the
// public TierStats view (which applies the lazy rollover the production read
// path uses), so the assertion reflects what an operator / `tokenctl top` sees.
func opusTierConsumed(tr *Tree) int64 {
	for _, s := range tr.TierStats() {
		if s.Name == "opus" {
			return s.ConsumedTokens
		}
	}
	return -1
}

// TestTree_TierCounterPersistsAcrossRestart is the regression for
// fix-tier-counter-not-persisted-across-restart: attribute() must persist the
// per-tier windowed counter via SaveCounter on every attribution (mirroring
// the node path at tree.go:906 and the wallet path at tree.go:941), and
// SetModelTiers must restore it via LoadCounter (mirroring buildNode at
// tree.go:499-505). Without both halves the per-tier hard sub-ceiling was the
// ONLY budget counter not durable across restarts: a crash / SIGKILL / OOM /
// deployment mid-window reset the tier consumed to 0 and silently allowed up to
// ~2x the intended per-window spend (N before restart + N after in the same
// wall-clock window), defeating the v0.9.0 tier compliance guarantee.
func TestTree_TierCounterPersistsAcrossRestart(t *testing.T) {
	st := newMemState()
	tr := tierTreeWithState(t, st, opusHaikuTiers())

	// Window 1, pre-crash: 50 raw Opus tokens * mult 10 = 500 attributed. The
	// tier ceiling is 1000, so the tier is at 50% — under the cap but enough
	// that a restart mid-window must NOT zero it.
	adm, err := tr.Admit("k", "claude", "claude-opus-4-20250514")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	adm.AddInput(50)
	adm.Release()

	// The tier counter must have been persisted DURING attribution (the fix),
	// not only on graceful Close — the same guarantee the wallet counter has.
	if got, ok := st.saved("__tier__opus"); !ok || got != 500 {
		t.Fatalf("__tier__opus persisted = %d (ok=%v), want 500 (SaveCounter on attribution, not only on Close)", got, ok)
	}

	// Simulate a crash mid-window: build a fresh tree from the SAME state and
	// re-apply SetModelTiers, which must LoadCounter and restore the tier's
	// pre-crash consumed + windowStart — mirroring how buildNode restores node
	// counters. (tr's Close runs at test cleanup, AFTER the assertions, so it
	// does not affect the reload semantics here.)
	tr2, err := NewTree(budgetNode("org", 1, 1_000_000, 0.999), st)
	if err != nil {
		t.Fatalf("NewTree reload: %v", err)
	}
	tr2.SetReserveEstimate(0)
	tr2.arb.shutdown()
	t.Cleanup(func() { _ = tr2.Close() })
	if err := tr2.Bind("k", "org"); err != nil {
		t.Fatalf("Bind reload: %v", err)
	}
	if err := tr2.SetModelTiers(opusHaikuTiers()); err != nil {
		t.Fatalf("SetModelTiers reload: %v", err)
	}

	// The opus tier must have reloaded the pre-crash spend, not 0.
	if got := opusTierConsumed(tr2); got != 500 {
		t.Fatalf("reloaded opus tier consumed = %d, want 500 (pre-crash spend must survive restart via LoadCounter, not reset to 0)", got)
	}

	// The hard cap must survive the restart: crediting another 500 attributed
	// (50 raw * mult 10) brings the tier to the 1000-token ceiling, and the
	// NEXT Opus admit must be ErrTierDenied. Without the fix the tier would
	// have reloaded as 0, so this admit would succeed — silently allowing up
	// to ~2x the intended per-window spend in the same wall-clock window.
	adm2, err := tr2.Admit("k", "claude", "claude-opus-4-20250514")
	if err != nil {
		t.Fatalf("post-restart Admit: %v", err)
	}
	adm2.AddInput(50) // +500 attributed → tier now at the 1000 ceiling
	adm2.Release()
	if _, err := tr2.Admit("k", "claude", "claude-opus-4-20250514"); !errors.Is(err, ErrTierDenied) {
		t.Fatalf("post-restart Opus admit err = %v, want ErrTierDenied (tier hard cap must survive restart; cumulative windowed spend enforced)", err)
	}
}

// TestTree_TierCounterIdempotentOnEmptyTiers guards the "configs without a
// model_tiers block are unaffected" acceptance criterion: SetModelTiers on an
// empty slice remains a no-op + idempotent after the LoadCounter/SaveCounter
// wiring, and attribute()/flushAll() skip the (empty) tier path cleanly.
func TestTree_TierCounterIdempotentOnEmptyTiers(t *testing.T) {
	st := newMemState()
	tr, err := NewTree(budgetNode("org", 1, 1_000_000, 0.8), st)
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	tr.SetReserveEstimate(0)
	tr.arb.shutdown()
	t.Cleanup(func() { _ = tr.Close() })
	if err := tr.Bind("k", "org"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Empty tiers: no-op, no error.
	if err := tr.SetModelTiers(nil); err != nil {
		t.Fatalf("SetModelTiers(nil): %v", err)
	}
	// Idempotent re-apply.
	if err := tr.SetModelTiers(nil); err != nil {
		t.Fatalf("SetModelTiers(nil) second call: %v", err)
	}

	adm, err := tr.Admit("k", "claude", "")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	adm.AddInput(100)
	adm.Release()

	// No tier counter key should have been written.
	if _, ok := st.saved("__tier__"); ok {
		t.Fatal("an empty __tier__ key should not be written when no tiers are configured")
	}
	// flushAll (also called by Close at cleanup) must not panic or write tier
	// keys for the empty set — exercised implicitly by t.Cleanup; assert the
	// close path explicitly here too.
	tr.flushAll()
}
