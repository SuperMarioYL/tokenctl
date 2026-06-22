package budget

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SuperMarioYL/tokenctl/internal/config"
)

// memState is an in-memory budget.State used by the persistence regression
// tests. It records every SaveCounter so a test can replay the last persisted
// value into a fresh Tree, simulating a crash + reboot.
type memState struct {
	mu       sync.Mutex
	counters map[string]struct {
		consumed    int64
		windowStart time.Time
	}
	saves int
}

func newMemState() *memState {
	return &memState{counters: map[string]struct {
		consumed    int64
		windowStart time.Time
	}{}}
}

func (m *memState) LoadCounter(group string) (int64, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.counters[group]
	if !ok {
		return 0, time.Time{}, nil
	}
	return r.consumed, r.windowStart, nil
}

func (m *memState) SaveCounter(group string, consumed int64, windowStart time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[group] = struct {
		consumed    int64
		windowStart time.Time
	}{consumed, windowStart}
	m.saves++
	return nil
}

func (m *memState) AppendAudit(AuditEvent) error { return nil }

func (m *memState) saved(group string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.counters[group]
	return r.consumed, ok
}

// newTestTree builds a Tree and immediately stops the background arbiter so
// concurrent preempt ticks can't race the assertions. Tests that need the
// arbiter still drive it deterministically via Tree.arb.tickOnce().
func newTestTree(t *testing.T, root *config.GroupConfig) *Tree {
	t.Helper()
	tr, err := NewTree(root, nil)
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	// Disable the in-flight reservation by default so the existing
	// table-driven tests keep their exact admit/deny/throttle semantics
	// against small budgets. The fix-admit-check-then-act-overshoot test
	// opts back in via SetReserveEstimate.
	tr.SetReserveEstimate(0)
	tr.arb.shutdown()
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// budgetNode is a small literal helper that keeps the table-driven configs
// readable.
func budgetNode(name string, weight int, tokens int64, soft float64, kids ...*config.GroupConfig) *config.GroupConfig {
	return &config.GroupConfig{
		Name:     name,
		Weight:   weight,
		Budget:   &config.TokenBudget{Tokens: tokens, Window: "1h", SoftThrottleAt: soft},
		Children: kids,
	}
}

// TestTree_QuotaAccountingRollsUp covers the basic hierarchical roll-up
// invariant: tokens credited to a leaf show up on every ancestor on the
// leaf->root chain.
func TestTree_QuotaAccountingRollsUp(t *testing.T) {
	root := budgetNode("org", 100, 10000, 0.8,
		budgetNode("team", 100, 5000, 0.8,
			budgetNode("dev", 100, 1000, 0.8),
		),
	)
	tr := newTestTree(t, root)
	if err := tr.Bind("k", "org.team.dev"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	adm, err := tr.Admit("k", "claude")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if adm.GroupPath() != "org.team.dev" {
		t.Errorf("GroupPath = %q, want org.team.dev", adm.GroupPath())
	}
	adm.AddInput(100)
	adm.AddOutput(50)
	adm.Release()

	leaf, ok := tr.leafByPath["org.team.dev"]
	if !ok {
		t.Fatal("leafByPath missing org.team.dev")
	}
	if c, _ := leaf.snapshotConsumed(); c != 150 {
		t.Errorf("leaf consumed = %d, want 150", c)
	}
	if leaf.parent == nil {
		t.Fatal("expected team parent on dev")
	}
	if c, _ := leaf.parent.snapshotConsumed(); c != 150 {
		t.Errorf("team consumed = %d, want 150 (input+output rolled up)", c)
	}
	if c, _ := tr.root.snapshotConsumed(); c != 150 {
		t.Errorf("root consumed = %d, want 150 (input+output rolled up)", c)
	}

	// Idempotent Release: calling twice must not double-decrement counters.
	adm.Release()
	if c, _ := leaf.snapshotConsumed(); c != 150 {
		t.Errorf("after double-release leaf consumed = %d, want 150 (Release is idempotent)", c)
	}
}

// TestTree_SoftThrottleBoundary pins down the >= 0.8 boundary. At exactly the
// threshold we expect ErrThrottled; just below it (0.799) the admit succeeds.
func TestTree_SoftThrottleBoundary(t *testing.T) {
	cases := []struct {
		name       string
		preConsume int64
		wantErr    error
		wantErrIs  bool // whether wantErr is meaningful
	}{
		{name: "under_threshold_79pct", preConsume: 799, wantErr: nil, wantErrIs: false},
		{name: "exactly_at_threshold_80pct", preConsume: 800, wantErr: ErrThrottled, wantErrIs: true},
		{name: "above_threshold_95pct", preConsume: 950, wantErr: ErrThrottled, wantErrIs: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTree(t, budgetNode("org", 1, 1000, 0.8))
			if err := tr.Bind("k", "org"); err != nil {
				t.Fatal(err)
			}

			// Pre-consume on a throwaway admission so the next Admit sees the
			// expected fraction.
			if tc.preConsume > 0 {
				adm, err := tr.Admit("k", "claude")
				if err != nil {
					t.Fatalf("setup Admit: %v", err)
				}
				adm.AddInput(tc.preConsume)
				adm.Release()
			}

			adm, err := tr.Admit("k", "claude")
			if tc.wantErrIs {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Admit err = %v, want %v", err, tc.wantErr)
				}
				if adm != nil {
					t.Fatal("expected nil admission on throttle")
				}
			} else {
				if err != nil {
					t.Fatalf("Admit err = %v, want nil", err)
				}
				if adm == nil {
					t.Fatal("expected non-nil admission")
				}
				adm.Release()
			}
		})
	}
}

