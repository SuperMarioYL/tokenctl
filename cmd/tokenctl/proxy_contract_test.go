package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/SuperMarioYL/tokenctl/internal/store"
)

// TestProxyAdmitContract is the m4 integration regression for the shipped m2
// tree-weighting contract through the REAL `tokenctl up` proxy path. It
// pre-seeds the BoltDB counters (so the live tree restores a known consumed
// level at boot), starts runProxyCtx against an httptest upstream, sends ONE
// request per case, and asserts the (status code, X-TokenCtl-Reason) pair for
// every shipped admission outcome:
//
//	admitted (200) · unknown_key (401) · missing_api_key (401) ·
//	soft_throttle (429) · leaf budget_exceeded (429) · wallet budget_exceeded (429)
//
// Pre-seeding means every decision is made at admit time — no async metering
// or poll loops are needed, so the table is deterministic and fast.
func TestProxyAdmitContract(t *testing.T) {
	const (
		orgBudget    = 1_000_000
		leafBudget   = 1_000_000
		walletBudget = 1_000_000
	)

	cases := []struct {
		name       string
		seed       map[string]int64 // counters to pre-seed in BoltDB before boot
		apiKey     string           // x-api-key header value; "" omits the header entirely
		wantStatus int
		wantReason string // X-TokenCtl-Reason; "" asserts the header is absent
	}{
		{name: "admitted", apiKey: "alice-key", wantStatus: http.StatusOK, wantReason: ""},
		{name: "unknown_key", apiKey: "not-bound", wantStatus: http.StatusUnauthorized, wantReason: "unknown_key"},
		{name: "missing_api_key", apiKey: "", wantStatus: http.StatusUnauthorized, wantReason: "missing_api_key"},
		{name: "soft_throttle", seed: map[string]int64{"acme.alice": 800_000, "acme": 800_000}, apiKey: "alice-key", wantStatus: http.StatusTooManyRequests, wantReason: "soft_throttle"},
		{name: "leaf_budget_exceeded", seed: map[string]int64{"acme.alice": 1_000_000, "acme": 1_000_000}, apiKey: "alice-key", wantStatus: http.StatusTooManyRequests, wantReason: "budget_exceeded"},
		{name: "wallet_budget_exceeded", seed: map[string]int64{"__wallet__": 1_000_000}, apiKey: "alice-key", wantStatus: http.StatusTooManyRequests, wantReason: "budget_exceeded"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Stub Anthropic upstream: only the "admitted" row reaches it; the
			// deny/throttle rows fail at Admit before the reverse proxy runs.
			upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				rw.Header().Set("Content-Type", "application/json")
				rw.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(rw, `{"id":"m","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer upstream.Close()

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "tokenctl.yaml")
			proxyAddr := freePort(t)
			metricsAddr := freePort(t)

			// Wallet soft_throttle_at=1.0 so the wallet only hard-denies (no
			// wallet soft throttle interfering with the leaf-soft row); leaf/org
			// soft_throttle_at=0.8 is the shipped default the soft_throttle row
			// exercises. Budgets are large relative to the 8000-token default
			// in-flight reserve so the reserve never itself triggers a deny on
			// the "admitted" baseline.
			cfg := fmt.Sprintf(`version: v0.1
listen: %q
store:
  path: tokenctl.db
metrics:
  listen: %q
  path: /metrics
wallet:
  budget:
    tokens: %d
    window: 24h
    soft_throttle_at: 1.0
providers:
  - name: claude
    upstream: %q
tree:
  name: acme
  weight: 100
  budget:
    tokens: %d
    window: 24h
    soft_throttle_at: 0.8
  children:
    - name: alice
      weight: 100
      budget:
        tokens: %d
        window: 24h
        soft_throttle_at: 0.8
api_keys:
  - key: alice-key
    group: acme.alice
`, proxyAddr, metricsAddr, walletBudget, upstream.URL, orgBudget, leafBudget)
			if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			// Pre-seed the BoltDB counters so the live tree restores a known
			// consumed level at boot and the first request hits the intended
			// decision immediately (no async attribution / polling required).
			if len(tc.seed) > 0 {
				seedCounters(t, filepath.Join(dir, "tokenctl.db"), tc.seed)
			}

			// Start the real runProxy (the path `tokenctl up` takes).
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			cmd.SetOut(io.Discard)
			runErr := make(chan error, 1)
			go func() { runErr <- runProxyCtx(ctx, cmd, cfgPath) }()

			base := "http://" + proxyAddr
			waitHealthy(t, base)

			req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{"model":"claude"}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tc.apiKey != "" {
				req.Header.Set("x-api-key", tc.apiKey)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q reason=%q)",
					resp.StatusCode, tc.wantStatus, body, resp.Header.Get("X-TokenCtl-Reason"))
			}
			if got := resp.Header.Get("X-TokenCtl-Reason"); got != tc.wantReason {
				t.Fatalf("X-TokenCtl-Reason = %q, want %q (body=%q)", got, tc.wantReason, body)
			}
			// soft_throttle must also advertise a Retry-After (the shipped m2
			// soft-throttle response contract).
			if tc.wantReason == "soft_throttle" && resp.Header.Get("Retry-After") == "" {
				t.Fatalf("soft_throttle response missing Retry-After header (body=%q)", body)
			}

			// Shut the proxy down cleanly.
			cancel()
			select {
			case err := <-runErr:
				if err != nil && !strings.Contains(err.Error(), "context canceled") {
					t.Fatalf("runProxy returned error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("runProxy did not return after cancel")
			}
		})
	}
}

// seedCounters writes the supplied consumed counters into the BoltDB file at
// dbPath and flushes them to disk (via Close) so a subsequent store.Open (the
// proxy's boot path) restores them through budget.NewTree's LoadCounter.
func seedCounters(t *testing.T, dbPath string, counters map[string]int64) {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seed store.Open: %v", err)
	}
	now := time.Now()
	for g, c := range counters {
		if err := st.SaveCounter(g, c, now); err != nil {
			t.Fatalf("seed SaveCounter(%s): %v", g, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("seed Close (flush): %v", err)
	}
}
