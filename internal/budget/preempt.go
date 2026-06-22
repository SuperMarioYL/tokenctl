// preempt.go — the m3_preempt_arb milestone.
//
// The arbiter is a single goroutine started by NewTree and stopped by Close.
// On every tick it walks the tree looking for parents where:
//
//  1. the parent is at or above its soft-throttle threshold, AND
//  2. at least one child is itself at or above its soft threshold (the
//     "starving" child — it would like to consume more but can't), AND
//  3. another sibling has consumed materially more than its weight share AND
//     holds in-flight admissions older than minPreemptLifetime.
//
// In that case, the arbiter cancels the oldest in-flight admission in the
// over-consuming sibling, reclaiming budget for the starving (higher-weight)
// child. Cancellation is signalled via Admission.cancel; the proxy package
// receives the cancellation via Admission.Context() and (m3 wiring) tears
// down the upstream stream with a `499 Client Closed` + `X-TokenCtl-Reason:
// preempted_by_sibling` trailer.
//
// The wallet ceiling is treated as an implicit parent over all providers:
// when the wallet is at hard limit and one provider holds a disproportionate
// share, the arbiter preempts in-flight admissions on the heaviest provider
// first.
//
// Design notes:
//   - The arbiter never blocks Tree.Admit — it only mutates Admission.ctx.
//   - Tick interval is short (200ms) so a runaway agent swarm gets a deny
//     before it can burn another minute of budget; the cost is bounded by
//     the size of t.flat (one O(tree) walk per tick).
//   - Preemption is logged as an AuditEvent so retros can quantify how much
//     headroom the arbiter recovered.
package budget

import (
	"time"
)

// defaultArbiterTick is short enough that a runaway agent loses budget
// within one second, but long enough that idle clusters cost ~0 CPU.
const defaultArbiterTick = 200 * time.Millisecond

// minPreemptLifetime guards against killing requests that just landed —
// most LLM calls complete in <2s and short calls don't usefully free
// budget.
const minPreemptLifetime = 750 * time.Millisecond

// preemptHardLimitRatio is how far above the soft threshold a parent must
// be before the arbiter considers active preemption (vs just letting the
// admission-time check 429 incoming requests). Set to 0.95 so we still
// preempt before a hard 1.0 deny would trigger and lose attribution.
const preemptHardLimitRatio = 0.95

type arbiter struct {
	tree *Tree
	tick time.Duration
	stop chan struct{}
	done chan struct{}
}

func newArbiter(t *Tree, tick time.Duration) *arbiter {
	if tick <= 0 {
		tick = defaultArbiterTick
	}
	return &arbiter{
		tree: t,
		tick: tick,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

func (a *arbiter) start() {
	go a.loop()
}

// shutdown signals the arbiter loop and waits for it to drain. Idempotent: a
// second Close from main() after a SIGINT-triggered first Close is a
// no-op. Guarded by recover() so the double-close on the channel doesn't
// panic.
func (a *arbiter) shutdown() {
	defer func() { _ = recover() }()
	close(a.stop)
	<-a.done
}

func (a *arbiter) loop() {
	defer close(a.done)
	tk := time.NewTicker(a.tick)
	defer tk.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-tk.C:
			a.tickOnce()
		}
	}
}

// tickOnce is broken out from loop so tests can drive the arbiter
// deterministically without waiting on the ticker.
func (a *arbiter) tickOnce() {
	// Roll the whole tree over against a single now first so all subsequent
	// preempt decisions on this tick see coherent windowStarts
	// (fix-uncoordinated-window-reset-breaks-rollup).
	a.tree.rolloverAll(time.Now())
	a.walk(a.tree.root)
	a.walletGuard()
}

// walk visits every parent and considers preemption for its children.
func (a *arbiter) walk(n *node) {
	if n == nil || len(n.children) == 0 {
		return
	}
	a.consider(n)
	for _, c := range n.children {
		a.walk(c)
	}
}

