package budget

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SuperMarioYL/tokenctl/internal/config"
)

// captureState is an in-memory budget.State that records every audit event so
// a precedence test can assert WHICH constraint bit (the reason / group on the
// deny/throttle event), not just that some error fired.
type captureState struct {
	mu       sync.Mutex
	consumed map[string]int64
	events   []AuditEvent
}

func newCaptureState(seed map[string]int64) *captureState {
	out := &captureState{consumed: map[string]int64{}}
	for k, v := range seed {
		out.consumed[k] = v
	}
	return out
}

// LoadCounter restores a pre-seeded consumed so the tree boots at a known
// saturation level. windowStart is returned as now so the 1h window never
// rolls over mid-test.
func (c *captureState) LoadCounter(group string) (int64, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consumed[group], time.Now(), nil
}

func (c *captureState) SaveCounter(group string, consumed int64, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consumed[group] = consumed
	return nil
}

func (c *captureState) AppendAudit(e AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

// lastEvent returns the most recently appended audit event, or nil.
func (c *captureState) lastEvent() *AuditEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		return nil
	}
	e := c.events[len(c.events)-1]
	return &e
}

// TestTree_AdmitPrecedence is the m4 regression pinning the precedence order
// the m2/m3 arbiter + admission gate depend on. Admit checks in this order:
//
//	tier hard-deny → node-chain hard-deny → wallet hard-deny
//	→ node-chain soft-throttle → wallet soft-throttle
//
// So when several constraints are simultaneously at their limit, the ONE that
// fires identifies itself by the audit reason (budget_exceeded vs
// wallet_exceeded vs soft_throttle) and group. The table asserts each
// precedence rule with a pre-seeded saturation level so the decision is
// deterministic and the reservation is disabled (exact accounting).
func TestTree_AdmitPrecedence(t *testing.T) {
	// Single-leaf tree: org (1000, soft 0.8) → dev (1000, soft 0.8), wallet
	// (1000, soft 0.8). Pre-seeded consumed decides which constraint bites.
	precedenceTree := func(t *testing.T, seed map[string]int64) (*Tree, *captureState) {
		t.Helper()
		st := newCaptureState(seed)
		root := budgetNode("org", 1, 1000, 0.8,
			budgetNode("dev", 1, 1000, 0.8),
		)
		tr, err := NewTree(root, st)
		if err != nil {
			t.Fatalf("NewTree: %v", err)
		}
		tr.SetReserveEstimate(0)
		tr.arb.shutdown()
		t.Cleanup(func() { _ = tr.Close() })
		if err := tr.Bind("k", "org.dev"); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if err := tr.SetWallet(&config.WalletConfig{
			Budget: &config.TokenBudget{Tokens: 1000, Window: "1h", SoftThrottleAt: 0.8},
		}); err != nil {
			t.Fatalf("SetWallet: %v", err)
		}
		return tr, st
	}

	cases := []struct {
		name       string
		seed       map[string]int64
		wantErr    error // nil for admit
		wantKind   string
		wantReason string
		wantGroup  string
	}{
		{
			name: "all_clear_admit", seed: nil,
			wantErr: nil, wantKind: "admit", wantReason: "", wantGroup: "org.dev",
		},
		{
			// Both the leaf node and the wallet are at the hard ceiling. The
			// node-chain hard check runs before the wallet hard check, so the
			// deny reason must be budget_exceeded (node), not wallet_exceeded.
			name:    "node_hard_wins_over_wallet_hard",
			seed:    map[string]int64{"org.dev": 1000, "org": 1000, "__wallet__": 1000},
			wantErr: ErrDenied, wantKind: "deny", wantReason: "budget_exceeded", wantGroup: "org.dev",
		},
		{
			// Leaf at soft (800) but wallet at hard (1000). Hard checks run
			// before soft, so the wallet hard-deny wins — reason
			// wallet_exceeded, proving the node-soft path never fired.
			name:    "wallet_hard_wins_over_node_soft",
			seed:    map[string]int64{"org.dev": 800, "org": 800, "__wallet__": 1000},
			wantErr: ErrDenied, wantKind: "deny", wantReason: "wallet_exceeded", wantGroup: "",
		},
		{
			// Both leaf and wallet at soft (800). The node-chain soft check runs
			// before the wallet soft check, so the throttle fires on the leaf —
			// reason soft_throttle, group org.dev.
			name:    "node_soft_wins_over_wallet_soft",
			seed:    map[string]int64{"org.dev": 800, "org": 800, "__wallet__": 800},
			wantErr: ErrThrottled, wantKind: "throttle", wantReason: "soft_throttle", wantGroup: "org.dev",
		},
		{
			// Node at hard, wallet clear — the baseline node hard-deny.
			name:    "node_hard_only",
			seed:    map[string]int64{"org.dev": 1000, "org": 1000},
			wantErr: ErrDenied, wantKind: "deny", wantReason: "budget_exceeded", wantGroup: "org.dev",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, st := precedenceTree(t, tc.seed)
			adm, err := tr.Admit("k", "claude", "")
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Admit err = %v, want nil (admit)", err)
				}
				if adm == nil {
					t.Fatal("expected non-nil admission on admit")
				}
			} else {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Admit err = %v, want %v", err, tc.wantErr)
				}
				if adm != nil {
					t.Fatal("expected nil admission on deny/throttle")
				}
			}

			// Assert the audit trail BEFORE Release() appends its own "release"
			// event — for the admit case the last event right now is "admit".
			ev := st.lastEvent()
			if ev == nil {
				t.Fatalf("no audit event recorded for %q", tc.name)
			}
			if ev.Kind != tc.wantKind {
				t.Errorf("audit Kind = %q, want %q", ev.Kind, tc.wantKind)
			}
			if ev.Reason != tc.wantReason {
				t.Errorf("audit Reason = %q, want %q", ev.Reason, tc.wantReason)
			}
			if ev.Group != tc.wantGroup {
				t.Errorf("audit Group = %q, want %q", ev.Group, tc.wantGroup)
			}

			// Now safe to release the admitted ticket (no-op for deny/throttle).
			if adm != nil {
				adm.Release()
			}
		})
	}
}
