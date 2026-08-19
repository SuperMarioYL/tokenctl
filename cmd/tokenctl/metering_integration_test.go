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
)

// TestProxyMeteringIntegration is the m4 integration regression for the shipped
// m1 proxy metering. It streams a canned Claude response (SSE for the streamed
// path, buffered JSON for the non-streamed path) through the REAL `tokenctl up`
// proxy against an httptest upstream and asserts the metered input/output tokens
// are attributed to the bound leaf and surface in /v1/snapshot — the
// streamed-token attribution the product headline rests on, guarded end-to-end
// (not just at the reader unit level the existing *_test.go files cover).
//
// Both rows meter to input=100 / output=50, so the leaf (acme.alice) consumed
// must reach 150 (input + output both credit the leaf->root chain).
func TestProxyMeteringIntegration(t *testing.T) {
	// SSE stream: message_start carries input_tokens=100 (cumulative HWM);
	// message_delta advances output_tokens to 50 (delta 50). The Claude meter
	// diffs against the HWM and reports deltas, so attribution = 100 + 50.
	claudeSSE := "event: message_start\n" +
		"data: {\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":0}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"usage\":{\"output_tokens\":50}}\n\n"

	// Non-streamed buffered JSON: the usage object at the end of the body is
	// what the JSON metered reader reconstructs from its bounded tail.
	claudeJSON := `{"id":"msg_1","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":100,"output_tokens":50}}`

	cases := []struct {
		name        string
		contentType string
		body        string
		wantInput   int64
		wantOutput  int64
		// bodyMarker is a substring the client must still receive (the meter
		// must forward the stream intact, not corrupt it).
		bodyMarker string
	}{
		{name: "claude_sse_stream", contentType: "text/event-stream", body: claudeSSE, wantInput: 100, wantOutput: 50, bodyMarker: "message_delta"},
		{name: "claude_buffered_json", contentType: "application/json", body: claudeJSON, wantInput: 100, wantOutput: 50, bodyMarker: "usage"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				rw.Header().Set("Content-Type", tc.contentType)
				rw.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(rw, tc.body)
			}))
			defer upstream.Close()

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "tokenctl.yaml")
			proxyAddr := freePort(t)
			metricsAddr := freePort(t)

			// Generous budgets with soft_throttle_at=1.0 so only the hard cap
			// (never reached here) could ever bite — the metering path is the
			// sole thing under test. No wallet block keeps the wallet out of
			// the way.
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
			clientBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (admitted + proxied); body=%q reason=%q",
					resp.StatusCode, clientBody, resp.Header.Get("X-TokenCtl-Reason"))
			}
			// The proxy stamps the attributed leaf on every proxied response —
			// proof the admission (not a 401 bail-out) produced this response.
			if got := resp.Header.Get("X-TokenCtl-Group"); got != "acme.alice" {
				t.Fatalf("X-TokenCtl-Group = %q, want acme.alice", got)
			}
			// The meter must forward the stream intact to the client.
			if !strings.Contains(string(clientBody), tc.bodyMarker) {
				t.Fatalf("client body missing %q — meter corrupted the forwarded stream:\n%s", tc.bodyMarker, clientBody)
			}

			// The metered tokens must attribute to the leaf and surface in
			// /v1/snapshot. Attribution lands synchronously as the body streams
			// (the meter reports inside the copy loop), but poll briefly so the
			// table is flake-free under package-wide contention.
			metricsBase := "http://" + metricsAddr
			wantConsumed := tc.wantInput + tc.wantOutput
			deadline := time.Now().Add(3 * time.Second)
			var leafConsumed int64
			for time.Now().Before(deadline) {
				snap, err := fetchSnapshot(ctx, &http.Client{Timeout: 2 * time.Second}, metricsBase+"/v1/snapshot")
				if err == nil {
					if g := findTopGroup(snap, "acme.alice"); g != nil {
						leafConsumed = g.ConsumedTokens
						if leafConsumed == wantConsumed {
							break
						}
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			if leafConsumed != wantConsumed {
				t.Fatalf("acme.alice consumed = %d, want %d (input %d + output %d metered through the proxy)",
					leafConsumed, wantConsumed, tc.wantInput, tc.wantOutput)
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
		})
	}
}