// TestTree_HardDenyAt100Pct asserts that consumed >= budget short-circuits to
// ErrDenied — and that hard-deny wins over soft-throttle (the deny path runs
// before the throttle path in Admit).
func TestTree_HardDenyAt100Pct(t *testing.T) {
	tr := newTestTree(t, budgetNode("org", 1, 1000, 0.8))
	if err := tr.Bind("k", "org"); err != nil {
		t.Fatal(err)
	}

	adm, err := tr.Admit("k", "claude")
	if err != nil {
		t.Fatalf("setup Admit: %v", err)
	}
	adm.AddInput(1000)
	adm.Release()

	if c, _ := tr.root.snapshotConsumed(); c != 1000 {
		t.Fatalf("setup consumed = %d, want 1000", c)
	}

	_, err = tr.Admit("k", "claude")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Admit err = %v, want ErrDenied (consumed == budget)", err)
	}
}

// TestTree_HardDenyAncestorWinsOverHealthyLeaf verifies the chain enforcement:
// the leaf is well under its own budget, but the root is at its hard ceiling.
// Admit must reject because every ancestor is checked.
func TestTree_HardDenyAncestorWinsOverHealthyLeaf(t *testing.T) {
	root := budgetNode("org", 1, 1000, 0.8,
		budgetNode("dev", 1, 10000, 0.8),
	)
	tr := newTestTree(t, root)
	if err := tr.Bind("k", "org.dev"); err != nil {
		t.Fatal(err)
	}

	adm, err := tr.Admit("k", "claude")
	if err != nil {
		t.Fatalf("setup Admit: %v", err)
	}
	adm.AddInput(1000) // pushes root to its hard limit; leaf is at 10%.
	adm.Release()

	_, err = tr.Admit("k", "claude")
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("Admit err = %v, want ErrDenied (ancestor saturated)", err)
	}
}

