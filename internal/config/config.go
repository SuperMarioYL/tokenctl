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
	Version   string           `yaml:"version"`
	Listen    string           `yaml:"listen"`
	TLS       TLSConfig        `yaml:"tls,omitempty"`
	Store     StoreConfig      `yaml:"store"`
	Metrics   MetricsConfig    `yaml:"metrics"`
	Wallet    *WalletConfig    `yaml:"wallet,omitempty"`
	Providers []ProviderConfig `yaml:"providers"`
	Tree      *GroupConfig     `yaml:"tree"`
	APIKeys   []APIKeyBinding  `yaml:"api_keys"`

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
	Name     string         `yaml:"name"`
	Weight   int            `yaml:"weight"`
	Budget   *TokenBudget   `yaml:"budget,omitempty"`
	Children []*GroupConfig `yaml:"children,omitempty"`
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
