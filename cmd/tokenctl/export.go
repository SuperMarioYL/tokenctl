package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/SuperMarioYL/tokenctl/internal/audit"
	"github.com/SuperMarioYL/tokenctl/internal/config"
	"github.com/SuperMarioYL/tokenctl/internal/store"
)

// newExportCmd wires `tokenctl export --window <id> --format csv|json` — the
// finance-reconciliation surface (feat_cost_export_reconcile). It loads the
// config, opens the BoltDB store read-side, compiles the model_tiers resolver
// + pricing table, and emits a per-(team, provider, model_tier) spend table
// whose token sums reconcile against `tokenctl top`'s windowed snapshot and
// whose cost_estimate column is a hand-verifiable price join.
func newExportCmd() *cobra.Command {
	var (
		cfgPath string
		window  string
		format  string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Emit a per-team / per-provider / per-model-tier spend table for invoice reconciliation",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			return runExport(ctx, cmd, cfgPath, window, format)
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "tokenctl.yaml", "config path")
	cmd.Flags().StringVar(&window, "window", "", "window identifier (YYYY-MM calendar month, e.g. 2026-08)")
	cmd.Flags().StringVar(&format, "format", "csv", "output format: csv | json")
	return cmd
}

func runExport(ctx context.Context, cmd *cobra.Command, cfgPath, window, format string) error {
	if window == "" {
		// Default to the current UTC calendar month so a bare `tokenctl export`
		// reconciles the month-in-progress.
		window = time.Now().UTC().Format("2006-01")
	}
	start, end, err := audit.ParseWindowMonth(window)
	if err != nil {
		return err
	}
	switch format {
	case "csv", "json":
	default:
		return fmt.Errorf("--format %q: must be csv or json", format)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	state, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("open store: %w (is `tokenctl up` running? export needs read access to %s)", err, cfg.Store.Path)
	}
	defer state.Close()

	// Build the model_tiers resolver (first regex match wins) from the config.
	tierOf, err := compileTierResolver(cfg.ModelTiers)
	if err != nil {
		return err
	}
	// Build the pricing table from the config (optional — unpriced traffic
	// contributes 0 to cost_estimate rather than failing).
	//
	// Pricing is a *PricingConfig (yaml omitempty) that applyDefaults never
	// initializes, and is absent from both `tokenctl init`'s Sample()
	// (internal/config/config.go) and configs/tokenctl.example.yaml. Guard the
	// nil path so a bare `tokenctl export -c tokenctl.yaml` on the shipped
	// init/example config emits a valid zero-pricing export instead of panicking
	// with a nil-pointer dereference on the finance-reconciliation surface
	// (fix-export-nil-pricing-panic). The reconciler already tolerates an empty
	// price table — unpriced traffic contributes 0 to cost_estimate.
	var specs []audit.PriceSpec
	if cfg.Pricing != nil {
		specs = make([]audit.PriceSpec, 0, len(cfg.Pricing.Models))
		for _, mp := range cfg.Pricing.Models {
			specs = append(specs, audit.PriceSpec{
				Pattern:          mp.Pattern,
				InputPerMillion:  mp.InputPerMillion,
				OutputPerMillion: mp.OutputPerMillion,
			})
		}
	}
	prices, err := audit.CompilePrices(specs)
	if err != nil {
		return err
	}

	rows, err := audit.Reconcile(state, prices, tierOf, start, end)
	if err != nil {
		return err
	}
	// ctx only cancels on signal; the read is fast enough that mid-scan cancel
	// is not a practical concern, but we honour it for symmetry with `top`.
	_ = ctx

	out := cmd.OutOrStdout()
	switch format {
	case "json":
		return writeExportJSON(out, rows, window)
	default:
		return writeExportCSV(out, rows, window)
	}
}

// compileTierResolver returns a func mapping a request model to its tier name
// (first config pattern that matches), or "" when no tier matches.
func compileTierResolver(tiers []config.ModelTier) (func(string) string, error) {
	type compiled struct {
		re   *regexp.Regexp
		name string
	}
	out := make([]compiled, 0, len(tiers))
	for _, mt := range tiers {
		re, err := regexp.Compile(mt.Pattern)
		if err != nil {
			return nil, fmt.Errorf("export: tier %q pattern %q: %w", mt.Name, mt.Pattern, err)
		}
		out = append(out, compiled{re: re, name: mt.Name})
	}
	return func(model string) string {
		if model == "" {
			return ""
		}
		for _, c := range out {
			if c.re.MatchString(model) {
				return c.name
			}
		}
		return ""
	}, nil
}

func writeExportCSV(w io.Writer, rows []audit.Row, window string) error {
	fmt.Fprintf(w, "# tokenctl export window=%s generated=%s\n", window, time.Now().UTC().Format(time.RFC3339))
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{"team", "provider", "model_tier", "input_tokens", "output_tokens", "cost_estimate"}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := cw.Write([]string{
			r.Team, r.Provider, r.ModelTier,
			fmt.Sprintf("%d", r.InputTokens),
			fmt.Sprintf("%d", r.OutputTokens),
			fmt.Sprintf("%.6f", r.CostEstimate),
		}); err != nil {
			return err
		}
	}
	return nil
}

func writeExportJSON(w io.Writer, rows []audit.Row, window string) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"window":    window,
		"generated": time.Now().UTC().Format(time.RFC3339),
		"rows":      rows,
	})
}
