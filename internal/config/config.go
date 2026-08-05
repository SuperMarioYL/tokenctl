// Package config loads, validates and renders the tokenctl YAML configuration.
//
// The configuration describes:
//   - where the proxy listens and how it terminates TLS
//   - which upstream LLM providers are governed (Claude / OpenAI / Bedrock)
//   - the hierarchical TokenGroup tree (org -> team -> dev)
//   - the wallet block (optional aggregate cap across providers)
//   - the API-key bindings that pin each inbound request to a leaf
//   - storage (BoltDB) and metrics (Prometheus) endpoints
//
// The runtime budget tree (internal/budget) consumes GroupConfig and builds
// the live TokenGroup with arbiter state attached; this package only owns
// the on-disk shape.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Version is the configuration schema version. Bump when breaking changes land.
const Version = "v0.1"

// Provider names supported by tokenctl v0.1.
const (
	ProviderClaude  = "claude"
	ProviderOpenAI  = "openai"
	ProviderBedrock = "bedrock"
)

// Config is the root of tokenctl.yaml.
type Config struct {
	Version     string           `yaml:"version"`
	Listen      string           `yaml:"listen"`
	TLS         TLSConfig        `yaml:"tls,omitempty"`
	Store       StoreConfig      `yaml:"store"`
	Metrics     MetricsConfig    `yaml:"metrics"`
	Wallet      *WalletConfig    `yaml:"wallet,omitempty"`
	Providers   []ProviderConfig `yaml:"providers"`
	Tree        *GroupConfig     `yaml:"tree"`
	APIKeys     []APIKeyBinding  `yaml:"api_keys"`
	ModelTiers  []ModelTier      `yaml:"model_tiers,omitempty"`
	Pricing     *PricingConfig   `yaml:"pricing,omitempty"`

	// path is the file the config was loaded from; used for relative store paths.
	path string `yaml:"-"`
}

// TLSConfig terminates TLS at the proxy. Empty means plain HTTP (development).
type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// StoreConfig points at the embedded BoltDB file that holds consumed counters
// and the append-only audit log.
type StoreConfig struct {
	Path string `yaml:"path"`
}

// MetricsConfig configures the Prometheus scrape endpoint.
type MetricsConfig struct {
	Listen string `yaml:"listen"`
	Path   string `yaml:"path"`
}

// WalletConfig is the optional org-level aggregate cap across all providers.
// When set, the arbiter enforces sum(provider_consumed) <= wallet.budget.
type WalletConfig struct {
	Budget *TokenBudget `yaml:"budget,omitempty"`
}

// ProviderConfig describes one upstream LLM API the proxy fronts.
type ProviderConfig struct {
	Name     string `yaml:"name"`     // claude | openai | bedrock
	Upstream string `yaml:"upstream"` // https://api.anthropic.com etc.
	// Region is only meaningful for bedrock.
	Region string `yaml:"region,omitempty"`
}

// GroupConfig is the YAML shape of a TokenGroup tree node.
//
// The tree is recursive: a node either holds a budget directly, has children
// that hold budgets, or both (a parent budget acts as a ceiling for the sum of
// child consumption). Leaves are nodes with len(Children) == 0 and are the
// only nodes an inbound API key may be bound to.
type GroupConfig struct {
	Name         string         `yaml:"name"`
	Weight       int            `yaml:"weight"`
	Budget       *TokenBudget   `yaml:"budget,omitempty"`
	ResetPolicy  *ResetPolicy   `yaml:"reset_policy,omitempty"`
	Children     []*GroupConfig `yaml:"children,omitempty"`
}

// TokenBudget is a per-window token ceiling with a soft-throttle threshold.
//
// Window is a Go duration string (e.g. "1h", "24h", "720h"). SoftThrottleAt
// is in [0,1] and defaults to 0.8 when zero. At or above SoftThrottleAt the
// arbiter starts FIFO-delaying new requests on the node; at 1.0 it hard-denies
// with 429 and X-TokenCtl-Reason: budget_exceeded.
type TokenBudget struct {
	Tokens         int64   `yaml:"tokens"`
	Window         string  `yaml:"window"`
	SoftThrottleAt float64 `yaml:"soft_throttle_at,omitempty"`
}