// TestTree_SiblingsHaveIndependentBudgets verifies the weighted-distribution
// invariant: consumption credited to one child does not bleed into a sibling's
// counter, and the sibling can still admit + spend its own share even after a
// neighbour has exhausted theirs.
func TestTree_SiblingsHaveIndependentBudgets(t *testing.T) {
	root := budgetNode("org", 100, 10000, 0.8,
		budgetNode("a", 50, 1000, 0.8),
		budgetNode("b", 50, 500, 0.8),
	)
	tr := newTestTree(t, root)
	if err := tr.Bind("ka", "org.a"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Bind("kb", "org.b"); err != nil {
		t.Fatal(err)
	}

	// Hammer leaf 'a' close to its soft threshold.
	adm, err := tr.Admit("ka", "claude")
	if err != nil {
		t.Fatalf("Admit a: %v", err)
	}
	adm.AddInput(700)
	adm.Release()

	leafA := tr.leafByPath["org.a"]
	leafB := tr.leafByPath["org.b"]

	if c, _ := leafA.snapshotConsumed(); c != 700 {
		t.Errorf("leaf a consumed = %d, want 700", c)
	}
	if c, _ := leafB.snapshotConsumed(); c != 0 {
		t.Errorf("leaf b consumed = %d, want 0 (sibling consumption must not bleed)", c)
	}
	if c, _ := tr.root.snapshotConsumed(); c != 700 {
		t.Errorf("root consumed = %d, want 700 (rolled up from a only)", c)
	}

	// 'b' has its own 500-token budget and is untouched. Admission succeeds
	// and counts only against b + root.
	admB, err := tr.Admit("kb", "claude")
	if err != nil {
		t.Fatalf("Admit b: %v (a's heavy use must not block b)", err)
	}
	admB.AddInput(100)
	admB.Release()

	if c, _ := leafB.snapshotConsumed(); c != 100 {
		t.Errorf("leaf b consumed = %d, want 100", c)
	}
	if c, _ := tr.root.snapshotConsumed(); c != 800 {
		t.Errorf("root consumed = %d, want 800 (700+100 rolled up)", c)
	}
}

// TestTree_UnknownKeyRejected covers the unbound-key sentinel path.
func TestTree_UnknownKeyRejected(t *testing.T) {
	tr := newTestTree(t, budgetNode("org", 1, 1000, 0.8))
	if _, err := tr.Admit("nope", "claude"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Admit unknown key err = %v, want ErrUnknownKey", err)
	}
}

// TestTree_BindRejectsNonLeaf documents that only leaves can hold inbound
// keys. Pinning a key to an interior node is a config error.
func TestTree_BindRejectsNonLeaf(t *testing.T) {
	root := budgetNode("org", 1, 1000, 0.8,
		budgetNode("dev", 1, 1000, 0.8),
	)
	tr := newTestTree(t, root)
	if err := tr.Bind("k", "org"); err == nil {
		t.Fatal("expected Bind to reject non-leaf path 'org'")
	}
}

// TestTree_BindIdempotentSameLeafConflictDifferentLeaf cover the dual cases
// described in the Bind doc-comment.
func TestTree_BindIdempotentSameLeafConflictDifferentLeaf(t *testing.T) {
	root := budgetNode("org", 1, 1000, 0.8,
		budgetNode("a", 1, 500, 0.8),
		budgetNode("b", 1, 500, 0.8),
	)
	tr := newTestTree(t, root)

	if err := tr.Bind("k", "org.a"); err != nil {
		t.Fatalf("first Bind: %v", err)
	}
	// Same (key, leaf) is a no-op.
	if err := tr.Bind("k", "org.a"); err != nil {
		t.Fatalf("idempotent Bind: %v", err)
	}
	// Same key to a different leaf is rejected.
	if err := tr.Bind("k", "org.b"); err == nil {
		t.Fatal("expected Bind to reject re-binding same key to a different leaf")
	}
}

// ---------------------------------------------------------------------------
// fix-wallet-counter-not-persisted-on-attribution
// ---------------------------------------------------------------------------

// newWalletTree builds a single-leaf tree with an org wallet and the supplied
// state, leaving the reservation disabled so wallet accounting is exact.
func newWalletTree(t *testing.T, st State, walletTokens int64) *Tree {
	t.Helper()
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
	if err := tr.SetWallet(&config.WalletConfig{
		Budget: &config.TokenBudget{Tokens: walletTokens, Window: "1h", SoftThrottleAt: 0.8},
	}); err != nil {
		t.Fatalf("SetWallet: %v", err)
	}
	return tr
}

// TestTree_WalletCounterPersistedOnAttribution is the regression for the HIGH
// fix: the org wallet counter must be flushed on every attribution (not only
// on graceful Close), so a crash between windows doesn't lose the whole
// window's org-level spend and silently reset the hard cap to 0. We simulate a
// crash by NEVER calling Close() and reloading a fresh tree from the same
// state.
func TestTree_WalletCounterPersistedOnAttribution(t *testing.T) {
	st := newMemState()
	tr := newWalletTree(t, st, 1_000_000)

	adm, err := tr.Admit("k", "claude")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	adm.AddInput(120_000)
	adm.AddOutput(80_000) // wallet should now be at 200k
	adm.Release()

	// The wallet must have been persisted DURING attribution, before any Close.
	if got, ok := st.saved("__wallet__"); !ok || got != 200_000 {
		t.Fatalf("__wallet__ persisted = %d (ok=%v), want 200000 flushed on attribution (not Close)", got, ok)
	}

	// Simulate crash: do NOT Close tr. Build a fresh tree from the same state
	// and re-attach the wallet — it must reload the persisted spend, not 0.
	tr2, err := NewTree(budgetNode("org", 1, 1_000_000, 0.8), st)
	if err != nil {
		t.Fatalf("NewTree reload: %v", err)
	}
	tr2.SetReserveEstimate(0)
	tr2.arb.shutdown()
	t.Cleanup(func() { _ = tr2.Close() })
	if err := tr2.SetWallet(&config.WalletConfig{
		Budget: &config.TokenBudget{Tokens: 1_000_000, Window: "1h", SoftThrottleAt: 0.8},
	}); err != nil {
		t.Fatalf("SetWallet reload: %v", err)
	}
	if wc, _ := tr2.walletSnapshot(); wc != 200_000 {
		t.Fatalf("reloaded wallet consumed = %d, want 200000 (the pre-crash window spend survives)", wc)
	}
}

// ---------------------------------------------------------------------------
// fix-admit-check-then-act-overshoot
// ---------------------------------------------------------------------------

// TestTree_ConcurrentAdmitsRespectHardCeilingViaReservation is the regression
// for the swarm-overshoot fix: with the in-flight reservation enabled, N
// concurrent agents that all admit before any tokens are credited cannot
// collectively exceed the hard ceiling, because each admission reserves an
// estimate that the next admission sees.
func TestTree_ConcurrentAdmitsRespectHardCeilingViaReservation(t *testing.T) {
	// Budget 100k, reserve 10k/request => at most ~10 concurrent admits before
	// the reserved load reaches the ceiling and further admits hard-deny.
	tr, err := NewTree(budgetNode("org", 1, 100_000, 0.999), nil)
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	tr.arb.shutdown()
	t.Cleanup(func() { _ = tr.Close() })
	tr.SetReserveEstimate(10_000)
	if err := tr.Bind("k", "org"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	const attempts = 200
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted []*Admission
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := tr.Admit("k", "claude")
			if err != nil {
				return // denied/throttled — expected once reservations fill up
			}
			mu.Lock()
			admitted = append(admitted, a.(*Admission))
			mu.Unlock()
		}()
	}
	wg.Wait()

	// The reserved load held by all live admissions must not have let the gate
	// admit unboundedly. With a 10k reserve against a 100k ceiling, no more
	// than ~10 should ever be admitted concurrently (the (k-1)*reserve >=
	// budget cutoff bites at k≈11).
	if len(admitted) == 0 {
		t.Fatal("expected at least one admission")
	}
	if len(admitted) > 11 {
		t.Fatalf("admitted %d concurrent requests against a 100k ceiling with 10k reservations — "+
			"the check-then-act overshoot is not bounded", len(admitted))
	}

	// Effective load (consumed+reserved) on the root must not exceed budget by
	// more than one reservation grant (the last admit can tip it just over).
	load, _ := tr.root.effectiveLoad(time.Now())
	if load > 100_000+10_000 {
		t.Fatalf("root effective load = %d, want <= 110000 (bounded overshoot)", load)
	}

	// Releasing every admission must drain the reservation back to ~0.
	for _, a := range admitted {
		a.Release()
	}
	if load, _ := tr.root.effectiveLoad(time.Now()); load != 0 {
		t.Fatalf("after releasing all admissions, root load = %d, want 0 (reservations released)", load)
	}
}

