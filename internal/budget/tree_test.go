package budget

import (
	"errors"
	"testing"

	"github.com/SuperMarioYL/tokenctl/internal/config"
)

// newTestTree builds a Tree and immediately stops the background arbiter so
// concurrent preempt ticks can't race the assertions. Tests that need the
// arbiter still drive it deterministically via Tree.arb.tickOnce().
func newTestTree(t *testing.T, root *config.GroupConfig) *Tree {
	t.Helper()
	tr, err := NewTree(root, nil)
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
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
		name        string
		preConsume  int64
		wantErr     error
		wantErrIs   bool // whether wantErr is meaningful
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
