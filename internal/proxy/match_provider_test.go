package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SuperMarioYL/tokenctl/internal/config"
	"github.com/SuperMarioYL/tokenctl/internal/providers"
)

// TestMatchProviderClaudeCompleteDoesNotClaimOpenAICompletions is the regression
// for fix-claude-complete-prefix-claims-openai-completions: the default config
// lists claude before openai, so first-match wins. Claude's legacy
// /v1/complete match used a plain HasPrefix, which also matched OpenAI's
// /v1/completions — so POST /v1/completions was claimed by Claude and proxied
// to api.anthropic.com (which does not serve that path), and OpenAI never saw
// the request. The fix makes /v1/complete boundary-aware; this test asserts
// the ordered first-match resolution under the real provider constructors.
func TestMatchProviderClaudeCompleteDoesNotClaimOpenAICompletions(t *testing.T) {
	// Default config ordering: claude first, then openai — mirrors the
	// shipped sample/default config.
	provs, err := buildTestProviders(
		config.ProviderConfig{Name: config.ProviderClaude, Upstream: "https://api.anthropic.com"},
		config.ProviderConfig{Name: config.ProviderOpenAI, Upstream: "https://api.openai.com"},
	)
	if err != nil {
		t.Fatalf("build providers: %v", err)
	}

	matchProvider := func(method, path string) string {
		r := httptest.NewRequest(method, path, nil)
		// httptest.NewRequest sets r.URL.Path but not RawQuery for a clean
		// path; exercise the query-path case explicitly below.
		for _, p := range provs {
			if p.Matches(r) {
				return p.Name()
			}
		}
		return ""
	}

	for _, tc := range []struct {
		name string
		method string
		path  string
		want  string
	}{
		// The headline assertion: /v1/completions must reach the OpenAI
		// provider, not be claimed by Claude's /v1/complete prefix.
		{name: "openai legacy completions", method: http.MethodPost, path: "/v1/completions", want: config.ProviderOpenAI},
		{name: "openai chat completions", method: http.MethodPost, path: "/v1/chat/completions", want: config.ProviderOpenAI},
		{name: "openai responses", method: http.MethodPost, path: "/v1/responses", want: config.ProviderOpenAI},
		// Claude still owns its own endpoints.
		{name: "claude messages", method: http.MethodPost, path: "/v1/messages", want: config.ProviderClaude},
		{name: "claude legacy complete exact", method: http.MethodPost, path: "/v1/complete", want: config.ProviderClaude},
		{name: "claude legacy complete subpath", method: http.MethodPost, path: "/v1/complete/abc", want: config.ProviderClaude},
		{name: "claude legacy complete with query", method: http.MethodPost, path: "/v1/complete?stream=true", want: config.ProviderClaude},
		// Path-boundary edge: a path that merely starts with /v1/complete
		// but extends past it as an unrelated sibling must NOT match Claude.
		{name: "completions not claimed by claude", method: http.MethodPost, path: "/v1/completions", want: config.ProviderOpenAI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := matchProvider(tc.method, tc.path)
			if got != tc.want {
				t.Fatalf("matchProvider(%s %s) = %q, want %q", tc.method, tc.path, got, tc.want)
			}
		})
	}

	// Explicit full-URL parse check for the query case so the boundary on "?"
	// is exercised against a real *http.Request URL (httptest.NewRequest
	// splits RawQuery into r.URL.RawQuery, leaving Path = "/v1/complete").
	t.Run("claude legacy complete with query via parsed URL", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/complete?stream=true", nil)
		got := ""
		for _, p := range provs {
			if p.Matches(r) {
				got = p.Name()
				break
			}
		}
		if got != config.ProviderClaude {
			t.Fatalf("Matches(/v1/complete?stream=true) = %q, want %q", got, config.ProviderClaude)
		}
	})
}

// buildTestProviders constructs the real ClaudeProvider + OpenAIProvider in
// config order via the package factory, so the ordered first-match loop
// mirrors (*Server).matchProvider exactly without wiring a full *Server.
func buildTestProviders(cfgs ...config.ProviderConfig) ([]providers.Provider, error) {
	provs := make([]providers.Provider, 0, len(cfgs))
	for _, c := range cfgs {
		p, err := providers.Build(c)
		if err != nil {
			return nil, err
		}
		provs = append(provs, p)
	}
	return provs, nil
}
