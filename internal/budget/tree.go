// Package budget is the runtime hierarchical TokenGroup tree — the core
// primitive of tokenctl. It exists as a separate package from proxy so that:
//
//   - the proxy can be tested in isolation against a stub Tree,
//   - the arbiter goroutine (see preempt.go) owns its own ticker without
//     entangling the proxy's request hot path,
//   - persistence (consumed counters, audit log) can attach via a narrow
//     state.Persister contract instead of dragging BoltDB into the proxy.
//
// The tree is built once from a config.Config and is safe for concurrent use
// from many proxy goroutines. Each node tracks a windowed consumed counter
// that resets when the configured Window elapses; the per-request Admission
// is the bridge between an in-flight HTTP call and the leaf it was attributed
// to.
//
// m2_tree_weight uses Admit to enforce soft-throttle (>= soft_throttle_at)
// and hard-deny (>= 1.0) on every ancestor on the path leaf->root + the
// optional wallet ceiling. m3_preempt_arb adds the arbiter goroutine that
// cancels low-weight in-flight admissions when a higher-weight sibling needs
// headroom; preemption is exposed via Admission.Context() so the proxy can
// (in a future polish step) abort the upstream call.
package budget

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/supermario-leo/tokenctl/internal/config"
	"github.com/supermario-leo/tokenctl/internal/proxy"
)

// Tree.Admit error sentinels — re-exported from proxy so callers in this
// package can return them without importing proxy explicitly at the call site.
// proxy owns the canonical declarations (see internal/proxy/proxy.go) because
// it is also the package that translates them into HTTP responses.
var (
	ErrUnknownKey = proxy.ErrUnknownKey
	ErrThrottled  = proxy.ErrThrottled
	ErrDenied     = proxy.ErrDenied
)

// State describes the optional persistence hook the tree calls into on every
// counter mutation. The store package (polish stage) implements it; passing
// nil is supported for in-memory runs (tests, dry-runs) and is the default
// when the BoltDB file is unavailable.
//
// LoadCounter is consulted once per node at NewTree time. SaveCounter is
// called best-effort after every successful attribution; failures are logged
// at the arbiter tick and never block admission. AppendAudit records a
// terminal event for the request — admit / deny / throttle / preempt /
// release — so audit log reconstruction is possible from BoltDB alone.
type State interface {
	LoadCounter(group string) (consumed int64, windowStart time.Time, err error)
	SaveCounter(group string, consumed int64, windowStart time.Time) error
	AppendAudit(event AuditEvent) error
}

