package budget

import (
	"testing"
	"time"

	"github.com/supermario-leo/tokenctl/internal/config"
)

// preemptScenario is the shared fixture for preempt tests: a 1000-token root
// with two children — "high" (weight 80) and "low" (weight 20). All preempt
// tests vary admissions on top of this skeleton.
func preemptScenario(t *testing.T) *Tree {
	t.Helper()
	root := budgetNode("org", 100, 1000, 0.8,
		budgetNode("high", 80, 500, 0.5),
		budgetNode("low", 20, 500, 0.5),
	)
	tr := newTestTree(t, root)
	if err := tr.Bind("k-high", "org.high"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Bind("k-low", "org.low"); err != nil {
		t.Fatal(err)
	}
	return tr
}

// admit is a test helper that admits, asserts no error, and returns the
// concrete *Admission (so tests can backdate startedAt + read Preempted /
// Context, which are not on the proxy.Admission interface).
func admit(t *testing.T, tr *Tree, key, provider string) *Admission {
	t.Helper()
	a, err := tr.Admit(key, provider)
	if err != nil {
		t.Fatalf("Admit(%q,%q): %v", key, provider, err)
	}
	adm, ok := a.(*Admission)
	if !ok {
		t.Fatalf("Admit returned %T, want *budget.Admission", a)
	}
	return adm
}

// TestArbiter_PreemptsLowWeightSibling is the canonical m3 scenario: parent is
// past the 0.95 preempt threshold, a high-weight child is starving, the
// low-weight sibling holds an in-flight admission older than minPreemptLifetime
// and beyond its fair-weight share — the arbiter must cancel the low-weight
// admission, not the high-weight one.
func TestArbiter_PreemptsLowWeightSibling(t *testing.T) {
	tr := preemptScenario(t)

	// Low-weight sibling holds an in-flight admission consuming 250/950 ≈ 26%
	// of the parent's spend — well above its 20% weight share.
	admLow := admit(t, tr, "k-low", "claude")
	admLow.AddInput(250)

	// High-weight sibling consumes enough to push the root past 0.95 and is
	// itself above its own soft threshold (700/500 > 0.5 → "starving").
	admHigh := admit(t, tr, "k-high", "claude")
	admHigh.AddInput(700)

	// Backdate the low-weight admission so the arbiter considers it
	// preemptable. minPreemptLifetime is 750ms; 2s is safely past it.
	admLow.startedAt = time.Now().Add(-2 * time.Second)

	tr.arb.tickOnce()

	if !admLow.Preempted() {
		t.Fatalf("expected low-weight admission to be preempted, got Preempted()=%v",
			admLow.Preempted())
	}
	if admHigh.Preempted() {
		t.Fatal("did not expect high-weight (starving) admission to be preempted")
	}

	// The preempt signal must propagate through Admission.Context so the proxy
	// can tear down the upstream call.
	select {
	case <-admLow.Context().Done():
	default:
		t.Fatal("expected low admission context to be cancelled after preempt")
	}

	admLow.Release()
	admHigh.Release()
}

// TestArbiter_DoesNotPreemptWhenParentBelowThreshold covers the negative case:
// if the parent has not yet crossed preemptHardLimitRatio (0.95), no
// arbitration runs — even when the low-weight admission is old enough to be
// eligible.
func TestArbiter_DoesNotPreemptWhenParentBelowThreshold(t *testing.T) {
	tr := preemptScenario(t)

	admLow := admit(t, tr, "k-low", "claude")
	admLow.AddInput(250) // root only at 25% — well below 95%.
	admLow.startedAt = time.Now().Add(-2 * time.Second)

	tr.arb.tickOnce()

	if admLow.Preempted() {
		t.Fatal("did not expect preempt while parent is well below 95%")
	}
	admLow.Release()
}

// TestArbiter_DoesNotPreemptYoungAdmissions covers the minPreemptLifetime
// guard: a low-weight in-flight admission that just started must not be killed
// even when all other preempt conditions hold. Otherwise normal short LLM
// calls would all be casualties.
func TestArbiter_DoesNotPreemptYoungAdmissions(t *testing.T) {
	tr := preemptScenario(t)

	admLow := admit(t, tr, "k-low", "claude")
	admLow.AddInput(250)
	admHigh := admit(t, tr, "k-high", "claude")
	admHigh.AddInput(700)
	// Note: admLow.startedAt is left at the live wall-clock time — well under
	// minPreemptLifetime (750ms).

	tr.arb.tickOnce()

	if admLow.Preempted() {
		t.Fatal("did not expect young admission (< minPreemptLifetime) to be preempted")
	}
	admLow.Release()
	admHigh.Release()
}

// TestArbiter_PreemptsOldestInSubtreeFirst pins down the arbitration ordering
// within the targeted (low-weight) subtree: when multiple eligible admissions
// exist, the OLDEST one is preempted first. This matches the doc-comment in
// preemptOldestInSubtree.
func TestArbiter_PreemptsOldestInSubtreeFirst(t *testing.T) {
	tr := preemptScenario(t)

	admOld := admit(t, tr, "k-low", "claude")
	admOld.AddInput(150)

	admNew := admit(t, tr, "k-low", "claude")
	admNew.AddInput(100)

	admHigh := admit(t, tr, "k-high", "claude")
	admHigh.AddInput(700)

	// Both eligible by age, but admOld is older.
	admOld.startedAt = time.Now().Add(-5 * time.Second)
	admNew.startedAt = time.Now().Add(-1 * time.Second)

	tr.arb.tickOnce()

	if !admOld.Preempted() {
		t.Fatal("expected the older low-weight admission to be preempted first")
	}
	if admNew.Preempted() {
		t.Fatal("did not expect the newer admission to be preempted on the same tick (one per tick)")
	}
	if admHigh.Preempted() {
		t.Fatal("did not expect high-weight admission to be preempted")
	}

	admOld.Release()
	admNew.Release()
	admHigh.Release()
}

// TestArbiter_PreemptCountersIncrement asserts the side effects the dashboard
// + audit log rely on: preempt bumps preemptsTotal on the tree and the leaf,
// and a second firePreempt on the same admission is a no-op (Preempted is a
// CAS-guarded latch).
func TestArbiter_PreemptCountersIncrement(t *testing.T) {
	tr := preemptScenario(t)

	admLow := admit(t, tr, "k-low", "claude")
	admLow.AddInput(250)
	admHigh := admit(t, tr, "k-high", "claude")
	admHigh.AddInput(700)
	admLow.startedAt = time.Now().Add(-2 * time.Second)

	before := tr.preemptsTotal.Load()
	tr.arb.tickOnce()
	after := tr.preemptsTotal.Load()
	if after != before+1 {
		t.Fatalf("preemptsTotal delta = %d, want 1", after-before)
	}

	// A redundant manual firePreempt on the same admission must not
	// double-count — CompareAndSwap on .preempted returns false the second
	// time.
	tr.arb.firePreempt(admLow, "preempted_by_sibling")
	if got := tr.preemptsTotal.Load(); got != after {
		t.Fatalf("preemptsTotal after redundant firePreempt = %d, want unchanged %d", got, after)
	}

	admLow.Release()
	admHigh.Release()
}
