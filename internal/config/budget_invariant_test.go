package config

import (
	"strings"
	"testing"
)

func budget(tokens int64) *TokenBudget {
	return &TokenBudget{Tokens: tokens, Window: "24h", SoftThrottleAt: 0.8}
}

// validatableConfig wraps a tree in the minimum surrounding config needed for
// Validate to run (one provider, no api_keys).
func validatableConfig(tree *GroupConfig) *Config {
	return &Config{
		Tree:      tree,
		Providers: []ProviderConfig{{Name: ProviderClaude, Upstream: "https://api.anthropic.com"}},
	}
}

// TestValidate_ChildBudgetsWithinParent is the happy path: children whose
// budgets sum to exactly the parent ceiling validate cleanly. This is also the
// shape `tokenctl init` writes via Sample(), so it must always pass.
func TestValidate_ChildBudgetsWithinParent(t *testing.T) {
	tree := &GroupConfig{
		Name: "acme", Weight: 100, Budget: budget(20_000_000),
		Children: []*GroupConfig{
			{Name: "team-a", Weight: 50, Budget: budget(12_000_000)},
			{Name: "team-b", Weight: 50, Budget: budget(8_000_000)}, // 12M + 8M = 20M == parent
		},
	}
	if err := validatableConfig(tree).Validate(); err != nil {
		t.Fatalf("config with children summing to the parent ceiling should validate, got: %v", err)
	}
}

// TestValidate_SampleConfigStillValid guards that the regression check did not
// break the seed config `tokenctl init` ships.
func TestValidate_SampleConfigStillValid(t *testing.T) {
	if err := Sample("acme").Validate(); err != nil {
		t.Fatalf("Sample() config must remain valid after the budget-invariant check: %v", err)
	}
}

// TestValidate_OverSubscribedChildrenRejected is the regression for
// fix-child-budget-sum-exceeds-parent-unvalidated: a tree whose children's
// budgets sum ABOVE the parent ceiling must be rejected at load, because the
// core §2 invariant sum(child.consumed) <= parent.budget cannot hold for a
// config that lets each child individually outspend what the parent can ever
// have.
func TestValidate_OverSubscribedChildrenRejected(t *testing.T) {
	tree := &GroupConfig{
		Name: "acme", Weight: 100, Budget: budget(20_000_000),
		Children: []*GroupConfig{
			{Name: "team-a", Weight: 50, Budget: budget(15_000_000)},
			{Name: "team-b", Weight: 50, Budget: budget(15_000_000)}, // 15M + 15M = 30M > 20M parent
		},
	}
	err := validatableConfig(tree).Validate()
	if err == nil {
		t.Fatalf("over-subscribed children (30M under a 20M org) must be rejected, but Validate passed")
	}
	if !strings.Contains(err.Error(), "exceeds") && !strings.Contains(err.Error(), "<= parent") {
		t.Fatalf("error should explain the sum-exceeds-parent invariant, got: %v", err)
	}
}

// TestValidate_OverSubscribedDeepNodeRejected confirms the check fires on an
// inner node, not just the root: a team whose devs over-subscribe it is rejected
// even when the org-level sum is fine.
func TestValidate_OverSubscribedDeepNodeRejected(t *testing.T) {
	tree := &GroupConfig{
		Name: "acme", Weight: 100, Budget: budget(20_000_000),
		Children: []*GroupConfig{
			{
				Name: "team-a", Weight: 100, Budget: budget(10_000_000),
				Children: []*GroupConfig{
					{Name: "alice", Weight: 50, Budget: budget(8_000_000)},
					{Name: "bob", Weight: 50, Budget: budget(8_000_000)}, // 16M > 10M team ceiling
				},
			},
		},
	}
	if err := validatableConfig(tree).Validate(); err == nil {
		t.Fatalf("over-subscribed inner node (16M under a 10M team) must be rejected, but Validate passed")
	}
}

// TestValidate_UnbudgetedChildrenIgnoredInSum confirms a child with no budget of
// its own does not count toward the parent's sum (it is bounded by ancestors,
// not by a ceiling of its own), so a parent with one budgeted + one unbudgeted
// child within ceiling still validates.
func TestValidate_UnbudgetedChildrenIgnoredInSum(t *testing.T) {
	tree := &GroupConfig{
		Name: "acme", Weight: 100, Budget: budget(20_000_000),
		Children: []*GroupConfig{
			{Name: "team-a", Weight: 50, Budget: budget(20_000_000)},
			{Name: "team-b", Weight: 50}, // no budget — must not push the sum over
		},
	}
	if err := validatableConfig(tree).Validate(); err != nil {
		t.Fatalf("an unbudgeted child must not count toward the parent sum, got: %v", err)
	}
}
