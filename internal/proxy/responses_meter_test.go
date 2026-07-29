package proxy

import (
	"bytes"
	"io"
	"testing"

	"github.com/SuperMarioYL/tokenctl/internal/config"
	"github.com/SuperMarioYL/tokenctl/internal/providers"
)

// openAIResponsesStream builds a minimal OpenAI Responses API streaming body:
// typed SSE events (response.created, response.output_text.delta,
// response.completed) where only the terminal response.completed carries
// usage, NESTED under response.usage (not at the top level). The non-streaming
// /v1/responses body, by contrast, carries usage at the top level — which is
// why the non-streaming path was already metered but the streaming path was
// not (fix-openai-responses-streaming-not-metered).
func openAIResponsesStream() []byte {
	var b bytes.Buffer
	b.WriteString("event: response.created\n")
	b.WriteString(`data: {"type":"response.created","response":{"id":"resp_1","object":"response"}}` + "\n")
	b.WriteString("\n")
	b.WriteString("event: response.output_text.delta\n")
	b.WriteString(`data: {"type":"response.output_text.delta","delta":"Hello"}` + "\n")
	b.WriteString("\n")
	b.WriteString("event: response.completed\n")
	b.WriteString(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":314,"output_tokens":271,"total_tokens":585}}}` + "\n")
	b.WriteString("\n")
	return b.Bytes()
}

// TestSSEMeteredReader_OpenAIResponsesStreamingMetered is the regression for
// fix-openai-responses-streaming-not-metered: a streamed response.completed
// event (with usage nested under response.usage) must be metered so the org cap
// + node budgets actually fire on streaming /v1/responses. Before the fix,
// openaiMeter.Observe blanket-returned 0,0 for every named event, so the SSE
// reader forwarded bytes but AddInput/AddOutput never advanced — a
// budget-bypass vector. Mirrors the non-streaming JSON meter contract (a
// usage-bearing body round-trips into AddInput/AddOutput) for the streaming
// Responses shape. Must FAIL without the fix (0 tokens attributed), PASS with.
func TestSSEMeteredReader_OpenAIResponsesStreamingMetered(t *testing.T) {
	// Use the REAL OpenAI provider meter (not a test double) so this is an
	// end-to-end regression of the metering lane the proxy actually ships.
	prov, err := providers.Build(config.ProviderConfig{
		Name:     config.ProviderOpenAI,
		Upstream: "https://api.openai.com",
	})
	if err != nil {
		t.Fatalf("build openai provider: %v", err)
	}
	meter := prov.NewMeter()

	body := openAIResponsesStream()
	adm := newFakeAdmission()
	m := newMetrics()
	rc := newSSEMeteredReader(io.NopCloser(bytes.NewReader(body)), meter, adm, m, "openai")

	got := drainReader(t, rc)
	// The SSE reader forwards bytes untouched — metering must not alter the
	// client stream.
	if !bytes.Equal(got, body) {
		t.Fatalf("client bytes altered:\n got %q\nwant %q", got, body)
	}
	// The nested response.usage on the streamed response.completed event must
	// reach the admission (AddInput/AddOutput). Before the fix Observe returned
	// 0,0 for named events, so both stayed at 0 and the org cap + node budgets
	// were silently bypassed for streaming /v1/responses.
	if adm.in.Load() != 314 || adm.out.Load() != 271 {
		t.Fatalf("streaming /v1/responses metered in=%d out=%d, want 314/271 (budget bypassed)",
			adm.in.Load(), adm.out.Load())
	}
}
