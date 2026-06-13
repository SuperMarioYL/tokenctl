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

// NewMeter builds a per-request Meter that tracks usage high-water marks
// across the unnamed SSE data events OpenAI emits.
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
type openaiChunkEnvelope struct {
	Usage *openaiUsage `json:"usage,omitempty"`
}

type openaiUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

func (m *openaiMeter) Observe(event string, data []byte) (int64, int64) {
	// OpenAI uses unnamed events; ignore anything that came in with a name
	// (Anthropic-shaped events would be handled by the Claude meter).
	if event != "" {
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
	if c.Usage == nil {
		return 0, 0
	}
	in := c.Usage.PromptTokens
	if c.Usage.InputTokens > in {
		in = c.Usage.InputTokens
	}
	out := c.Usage.CompletionTokens
	if c.Usage.OutputTokens > out {
		out = c.Usage.OutputTokens
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