// findTopGroup returns the snapshot group with the given path, or nil.
func findTopGroup(snap *topSnapshot, path string) *topGroup {
	if snap == nil {
		return nil
	}
	for i := range snap.Groups {
		if snap.Groups[i].Path == path {
			return &snap.Groups[i]
		}
	}
	return nil
}

// TestProxyMeteringAttributesCacheInputTokens is the regression for
// fix-claude-meter-drops-cache-input-tokens. Anthropic's message_start usage
// object carries cache_creation_input_tokens + cache_read_input_tokens as
// SEPARATE fields from input_tokens (the prompt-cache billable input surface).
// claudeUsage only declared input_tokens/output_tokens, so json.Unmarshal
// silently dropped both cache fields — a cached Claude Code turn (where the
// cached prompt routinely runs 10-50x larger than input_tokens) under-counted
// input by that factor and the org cap + tier sub-ceiling never bit for cached
// traffic. This streams a cached SSE turn through the real `tokenctl up` proxy
// and asserts the cache tokens are attributed to the leaf, mirroring the
// non-cached metering regression above. Fails on the unfixed code (which would
// meter only input=100 + output=50 = 150, dropping the 8000 cache tokens).
func TestProxyMeteringAttributesCacheInputTokens(t *testing.T) {
	// SSE stream: message_start carries input_tokens=100 PLUS
	// cache_creation_input_tokens=5000 + cache_read_input_tokens=3000
	// (cumulative input HWM = 8100); message_delta advances output to 50.
	// Total attributed = 8100 input + 50 output = 8150.
	claudeCachedSSE := "event: message_start\n" +
		"data: {\"message\":{\"usage\":{\"input_tokens\":100,\"cache_creation_input_tokens\":5000,\"cache_read_input_tokens\":3000,\"output_tokens\":0}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"usage\":{\"output_tokens\":50}}\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/event-stream")
		rw.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(rw, claudeCachedSSE)
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
	req.Header.Set("x-api-key", "alice-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	clientBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (admitted + proxied); body=%q reason=%q",
			resp.StatusCode, clientBody, resp.Header.Get("X-TokenCtl-Reason"))
	}
	if got := resp.Header.Get("X-TokenCtl-Group"); got != "acme.alice" {
		t.Fatalf("X-TokenCtl-Group = %q, want acme.alice", got)
	}
	if !strings.Contains(string(clientBody), "message_delta") {
		t.Fatalf("client body missing message_delta — meter corrupted the forwarded stream:\n%s", clientBody)
	}

	// The cache tokens must be attributed: 100 input + 5000 cache_creation +
	// 3000 cache_read + 50 output = 8150. On the unfixed code the cache fields
	// were dropped by json.Unmarshal, so only 100 + 50 = 150 would land.
	metricsBase := "http://" + metricsAddr
	const wantConsumed int64 = 8150
	deadline := time.Now().Add(3 * time.Second)
	var leafConsumed int64
	for time.Now().Before(deadline) {
		snap, err := fetchSnapshot(ctx, &http.Client{Timeout: 2 * time.Second}, metricsBase+"/v1/snapshot")
		if err == nil {
			if g := findTopGroup(snap, "acme.alice"); g != nil {
				leafConsumed = g.ConsumedTokens
				if leafConsumed == wantConsumed {
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if leafConsumed != wantConsumed {
		t.Fatalf("acme.alice consumed = %d, want %d (input 100 + cache_creation 5000 + cache_read 3000 + output 50 — cache input tokens must be metered, not dropped)",
			leafConsumed, wantConsumed)
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