// consider evaluates one parent. It runs only when the parent has a budget
// (otherwise there's no shared ceiling to fight over).
func (a *arbiter) consider(parent *node) {
	if parent.budget == nil {
		return
	}
	pc, _ := parent.snapshotConsumed()
	pFrac := frac(pc, parent.budget.Tokens)
	if pFrac < preemptHardLimitRatio {
		return
	}

	var starving []*node
	for _, c := range parent.children {
		if c.budget == nil {
			continue
		}
		cc, _ := c.snapshotConsumed()
		cf := frac(cc, c.budget.Tokens)
		if cf >= c.budget.SoftThrottleAt {
			starving = append(starving, c)
		}
	}
	if len(starving) == 0 {
		return
	}

	// Sort starving by weight DESC — highest weight wins arbitration.
	highestStarvingWeight := 0
	for _, s := range starving {
		if s.weight > highestStarvingWeight {
			highestStarvingWeight = s.weight
		}
	}

	// Candidates to preempt: siblings whose weight is strictly lower than
	// the highest starving weight AND who currently hold in-flight calls.
	totalWeight := 0
	for _, c := range parent.children {
		totalWeight += c.weight
	}

	for _, c := range parent.children {
		if c.weight >= highestStarvingWeight {
			continue
		}
		// Is this sibling using more than its fair weighted share?
		cc, _ := c.snapshotConsumed()
		share := float64(c.weight) / float64(max1(totalWeight))
		usedFrac := float64(cc) / float64(max1(int(pc)))
		if usedFrac <= share*1.05 {
			// Within its fair share — leave it alone.
			continue
		}
		a.preemptOldestInSubtree(c, "preempted_by_sibling")
	}
}

// preemptOldestInSubtree finds the oldest in-flight admission rooted at n
// (a leaf or an inner node) that has lived past minPreemptLifetime, and
// signals it. It returns whether a preemption fired so the caller can
// recurse for further headroom on the next tick.
func (a *arbiter) preemptOldestInSubtree(n *node, reason string) bool {
	leaves := collectLeaves(n)
	type cand struct {
		adm *Admission
		age time.Duration
	}
	var pick *cand
	now := time.Now()
	for _, leaf := range leaves {
		leaf.mu.Lock()
		for adm := range leaf.inFlight {
			age := now.Sub(adm.startedAt)
			if age < minPreemptLifetime {
				continue
			}
			if adm.preempted.Load() {
				continue
			}
			if pick == nil || age > pick.age {
				pick = &cand{adm: adm, age: age}
			}
		}
		leaf.mu.Unlock()
	}
	if pick == nil {
		return false
	}
	a.firePreempt(pick.adm, reason)
	return true
}

func (a *arbiter) firePreempt(adm *Admission, reason string) {
	if !adm.preempted.CompareAndSwap(false, true) {
		return
	}
	adm.leaf.mu.Lock()
	adm.leaf.preempts++
	adm.leaf.mu.Unlock()
	a.tree.preemptsTotal.Add(1)
	a.tree.appendAudit(AuditEvent{
		At:       time.Now(),
		Kind:     "preempt",
		Group:    adm.leaf.path,
		Provider: adm.provider,
		Reason:   reason,
	})
	if adm.cancel != nil {
		adm.cancel()
	}
}

// walletGuard considers the org-level wallet. When the wallet is past the
// preempt threshold, the arbiter cancels admissions on whichever provider
// holds the biggest share — this is the "shared bucket" arbitration spelled
// out in mvp_plan §5 (m3).
func (a *arbiter) walletGuard() {
	t := a.tree
	if t.walletBudget == nil {
		return
	}
	wc, _ := t.walletSnapshot()
	if frac(wc, t.walletBudget.Tokens) < preemptHardLimitRatio {
		return
	}
	t.mu.Lock()
	var heaviest string
	var heaviestTotal int64
	for p, n := range t.providerConsumed {
		if n > heaviestTotal {
			heaviest, heaviestTotal = p, n
		}
	}
	t.mu.Unlock()
	if heaviest == "" {
		return
	}
	// Walk all leaves; preempt the oldest admission whose provider matches
	// the heaviest spender. One per tick — the next tick reconsiders.
	for _, leaf := range t.flat {
		if len(leaf.children) != 0 {
			continue
		}
		leaf.mu.Lock()
		var pick *Admission
		var pickAge time.Duration
		for adm := range leaf.inFlight {
			if adm.provider != heaviest || adm.preempted.Load() {
				continue
			}
			age := time.Since(adm.startedAt)
			if age < minPreemptLifetime {
				continue
			}
			if pick == nil || age > pickAge {
				pick, pickAge = adm, age
			}
		}
		leaf.mu.Unlock()
		if pick != nil {
			a.firePreempt(pick, "wallet_preempt")
			return
		}
	}
}

// collectLeaves returns every leaf under n (inclusive when n is itself a
// leaf). Used by the arbiter to find in-flight admissions in a subtree.
func collectLeaves(n *node) []*node {
	if len(n.children) == 0 {
		return []*node{n}
	}
	var out []*node
	for _, c := range n.children {
		out = append(out, collectLeaves(c)...)
	}
	return out
}

func max1(x int) int {
	if x <= 0 {
		return 1
	}
	return x
}
