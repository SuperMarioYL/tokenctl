package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SuperMarioYL/tokenctl/internal/config"
)

func init() {
	Register(config.ProviderOpenAI, newOpenAI)
}

// OpenAIProvider fronts api.openai.com (or any Chat Completions / Responses
// compatible base URL). The SSE shape carries usage only when the client sets
// stream_options.include_usage=true; without it, only the final non-streamed
// response carries usage. tokenctl meters whatever the upstream returns.
type OpenAIProvider struct {
	upstream *url.URL
}

func newOpenAI(p config.ProviderConfig) (Provider, error) {
	u, err := url.Parse(p.Upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream %q: %w", p.Upstream, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream %q must be an absolute URL", p.Upstream)
	}
	return &OpenAIProvider{upstream: u}, nil
}

// Name returns the canonical provider name ("openai").
func (o *OpenAIProvider) Name() string { return config.ProviderOpenAI }

// Upstream returns the parsed base URL of the OpenAI API.
func (o *OpenAIProvider) Upstream() *url.URL { return o.upstream }

// Matches recognises Chat Completions, legacy Completions and the newer
// Responses API endpoints.
func (o *OpenAIProvider) Matches(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/v1/chat/completions") ||
		strings.HasPrefix(p, "/v1/completions") ||
		strings.HasPrefix(p, "/v1/responses") ||
		strings.HasPrefix(p, "/v1/embeddings")
}

// APIKeyFromRequest extracts the Bearer token from the Authorization header,
// the canonical OpenAI auth shape.
func (o *OpenAIProvider) APIKeyFromRequest(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(a, "Bearer "))
	}
	return ""
}

// ModelFromRequest peeks the JSON request body for the `model` field so the
// budget tree can resolve a model_tiers sub-ceiling / cost_multiplier
// (feat_model_tier_override). The body is restored for the reverse proxy.
func (o *OpenAIProvider) ModelFromRequest(r *http.Request) string {
	return peekJSONModelField(r)
}

// NewMeter builds a per-request Meter that tracks usage high-water marks
// across the unnamed SSE data events Chat Completions emits and the typed
// response.completed event the Responses API streams (whose usage is nested
// under response.usage).
func (o *OpenAIProvider) NewMeter() Meter { return &openaiMeter{} }

type openaiMeter struct {
	inputHWM  int64
	outputHWM int64
}

// OpenAI streaming format:
//
//	data: {"id":"...","object":"chat.completion.chunk","choices":[...],"usage":null}
//	...
//	data: {"id":"...","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":N,"completion_tokens":M,"total_tokens":K}}
//	data: [DONE]
//
// Each chunk is the JSON body of one data: line. The Responses API streams a
// similar shape, except the usage block uses {input_tokens, output_tokens}
// (matching Anthropic-style naming). We accept either.
//
// The Responses API differs from Chat Completions in that it streams TYPED
// SSE events (response.created, response.output_text.delta, response.completed,
// ...). Only the terminal response.completed event carries usage, and it is
// NESTED under response.usage rather than at the top level (the non-streaming
// /v1/responses body, by contrast, carries usage at the top level). Before
// fix-openai-responses-streaming-not-metered, Observe blanket-returned 0,0 for
// every named event and only looked at top-level usage, so a streaming
// /v1/responses request was attributed zero tokens and silently bypassed the
// org cap + node budgets. We now also parse the nested response.usage on the
// response.completed event.
type openaiChunkEnvelope struct {
	Usage *openaiUsage `json:"usage,omitempty"`
	// Response captures the Responses API streaming envelope, whose usage lives
	// one level deeper under response.usage. Nil for Chat Completions chunks.
	Response *openaiResponsesEnvelope `json:"response,omitempty"`
}

// openaiResponsesEnvelope is the inner "response" object carried by Responses
// API streaming events; only its Usage field matters for metering.
type openaiResponsesEnvelope struct {
	Usage *openaiUsage `json:"usage,omitempty"`
}

type openaiUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

func (m *openaiMeter) Observe(event string, data []byte) (int64, int64) {
	// OpenAI Chat Completions streams unnamed data events (event == ""). The
	// Responses API streams typed events; only response.completed carries usage
	// (nested under response.usage). Every other named event (response.created,
	// response.output_text.delta, ...) carries no usage, so we still skip it to
	// avoid attributing spurious zero-deltas / parsing work per delta tick.
	switch event {
	case "", "response.completed":
		// fall through — these are the events that can carry usage.
	default:
		return 0, 0
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return 0, 0
	}
	var c openaiChunkEnvelope
	if err := json.Unmarshal(trimmed, &c); err != nil {
		return 0, 0
	}
	// Usage appears at the top level for Chat Completions and the non-streaming
	// Responses body, and nested under response.usage for the streaming
	// response.completed event. Accept whichever position carries it.
	u := c.Usage
	if u == nil {
		u = c.Response.Usage
	}
	if u == nil {
		return 0, 0
	}
	in := u.PromptTokens
	if u.InputTokens > in {
		in = u.InputTokens
	}
	out := u.CompletionTokens
	if u.OutputTokens > out {
		out = u.OutputTokens
	}
	return m.advance(in, out)
}

func (m *openaiMeter) advance(in, out int64) (int64, int64) {
	var dIn, dOut int64
	if in > m.inputHWM {
		dIn = in - m.inputHWM
		m.inputHWM = in
	}
	if out > m.outputHWM {
		dOut = out - m.outputHWM
		m.outputHWM = out
	}
	return dIn, dOut
}
