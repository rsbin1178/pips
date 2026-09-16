// Package gemini implements ai.LanguageModel against Google's Gemini API
// (generateContent / streamGenerateContent) without the vendor SDK. It
// supports text, vision, tool use, structured output, thinking, streaming,
// and the countTokens endpoint.
package gemini

import (
	"net/http"
	"os"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/httpx"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// DefaultBaseURL returns the official Gemini endpoint used by New.
func DefaultBaseURL() string { return defaultBaseURL }

// Model is an ai.LanguageModel backed by the Gemini API. Create one with
// [New]; it is immutable and safe for concurrent use.
type Model struct {
	model    string
	provider ai.Provider
	client   *httpx.Client
	apiKey   string
}

// Compile-time interface checks.
var (
	_ ai.LanguageModel = (*Model)(nil)
	_ ai.TokenCounter  = (*Model)(nil)
)

// Option configures a [Model].
type Option func(*options)

type options struct {
	cfg      httpx.Config
	provider ai.Provider
}

// WithProvider sets the service identity independently from the Gemini wire
// protocol. It is intended for compatible gateways and self-hosted APIs.
func WithProvider(provider ai.Provider) Option {
	return func(o *options) { o.provider = provider }
}

// WithAPIKey sets the API key. Defaults to the GEMINI_API_KEY environment
// variable, then GOOGLE_API_KEY.
func WithAPIKey(key string) Option {
	return func(o *options) { o.cfg.APIKey = key }
}

// WithBaseURL points the model at a different endpoint (proxy or Vertex
// gateway), including any version prefix.
func WithBaseURL(url string) Option {
	return func(o *options) { o.cfg.BaseURL = url }
}

// WithHTTPClient supplies a custom *http.Client, used as-is (the caller owns
// transport tuning and the SSRF guard).
func WithHTTPClient(client *http.Client) Option {
	return func(o *options) { o.cfg.HTTPClient = client }
}

// WithHeader adds a header to every request. It overrides same-named adapter
// headers.
func WithHeader(key, value string) Option {
	return func(o *options) {
		if o.cfg.Header == nil {
			o.cfg.Header = http.Header{}
		}

		o.cfg.Header.Add(key, value)
	}
}

// WithAllowHTTP permits a plain-HTTP base URL (local proxy).
func WithAllowHTTP() Option {
	return func(o *options) { o.cfg.AllowHTTP = true }
}

// WithAllowPrivateIPs disables the SSRF guard for private/loopback endpoints.
func WithAllowPrivateIPs() Option {
	return func(o *options) { o.cfg.AllowPrivateIPs = true }
}

// WithMaxStreamLineSize raises the per-line SSE ceiling (default 1 MiB).
func WithMaxStreamLineSize(n int) Option {
	return func(o *options) { o.cfg.MaxStreamLineSize = n }
}

// New returns a Model bound to the given model ID (for example
// "gemini-3.8-flash"). Configuration problems surface on the first call.
func New(model string, opts ...Option) *Model {
	o := options{provider: ai.ProviderGemini}
	for _, opt := range opts {
		opt(&o)
	}

	if o.cfg.APIKey == "" {
		o.cfg.APIKey = firstEnv("GEMINI_API_KEY", "GOOGLE_API_KEY")
	}

	return &Model{
		model:    model,
		provider: o.provider,
		client:   httpx.New(o.cfg, defaultBaseURL),
		apiKey:   o.cfg.APIKey,
	}
}

// Provider implements ai.LanguageModel.
func (m *Model) Provider() ai.Provider { return m.provider }

// ModelID implements ai.LanguageModel.
func (m *Model) ModelID() string { return m.model }

// Capabilities implements ai.LanguageModel. Gemini flash/pro models are
// multi-modal with tools, structured output, and thinking (2.5+).
func (m *Model) Capabilities() ai.Capabilities {
	return ai.Capabilities{
		Text:             true,
		Vision:           true,
		Documents:        true,
		AudioInput:       true,
		VideoInput:       true,
		Tools:            true,
		StructuredOutput: true,
		Reasoning:        true,
		PromptCaching:    true,
		TokenCounting:    true,
		WebSearch:        true,
		CodeExecution:    true,
	}
}

// authHeaders returns the per-request authentication headers.
func (m *Model) authHeaders() http.Header {
	h := http.Header{}
	if m.apiKey != "" {
		h.Set("x-goog-api-key", m.apiKey)
	}

	return h
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}

	return ""
}
