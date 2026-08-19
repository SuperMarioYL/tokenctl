package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SuperMarioYL/tokenctl/internal/audit"
	"github.com/SuperMarioYL/tokenctl/internal/budget"
	"github.com/SuperMarioYL/tokenctl/internal/config"
	"github.com/SuperMarioYL/tokenctl/internal/store"
)

// TestExport_ReconcileFixtureStore is the feat_cost_export_reconcile
// regression: against a seeded store it produces one row per
// (team, provider, model_tier) whose token sums reconcile against the audited
// release events and whose cost_estimate column matches a hand-computed price
// join. Non-release events (deny/throttle) are excluded.
func TestExport_ReconcileFixtureStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/tokenctl.db"
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Seed the audit log with release events in August 2026 plus a deny (which
	// must be excluded from the spend table).
	inWindow := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	outOfWindow := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	seed := []budget.AuditEvent{
		{At: inWindow, Kind: "release", Group: "acme.team-platform.alice", Provider: "claude", Model: "claude-opus-4-20250514", InTokens: 1000, OutTokens: 500},
		{At: inWindow, Kind: "release", Group: "acme.team-platform.alice", Provider: "claude", Model: "claude-haiku-3-5", InTokens: 2000, OutTokens: 1000},
		{At: inWindow, Kind: "release", Group: "acme.team-product.carol", Provider: "openai", Model: "gpt-4o", InTokens: 3000, OutTokens: 1500},
		// A deny must not count toward spend.
		{At: inWindow, Kind: "deny", Group: "acme.team-platform.alice", Provider: "claude", Model: "claude-opus-4-20250514", Reason: "budget_exceeded_tier"},
		// An out-of-window release must not count for the 2026-08 export.
		{At: outOfWindow, Kind: "release", Group: "acme.team-platform.alice", Provider: "claude", Model: "claude-opus-4-20250514", InTokens: 999, OutTokens: 999},
	}
	for _, e := range seed {
		if err := st.AppendAudit(e); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}

	// Build the tier resolver + pricing table the way the export command does.
	tierOf, err := compileTierResolver([]config.ModelTier{
		{Name: "opus", Pattern: "claude-opus.*"},
		{Name: "haiku", Pattern: "claude-haiku.*"},
		{Name: "gpt4", Pattern: "gpt-4.*"},
	})
	if err != nil {
		t.Fatalf("compileTierResolver: %v", err)
	}
	prices, err := audit.CompilePrices([]audit.PriceSpec{
		{Pattern: "claude-opus.*", InputPerMillion: 15, OutputPerMillion: 75},
		{Pattern: "claude-haiku.*", InputPerMillion: 0.25, OutputPerMillion: 1.25},
		{Pattern: "gpt-4.*", InputPerMillion: 2.5, OutputPerMillion: 10},
	})
	if err != nil {
		t.Fatalf("CompilePrices: %v", err)
	}

	start, end, err := audit.ParseWindowMonth("2026-08")
	if err != nil {
		t.Fatalf("ParseWindowMonth: %v", err)
	}
	rows, err := audit.Reconcile(st, prices, tierOf, start, end)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Expect 3 rows (opus, haiku, gpt4). The deny + out-of-window release must
	// NOT appear.
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (deny + out-of-window excluded): %+v", len(rows), rows)
	}

	want := map[string]audit.Row{
		"acme.team-platform.alice|claude|opus": {
			Team: "acme.team-platform.alice", Provider: "claude", ModelTier: "opus",
			InputTokens: 1000, OutputTokens: 500,
			// 1000*15/1e6 + 500*75/1e6 = 0.015 + 0.0375
			CostEstimate: 0.015 + 0.0375,
		},
		"acme.team-platform.alice|claude|haiku": {
			Team: "acme.team-platform.alice", Provider: "claude", ModelTier: "haiku",
			InputTokens: 2000, OutputTokens: 1000,
			// 2000*0.25/1e6 + 1000*1.25/1e6 = 0.0005 + 0.00125
			CostEstimate: 0.0005 + 0.00125,
		},
		"acme.team-product.carol|openai|gpt4": {
			Team: "acme.team-product.carol", Provider: "openai", ModelTier: "gpt4",
			InputTokens: 3000, OutputTokens: 1500,
			// 3000*2.5/1e6 + 1500*10/1e6 = 0.0075 + 0.015
			CostEstimate: 0.0075 + 0.015,
		},
	}
	for _, r := range rows {
		key := r.Team + "|" + r.Provider + "|" + r.ModelTier
		w, ok := want[key]
		if !ok {
			t.Errorf("unexpected row %q: %+v", key, r)
			continue
		}
		if r.InputTokens != w.InputTokens || r.OutputTokens != w.OutputTokens {
			t.Errorf("row %q tokens = in=%d out=%d, want in=%d out=%d", key, r.InputTokens, r.OutputTokens, w.InputTokens, w.OutputTokens)
		}
		if !nearlyEqual(r.CostEstimate, w.CostEstimate, 1e-9) {
			t.Errorf("row %q cost_estimate = %.9f, want %.9f", key, r.CostEstimate, w.CostEstimate)
		}
	}
}

