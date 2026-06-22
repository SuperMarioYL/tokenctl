// Command tokenctl is the cgroups-style budget controller for LLM tokens.
//
// It runs as a single static binary that:
//   - reverse-proxies Claude / OpenAI / Bedrock traffic
//   - meters streamed input + output tokens per inbound API key
//   - admits, throttles or preempts requests against a YAML-defined
//     org -> team -> dev TokenGroup tree
//   - exposes Prometheus metrics and a live `tokenctl top` view
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/SuperMarioYL/tokenctl/internal/budget"
	"github.com/SuperMarioYL/tokenctl/internal/config"
	"github.com/SuperMarioYL/tokenctl/internal/proxy"
	"github.com/SuperMarioYL/tokenctl/internal/store"
)

// Version is the binary's release tag. Overridable at link time:
//
//	go build -ldflags "-X main.Version=v0.1.0 -X main.Commit=$(git rev-parse --short HEAD)"
var (
	Version = "0.2.0-dev"
	Commit  = "unknown"
)

const longDescription = `tokenctl is the cgroups-style hierarchical budget controller for LLM tokens.

It fronts Claude, OpenAI and Bedrock as a single proxy, attributes streamed
tokens to a leaf in your org -> team -> dev tree, soft-throttles a node above
80% of its window budget, hard-denies at 100% with X-TokenCtl-Reason, and (m3)
preempts in-flight low-weight requests when a higher-weight sibling needs
headroom.

Quickstart:
    tokenctl init --org acme
    # edit tokenctl.yaml
    tokenctl up -c tokenctl.yaml
    export ANTHROPIC_BASE_URL=http://localhost:8080
    tokenctl top -c tokenctl.yaml   # in another terminal`

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "tokenctl",
		Short:         "cgroups for LLM tokens — hierarchical, preemptive, multi-provider",
		Long:          longDescription,
		SilenceUsage:  true,
		SilenceErrors: false,
		Version:       fmt.Sprintf("%s (commit %s, %s/%s)", Version, Commit, runtime.GOOS, runtime.GOARCH),
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.AddCommand(
		newInitCmd(),
		newUpCmd(),
		newServeCmd(),
		newTopCmd(),
		newVersionCmd(),
	)
	return root
}

// ---------------------------------------------------------------------------
// init
// ---------------------------------------------------------------------------

func newInitCmd() *cobra.Command {
	var org, out string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a sample tokenctl.yaml with a 3-team, 6-dev tree",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := config.WriteSample(out, org); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (org=%q)\n", out, org)
			fmt.Fprintf(cmd.OutOrStdout(), "next: edit budgets + api_keys, then `tokenctl up -c %s`\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&org, "org", "acme", "organisation name (becomes the tree root)")
	cmd.Flags().StringVarP(&out, "config", "c", "tokenctl.yaml", "output config path")
	return cmd
}

// ---------------------------------------------------------------------------
// up + serve proxy (alias)
// ---------------------------------------------------------------------------

func newUpCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start the proxy + arbiter + metrics endpoint",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProxy(cmd, cfgPath)
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "tokenctl.yaml", "config path")
	return cmd
}

func newServeCmd() *cobra.Command {
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Start a tokenctl subsystem",
	}
	var cfgPath string
	proxyCmd := &cobra.Command{
		Use:   "proxy",
		Short: "Alias for `tokenctl up` — start the proxy + arbiter",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProxy(cmd, cfgPath)
		},
	}
	proxyCmd.Flags().StringVarP(&cfgPath, "config", "c", "tokenctl.yaml", "config path")
	serve.AddCommand(proxyCmd)
	return serve
}