// APIKeyBinding pins an inbound credential to a leaf in the tree.
//
// Key may be the literal Bearer token sent upstream or a synthetic identifier
// the proxy maps via the Authorization header. Group is a dotted path through
// the tree, e.g. "acme.team-platform.alice".
type APIKeyBinding struct {
	Key   string `yaml:"key"`
	Group string `yaml:"group"`
}

// ModelTier maps a model-name pattern (regex on the parsed request `model`
// field, e.g. "claude-opus.*", "gpt-4.*") to either a cost_multiplier applied
// to attributed tokens for that tier, or a nested budget_tokens_per_window
// sub-ceiling enforced as a hard 429 when that tier alone crosses it, or both.
//
// This lets a team's budget tell Opus spend from Haiku spend: a single coding
// agent session on claude-opus (~10x the per-token cost of haiku) can be
// hard-capped independently of the team's flat token budget so it cannot
// invisibly consume a whole window's headroom.
type ModelTier struct {
	Name                 string        `yaml:"name"`
	Pattern              string        `yaml:"pattern"`
	CostMultiplier       float64       `yaml:"cost_multiplier,omitempty"`
	BudgetTokensPerWindow *TokenBudget `yaml:"budget_tokens_per_window,omitempty"`
}

// ResetPolicy is the per-node window rollover policy. It accepts either a bare
// string ("hard") via a custom UnmarshalYAML or a map with explicit fields.
//
//   - hard (default, today's behaviour): the consumed counter is zeroed on
//     rollover.
//   - rollover (object {rollover_cap_pct: N}): carry min(unspent, N% of
//     ceiling) into the next window as a transient headroom bump, consumed
//     first and released after one window (not re-baselined into the next
//     window's ceiling).
//   - grace (object {grace_period: Xm}): keep the old window's ceiling live
//     for X minutes past rollover before flipping.
type ResetPolicy struct {
	Mode           string  `yaml:"mode"`
	RolloverCapPct  float64 `yaml:"rollover_cap_pct,omitempty"`
	GracePeriod     string  `yaml:"grace_period,omitempty"`
}

// UnmarshalYAML accepts reset_policy as either a bare scalar (e.g.
// `reset_policy: hard`) or a mapping (`reset_policy: {mode: rollover,
// rollover_cap_pct: 50}`). A bare scalar populates Mode.
func (p *ResetPolicy) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		p.Mode = s
		return nil
	}
	type raw ResetPolicy
	return value.Decode((*raw)(p))
}

// PricingConfig is the per-model per-1M-tokens price table used by
// `tokenctl export` to compute a cost_estimate column for invoice
// reconciliation.
type PricingConfig struct {
	Models []ModelPrice `yaml:"models"`
}

// ModelPrice is one row of the pricing table. Pattern is a regex matched
// against the request `model` field (first match wins). Prices are per one
// million tokens.
type ModelPrice struct {
	Pattern           string  `yaml:"pattern"`
	InputPerMillion   float64 `yaml:"input_per_million"`
	OutputPerMillion  float64 `yaml:"output_per_million"`
}

// Path returns the file the config was loaded from, or empty if constructed
// in memory.
func (c *Config) Path() string { return c.path }

// Load reads, parses and validates tokenctl.yaml.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.path = path
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Version == "" {
		c.Version = Version
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Store.Path == "" {
		c.Store.Path = "tokenctl.db"
	}
	if c.Metrics.Listen == "" {
		c.Metrics.Listen = ":9090"
	}
	if c.Metrics.Path == "" {
		c.Metrics.Path = "/metrics"
	}
	if c.path != "" && !filepath.IsAbs(c.Store.Path) {
		c.Store.Path = filepath.Join(filepath.Dir(c.path), c.Store.Path)
	}
	defaultGroupSoftThrottle(c.Tree)
	defaultResetPolicy(c.Tree)
	for i := range c.ModelTiers {
		if c.ModelTiers[i].CostMultiplier == 0 {
			c.ModelTiers[i].CostMultiplier = 1.0
		}
		if b := c.ModelTiers[i].BudgetTokensPerWindow; b != nil && b.SoftThrottleAt == 0 {
			b.SoftThrottleAt = 0.8
		}
	}
	if c.Wallet != nil && c.Wallet.Budget != nil && c.Wallet.Budget.SoftThrottleAt == 0 {
		c.Wallet.Budget.SoftThrottleAt = 0.8
	}
}

