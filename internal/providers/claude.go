// Package providers contains the per-LLM adapters that let tokenctl recognise
// Claude / OpenAI / Bedrock traffic, identify the inbound API key carried by
// each request, and meter input + output tokens from the upstream response —
// streamed (SSE) or buffered (JSON) — without re-implementing a tokenizer.
//
// Each adapter registers itself with the package-level Build factory via
// init(); the proxy package iterates config.Providers and calls Build per
// entry. New provider files (e.g. bedrock.go) drop in with their own init()
// and require no edits here.
package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/SuperMarioYL/tokenctl/internal/config"
)

// Provider is the contract the proxy depends on per upstream LLM API.
//
// Matches inspects the inbound request and decides whether this provider owns
// it (path prefix, header sniff, etc). APIKeyFromRequest extracts the
// credential the operator uses to identify the request — this string is the
// key in config.APIKeyBinding that pins the request to a tree leaf.
// NewMeter returns a per-request stateful Meter that the proxy feeds either
// SSE events (event name + JSON data) or one complete JSON body.
type Provider interface {
	Name() string
	Upstream() *url.URL
	Matches(r *http.Request) bool
	APIKeyFromRequest(r *http.Request) string
	NewMeter() Meter
}

// Meter accumulates per-request token deltas from the upstream response.
//
// For SSE responses, the proxy calls Observe once per parsed event with the
// event name (e.g. "message_start", "message_delta", or "" for OpenAI which
// uses unnamed data events). For non-streaming JSON responses, the proxy
// calls Observe once with event="" and the full body bytes.
//
// Observe returns deltas — bytes attributable to THIS call only, not running
// totals. Providers whose upstream reports cumulative usage (Anthropic's
// message_delta carries cumulative output_tokens) MUST internally diff
// against the previous high-water mark so the proxy can simply sum.
type Meter interface {
	Observe(event string, data []byte) (inputDelta, outputDelta int64)
}

// Factory builds a Provider from a config.ProviderConfig entry.
type Factory func(config.ProviderConfig) (Provider, error)

var (
	factoryMu sync.RWMutex
	factories = map[string]Factory{}
)

// Register associates a provider name with a Factory. Called from each
// provider file's init().
func Register(name string, f Factory) {
	factoryMu.Lock()
	defer factoryMu.Unlock()
	factories[name] = f
}

// Build looks up the Factory for p.Name and constructs the Provider.
// Returns an error if no provider is registered under that name.
func Build(p config.ProviderConfig) (Provider, error) {
	factoryMu.RLock()
	f, ok := factories[p.Name]
	factoryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no provider registered as %q (built-in: claude, openai, bedrock)", p.Name)
	}
	return f(p)
}

// ---------------------------------------------------------------------------
// Anthropic / Claude
// ---------------------------------------------------------------------------

func init() {
	Register(config.ProviderClaude, newClaude)
}

// ClaudeProvider fronts api.anthropic.com (or any base URL that speaks the
// Messages API). It recognises POST /v1/messages and the legacy
// POST /v1/complete endpoint.
type ClaudeProvider struct {
	upstream *url.URL
}

func newClaude(p config.ProviderConfig) (Provider, error) {
	u, err := url.Parse(p.Upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream %q: %w", p.Upstream, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream %q must be an absolute URL", p.Upstream)
	}
	return &ClaudeProvider{upstream: u}, nil
}

// Name returns the canonical provider name ("claude").
func (c *ClaudeProvider) Name() string { return config.ProviderClaude }

// Upstream returns the parsed base URL of the Anthropic API.
func (c *ClaudeProvider) Upstream() *url.URL { return c.upstream }

// Matches recognises the Messages and legacy Complete endpoints. The proxy
// scans providers in registration order; first match wins.
func (c *ClaudeProvider) Matches(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/v1/messages") || strings.HasPrefix(p, "/v1/complete")
}

// APIKeyFromRequest prefers the Anthropic-native x-api-key header but accepts
// Authorization: Bearer ... for clients that proxy through an OpenAI-shaped
// SDK.
func (c *ClaudeProvider) APIKeyFromRequest(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("x-api-key")); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	return ""
}

// NewMeter builds a per-request Meter that tracks the highest reported
// input/output_tokens and emits deltas.
func (c *ClaudeProvider) NewMeter() Meter { return &claudeMeter{} }

type claudeMeter struct {
	inputHWM  int64
	outputHWM int64
}

// Wire shapes for the two events that carry usage. Anthropic streams the
// initial usage in message_start and updates output_tokens (cumulative) on
// each message_delta. We tolerate either flat or nested shapes.
type claudeStartEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		Usage claudeUsage `json:"usage"`
	} `json:"message"`
}

type claudeDeltaEnvelope struct {
	Type  string      `json:"type"`
	Usage claudeUsage `json:"usage"`
}

type claudeBufferedResponse struct {
	Usage claudeUsage `json:"usage"`
}

type claudeUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (m *claudeMeter) Observe(event string, data []byte) (int64, int64) {
	switch event {
	case "message_start":
		var s claudeStartEnvelope
		if err := json.Unmarshal(data, &s); err != nil {
			return 0, 0
		}
		return m.advance(s.Message.Usage)

	case "message_delta":
		var d claudeDeltaEnvelope
		if err := json.Unmarshal(data, &d); err != nil {
			return 0, 0
		}
		return m.advance(d.Usage)

	case "":
		// Non-streaming response body.
		var r claudeBufferedResponse
		if err := json.Unmarshal(data, &r); err != nil {
			return 0, 0
		}
		return m.advance(r.Usage)
	}
	return 0, 0
}

// advance promotes the high-water marks and returns the deltas. Cumulative
// counters that go backwards (rare upstream bug) are clamped to zero delta
// rather than producing a negative attribution.
func (m *claudeMeter) advance(u claudeUsage) (int64, int64) {
	var in, out int64
	if u.InputTokens > m.inputHWM {
		in = u.InputTokens - m.inputHWM
		m.inputHWM = u.InputTokens
	}
	if u.OutputTokens > m.outputHWM {
		out = u.OutputTokens - m.outputHWM
		m.outputHWM = u.OutputTokens
	}
	return in, out
}
