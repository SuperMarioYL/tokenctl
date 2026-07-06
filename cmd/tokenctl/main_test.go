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