// TestTree_ReservationReleasedOnAttribution checks the reconcile path: as real
// tokens are credited they draw down the reservation so consumed+reserved is
// conserved and a request that fully reports usage leaves no phantom load.
func TestTree_ReservationReleasedOnAttribution(t *testing.T) {
	tr, err := NewTree(budgetNode("org", 1, 1_000_000, 0.999), nil)
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	tr.arb.shutdown()
	t.Cleanup(func() { _ = tr.Close() })
	tr.SetReserveEstimate(5_000)
	if err := tr.Bind("k", "org"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	adm := func() *Admission { a, _ := tr.Admit("k", "claude"); return a.(*Admission) }()
	// Right after admit: consumed 0, reserved 5000 => load 5000.
	if load, _ := tr.root.effectiveLoad(time.Now()); load != 5_000 {
		t.Fatalf("post-admit load = %d, want 5000 (reservation held)", load)
	}
	// Credit 2000 real tokens — draws down reservation, load stays 5000.
	adm.AddInput(2_000)
	if load, _ := tr.root.effectiveLoad(time.Now()); load != 5_000 {
		t.Fatalf("load after 2000 credited = %d, want 5000 (consumed 2000 + reserved 3000)", load)
	}
	// Credit beyond the estimate — reservation floors at 0, load == consumed.
	adm.AddInput(10_000)
	if load, _ := tr.root.effectiveLoad(time.Now()); load != 12_000 {
		t.Fatalf("load after over-running estimate = %d, want 12000 (reservation exhausted)", load)
	}
	adm.Release()
	if load, _ := tr.root.effectiveLoad(time.Now()); load != 12_000 {
		t.Fatalf("post-release load = %d, want 12000 (consumed only, no phantom reserve)", load)
	}
}

// ---------------------------------------------------------------------------
// fix-uncoordinated-window-reset-breaks-rollup
// ---------------------------------------------------------------------------

// TestTree_CoherentWindowRolloverPreservesInvariant pins down the rollup
// invariant across a window boundary: after a coherent rollover driven by a
// single now, parent and child share the same windowStart and
// sum(child.consumed) <= parent.consumed holds — they cannot drift to
// different instants the way independent lazy per-node resets did.
func TestTree_CoherentWindowRolloverPreservesInvariant(t *testing.T) {
	// Short window so we can cross a boundary deterministically with a backdated
	// windowStart rather than sleeping.
	root := &config.GroupConfig{
		Name:   "org",
		Weight: 1,
		Budget: &config.TokenBudget{Tokens: 1_000_000, Window: "50ms", SoftThrottleAt: 0.8},
		Children: []*config.GroupConfig{{
			Name:   "dev",
			Weight: 1,
			Budget: &config.TokenBudget{Tokens: 1_000_000, Window: "50ms", SoftThrottleAt: 0.8},
		}},
	}
	tr := newTestTree(t, root)
	if err := tr.Bind("k", "org.dev"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	adm, err := tr.Admit("k", "claude")
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	adm.AddInput(400_000)
	adm.Release()

	leaf := tr.leafByPath["org.dev"]
	rootN := tr.root

	// Force both nodes past the window boundary by backdating their starts to
	// DIFFERENT instants — this is exactly the drift the old lazy per-node
	// reset produced. The coherent arbiter rollover must realign them.
	leaf.mu.Lock()
	leaf.windowStart = time.Now().Add(-200 * time.Millisecond)
	leaf.mu.Unlock()
	rootN.mu.Lock()
	rootN.windowStart = time.Now().Add(-100 * time.Millisecond)
	rootN.mu.Unlock()

	now := time.Now()
	tr.rolloverAll(now)

	// Both reset against the same now => identical windowStart, both consumed 0.
	lc, lws := leaf.snapshotConsumed()
	rc, rws := rootN.snapshotConsumed()
	if lc != 0 || rc != 0 {
		t.Fatalf("after coherent rollover consumed leaf=%d root=%d, want 0/0", lc, rc)
	}
	if !lws.Equal(rws) {
		t.Fatalf("windowStart drifted after rollover: leaf=%v root=%v (must be coherent)", lws, rws)
	}

	// And the invariant holds: credit again, sum(child) <= parent.
	adm2, err := tr.Admit("k", "claude")
	if err != nil {
		t.Fatalf("Admit post-rollover: %v", err)
	}
	adm2.AddInput(50_000)
	adm2.Release()
	lc2, _ := leaf.snapshotConsumed()
	rc2, _ := rootN.snapshotConsumed()
	if lc2 > rc2 {
		t.Fatalf("invariant broken: child consumed %d > parent consumed %d", lc2, rc2)
	}
	if lc2 != 50_000 || rc2 != 50_000 {
		t.Fatalf("post-rollover consumed leaf=%d root=%d, want 50000/50000", lc2, rc2)
	}
}