func runProxy(cmd *cobra.Command, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	state, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer state.Close()

	tree, err := budget.NewTree(cfg.Tree, state)
	if err != nil {
		return fmt.Errorf("build tree: %w", err)
	}
	defer tree.Close()

	srv, err := proxy.New(cfg, state, tree)
	if err != nil {
		return fmt.Errorf("build proxy: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "tokenctl %s — proxy on %s, metrics on %s%s, store %s\n",
		Version, cfg.Listen, cfg.Metrics.Listen, cfg.Metrics.Path, cfg.Store.Path)
	fmt.Fprintln(out, "export the following so your clients route through the proxy:")
	fmt.Fprintf(out, "  export ANTHROPIC_BASE_URL=http://%s\n", normaliseListen(cfg.Listen))
	fmt.Fprintf(out, "  export OPENAI_BASE_URL=http://%s/v1\n", normaliseListen(cfg.Listen))
	fmt.Fprintln(out, "ctrl-c to stop.")

	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	fmt.Fprintln(out, "tokenctl stopped cleanly.")
	return nil
}

// normaliseListen turns ":8080" into "localhost:8080" for printable URLs.
func normaliseListen(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

// ---------------------------------------------------------------------------
// top
// ---------------------------------------------------------------------------

func newTopCmd() *cobra.Command {
	var cfgPath string
	var interval time.Duration
	var once bool
	cmd := &cobra.Command{
		Use:   "top",
		Short: "Live view of token burn per group",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			return runTop(ctx, cmd.OutOrStdout(), cfg, interval, once)
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "tokenctl.yaml", "config path")
	cmd.Flags().DurationVar(&interval, "interval", 500*time.Millisecond, "refresh interval")
	cmd.Flags().BoolVar(&once, "once", false, "render a single snapshot and exit")
	return cmd
}

// topSnapshot is the wire shape served by the proxy at /v1/snapshot.
//
// The runtime budget tree (internal/budget) produces this; we decode loosely so
// added fields don't break older clients.
type topSnapshot struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Wallet      *topWallet  `json:"wallet,omitempty"`
	Groups      []topGroup  `json:"groups"`
	InFlight    int         `json:"in_flight"`
	Denies      int64       `json:"denies_total"`
	Throttles   int64       `json:"throttles_total"`
	Providers   []topByName `json:"providers,omitempty"`
}

type topGroup struct {
	Path           string  `json:"path"`
	Weight         int     `json:"weight"`
	Window         string  `json:"window"`
	BudgetTokens   int64   `json:"budget_tokens"`
	ConsumedTokens int64   `json:"consumed_tokens"`
	InFlight       int     `json:"in_flight"`
	State          string  `json:"state"`     // ok | soft | hard | preempted
	Frac           float64 `json:"frac_used"` // 0..1+
}

type topWallet struct {
	BudgetTokens   int64   `json:"budget_tokens"`
	ConsumedTokens int64   `json:"consumed_tokens"`
	Frac           float64 `json:"frac_used"`
}

type topByName struct {
	Name           string `json:"name"`
	ConsumedTokens int64  `json:"consumed_tokens"`
}

func runTop(ctx context.Context, out io.Writer, cfg *config.Config, interval time.Duration, once bool) error {
	endpoint := "http://" + normaliseListen(cfg.Metrics.Listen) + "/v1/snapshot"
	client := &http.Client{Timeout: 2 * time.Second}

	render := func() error {
		snap, err := fetchSnapshot(ctx, client, endpoint)
		if err != nil {
			return err
		}
		renderTop(out, snap)
		return nil
	}

	if once {
		return render()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	if err := render(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := render(); err != nil {
				fmt.Fprintf(out, "\n[tokenctl top] %v\n", err)
			}
		}
	}
}

func fetchSnapshot(ctx context.Context, client *http.Client, url string) (*topSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w (is `tokenctl up` running?)", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
	}
	var snap topSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return &snap, nil
}

const ansiClear = "\033[H\033[2J"

func renderTop(out io.Writer, snap *topSnapshot) {
	fmt.Fprint(out, ansiClear)
	fmt.Fprintf(out, "tokenctl top  %s  in-flight=%d  throttles=%d  denies=%d\n",
		snap.GeneratedAt.Format(time.RFC3339), snap.InFlight, snap.Throttles, snap.Denies)
	if snap.Wallet != nil {
		fmt.Fprintf(out, "wallet: %s  (%s)\n",
			fmtBar(snap.Wallet.Frac, 32),
			fmtFrac(snap.Wallet.ConsumedTokens, snap.Wallet.BudgetTokens))
	}
	fmt.Fprintln(out, strings.Repeat("─", 80))
	fmt.Fprintf(out, "%-32s  %-6s  %-10s  %-10s  %s\n", "GROUP", "WEIGHT", "USAGE", "BUDGET", "STATE")
	rows := append([]topGroup(nil), snap.Groups...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	for _, g := range rows {
		fmt.Fprintf(out, "%-32s  %-6d  %-10s  %-10s  %s\n",
			truncate(g.Path, 32),
			g.Weight,
			humanTokens(g.ConsumedTokens),
			humanTokens(g.BudgetTokens),
			stateLabel(g.State, g.Frac),
		)
	}
	if len(snap.Providers) > 0 {
		fmt.Fprintln(out, strings.Repeat("─", 80))
		fmt.Fprintln(out, "by provider:")
		for _, p := range snap.Providers {
			fmt.Fprintf(out, "  %-12s  %s\n", p.Name, humanTokens(p.ConsumedTokens))
		}
	}
}

func fmtBar(frac float64, width int) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(float64(width) * frac)
	return "[" + strings.Repeat("█", filled) + strings.Repeat("·", width-filled) + "]"
}

func fmtFrac(consumed, budget int64) string {
	if budget <= 0 {
		return humanTokens(consumed)
	}
	return fmt.Sprintf("%s / %s = %.0f%%",
		humanTokens(consumed), humanTokens(budget), 100*float64(consumed)/float64(budget))
}

func humanTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func stateLabel(state string, frac float64) string {
	switch state {
	case "hard":
		return fmt.Sprintf("DENY (%.0f%%)", 100*frac)
	case "soft":
		return fmt.Sprintf("throttle (%.0f%%)", 100*frac)
	case "preempted":
		return "preempted"
	default:
		return fmt.Sprintf("ok (%.0f%%)", 100*frac)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print tokenctl version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "tokenctl %s (commit %s, %s/%s, %s)\n",
				Version, Commit, runtime.GOOS, runtime.GOARCH, runtime.Version())
			return nil
		},
	}
}
