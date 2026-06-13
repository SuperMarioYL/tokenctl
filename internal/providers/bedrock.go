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
	Register(config.ProviderBedrock, newBedrock)
}

// BedrockProvider fronts the AWS Bedrock Runtime API. It owns three concrete
// endpoint families:
//
//   - POST /model/{modelId}/invoke              (non-streamed JSON, model-native body)
//   - POST /model/{modelId}/invoke-with-response-stream
//                                                (AWS event-stream, binary framing)
//   - POST /model/{modelId}/converse             (non-streamed, unified schema)
//   - POST /model/{modelId}/converse-stream      (SSE-shaped converse stream)
//
// The Converse family is the strategic target: AWS made `usage` mandatory on
// every Converse response and the streaming shape is canonical SSE. invoke /
// invoke-with-response-stream stay supported because most existing
// Bedrock-using agents still hit them, but token attribution there relies on
// model-specific usage blocks AND the upstream `x-amzn-bedrock-*-token-count`
// headers (which are surfaced via the upstream response and not the body, so
// the proxy's metering reader sees the body only — those headers should be
// consumed by the proxy's ModifyResponse hook in a future polish step).
type BedrockProvider struct {
	upstream *url.URL
	region   string
}

func newBedrock(p config.ProviderConfig) (Provider, error) {
	u, err := url.Parse(p.Upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream %q: %w", p.Upstream, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("upstream %q must be an absolute URL", p.Upstream)
	}
	return &BedrockProvider{upstream: u, region: p.Region}, nil
}

// Name returns the canonical provider name ("bedrock").
func (b *BedrockProvider) Name() string { return config.ProviderBedrock }

// Upstream returns the parsed regional Bedrock Runtime base URL.
func (b *BedrockProvider) Upstream() *url.URL { return b.upstream }

// Region returns the AWS region this provider is pinned to.
func (b *BedrockProvider) Region() string { return b.region }

// Matches recognises the four Bedrock Runtime entry points. The model id may
// contain "/" (cross-region inference profiles), so we accept any non-empty
// suffix on /model/.
func (b *BedrockProvider) Matches(r *http.Request) bool {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/model/") {
		return false
	}
	return strings.Contains(p, "/invoke") || strings.Contains(p, "/converse")
}

// APIKeyFromRequest extracts an identifier for budget binding. Bedrock signs
// requests with SigV4, so the canonical key the operator has on hand is the
// AccessKeyId carried inside the Authorization header. The format is:
//
//	Authorization: AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20260605/us-east-1/bedrock/aws4_request, SignedHeaders=..., Signature=...
//
// We prefer:
//  1. an explicit x-tokenctl-key header (lets operators opt out of SigV4
//     parsing entirely and bind by a synthetic name),
//  2. the AccessKeyId from the Credential= field,
//  3. an Authorization: Bearer ... fallback (some Bedrock-compatible proxies
//     in the wild forward Bearer auth onward).
//
// The empty string return triggers the proxy's 401 path.
func (b *BedrockProvider) APIKeyFromRequest(r *http.Request) string {
	if k := strings.TrimSpace(r.Header.Get("x-tokenctl-key")); k != "" {
		return k
	}
	auth := r.Header.Get("Authorization")
	if id := parseSigV4AccessKeyID(auth); id != "" {
		return id
	}
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	return ""
}

// parseSigV4AccessKeyID returns the AccessKeyId in an AWS4-HMAC-SHA256
// Authorization header, or "" on malformed input.
func parseSigV4AccessKeyID(auth string) string {
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256") {
		return ""
	}
	// Find "Credential=" — comma-separated kv pairs.
	const tag = "Credential="
	i := strings.Index(auth, tag)
	if i < 0 {
		return ""
	}
	cred := auth[i+len(tag):]
	// Credential is "<AccessKeyId>/<date>/<region>/<service>/aws4_request".
	if j := strings.Index(cred, ","); j >= 0 {
		cred = cred[:j]
	}
	cred = strings.TrimSpace(cred)
	if k := strings.Index(cred, "/"); k > 0 {
		return cred[:k]
	}
	return ""
}

// NewMeter builds a per-request Meter that handles both the Converse JSON
// shape and the Converse stream's named SSE events.
func (b *BedrockProvider) NewMeter() Meter { return &bedrockMeter{} }

// bedrockMeter tracks input/output high-water marks. Bedrock's usage block
// is reported once per non-streamed response or on the trailing `metadata`
// event of a converse-stream; we still HWM-diff so a buggy upstream that
// emits cumulative counters across multiple events doesn't double-count.
type bedrockMeter struct {
	inputHWM  int64
	outputHWM int64
}

// Bedrock Converse non-streamed response:
//
//	{
//	  "output": {"message": {...}},
//	  "usage": {"inputTokens": N, "outputTokens": M, "totalTokens": K},
//	  "metrics": {"latencyMs": ...}
//	}
//
// Converse-stream named events of interest:
//
//	event: metadata
//	data: {"usage": {"inputTokens": N, "outputTokens": M, "totalTokens": K}, ...}
//
// invoke (model-native) bodies vary by model — Anthropic on Bedrock returns
// {"usage": {"input_tokens": N, "output_tokens": M}} (snake_case), Mistral
// returns {"usage": {...}} similar, Llama returns {"prompt_token_count":
// N, "generation_token_count": M}. We accept the common shapes; rare/legacy
// models that bury usage elsewhere will simply not be metered (a known v0.1
// limitation documented in the README's "what's metered" table).
type bedrockUsage struct {
	InputTokens          int64 `json:"inputTokens"`
	OutputTokens         int64 `json:"outputTokens"`
	InputTokensSnake     int64 `json:"input_tokens"`
	OutputTokensSnake    int64 `json:"output_tokens"`
	PromptTokenCount     int64 `json:"prompt_token_count"`
	GenerationTokenCount int64 `json:"generation_token_count"`
}

// Multiple top-level shapes — we union them into one struct via a single
// json.Unmarshal pass; absent fields stay zero and are ignored.
type bedrockEnvelope struct {
	Usage *bedrockUsage `json:"usage,omitempty"`
	// Llama-style flat fields at top level.
	PromptTokenCount     int64 `json:"prompt_token_count,omitempty"`
	GenerationTokenCount int64 `json:"generation_token_count,omitempty"`
}

func (m *bedrockMeter) Observe(event string, data []byte) (int64, int64) {
	// Converse-stream — only `metadata` carries usage in the stream. Other
	// event names (contentBlockDelta, messageStart, messageStop) are noisy
	// and never contain attributable counts.
	if event != "" && event != "metadata" {
		return 0, 0
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return 0, 0
	}
	var env bedrockEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return 0, 0
	}
	var in, out int64
	if env.Usage != nil {
		in = pickMax(env.Usage.InputTokens, env.Usage.InputTokensSnake, env.Usage.PromptTokenCount)
		out = pickMax(env.Usage.OutputTokens, env.Usage.OutputTokensSnake, env.Usage.GenerationTokenCount)
	}
	if env.PromptTokenCount > in {
		in = env.PromptTokenCount
	}
	if env.GenerationTokenCount > out {
		out = env.GenerationTokenCount
	}
	if in == 0 && out == 0 {
		return 0, 0
	}
	return m.advance(in, out)
}

func (m *bedrockMeter) advance(in, out int64) (int64, int64) {
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

func pickMax(xs ...int64) int64 {
	var m int64
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}
