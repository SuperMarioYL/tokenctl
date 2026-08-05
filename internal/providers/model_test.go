package providers

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/SuperMarioYL/tokenctl/internal/config"
)

// TestClaudeModelFromRequestPeeksAndRestoresBody verifies Claude/OpenAI's
// ModelFromRequest peeks the JSON `model` field without consuming the body the
// reverse proxy still needs to stream upstream (feat_model_tier_override).
func TestClaudeModelFromRequestPeeksAndRestoresBody(t *testing.T) {
	body := `{"model":"claude-opus-4-20250514","max_tokens":1024,"messages":[]}`
	r, _ := http.NewRequest(http.MethodPost, "https://proxy.local/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")

	c, err := newClaude(config.ProviderConfig{Name: config.ProviderClaude, Upstream: "https://api.anthropic.com"})
	if err != nil {
		t.Fatalf("newClaude: %v", err)
	}
	if got := c.ModelFromRequest(r); got != "claude-opus-4-20250514" {
		t.Fatalf("model = %q, want claude-opus-4-20250514", got)
	}
	// Body must be fully restorable so the reverse proxy streams it upstream.
	rest, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(rest) != body {
		t.Fatalf("restored body = %q, want original %q", string(rest), body)
	}
}

// TestOpenAIModelFromRequest covers the OpenAI provider's body-peek path.
func TestOpenAIModelFromRequest(t *testing.T) {
	body := `{"model":"gpt-4o-mini","input":"hello"}`
	r, _ := http.NewRequest(http.MethodPost, "https://proxy.local/v1/responses", strings.NewReader(body))
	o, err := newOpenAI(config.ProviderConfig{Name: config.ProviderOpenAI, Upstream: "https://api.openai.com"})
	if err != nil {
		t.Fatalf("newOpenAI: %v", err)
	}
	if got := o.ModelFromRequest(r); got != "gpt-4o-mini" {
		t.Fatalf("model = %q, want gpt-4o-mini", got)
	}
}

// TestBedrockModelFromRequest extracts the modelId from the Bedrock Runtime URL
// path (the model lives in the path, not the body).
func TestBedrockModelFromRequest(t *testing.T) {
	b, err := newBedrock(config.ProviderConfig{Name: config.ProviderBedrock, Upstream: "https://bedrock-runtime.us-east-1.amazonaws.com", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("newBedrock: %v", err)
	}
	cases := []struct {
		path string
		want string
	}{
		{"/model/anthropic.claude-opus-4/invoke", "anthropic.claude-opus-4"},
		{"/model/us.anthropic.claude-3-haiku/invoke-with-response-stream", "us.anthropic.claude-3-haiku"},
		{"/model/anthropic.claude-3-5-sonnet/converse", "anthropic.claude-3-5-sonnet"},
		{"/model/meta.llama3/converse-stream", "meta.llama3"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			r := &http.Request{URL: &url.URL{Path: tc.path}}
			if got := b.ModelFromRequest(r); got != tc.want {
				t.Errorf("model = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestScanJSONStringField covers the cheap body scanner: finds the first
// quoted value for a key, handles escaped quotes, and returns "" when absent.
func TestScanJSONStringField(t *testing.T) {
	cases := []struct {
		name string
		data string
		key  string
		want string
	}{
		{name: "first_field", data: `{"model":"claude-opus-4","x":1}`, key: "model", want: "claude-opus-4"},
		{name: "spaced", data: `{"model": "gpt-4o"}`, key: "model", want: "gpt-4o"},
		{name: "not_first", data: `{"max_tokens":1024,"model":"claude-haiku-3"}`, key: "model", want: "claude-haiku-3"},
		{name: "absent", data: `{"foo":"bar"}`, key: "model", want: ""},
		{name: "empty_body", data: ``, key: "model", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanJSONStringField([]byte(tc.data), tc.key); got != tc.want {
				t.Errorf("scanJSONStringField(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

// TestPeekJSONModelFieldFailSoftOnEmptyBody confirms a nil/empty body is
// handled without a panic and yields "" (no tier applies).
func TestPeekJSONModelFieldFailSoftOnEmptyBody(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "https://proxy.local/v1/messages", nil)
	if got := peekJSONModelField(r); got != "" {
		t.Fatalf("empty body model = %q, want empty (fail-soft)", got)
	}
}
