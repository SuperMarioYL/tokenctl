package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadModelTiers covers parsing + validation of the model_tiers block:
// valid tiers load, a duplicate tier name is rejected, and a bad regex is
// rejected.
func TestLoadModelTiers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenctl.yaml")
	body := `
providers:
  - name: claude
    upstream: https://api.anthropic.com
tree:
  name: org
  weight: 100
  budget:
    tokens: 10000
    window: 1h
model_tiers:
  - name: opus
    pattern: "claude-opus.*"
    cost_multiplier: 10
    budget_tokens_per_window:
      tokens: 1000
      window: 1h
  - name: haiku
    pattern: "claude-haiku.*"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ModelTiers) != 2 {
		t.Fatalf("ModelTiers = %d rows, want 2", len(cfg.ModelTiers))
	}
	if cfg.ModelTiers[0].Name != "opus" || cfg.ModelTiers[0].CostMultiplier != 10 {
		t.Errorf("tier 0 = %+v, want opus mult 10", cfg.ModelTiers[0])
	}
	// haiku omits cost_multiplier → defaults to 1.0.
	if cfg.ModelTiers[1].CostMultiplier != 1.0 {
		t.Errorf("haiku cost_multiplier = %v, want 1.0 (default)", cfg.ModelTiers[1].CostMultiplier)
	}
}

// TestLoadModelTiersRejectsBadPattern verifies an un-compilable regex is a
// load error (not a silent no-op at runtime).
func TestLoadModelTiersRejectsBadPattern(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenctl.yaml")
	body := `
providers:
  - name: claude
    upstream: https://api.anthropic.com
tree:
  name: org
  weight: 100
  budget:
    tokens: 10000
    window: 1h
model_tiers:
  - name: bad
    pattern: "["
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an un-compilable tier pattern; want error")
	}
}

// TestLoadResetPolicyBareScalar covers the custom UnmarshalYAML: a bare
// `reset_policy: hard` scalar populates Mode, and an object form populates the
// fields.
func TestLoadResetPolicyBareScalar(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		wantMode string
		wantPct float64
	}{
		{name: "bare_hard", yaml: "hard", wantMode: "hard"},
		{name: "object_rollover", yaml: "{mode: rollover, rollover_cap_pct: 50}", wantMode: "rollover", wantPct: 50},
		{name: "object_grace", yaml: "{mode: grace, grace_period: 5m}", wantMode: "grace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tokenctl.yaml")
			body := "providers:\n  - name: claude\n    upstream: https://api.anthropic.com\n" +
				"tree:\n  name: org\n  weight: 100\n  budget:\n    tokens: 10000\n    window: 1h\n" +
				"  reset_policy: " + tc.yaml + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Tree.ResetPolicy == nil || cfg.Tree.ResetPolicy.Mode != tc.wantMode {
				t.Fatalf("ResetPolicy.Mode = %v, want %q", cfg.Tree.ResetPolicy, tc.wantMode)
			}
			if tc.wantPct != 0 && cfg.Tree.ResetPolicy.RolloverCapPct != tc.wantPct {
				t.Errorf("RolloverCapPct = %v, want %v", cfg.Tree.ResetPolicy.RolloverCapPct, tc.wantPct)
			}
		})
	}
}

// TestLoadResetPolicyDefaultsHard verifies a node that omits reset_policy gets
// the hard default (today's behaviour) so existing configs are unaffected.
func TestLoadResetPolicyDefaultsHard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenctl.yaml")
	body := `
providers:
  - name: claude
    upstream: https://api.anthropic.com
tree:
  name: org
  weight: 100
  budget:
    tokens: 10000
    window: 1h
  children:
    - name: dev
      weight: 100
      budget:
        tokens: 1000
        window: 1h
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tree.ResetPolicy == nil || cfg.Tree.ResetPolicy.Mode != "hard" {
		t.Errorf("root reset_policy = %+v, want hard (default)", cfg.Tree.ResetPolicy)
	}
	child := cfg.Tree.Children[0]
	if child.ResetPolicy == nil || child.ResetPolicy.Mode != "hard" {
		t.Errorf("child reset_policy = %+v, want hard (default)", child.ResetPolicy)
	}
}

// TestLoadPricing covers the pricing block parse + validation.
func TestLoadPricing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenctl.yaml")
	body := `
providers:
  - name: claude
    upstream: https://api.anthropic.com
tree:
  name: org
  weight: 100
  budget:
    tokens: 10000
    window: 1h
pricing:
  models:
    - pattern: "claude-opus.*"
      input_per_million: 15
      output_per_million: 75
    - pattern: "claude-haiku.*"
      input_per_million: 0.25
      output_per_million: 1.25
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pricing == nil || len(cfg.Pricing.Models) != 2 {
		t.Fatalf("Pricing models = %d rows, want 2", len(cfg.Pricing.Models))
	}
	if cfg.Pricing.Models[0].InputPerMillion != 15 {
		t.Errorf("opus input price = %v, want 15", cfg.Pricing.Models[0].InputPerMillion)
	}
}