func defaultGroupSoftThrottle(g *GroupConfig) {
	if g == nil {
		return
	}
	if g.Budget != nil && g.Budget.SoftThrottleAt == 0 {
		g.Budget.SoftThrottleAt = 0.8
	}
	for _, child := range g.Children {
		defaultGroupSoftThrottle(child)
	}
}

// defaultResetPolicy fills in Mode="hard" (today's behaviour) for any node that
// omits reset_policy. Rollover/grace nodes keep their explicit Mode.
func defaultResetPolicy(g *GroupConfig) {
	if g == nil {
		return
	}
	if g.ResetPolicy == nil {
		g.ResetPolicy = &ResetPolicy{Mode: "hard"}
	} else if g.ResetPolicy.Mode == "" {
		g.ResetPolicy.Mode = "hard"
	}
	for _, child := range g.Children {
		defaultResetPolicy(child)
	}
}

// Validate checks the parsed config for structural errors.
func (c *Config) Validate() error {
	if c.Tree == nil {
		return errors.New("tree: required (no TokenGroup root defined)")
	}
	if len(c.Providers) == 0 {
		return errors.New("providers: at least one provider required")
	}
	for i, p := range c.Providers {
		if err := validateProvider(p); err != nil {
			return fmt.Errorf("providers[%d]: %w", i, err)
		}
	}
	leafPaths := map[string]bool{}
	if err := validateGroup(c.Tree, "", leafPaths); err != nil {
		return err
	}
	if c.Wallet != nil && c.Wallet.Budget != nil {
		if err := validateBudget(c.Wallet.Budget); err != nil {
			return fmt.Errorf("wallet.budget: %w", err)
		}
	}
	seenKeys := map[string]bool{}
	for i, b := range c.APIKeys {
		if b.Key == "" {
			return fmt.Errorf("api_keys[%d]: key is empty", i)
		}
		if seenKeys[b.Key] {
			return fmt.Errorf("api_keys[%d]: duplicate key %q", i, b.Key)
		}
		seenKeys[b.Key] = true
		if b.Group == "" {
			return fmt.Errorf("api_keys[%d]: group is empty", i)
		}
		if !leafPaths[b.Group] {
			return fmt.Errorf("api_keys[%d]: group %q does not resolve to a leaf in tree", i, b.Group)
		}
	}
	if err := validateModelTiers(c.ModelTiers); err != nil {
		return err
	}
	if c.Pricing != nil {
		if err := validatePricing(c.Pricing); err != nil {
			return fmt.Errorf("pricing: %w", err)
		}
	}
	return nil
}

// validateModelTiers checks model_tiers entries have unique names, compilable
// regex patterns, positive cost multipliers, and valid sub-ceilings.
func validateModelTiers(tiers []ModelTier) error {
	seen := map[string]bool{}
	for i, mt := range tiers {
		if mt.Name == "" {
			return fmt.Errorf("model_tiers[%d]: name is empty", i)
		}
		if seen[mt.Name] {
			return fmt.Errorf("model_tiers[%d]: duplicate tier name %q", i, mt.Name)
		}
		seen[mt.Name] = true
		if mt.Pattern == "" {
			return fmt.Errorf("model_tiers[%d]: pattern is empty", i)
		}
		if _, err := regexp.Compile(mt.Pattern); err != nil {
			return fmt.Errorf("model_tiers[%d]: pattern %q: %w", i, mt.Pattern, err)
		}
		if mt.CostMultiplier < 0 {
			return fmt.Errorf("model_tiers[%d]: cost_multiplier must be >= 0", i)
		}
		if mt.BudgetTokensPerWindow != nil {
			if err := validateBudget(mt.BudgetTokensPerWindow); err != nil {
				return fmt.Errorf("model_tiers[%d].budget_tokens_per_window: %w", i, err)
			}
		}
	}
	return nil
}

