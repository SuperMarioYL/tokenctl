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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
// ModelFromRequest extracts the request `model` field (e.g. "claude-opus-4-",
// "gpt-4o") so the budget tree can resolve a model_tiers sub-ceiling /
// cost_multiplier (feat_model_tier_override); it MUST restore r.Body so the
// reverse proxy can still stream it upstream, and return "" when no model is
// recoverable. NewMeter returns a per-request stateful Meter that the proxy
// feeds either SSE events (event name + JSON data) or one complete JSON body.
type Provider interface {
	Name() string
	Upstream() *url.URL
	Matches(r *http.Request) bool
	APIKeyFromRequest(r *http.Request) string
	ModelFromRequest(r *http.Request) string
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
//
// The legacy /v1/complete match is boundary-aware: a literal HasPrefix on
// "/v1/complete" would also claim OpenAI's "/v1/completions" endpoint (which
// is a longer string sharing the prefix), and since the default config lists
// claude before openai, first-match would hand POST /v1/completions to Claude
// — which reverse-proxies to api.anthropic.com, a host that does not serve
// that path. We therefore require a path boundary (end-of-string, "/", or "?")
// after "/v1/complete" so /v1/completions falls through to the OpenAI provider.
// /v1/messages has no overlapping sibling and keeps the plain HasPrefix match.
func (c *ClaudeProvider) Matches(r *http.Request) bool {
	p := r.URL.Path
	if strings.HasPrefix(p, "/v1/messages") {
		return true
	}
	// Boundary-aware match for the legacy /v1/complete endpoint so the OpenAI
	// /v1/completions path is NOT claimed by Claude.
	if p == "/v1/complete" || strings.HasPrefix(p, "/v1/complete/") || strings.HasPrefix(p, "/v1/complete?") {
		return true
	}
	return false
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

// ModelFromRequest peeks the JSON request body for the `model` field so the
// budget tree can resolve a model_tiers sub-ceiling / cost_multiplier
// (feat_model_tier_override). The body is restored via a combined reader so
// the reverse proxy can still stream it upstream unchanged.
func (c *ClaudeProvider) ModelFromRequest(r *http.Request) string {
	return peekJSONModelField(r)
}

// peekJSONModelField reads up to peekLimit bytes from the request body, scans
// for the first `"model"` JSON string value, and restores r.Body so the caller
// (the reverse proxy) still sees the full stream. Returns "" when the field is
// absent or the body is not a JSON object small enough to contain it up front.
// This is fail-soft: a model we cannot recover simply gets no tier applied.
const peekLimit = 64 * 1024

func peekJSONModelField(r *http.Request) string {
	if r == nil || r.Body == nil {
		return ""
	}
	buf := make([]byte, peekLimit)
	n, _ := io.ReadFull(r.Body, buf)
	have := buf[:n]
	// Restore the body so the reverse proxy streams the full content upstream.
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(have), r.Body))
	if n == 0 {
		return ""
	}
	return scanJSONStringField(have, "model")
}

// scanJSONStringField finds the first `"key": "value"` (or `"key":"value"`) in
// data and returns the unquoted value. It is a deliberately small, allocation-
// free scanner: it does not parse the whole document, so a `model` key
// appearing inside a nested object or string literal could in principle be a
// false positive — but every supported provider places `model` at the top
// level of the request body, so the cheap scan suffices and stays off the hot
// path's allocation budget.
func scanJSONStringField(data []byte, key string) string {
	needle := []byte(`"` + key + `"`)
	idx := bytes.Index(data, needle)
	if idx < 0 {
		return ""
	}
	i := idx + len(needle)
	// Skip whitespace and the ':'.
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r' || data[i] == ':') {
		i++
	}
	if i >= len(data) || data[i] != '"' {
		return ""
	}
	i++ // opening quote
	start := i
	for i < len(data) {
		switch data[i] {
		case '\\':
			i += 2
			continue
		case '"':
			return string(data[start:i])
		}
		i++
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

// claudeUsage mirrors the Anthropic Messages usage object. message_start
// reports input_tokens once alongside the cache_creation_input_tokens and
// cache_read_input_tokens fields (the prompt-cache billable input surface);
// message_delta reports output_tokens only (cumulative). The two cache fields
// are decoded here so a cached Claude Code turn — where the cached prompt
// routinely runs 10-50x larger than input_tokens — is attributed in full,
// otherwise the leaf/node/wallet counters and the tier sub-ceiling under-count
// input by that factor and the org cap effectively never bites for cached
// traffic (fix-claude-meter-drops-cache-input-tokens).
type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
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
//
// The input HWM is advanced against the SUM of input_tokens +
// cache_creation_input_tokens + cache_read_input_tokens so every billed input
// token is attributed once. message_start reports all three once (the cache
// fields are absent on message_delta), so the HWM diff stays correct: a
// subsequent message_delta carrying only output_tokens sees the same input
// HWM and emits a zero input delta (fix-claude-meter-drops-cache-input-tokens).
func (m *claudeMeter) advance(u claudeUsage) (int64, int64) {
	totalInput := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	var in, out int64
	if totalInput > m.inputHWM {
		in = totalInput - m.inputHWM
		m.inputHWM = totalInput
	}
	if u.OutputTokens > m.outputHWM {
		out = u.OutputTokens - m.outputHWM
		m.outputHWM = u.OutputTokens
	}
	return in, out
}
