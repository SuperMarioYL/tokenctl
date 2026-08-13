package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// freePort asks the OS for an unused TCP port and returns ":<port>".
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close()
	return fmt.Sprintf("127.0.0.1:%d", addr.Port)
}

// waitHealthy polls the proxy's /healthz until it answers or the deadline hits.
func waitHealthy(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("proxy never became healthy")
}

// TestRunProxyBindsKeysAndWallet is the end-to-end regression for
// fix-runproxy-never-binds-keys-or-wallet. It starts the REAL proxy via
// runProxy (the same path `tokenctl up` takes) from an on-disk config with a
// bound API key and a wallet cap, pointed at an httptest stub upstream, and
// asserts:
//
//	(a) a request carrying the bound key is ADMITTED (non-401) and reaches the
//	    stub upstream — proving tree.BindAll(cfg.APIKeys) ran, so the leaf lookup
//	    no longer misses on every request (the pre-fix bug was a blanket 401
//	    unknown_key for ALL live traffic);
//	(b) the advertised org wallet cap is actually enforced — once the metered
//	    spend crosses the cap, subsequent traffic is hard-denied with 429 +
//	    X-TokenCtl-Reason: budget_exceeded — proving tree.SetWallet(cfg.Wallet)
//	    ran (the pre-fix bug left t.walletBudget nil, a silent no-op cap).
//
// Against the unwired v0.4.0 runProxy this test FAILS at (a): the bound key is
// rejected 401 because leafByKey is empty.
func TestRunProxyBindsKeysAndWallet(t *testing.T) {
	var upstreamHits int
	// Stub Anthropic upstream: returns a buffered JSON body whose usage pushes
	// the wallet past its cap in a single request, so the NEXT request is denied.
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		// input_tokens far exceeds the 1,000,000-token wallet cap below, so after
		// this single completion walletConsumed > cap and the wallet hard-denies.
		_, _ = io.WriteString(rw, `{"id":"msg_1","usage":{"input_tokens":2000000,"output_tokens":0}}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tokenctl.yaml")
	proxyAddr := freePort(t)
	metricsAddr := freePort(t)

	// A minimal single-org / single-leaf tree so the wallet is the only binding
	// constraint (leaf + org budgets are large; wallet soft_throttle_at=1.0 so
	// only the hard-deny at 100% fires, not a soft throttle).
	cfg := fmt.Sprintf(`version: v0.1
listen: %q
store:
  path: tokenctl.db
metrics:
  listen: %q
  path: /metrics
wallet:
  budget:
    tokens: 1000000
    window: 24h
    soft_throttle_at: 1.0
providers:
  - name: claude
    upstream: %q
tree:
  name: acme
  weight: 100
  budget:
    tokens: 100000000
    window: 24h
    soft_throttle_at: 1.0
  children:
    - name: alice
      weight: 100
      budget:
        tokens: 100000000
        window: 24h
        soft_throttle_at: 1.0
api_keys:
  - key: alice-key
    group: acme.alice
`, proxyAddr, metricsAddr, upstream.URL)

	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Run the real runProxy (what `tokenctl up` calls) in a goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	runErr := make(chan error, 1)
	go func() { runErr <- runProxyCtx(ctx, cmd, cfgPath) }()

	base := "http://" + proxyAddr
	waitHealthy(t, base)

	post := func() *http.Response {
		req, err := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{"model":"claude"}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("x-api-key", "alice-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		return resp
	}

	// (a) The bound key is admitted (NOT 401) and reaches the stub upstream.
	resp1 := post()
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if resp1.StatusCode == http.StatusUnauthorized {
		t.Fatalf("bound key got 401 (unknown_key) — tree.BindAll(cfg.APIKeys) was never called; body=%q", body1)
	}
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d, want 200 (admitted + proxied); body=%q reason=%q",
			resp1.StatusCode, body1, resp1.Header.Get("X-TokenCtl-Reason"))
	}
	if upstreamHits == 0 {
		t.Fatal("request never reached the stub upstream despite a bound key")
	}
	// The proxy stamps the attributed leaf on every proxied response — a further
	// proof the admission (not a 401 bail-out) produced this response.
	if got := resp1.Header.Get("X-TokenCtl-Group"); got != "acme.alice" {
		t.Fatalf("X-TokenCtl-Group = %q, want acme.alice", got)
	}

	// (b) The wallet cap is honoured: after the first completion metered
	// 2,000,000 tokens against a 1,000,000 cap, the next request is hard-denied.
	// Attribution is async (credited as the body streams), so poll briefly for
	// the deny to appear rather than assuming instantaneous crediting.
	var denied *http.Response
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r := post()
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		if r.StatusCode == http.StatusTooManyRequests {
			denied = r
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if denied == nil {
		t.Fatal("wallet cap was never enforced — traffic over the cap kept being admitted; tree.SetWallet(cfg.Wallet) was never called (t.walletBudget stayed nil)")
	}
	if got := denied.Header.Get("X-TokenCtl-Reason"); got != "budget_exceeded" {
		t.Fatalf("deny X-TokenCtl-Reason = %q, want budget_exceeded", got)
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
}

// TestRunProxyStartsWithoutWallet guards the "wallet is optional" branch: a
// config with no wallet block must still start (SetWallet(nil) is a no-op) and
// admit bound-key traffic.
func TestRunProxyStartsWithoutWallet(t *testing.T) {
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

	cfg := fmt.Sprintf(`version: v0.1
listen: %q
store:
  path: tokenctl.db
metrics:
  listen: %q
  path: /metrics
providers:
  - name: claude
    upstream: %q
tree:
  name: acme
  weight: 100
  budget:
    tokens: 100000000
    window: 24h
    soft_throttle_at: 0.8
  children:
    - name: alice
      weight: 100
      budget:
        tokens: 100000000
        window: 24h
        soft_throttle_at: 0.8
api_keys:
  - key: alice-key
    group: acme.alice
`, proxyAddr, metricsAddr, upstream.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	runErr := make(chan error, 1)
	go func() { runErr <- runProxyCtx(ctx, cmd, cfgPath) }()

	base := "http://" + proxyAddr
	waitHealthy(t, base)

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("x-api-key", "alice-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no-wallet config: status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("runProxy did not return after cancel")
	}
}

// TestRunProxyWiresModelTiers is the fix-model-tiers-not-wired-in-main
// regression. runProxyCtx wired BindAll + SetWallet but never called
// tree.SetModelTiers, so t.tiers stayed empty and resolveTier() returned nil
// for every model — the entire v0.9.0 per-model-tier override feature
// (cost_multiplier + budget_tokens_per_window sub-ceilings) was a silent no-op
// at runtime, only exercised in internal/budget/tier_test.go which calls
// SetModelTiers directly and bypasses this CLI path.
//
// This starts the REAL proxy via runProxyCtx (the path `tokenctl up` takes)
// against a config with an opus model_tier whose budget_tokens_per_window
// sub-ceiling (1000 tokens) is smaller than the default per-request reserve
// (8000 * cost_multiplier 10 = 80000 attributed). The tier hard-deny runs at
// Admit time, BEFORE the node chain, so the FIRST Opus request is hard-denied
// at the tier ceiling with X-TokenCtl-Reason: budget_exceeded_tier and NEVER
// reaches the stub upstream.
//
// Against the unwired code this test FAILS: resolveTier returns nil (no tiers
// installed), so no tier check fires, the request is admitted (200) and reaches
// the upstream — the tier cap is never enforced through the CLI/proxy path.
func TestRunProxyWiresModelTiers(t *testing.T) {
	var upstreamHits int
	// Stub Anthropic upstream. The tier cap must deny at Admit time, so a
	// well-behaved proxy NEVER calls the upstream for the capped Opus request.
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, `{"id":"m","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tokenctl.yaml")
	proxyAddr := freePort(t)
	metricsAddr := freePort(t)

	// Large node + leaf budgets (soft_throttle_at=1.0 so only hard-denies fire)
	// and NO wallet — so the ONLY binding constraint is the opus tier
	// sub-ceiling. The tier carries cost_multiplier=10 + a 1000-token
	// budget_tokens_per_window; with the default 8000-token reserve the
	// attributed reservation (80000) exceeds the tier ceiling on the first
	// Opus admit, exercising the tier hard-deny path.
	cfg := fmt.Sprintf(`version: v0.1
listen: %q
store:
  path: tokenctl.db
metrics:
  listen: %q
  path: /metrics
providers:
  - name: claude
    upstream: %q
tree:
  name: acme
  weight: 100
  budget:
    tokens: 100000000
    window: 24h
    soft_throttle_at: 1.0
  children:
    - name: alice
      weight: 100
      budget:
        tokens: 100000000
        window: 24h
        soft_throttle_at: 1.0
api_keys:
  - key: alice-key
    group: acme.alice
model_tiers:
  - name: opus
    pattern: "claude-opus.*"
    cost_multiplier: 10
    budget_tokens_per_window:
      tokens: 1000
      window: 1h
      soft_throttle_at: 1.0
`, proxyAddr, metricsAddr, upstream.URL)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	runErr := make(chan error, 1)
	go func() { runErr <- runProxyCtx(ctx, cmd, cfgPath) }()

	base := "http://" + proxyAddr
	waitHealthy(t, base)

	// Send a Claude Messages request carrying the Opus model. The proxy parses
	// the model field and hands it to Tree.Admit, which resolves the opus tier
	// and hard-denies at the tier sub-ceiling.
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-20250514"}`))
	req.Header.Set("x-api-key", "alice-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("opus request status = %d, want 429 (tier cap enforced via runProxyCtx wiring); body=%q reason=%q\n"+
			"If status was 200, tree.SetModelTiers was never called in runProxyCtx — resolveTier returned nil and the tier cap was a silent no-op.",
			resp.StatusCode, body, resp.Header.Get("X-TokenCtl-Reason"))
	}
	if got := resp.Header.Get("X-TokenCtl-Reason"); got != "budget_exceeded_tier" {
		t.Fatalf("X-TokenCtl-Reason = %q, want budget_exceeded_tier (the tier sub-ceiling must fire, not a node/wallet budget_exceeded)", got)
	}
	if upstreamHits != 0 {
		t.Fatalf("tier-capped Opus request reached the upstream %d time(s) — a tier hard-deny at Admit must short-circuit before proxying", upstreamHits)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("runProxy returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runProxy did not return after cancel")
	}
}