// AuditEvent is the structured record persisted per request lifecycle event.
type AuditEvent struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"` // admit | deny | throttle | preempt | release
	Group     string    `json:"group"`
	Provider  string    `json:"provider,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	InTokens  int64     `json:"in_tokens,omitempty"`
	OutTokens int64     `json:"out_tokens,omitempty"`
}

// Tree is the runtime budget tree.
//
// Construct with NewTree, call Close to stop the arbiter. Implements
// proxy.Tree and prometheus.Collector. Safe for concurrent use; node-level
// locks keep per-leaf updates contention-free except where parent invariants
// must be checked.
type Tree struct {
	root       *node
	flat       []*node
	leafByPath map[string]*node
	leafByKey  map[string]*node

	walletBudget  *config.TokenBudget
	walletWindowD time.Duration

	state State

	mu                sync.Mutex
	walletConsumed    int64
	walletWindowStart time.Time
	providerConsumed  map[string]int64
	inFlightCount     int64
	deniesTotal       atomic.Int64
	throttlesTotal    atomic.Int64
	preemptsTotal    atomic.Int64

	arb *arbiter

	descConsumed *prometheus.Desc
	descBudget   *prometheus.Desc
	descInFlight *prometheus.Desc
	descWallet   *prometheus.Desc
	descDenies   *prometheus.Desc
	descThrottle *prometheus.Desc
	descPreempts *prometheus.Desc
}

// node is the runtime counterpart of config.GroupConfig.
//
// Held state: consumed (current-window total), windowStart (when this window
// began), inFlight (the set of live admissions attributed to this subtree),
// and three lifetime counters used for prometheus + snapshot.
type node struct {
	name     string
	path     string
	weight   int
	budget   *config.TokenBudget
	windowD  time.Duration
	parent   *node
	children []*node

	mu          sync.Mutex
	consumed    int64
	windowStart time.Time
	inFlight    map[*Admission]struct{}
	denies      int64
	throttles   int64
	preempts    int64
}

// Admission is the per-request ticket Tree.Admit returns. It carries:
//
//   - the leaf the request was attributed to
//   - a context cancelled when the arbiter preempts the request OR when
//     Release is called — wiring it into the upstream call lets m3 abort
//     mid-flight
//   - the running input/output token totals (for the audit log on Release)
type Admission struct {
	tree     *Tree
	leaf     *node
	provider string
	chain    []*node // leaf .. root (deepest first)

	startedAt time.Time

	mu  sync.Mutex
	in  int64
	out int64

	preempted atomic.Bool
	released  atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc
}

// NewTree builds the runtime tree from a GroupConfig and attaches optional
// persistence + arbiter.
//
// The signature deliberately takes only the GroupConfig (not the whole
// Config) so the proxy can build a tree against a synthesised hierarchy in
// tests without a full on-disk config. API key bindings and the wallet
// ceiling are wired separately via Bind and SetWallet — the caller (main.go
// + the store package's restore step) iterates config.APIKeys and
// config.Wallet at boot.
//
// state may be nil; if non-nil and it satisfies the State interface,
// counters are restored at boot and audit events are appended on every
// admission outcome. The arbiter goroutine starts immediately and runs until
// Close.
func NewTree(root *config.GroupConfig, state any) (*Tree, error) {
	if root == nil {
		return nil, fmt.Errorf("budget: nil group config")
	}
	t := &Tree{
		leafByPath:       map[string]*node{},
		leafByKey:        map[string]*node{},
		providerConsumed: map[string]int64{},
	}
	if s, ok := state.(State); ok {
		t.state = s
	}

	rootNode, err := t.buildNode(root, nil, "")
	if err != nil {
		return nil, err
	}
	t.root = rootNode
	t.collectFlat(rootNode)
	t.collectLeaves(rootNode)

	t.initPromDescs()
	t.arb = newArbiter(t, defaultArbiterTick)
	t.arb.start()

	return t, nil
}

// Bind associates an inbound API key with a leaf path (the dotted group
// path, e.g. "acme.team-platform.alice"). Returns an error if the path is
// not a leaf or the key is already bound to a different leaf.
//
// Idempotent for the same (key, group) pair so a config reload that re-binds
// unchanged keys is a no-op.
func (t *Tree) Bind(apiKey, group string) error {
	leaf, ok := t.leafByPath[group]
	if !ok {
		return fmt.Errorf("budget: group %q is not a leaf in the tree", group)
	}
	if existing, dup := t.leafByKey[apiKey]; dup {
		if existing == leaf {
			return nil
		}
		return fmt.Errorf("budget: api_key already bound to %q", existing.path)
	}
	t.leafByKey[apiKey] = leaf
	return nil
}

// BindAll is a convenience that calls Bind for each entry; the first error
// stops the loop.
func (t *Tree) BindAll(bindings []config.APIKeyBinding) error {
	for _, b := range bindings {
		if err := t.Bind(b.Key, b.Group); err != nil {
			return err
		}
	}
	return nil
}

// SetWallet attaches the org-level aggregate ceiling enforced across all
// providers. Call before Admit fires; the arbiter picks up the wallet
// budget on its next tick.
func (t *Tree) SetWallet(w *config.WalletConfig) error {
	if w == nil || w.Budget == nil {
		return nil
	}
	d, err := time.ParseDuration(w.Budget.Window)
	if err != nil {
		return fmt.Errorf("budget: parse wallet.window: %w", err)
	}
	t.walletBudget = w.Budget
	t.walletWindowD = d
	t.walletWindowStart = time.Now()
	if t.state != nil {
		c, ws, lerr := t.state.LoadCounter("__wallet__")
		if lerr == nil && !ws.IsZero() {
			t.walletConsumed = c
			t.walletWindowStart = ws
		}
	}
	return nil
}

// buildNode recursively materialises a GroupConfig into a node + collects the
// dotted path.
func (t *Tree) buildNode(g *config.GroupConfig, parent *node, parentPath string) (*node, error) {
	path := g.Name
	if parentPath != "" {
		path = parentPath + "." + g.Name
	}
	n := &node{
		name:        g.Name,
		path:        path,
		weight:      g.Weight,
		budget:      g.Budget,
		parent:      parent,
		inFlight:    map[*Admission]struct{}{},
		windowStart: time.Now(),
	}
	if g.Budget != nil {
		d, err := time.ParseDuration(g.Budget.Window)
		if err != nil {
			return nil, fmt.Errorf("budget: parse window for %s: %w", path, err)
		}
		n.windowD = d
	}
	if t.state != nil {
		c, ws, err := t.state.LoadCounter(path)
		if err == nil && !ws.IsZero() {
			n.consumed = c
			n.windowStart = ws
		}
	}
	for _, child := range g.Children {
		c, err := t.buildNode(child, n, path)
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, c)
	}
	return n, nil
}

func (t *Tree) collectFlat(n *node) {
	t.flat = append(t.flat, n)
	for _, c := range n.children {
		t.collectFlat(c)
	}
}

func (t *Tree) collectLeaves(n *node) {
	if len(n.children) == 0 {
		t.leafByPath[n.path] = n
		return
	}
	for _, c := range n.children {
		t.collectLeaves(c)
	}
}

// Close stops the arbiter goroutine and flushes any pending state. Safe to
// call multiple times.
func (t *Tree) Close() error {
	if t.arb != nil {
		t.arb.shutdown()
	}
	if t.state != nil {
		t.flushAll()
	}
	return nil
}

func (t *Tree) flushAll() {
	for _, n := range t.flat {
		n.mu.Lock()
		c, ws := n.consumed, n.windowStart
		n.mu.Unlock()
		_ = t.state.SaveCounter(n.path, c, ws)
	}
	if t.walletBudget != nil {
		t.mu.Lock()
		c, ws := t.walletConsumed, t.walletWindowStart
		t.mu.Unlock()
		_ = t.state.SaveCounter("__wallet__", c, ws)
	}
}

// ---------------------------------------------------------------------------
// Admission path
// ---------------------------------------------------------------------------

// Admit pins an inbound API key to its leaf, walks the leaf->root chain
// (plus optional wallet) and enforces budget thresholds. The returned ticket
// must have Release() called exactly once.
func (t *Tree) Admit(apiKey, provider string) (proxy.Admission, error) {
	leaf, ok := t.leafByKey[apiKey]
	if !ok {
		t.appendAudit(AuditEvent{At: time.Now(), Kind: "deny", Reason: "unknown_key", Provider: provider})
		return nil, ErrUnknownKey
	}

	chain := chainToRoot(leaf)

	// Hard-deny + soft-throttle pre-checks on every ancestor. Hard wins over
	// soft (deny is final), so we run all hard checks first.
	for _, n := range chain {
		if n.budget == nil {
			continue
		}
		c, _ := n.snapshotConsumed()
		if c >= n.budget.Tokens {
			n.bumpDenies()
			t.deniesTotal.Add(1)
			t.appendAudit(AuditEvent{At: time.Now(), Kind: "deny", Reason: "budget_exceeded", Group: n.path, Provider: provider})
			return nil, ErrDenied
		}
	}
	if t.walletBudget != nil {
		wc, _ := t.walletSnapshot()
		if wc >= t.walletBudget.Tokens {
			t.deniesTotal.Add(1)
			t.appendAudit(AuditEvent{At: time.Now(), Kind: "deny", Reason: "wallet_exceeded", Provider: provider})
			return nil, ErrDenied
		}
	}

	for _, n := range chain {
		if n.budget == nil {
			continue
		}
		c, _ := n.snapshotConsumed()
		if frac(c, n.budget.Tokens) >= n.budget.SoftThrottleAt {
			n.bumpThrottles()
			t.throttlesTotal.Add(1)
			t.appendAudit(AuditEvent{At: time.Now(), Kind: "throttle", Reason: "soft_throttle", Group: n.path, Provider: provider})
			return nil, ErrThrottled
		}
	}
	if t.walletBudget != nil {
		wc, _ := t.walletSnapshot()
		if frac(wc, t.walletBudget.Tokens) >= t.walletBudget.SoftThrottleAt {
			t.throttlesTotal.Add(1)
			t.appendAudit(AuditEvent{At: time.Now(), Kind: "throttle", Reason: "wallet_soft", Provider: provider})
			return nil, ErrThrottled
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	a := &Admission{
		tree:      t,
		leaf:      leaf,
		provider:  provider,
		chain:     chain,
		startedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
	}
	leaf.mu.Lock()
	leaf.inFlight[a] = struct{}{}
	leaf.mu.Unlock()
	t.mu.Lock()
	t.inFlightCount++
	t.mu.Unlock()
	t.appendAudit(AuditEvent{At: time.Now(), Kind: "admit", Group: leaf.path, Provider: provider})
	return a, nil
}

// chainToRoot returns [leaf, parent, ..., root].
func chainToRoot(n *node) []*node {
	out := make([]*node, 0, 6)
	for cur := n; cur != nil; cur = cur.parent {
		out = append(out, cur)
	}
	return out
}

// GroupPath returns the dotted leaf path for this admission.
func (a *Admission) GroupPath() string { return a.leaf.path }

// AddInput credits n input tokens to the leaf and every ancestor, updates
// the wallet (if configured) and the per-provider total.
func (a *Admission) AddInput(n int64) {
	if n <= 0 {
		return
	}
	a.mu.Lock()
	a.in += n
	a.mu.Unlock()
	a.tree.attribute(a, n)
}

// AddOutput is the symmetric counterpart of AddInput.
func (a *Admission) AddOutput(n int64) {
	if n <= 0 {
		return
	}
	a.mu.Lock()
	a.out += n
	a.mu.Unlock()
	a.tree.attribute(a, n)
}

// Release returns the in-flight slot. Idempotent; safe in defer.
func (a *Admission) Release() {
	if !a.released.CompareAndSwap(false, true) {
		return
	}
	a.leaf.mu.Lock()
	delete(a.leaf.inFlight, a)
	a.leaf.mu.Unlock()
	a.tree.mu.Lock()
	a.tree.inFlightCount--
	a.tree.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	a.tree.appendAudit(AuditEvent{
		At:        time.Now(),
		Kind:      "release",
		Group:     a.leaf.path,
		Provider:  a.provider,
		InTokens:  a.in,
		OutTokens: a.out,
	})
}

// Context returns a context that is cancelled when the request is preempted
// by the arbiter or released by the proxy. Proxy m3 wiring will pass this as
// the upstream HTTP context so cancellation tears down the upstream connection.
func (a *Admission) Context() context.Context { return a.ctx }

// Preempted reports whether this admission was cancelled by the arbiter.
func (a *Admission) Preempted() bool { return a.preempted.Load() }

// attribute walks the chain and credits n tokens to each ancestor, the
// wallet, and the per-provider counter. It also persists best-effort.
func (t *Tree) attribute(a *Admission, n int64) {
	for _, anc := range a.chain {
		anc.mu.Lock()
		if anc.windowD > 0 && time.Since(anc.windowStart) >= anc.windowD {
			anc.consumed = 0
			anc.windowStart = time.Now()
		}
		anc.consumed += n
		c, ws := anc.consumed, anc.windowStart
		anc.mu.Unlock()
		if t.state != nil {
			_ = t.state.SaveCounter(anc.path, c, ws)
		}
	}
	t.mu.Lock()
	if t.walletBudget != nil {
		if t.walletWindowD > 0 && time.Since(t.walletWindowStart) >= t.walletWindowD {
			t.walletConsumed = 0
			t.walletWindowStart = time.Now()
		}
		t.walletConsumed += n
	}
	t.providerConsumed[a.provider] += n
	t.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Snapshot for /v1/snapshot (consumed by `tokenctl top`)
// ---------------------------------------------------------------------------

// Snapshot returns a JSON-ready view matching cmd/tokenctl's topSnapshot
// shape. Fields named here are stable; callers MAY ignore unknown fields.
func (t *Tree) Snapshot() any {
	out := struct {
		GeneratedAt time.Time `json:"generated_at"`
		Wallet      *struct {
			BudgetTokens   int64   `json:"budget_tokens"`
			ConsumedTokens int64   `json:"consumed_tokens"`
			Frac           float64 `json:"frac_used"`
		} `json:"wallet,omitempty"`
		Groups []struct {
			Path           string  `json:"path"`
			Weight         int     `json:"weight"`
			Window         string  `json:"window"`
			BudgetTokens   int64   `json:"budget_tokens"`
			ConsumedTokens int64   `json:"consumed_tokens"`
			InFlight       int     `json:"in_flight"`
			State          string  `json:"state"`
			Frac           float64 `json:"frac_used"`
		} `json:"groups"`
		InFlight  int   `json:"in_flight"`
		Denies    int64 `json:"denies_total"`
		Throttles int64 `json:"throttles_total"`
		Preempts  int64 `json:"preempts_total"`
		Providers []struct {
			Name           string `json:"name"`
			ConsumedTokens int64  `json:"consumed_tokens"`
		} `json:"providers,omitempty"`
	}{GeneratedAt: time.Now()}

	for _, n := range t.flat {
		c, _ := n.snapshotConsumed()
		var (
			budget int64
			window string
			state  = "ok"
			fr     float64
		)
		if n.budget != nil {
			budget = n.budget.Tokens
			window = n.budget.Window
			fr = frac(c, budget)
			switch {
			case fr >= 1.0:
				state = "hard"
			case fr >= n.budget.SoftThrottleAt:
				state = "soft"
			}
		}
		n.mu.Lock()
		inflight := len(n.inFlight)
		n.mu.Unlock()
		out.Groups = append(out.Groups, struct {
			Path           string  `json:"path"`
			Weight         int     `json:"weight"`
			Window         string  `json:"window"`
			BudgetTokens   int64   `json:"budget_tokens"`
			ConsumedTokens int64   `json:"consumed_tokens"`
			InFlight       int     `json:"in_flight"`
			State          string  `json:"state"`
			Frac           float64 `json:"frac_used"`
		}{n.path, n.weight, window, budget, c, inflight, state, fr})
	}

	if t.walletBudget != nil {
		wc, _ := t.walletSnapshot()
		out.Wallet = &struct {
			BudgetTokens   int64   `json:"budget_tokens"`
			ConsumedTokens int64   `json:"consumed_tokens"`
			Frac           float64 `json:"frac_used"`
		}{t.walletBudget.Tokens, wc, frac(wc, t.walletBudget.Tokens)}
	}

	t.mu.Lock()
	out.InFlight = int(t.inFlightCount)
	keys := make([]string, 0, len(t.providerConsumed))
	for k := range t.providerConsumed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Providers = append(out.Providers, struct {
			Name           string `json:"name"`
			ConsumedTokens int64  `json:"consumed_tokens"`
		}{k, t.providerConsumed[k]})
	}
	t.mu.Unlock()
	out.Denies = t.deniesTotal.Load()
	out.Throttles = t.throttlesTotal.Load()
	out.Preempts = t.preemptsTotal.Load()
	return out
}

// ---------------------------------------------------------------------------
// Prometheus collector
// ---------------------------------------------------------------------------

func (t *Tree) initPromDescs() {
	t.descConsumed = prometheus.NewDesc(
		"tokenctl_group_consumed_tokens",
		"Tokens consumed in the current window per tree node.",
		[]string{"group"}, nil,
	)
	t.descBudget = prometheus.NewDesc(
		"tokenctl_group_budget_tokens",
		"Configured token ceiling per tree node.",
		[]string{"group"}, nil,
	)
	t.descInFlight = prometheus.NewDesc(
		"tokenctl_group_in_flight",
		"In-flight admissions held by each leaf.",
		[]string{"group"}, nil,
	)
	t.descWallet = prometheus.NewDesc(
		"tokenctl_wallet_consumed_tokens",
		"Tokens consumed against the org-level wallet ceiling.",
		nil, nil,
	)
	t.descDenies = prometheus.NewDesc(
		"tokenctl_denies_total",
		"Total hard denies issued (sum across groups + wallet).",
		nil, nil,
	)
	t.descThrottle = prometheus.NewDesc(
		"tokenctl_throttles_total",
		"Total soft-throttle responses issued.",
		nil, nil,
	)
	t.descPreempts = prometheus.NewDesc(
		"tokenctl_preempts_total",
		"Total admissions cancelled by the arbiter.",
		nil, nil,
	)
}

// Describe implements prometheus.Collector.
func (t *Tree) Describe(ch chan<- *prometheus.Desc) {
	ch <- t.descConsumed
	ch <- t.descBudget
	ch <- t.descInFlight
	ch <- t.descWallet
	ch <- t.descDenies
	ch <- t.descThrottle
	ch <- t.descPreempts
}

// Collect implements prometheus.Collector by walking the current tree state.
// Using a custom collector keeps the gauge values authoritative (no drift
// vs the snapshot endpoint).
func (t *Tree) Collect(ch chan<- prometheus.Metric) {
	for _, n := range t.flat {
		c, _ := n.snapshotConsumed()
		ch <- prometheus.MustNewConstMetric(t.descConsumed, prometheus.GaugeValue, float64(c), n.path)
		if n.budget != nil {
			ch <- prometheus.MustNewConstMetric(t.descBudget, prometheus.GaugeValue, float64(n.budget.Tokens), n.path)
		}
		n.mu.Lock()
		ch <- prometheus.MustNewConstMetric(t.descInFlight, prometheus.GaugeValue, float64(len(n.inFlight)), n.path)
		n.mu.Unlock()
	}
	if t.walletBudget != nil {
		wc, _ := t.walletSnapshot()
		ch <- prometheus.MustNewConstMetric(t.descWallet, prometheus.GaugeValue, float64(wc))
	}
	ch <- prometheus.MustNewConstMetric(t.descDenies, prometheus.CounterValue, float64(t.deniesTotal.Load()))
	ch <- prometheus.MustNewConstMetric(t.descThrottle, prometheus.CounterValue, float64(t.throttlesTotal.Load()))
	ch <- prometheus.MustNewConstMetric(t.descPreempts, prometheus.CounterValue, float64(t.preemptsTotal.Load()))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// snapshotConsumed returns (consumed, windowStart) for a node, applying a
// lazy window reset so callers always see a fresh-relative value.
func (n *node) snapshotConsumed() (int64, time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.windowD > 0 && time.Since(n.windowStart) >= n.windowD {
		n.consumed = 0
		n.windowStart = time.Now()
	}
	return n.consumed, n.windowStart
}

func (n *node) bumpDenies() {
	n.mu.Lock()
	n.denies++
	n.mu.Unlock()
}

func (n *node) bumpThrottles() {
	n.mu.Lock()
	n.throttles++
	n.mu.Unlock()
}

func (t *Tree) walletSnapshot() (int64, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.walletWindowD > 0 && time.Since(t.walletWindowStart) >= t.walletWindowD {
		t.walletConsumed = 0
		t.walletWindowStart = time.Now()
	}
	return t.walletConsumed, t.walletWindowStart
}

func frac(consumed, budget int64) float64 {
	if budget <= 0 {
		return 0
	}
	return float64(consumed) / float64(budget)
}

func (t *Tree) appendAudit(e AuditEvent) {
	if t.state == nil {
		return
	}
	_ = t.state.AppendAudit(e)
}

// debugString is a compact dump of the tree, useful only for tests / dev.
// Exported via String() so callers can `%v` a *Tree in logs.
func (t *Tree) String() string {
	var b strings.Builder
	t.printNode(&b, t.root, 0)
	return b.String()
}

func (t *Tree) printNode(b *strings.Builder, n *node, depth int) {
	if n == nil {
		return
	}
	indent := strings.Repeat("  ", depth)
	c, _ := n.snapshotConsumed()
	var budget int64
	if n.budget != nil {
		budget = n.budget.Tokens
	}
	fmt.Fprintf(b, "%s- %s (weight=%d consumed=%d budget=%d)\n",
		indent, n.path, n.weight, c, budget)
	for _, child := range n.children {
		t.printNode(b, child, depth+1)
	}
}