// TestExport_ReconcileMatchesWindowedCounters verifies the export's raw token
// sums match the windowed counter snapshot when no cost_multiplier is in play
// (the pre-v0.9.0 reconciliation guarantee): the audit-derived in+out for a
// team equals the persisted consumed counter for that group.
func TestExport_ReconcileMatchesWindowedCounters(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/tokenctl.db"
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	at := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	if err := st.AppendAudit(budget.AuditEvent{
		At: at, Kind: "release", Group: "acme.alice", Provider: "claude", Model: "claude-opus-4", InTokens: 400, OutTokens: 600,
	}); err != nil {
		t.Fatal(err)
	}
	// Persist a windowed counter that agrees (in+out = 1000 = consumed).
	// SaveCounter is buffered, so Close flushes it to bbolt before we reopen.
	if err := st.SaveCounter("acme.alice", 1000, at); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen for the read side: audit log + counter are now both on disk.
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open reopen: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	start, end, _ := audit.ParseWindowMonth("2026-08")
	rows, err := audit.Reconcile(st, nil, nil, start, end)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].InputTokens+rows[0].OutputTokens != 1000 {
		t.Fatalf("export token sum = %d, want 1000 (matches windowed counter)", rows[0].InputTokens+rows[0].OutputTokens)
	}

	// And the windowed counter agrees.
	var counter int64
	if err := st.ScanCounters(func(group string, consumed int64, _ time.Time) bool {
		if group == "acme.alice" {
			counter = consumed
		}
		return true
	}); err != nil {
		t.Fatalf("ScanCounters: %v", err)
	}
	if counter != 1000 {
		t.Fatalf("windowed counter = %d, want 1000", counter)
	}
}

// TestExport_CSVAndJSONFormatters verifies the CSV + JSON output shapes carry
// every reconciliation column.
func TestExport_CSVAndJSONFormatters(t *testing.T) {
	rows := []audit.Row{
		{Team: "acme.alice", Provider: "claude", ModelTier: "opus", InputTokens: 1000, OutputTokens: 500, CostEstimate: 0.0525},
	}
	var buf bytes.Buffer
	if err := writeExportCSV(&buf, rows, "2026-08"); err != nil {
		t.Fatalf("writeExportCSV: %v", err)
	}
	csvOut := buf.String()
	for _, col := range []string{"team", "provider", "model_tier", "input_tokens", "output_tokens", "cost_estimate", "acme.alice", "opus"} {
		if !strings.Contains(csvOut, col) {
			t.Errorf("CSV missing %q:\n%s", col, csvOut)
		}
	}

	buf.Reset()
	if err := writeExportJSON(&buf, rows, "2026-08"); err != nil {
		t.Fatalf("writeExportJSON: %v", err)
	}
	var doc struct {
		Window string      `json:"window"`
		Rows   []audit.Row `json:"rows"`
	}
	if err := json.NewDecoder(&buf).Decode(&doc); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if doc.Window != "2026-08" || len(doc.Rows) != 1 || doc.Rows[0].ModelTier != "opus" {
		t.Errorf("JSON doc = %+v, want window=2026-08 one opus row", doc)
	}
}

func nearlyEqual(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
