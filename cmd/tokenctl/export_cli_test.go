package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/SuperMarioYL/tokenctl/internal/budget"
	"github.com/SuperMarioYL/tokenctl/internal/store"
)

// TestExportCLI is the m4 integration regression for the shipped
// feat_cost_export_reconcile surface. It exercises the runExport CLI entrypoint
// (the path `tokenctl export --format ... --window ...` takes) across format,
// window defaulting, and invalid format/window rejection — guarding the
// finance-reconciliation command at the CLI level rather than only at the
// formatter / reconciler unit level the existing export_test.go covers.
//
// Every row shares one seeded store: a single 2026-08 release event on
// acme.alice / claude / claude-opus, which reconciles to one row whose
// cost_estimate is a hand-verifiable price join.
func TestExportCLI(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tokenctl.yaml")
	dbPath := filepath.Join(dir, "tokenctl.db")

	cfg := `version: v0.1
listen: ":8080"
store:
  path: tokenctl.db
metrics:
  listen: ":9090"
  path: /metrics
providers:
  - name: claude
    upstream: https://api.anthropic.com
  - name: openai
    upstream: https://api.openai.com
tree:
  name: acme
  weight: 100
  budget:
    tokens: 1000000
    window: 24h
    soft_throttle_at: 0.8
  children:
    - name: alice
      weight: 100
      budget:
        tokens: 500000
        window: 24h
        soft_throttle_at: 0.8
api_keys:
  - key: alice-key
    group: acme.alice
model_tiers:
  - name: opus
    pattern: "claude-opus.*"
  - name: haiku
    pattern: "claude-haiku.*"
  - name: gpt4
    pattern: "gpt-4.*"
pricing:
  models:
    - pattern: "claude-opus.*"
      input_per_million: 15
      output_per_million: 75
    - pattern: "claude-haiku.*"
      input_per_million: 0.25
      output_per_million: 1.25
    - pattern: "gpt-4.*"
      input_per_million: 2.5
      output_per_million: 10
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Seed one in-window release event. AppendAudit is synchronous, but Close
	// releases the bbolt flock so runExport's store.Open does not contend.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seed store.Open: %v", err)
	}
	at := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	if err := st.AppendAudit(budget.AuditEvent{
		At: at, Kind: "release", Group: "acme.alice", Provider: "claude",
		Model: "claude-opus-4-20250514", InTokens: 1000, OutTokens: 500,
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	cases := []struct {
		name     string
		format   string
		window   string
		wantErr  string // non-empty substring the returned error must contain
		checkOut func(t *testing.T, out string)
	}{
		{
			name: "csv_ok", format: "csv", window: "2026-08",
			checkOut: func(t *testing.T, out string) {
				for _, want := range []string{
					"# tokenctl export window=2026-08",
					"team,provider,model_tier,input_tokens,output_tokens,cost_estimate",
					"acme.alice,claude,opus,1000,500,0.052500", // 1000*15/1e6 + 500*75/1e6
				} {
					if !strings.Contains(out, want) {
						t.Errorf("csv output missing %q:\n%s", want, out)
					}
				}
			},
		},
		{
			name: "json_ok", format: "json", window: "2026-08",
			checkOut: func(t *testing.T, out string) {
				var doc struct {
					Window string `json:"window"`
					Rows   []struct {
						Team         string  `json:"team"`
						Provider     string  `json:"provider"`
						ModelTier    string  `json:"model_tier"`
						InputTokens  int64   `json:"input_tokens"`
						OutputTokens int64   `json:"output_tokens"`
						CostEstimate float64 `json:"cost_estimate"`
					} `json:"rows"`
				}
				if err := json.NewDecoder(strings.NewReader(out)).Decode(&doc); err != nil {
					t.Fatalf("decode JSON export: %v\nraw:\n%s", err, out)
				}
				if doc.Window != "2026-08" || len(doc.Rows) != 1 {
					t.Fatalf("json doc = %+v, want window=2026-08 one row", doc)
				}
				r := doc.Rows[0]
				if r.Team != "acme.alice" || r.Provider != "claude" || r.ModelTier != "opus" ||
					r.InputTokens != 1000 || r.OutputTokens != 500 {
					t.Errorf("json row = %+v, want acme.alice/claude/opus/1000/500", r)
				}
				if !nearlyEqual(r.CostEstimate, 0.0525, 1e-9) {
					t.Errorf("json cost_estimate = %.9f, want 0.0525", r.CostEstimate)
				}
			},
		},
		{name: "bad_format_rejected", format: "xml", window: "2026-08", wantErr: "must be csv or json"},
		{name: "bad_window_rejected", format: "csv", window: "not-a-month", wantErr: "want YYYY-MM"},
		{
			name: "empty_window_defaults_to_current_month", format: "csv", window: "",
			checkOut: func(t *testing.T, out string) {
				// The default month is valid; the CSV header must still appear.
				if !strings.Contains(out, "team,provider,model_tier,input_tokens,output_tokens,cost_estimate") {
					t.Errorf("default-window csv missing header:\n%s", out)
				}
				if !strings.HasPrefix(out, "# tokenctl export window=") {
					t.Errorf("default-window csv missing window comment:\n%s", out)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			err := runExport(context.Background(), cmd, cfgPath, tc.window, tc.format)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("runExport err = nil, want error containing %q (out=%q)", tc.wantErr, out.String())
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("runExport err = %v, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("runExport err = %v, want nil", err)
			}
			if tc.checkOut != nil {
				tc.checkOut(t, out.String())
			}
		})
	}
}

// TestExportCLINoPricingBlock is the fix-export-nil-pricing-panic regression.
// runExport unconditionally dereferenced cfg.Pricing.Models, but Config.Pricing
// is a *PricingConfig (yaml omitempty) that applyDefaults never initializes and
// is absent from both `tokenctl init`'s Sample() and the shipped
// configs/tokenctl.example.yaml — so a bare `tokenctl export -c tokenctl.yaml`
// on the shipped init/example config panicked with a nil-pointer dereference on
// the finance-reconciliation surface the command exists for. (The existing
// TestExportCLI fixtures always inject a pricing block, so the nil path was
// untested.)
//
// This config mirrors that shipped shape: no `pricing:` block at all. It must
// export successfully (NOT panic) and the seeded release event must reconcile to
// a row whose cost_estimate is 0.000000 — unpriced traffic contributes 0 to
// cost_estimate rather than aborting the export.
func TestExportCLINoPricingBlock(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tokenctl.yaml")
	dbPath := filepath.Join(dir, "tokenctl.db")

	// No `pricing:` block — mirrors tokenctl init's Sample() +
	// configs/tokenctl.example.yaml. model_tiers is kept so the tier resolver
	// still resolves; only pricing is absent.
	cfg := `version: v0.1
listen: ":8080"
store:
  path: tokenctl.db
metrics:
  listen: ":9090"
  path: /metrics
providers:
  - name: claude
    upstream: https://api.anthropic.com
tree:
  name: acme
  weight: 100
  budget:
    tokens: 1000000
    window: 24h
    soft_throttle_at: 0.8
  children:
    - name: alice
      weight: 100
      budget:
        tokens: 500000
        window: 24h
        soft_throttle_at: 0.8
api_keys:
  - key: alice-key
    group: acme.alice
model_tiers:
  - name: opus
    pattern: "claude-opus.*"
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Seed one in-window release event so there is traffic to reconcile; its
	// cost_estimate must be 0 because no pricing table is configured.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seed store.Open: %v", err)
	}
	at := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	if err := st.AppendAudit(budget.AuditEvent{
		At: at, Kind: "release", Group: "acme.alice", Provider: "claude",
		Model: "claude-opus-4-20250514", InTokens: 1000, OutTokens: 500,
	}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	// The recovered() guard turns a nil-pointer panic (the pre-fix bug) into a
	// test failure with the panic value instead of a process crash, so the
	// regression is observable even if the deref path slips back in.
	cases := []struct {
		name   string
		format string
	}{
		{name: "csv", format: "csv"},
		{name: "json", format: "json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)

			var runErr error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("runExport panicked on a config with no pricing block (nil cfg.Pricing deref): %v", r)
					}
				}()
				runErr = runExport(context.Background(), cmd, cfgPath, "2026-08", tc.format)
			}()
			if runErr != nil {
				t.Fatalf("runExport err = %v, want nil (no-pricing config must export, not error)", runErr)
			}

			body := out.String()
			switch tc.format {
			case "csv":
				// CSV header + the reconciled row; cost_estimate must be 0.000000
				// (unpriced traffic contributes 0 rather than failing).
				for _, want := range []string{
					"# tokenctl export window=2026-08",
					"team,provider,model_tier,input_tokens,output_tokens,cost_estimate",
					"acme.alice,claude,opus,1000,500,0.000000",
				} {
					if !strings.Contains(body, want) {
						t.Errorf("csv output missing %q:\n%s", want, body)
					}
				}
			case "json":
				var doc struct {
					Window string `json:"window"`
					Rows   []struct {
						Team         string  `json:"team"`
						Provider     string  `json:"provider"`
						ModelTier    string  `json:"model_tier"`
						InputTokens  int64   `json:"input_tokens"`
						OutputTokens int64   `json:"output_tokens"`
						CostEstimate float64 `json:"cost_estimate"`
					} `json:"rows"`
				}
				if err := json.NewDecoder(strings.NewReader(body)).Decode(&doc); err != nil {
					t.Fatalf("decode JSON export: %v\nraw:\n%s", err, body)
				}
				if doc.Window != "2026-08" || len(doc.Rows) != 1 {
					t.Fatalf("json doc = %+v, want window=2026-08 one row", doc)
				}
				r := doc.Rows[0]
				if r.Team != "acme.alice" || r.Provider != "claude" || r.ModelTier != "opus" ||
					r.InputTokens != 1000 || r.OutputTokens != 500 {
					t.Errorf("json row = %+v, want acme.alice/claude/opus/1000/500", r)
				}
				if !nearlyEqual(r.CostEstimate, 0, 1e-9) {
					t.Errorf("json cost_estimate = %.9f, want 0 (no pricing table configured)", r.CostEstimate)
				}
			}
		})
	}
}
