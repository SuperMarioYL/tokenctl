package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadValidYAML covers the happy path: an explicit, fully-specified config
// parses, defaults do not clobber the explicit values, and Path() returns the
// source file.
func TestLoadValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenctl.yaml")
	body := `
version: v0.1
listen: ":9000"
store:
  path: "custom.db"
metrics:
  listen: ":9999"
  path: "/m"
providers:
  - name: claude
    upstream: https://api.anthropic.com
wallet:
  budget:
    tokens: 5000
    window: 1h
    soft_throttle_at: 0.7
tree:
  name: org
  weight: 100
  budget:
    tokens: 10000
    window: 1h
    soft_throttle_at: 0.7
  children:
    - name: dev
      weight: 100
      budget:
        tokens: 1000
        window: 1h
        soft_throttle_at: 0.9
api_keys:
  - key: k1
    group: org.dev
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Version != "v0.1" {
		t.Errorf("Version = %q, want v0.1", cfg.Version)
	}
	if cfg.Listen != ":9000" {
		t.Errorf("Listen = %q, want :9000", cfg.Listen)
	}
	if cfg.Metrics.Listen != ":9999" || cfg.Metrics.Path != "/m" {
		t.Errorf("Metrics = %+v, want {:9999 /m}", cfg.Metrics)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != ProviderClaude {
		t.Errorf("Providers = %+v, want one claude", cfg.Providers)
	}
	if cfg.Tree == nil || cfg.Tree.Name != "org" {
		t.Fatalf("Tree root not loaded: %+v", cfg.Tree)
	}
	if cfg.Tree.Budget == nil || cfg.Tree.Budget.SoftThrottleAt != 0.7 {
		t.Errorf("Tree.Budget.SoftThrottleAt = %v, want 0.7 (explicit value preserved)",
			cfg.Tree.Budget)
	}
	if len(cfg.Tree.Children) != 1 || cfg.Tree.Children[0].Name != "dev" {
		t.Errorf("Tree children = %+v, want one 'dev'", cfg.Tree.Children)
	}
	if cfg.Wallet == nil || cfg.Wallet.Budget == nil || cfg.Wallet.Budget.Tokens != 5000 {
		t.Errorf("Wallet = %+v, want budget tokens=5000", cfg.Wallet)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0].Key != "k1" || cfg.APIKeys[0].Group != "org.dev" {
		t.Errorf("APIKeys = %+v, want one (k1, org.dev)", cfg.APIKeys)
	}
	if cfg.Path() != path {
		t.Errorf("Path() = %q, want %q", cfg.Path(), path)
	}
}

// TestLoadAppliesDefaults covers applyDefaults() — when fields are omitted the
// loader fills in the documented defaults (listen, metrics, store path, soft
// throttle 0.8) instead of failing validation.
func TestLoadAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenctl.yaml")
	body := `
providers:
  - name: openai
    upstream: https://api.openai.com
tree:
  name: org
  weight: 100
  budget:
    tokens: 5000
    window: 24h
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Version != Version {
		t.Errorf("Version default = %q, want %q", cfg.Version, Version)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen default = %q, want :8080", cfg.Listen)
	}
	if cfg.Metrics.Listen != ":9090" {
		t.Errorf("Metrics.Listen default = %q, want :9090", cfg.Metrics.Listen)
	}
	if cfg.Metrics.Path != "/metrics" {
		t.Errorf("Metrics.Path default = %q, want /metrics", cfg.Metrics.Path)
	}
	// Default store path is resolved relative to the config file's directory.
	wantStore := filepath.Join(dir, "tokenctl.db")
	if cfg.Store.Path != wantStore {
		t.Errorf("Store.Path default = %q, want %q", cfg.Store.Path, wantStore)
	}
	if cfg.Tree.Budget.SoftThrottleAt != 0.8 {
		t.Errorf("Tree SoftThrottleAt default = %v, want 0.8", cfg.Tree.Budget.SoftThrottleAt)
	}
}

// TestWalletDefaultSoftThrottle verifies the wallet block also picks up the
// 0.8 default when the field is omitted.
func TestWalletDefaultSoftThrottle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenctl.yaml")
	body := `
providers:
  - name: claude
    upstream: https://api.anthropic.com
wallet:
  budget:
    tokens: 100
    window: 1h
tree:
  name: org
  weight: 1
  budget:
    tokens: 10
    window: 1h
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Wallet == nil || cfg.Wallet.Budget == nil {
		t.Fatal("wallet/budget not loaded")
	}
	if cfg.Wallet.Budget.SoftThrottleAt != 0.8 {
		t.Errorf("wallet SoftThrottleAt default = %v, want 0.8",
			cfg.Wallet.Budget.SoftThrottleAt)
	}
}

// TestLoadMalformed exercises the failure paths: unparseable YAML, missing
// required fields, and validation errors. We expect Load to surface a non-nil
// error in every case.
func TestLoadMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "unparseable_yaml",
			body: "providers: [::::\n",
		},
		{
			name: "missing_tree",
			body: "providers:\n  - name: claude\n    upstream: https://x\n",
		},
		{
			name: "no_providers",
			body: "providers: []\ntree:\n  name: org\n  weight: 1\n",
		},
		{
			name: "unknown_provider_name",
			body: "providers:\n  - name: gemini\n    upstream: https://x\ntree:\n  name: org\n  weight: 1\n",
		},
		{
			name: "provider_missing_upstream",
			body: "providers:\n  - name: claude\ntree:\n  name: org\n  weight: 1\n",
		},
		{
			name: "bedrock_without_region",
			body: "providers:\n  - name: bedrock\n    upstream: https://bedrock.us-east-1.amazonaws.com\ntree:\n  name: org\n  weight: 1\n",
		},
		{
			name: "duplicate_api_keys",
			body: `
providers:
  - name: claude
    upstream: https://x
tree:
  name: org
  weight: 1
api_keys:
  - key: dup
    group: org
  - key: dup
    group: org
`,
		},
		{
			name: "api_key_points_at_non_leaf",
			body: `
providers:
  - name: claude
    upstream: https://x
tree:
  name: org
  weight: 1
  children:
    - name: dev
      weight: 1
api_keys:
  - key: k1
    group: org
`,
		},
		{
			name: "unknown_field_strict",
			body: `
providers:
  - name: claude
    upstream: https://x
tree:
  name: org
  weight: 1
totally_unknown_top_level_field: 42
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "bad.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err == nil {
				t.Fatalf("expected error, got cfg=%+v", cfg)
			}
		})
	}
}

// TestLoadMissingFile makes sure Load returns a wrapped error rather than
// panicking when the file does not exist.
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