// validatePricing checks the pricing.models rows have compilable regex
// patterns and non-negative prices.
func validatePricing(p *PricingConfig) error {
	seen := map[string]bool{}
	for i, mp := range p.Models {
		if mp.Pattern == "" {
			return fmt.Errorf("models[%d]: pattern is empty", i)
		}
		if seen[mp.Pattern] {
			return fmt.Errorf("models[%d]: duplicate pattern %q", i, mp.Pattern)
		}
		seen[mp.Pattern] = true
		if _, err := regexp.Compile(mp.Pattern); err != nil {
			return fmt.Errorf("models[%d]: pattern %q: %w", i, mp.Pattern, err)
		}
		if mp.InputPerMillion < 0 || mp.OutputPerMillion < 0 {
			return fmt.Errorf("models[%d]: prices must be >= 0", i)
		}
	}
	return nil
}

func validateProvider(p ProviderConfig) error {
	switch p.Name {
	case ProviderClaude, ProviderOpenAI, ProviderBedrock:
	default:
		return fmt.Errorf("name %q: must be one of claude|openai|bedrock", p.Name)
	}
	if p.Upstream == "" {
		return errors.New("upstream: required")
	}
	if !strings.HasPrefix(p.Upstream, "http://") && !strings.HasPrefix(p.Upstream, "https://") {
		return fmt.Errorf("upstream %q: must start with http:// or https://", p.Upstream)
	}
	if p.Name == ProviderBedrock && p.Region == "" {
		return errors.New("region: required for bedrock provider")
	}
	return nil
}

func validateGroup(g *GroupConfig, parentPath string, leaves map[string]bool) error {
	if g.Name == "" {
		return fmt.Errorf("group %q: name is empty", parentPath)
	}
	if strings.ContainsAny(g.Name, ".") {
		return fmt.Errorf("group %q: name must not contain '.'", g.Name)
	}
	path := g.Name
	if parentPath != "" {
		path = parentPath + "." + g.Name
	}
	if g.Weight < 0 {
		return fmt.Errorf("group %s: weight must be >= 0", path)
	}
	if g.Budget != nil {
		if err := validateBudget(g.Budget); err != nil {
			return fmt.Errorf("group %s.budget: %w", path, err)
		}
	}
	if g.ResetPolicy != nil {
		if err := validateResetPolicy(g.ResetPolicy); err != nil {
			return fmt.Errorf("group %s.reset_policy: %w", path, err)
		}
	}
	if len(g.Children) == 0 {
		leaves[path] = true
		return nil
	}
	seen := map[string]bool{}
	for _, child := range g.Children {
		if seen[child.Name] {
			return fmt.Errorf("group %s: duplicate child name %q", path, child.Name)
		}
		seen[child.Name] = true
		if err := validateGroup(child, path, leaves); err != nil {
			return err
		}
	}
	// Enforce the core hierarchical invariant documented in mvp_plan §2:
	// sum(child.budget.tokens) <= parent.budget.tokens. Without this, an
	// over-subscribed tree (e.g. three 10M teams under a 20M org) loads silently
	// and then enforces incoherent semantics — each child admits up to its own
	// ceiling while the parent hard-deny is only reachable in aggregate, so the
	// children can collectively overshoot the org cap the operator believes they
	// configured. The whole hierarchical-budget value proposition rests on this
	// holding, so reject it at load rather than mis-enforce at runtime. Only
	// budgeted children count toward the sum; an unbudgeted child is bounded by
	// its ancestors, not by a ceiling of its own.
	if g.Budget != nil {
		var childSum int64
		for _, child := range g.Children {
			if child.Budget != nil {
				childSum += child.Budget.Tokens
			}
		}
		if childSum > g.Budget.Tokens {
			return fmt.Errorf("group %s: children's budgets sum to %d tokens, which exceeds this node's budget of %d — a child can never spend budget its parent doesn't have (sum(child) must be <= parent)", path, childSum, g.Budget.Tokens)
		}
	}
	return nil
}

func validateBudget(b *TokenBudget) error {
	if b.Tokens <= 0 {
		return errors.New("tokens: must be > 0")
	}
	d, err := time.ParseDuration(b.Window)
	if err != nil {
		return fmt.Errorf("window %q: %w", b.Window, err)
	}
	if d <= 0 {
		return fmt.Errorf("window %q: must be > 0", b.Window)
	}
	if b.SoftThrottleAt <= 0 || b.SoftThrottleAt > 1 {
		return fmt.Errorf("soft_throttle_at %v: must be in (0, 1]", b.SoftThrottleAt)
	}
	return nil
}

