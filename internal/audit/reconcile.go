// Package audit owns the read-side reconciliation of tokenctl's append-only
// audit log + windowed counters into the spend tables a finance team matches
// against a provider invoice (feat_cost_export_reconcile).
//
// It is deliberately decoupled from config + the budget tree: the export
// subcommand (cmd/tokenctl/export.go) loads the config, compiles the
// model_tiers resolver + pricing table, and hands them to Reconcile alongside
// a ReconcileStore (satisfied by *store.Store). This keeps the audit package
// free of import cycles and lets it be unit-tested against an in-memory
// fixture store.
package audit

import (
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/SuperMarioYL/tokenctl/internal/budget"
)

// ReconcileStore is the read-only view of the persisted state the reconciler
// needs. *store.Store satisfies it via ScanAudit + ScanCounters.
type ReconcileStore interface {
	ScanAudit(fn func(budget.AuditEvent) bool) error
	ScanCounters(fn func(group string, consumed int64, windowStart time.Time) bool) error
}

// PriceSpec is one uncompiled row of the pricing table (per-1M-tokens).
type PriceSpec struct {
	Pattern          string
	InputPerMillion   float64
	OutputPerMillion  float64
}

// Price is a compiled pricing-table row. Pattern matches the request `model`
// field; the first match wins. Prices are USD per one million tokens.
type Price struct {
	Pattern          string
	re               *regexp.Regexp
	InputPerMillion   float64
	OutputPerMillion  float64
}

// CompilePrices compiles price specs into matchable Prices. Returns an error
// if a pattern does not compile.
func CompilePrices(specs []PriceSpec) ([]Price, error) {
	out := make([]Price, 0, len(specs))
	for _, s := range specs {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return nil, fmt.Errorf("audit: price pattern %q: %w", s.Pattern, err)
		}
		out = append(out, Price{
			Pattern:          s.Pattern,
			re:               re,
			InputPerMillion:   s.InputPerMillion,
			OutputPerMillion:  s.OutputPerMillion,
		})
	}
	return out, nil
}

// Row is one finance-reconciliation row: the window's spend for one
// (team, provider, model_tier) triple, with input/output token sums and a
// cost_estimate joined from the pricing table.
type Row struct {
	Team          string  `json:"team"`
	Provider      string  `json:"provider"`
	ModelTier     string  `json:"model_tier"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CostEstimate  float64 `json:"cost_estimate"`
}

// Reconcile aggregates the window's spend from the audit log's release events
// (each carries Group=leaf path, Provider, Model, InTokens, OutTokens) into
// one row per (team, provider, model_tier), then joins the pricing table to
// compute cost_estimate. The tierOf callback maps a model string to its tier
// name (compiled from config.ModelTiers by the caller); "" falls back to the
// "default" tier. Only release events whose At falls in [start, end) count.
//
// Tokens here are the RAW metered deltas (pre-cost-multiplier): finance
// reconciles an invoice against raw token usage, while the cost_multiplier is
// a budget-control concept that lives on the enforcement side. The windowed
// counters (ScanCounters) hold the cost-multiplied attributed totals; the
// export's raw sums match them exactly when no tier carries a multiplier.
func Reconcile(store ReconcileStore, prices []Price, tierOf func(model string) string, start, end time.Time) ([]Row, error) {
	if tierOf == nil {
		tierOf = func(string) string { return "default" }
	}
	type key struct{ team, provider, tier string }
	type acc struct {
		in, out int64
		// per-model token breakdown so cost_estimate can join the right price
		// when one tier spans several priced models.
		models map[string][2]int64
	}
	agg := map[key]*acc{}

	scanErr := store.ScanAudit(func(e budget.AuditEvent) bool {
		if e.Kind != "release" {
			return true
		}
		if e.At.Before(start) || !e.At.Before(end) {
			return true
		}
		tier := tierOf(e.Model)
		if tier == "" {
			tier = "default"
		}
		k := key{team: e.Group, provider: e.Provider, tier: tier}
		a, ok := agg[k]
		if !ok {
			a = &acc{models: map[string][2]int64{}}
			agg[k] = a
		}
		a.in += e.InTokens
		a.out += e.OutTokens
		m := a.models[e.Model]
		m[0] += e.InTokens
		m[1] += e.OutTokens
		a.models[e.Model] = m
		return true
	})
	if scanErr != nil {
		return nil, fmt.Errorf("audit: scan: %w", scanErr)
	}

	keys := make([]key, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].team != keys[j].team {
			return keys[i].team < keys[j].team
		}
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].tier < keys[j].tier
	})
	rows := make([]Row, 0, len(keys))
	for _, k := range keys {
		a := agg[k]
		var cost float64
		for model, toks := range a.models {
			inP, outP := priceFor(prices, model)
			cost += float64(toks[0])*inP/1e6 + float64(toks[1])*outP/1e6
		}
		rows = append(rows, Row{
			Team:         k.team,
			Provider:     k.provider,
			ModelTier:    k.tier,
			InputTokens:  a.in,
			OutputTokens: a.out,
			CostEstimate: cost,
		})
	}
	return rows, nil
}

// priceFor returns the (input, output) per-1M price for model, or (0, 0) when
// no pricing-table row matches (unpriced traffic contributes 0 to cost_estimate
// rather than aborting the export).
func priceFor(prices []Price, model string) (float64, float64) {
	if model == "" {
		return 0, 0
	}
	for _, p := range prices {
		if p.re.MatchString(model) {
			return p.InputPerMillion, p.OutputPerMillion
		}
	}
	return 0, 0
}

// ParseWindowMonth parses a "YYYY-MM" calendar window identifier (e.g.
// "2026-08") into the half-open [firstOfMonth, firstOfNextMonth) time range in
// UTC. This is the natural invoice-reconciliation granularity: a finance team
// matches a provider's monthly invoice against the window's audited spend.
func ParseWindowMonth(s string) (time.Time, time.Time, error) {
	t, err := time.Parse("2006-01", s)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("audit: window %q: %w (want YYYY-MM)", s, err)
	}
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	return start, end, nil
}