// validateResetPolicy checks Mode is one of hard|rollover|grace and that the
// mode-specific fields are present and valid.
func validateResetPolicy(p *ResetPolicy) error {
	switch p.Mode {
	case "hard":
		// no extra fields required
	case "rollover":
		if p.RolloverCapPct <= 0 || p.RolloverCapPct > 100 {
			return fmt.Errorf("rollover_cap_pct %v: must be in (0, 100]", p.RolloverCapPct)
		}
	case "grace":
		if p.GracePeriod == "" {
			return errors.New("grace_period: required for grace mode")
		}
		d, err := time.ParseDuration(p.GracePeriod)
		if err != nil {
			return fmt.Errorf("grace_period %q: %w", p.GracePeriod, err)
		}
		if d <= 0 {
			return fmt.Errorf("grace_period %q: must be > 0", p.GracePeriod)
		}
	default:
		return fmt.Errorf("mode %q: must be one of hard|rollover|grace", p.Mode)
	}
	return nil
}

// Marshal renders the config as YAML.
func (c *Config) Marshal() ([]byte, error) {
	return yaml.Marshal(c)
}

// Sample returns a 3-team, 6-dev example configuration scoped to org. It is
// the seed `tokenctl init --org <name>` writes to disk.
func Sample(org string) *Config {
	if org == "" {
		org = "acme"
	}
	hourly := func(t int64) *TokenBudget {
		return &TokenBudget{Tokens: t, Window: "24h", SoftThrottleAt: 0.8}
	}
	tree := &GroupConfig{
		Name:   org,
		Weight: 100,
		Budget: hourly(20_000_000),
		Children: []*GroupConfig{
			{
				Name:   "team-platform",
				Weight: 50,
				Budget: hourly(10_000_000),
				Children: []*GroupConfig{
					{Name: "alice", Weight: 50, Budget: hourly(5_000_000)},
					{Name: "bob", Weight: 50, Budget: hourly(5_000_000)},
				},
			},
			{
				Name:   "team-product",
				Weight: 30,
				Budget: hourly(6_000_000),
				Children: []*GroupConfig{
					{Name: "carol", Weight: 50, Budget: hourly(3_000_000)},
					{Name: "dave", Weight: 50, Budget: hourly(3_000_000)},
				},
			},
			{
				Name:   "team-research",
				Weight: 20,
				Budget: hourly(4_000_000),
				Children: []*GroupConfig{
					{Name: "erin", Weight: 50, Budget: hourly(2_000_000)},
					{Name: "frank", Weight: 50, Budget: hourly(2_000_000)},
				},
			},
		},
	}
	return &Config{
		Version: Version,
		Listen:  ":8080",
		Store:   StoreConfig{Path: "tokenctl.db"},
		Metrics: MetricsConfig{Listen: ":9090", Path: "/metrics"},
		Wallet:  &WalletConfig{Budget: hourly(20_000_000)},
		Providers: []ProviderConfig{
			{Name: ProviderClaude, Upstream: "https://api.anthropic.com"},
			{Name: ProviderOpenAI, Upstream: "https://api.openai.com"},
		},
		Tree: tree,
		APIKeys: []APIKeyBinding{
			{Key: "replace-me-alice", Group: org + ".team-platform.alice"},
			{Key: "replace-me-bob", Group: org + ".team-platform.bob"},
			{Key: "replace-me-carol", Group: org + ".team-product.carol"},
		},
	}
}

// WriteSample writes a Sample config to path. It refuses to overwrite an
// existing file.
func WriteSample(path, org string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing to overwrite existing file %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := Sample(org).Marshal()
	if err != nil {
		return err
	}
	header := []byte("# tokenctl configuration — generated by `tokenctl init`\n" +
		"# Edit budgets, providers and api_keys to suit your org, then run `tokenctl up -c " + path + "`.\n\n")
	return os.WriteFile(path, append(header, data...), 0o600)
}
