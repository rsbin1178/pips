// Package anthropic implements ai.LanguageModel against Anthropic's Messages
// API (POST /v1/messages) without the vendor SDK. It supports text, vision,
// tool use, structured output (via a forced tool call), extended thinking,
// streaming, prompt caching, and the count_tokens endpoint.
package anthropic

import (
	"net/http"
	"os"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/internal/httpx"
)

const (
	defaultBaseURL    = "https://api.anthropic.com/v1"
	defaultAPIVersion = "2023-06-01"
	// defaultMaxTokens is sent when a request omits MaxTokens, since the
	// Messages API requires the field.
	defaultMaxTokens = 4096
)

// Model is an ai.LanguageModel backed by the Anthropic Messages API. Create
// one with [New]; it is immutable and safe for concurrent use.
type Model struct {
	model      string
	apiVersion string
	beta       []string
	maxTokens  int
	client     *httpx.Client
	apiKey     string
}

// Compile-time interface checks.
var (
	_ ai.LanguageModel = (*Model)(nil)
	_ ai.TokenCounter  = (*Model)(nil)
)

// Option configures a [Model].
type Option func(*options)

type options struct {
	cfg        httpx.Config
	apiVersion string
	beta       []string
	maxTokens  int
}

// WithAPIKey sets the API key. Defaults to the ANTHROPIC_API_KEY environment
// variable.
func WithAPIKey(key string) Option {
	return func(o *options) { o.cfg.APIKey = key }
}

// WithBaseURL points the model at a different endpoint (proxy or gateway),
// including any path prefix.
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

// WithAPIVersion overrides the anthropic-version header (default
// "2023-06-01").
func WithAPIVersion(version string) Option {
	return func(o *options) { o.apiVersion = version }
}

// WithBeta appends an anthropic-beta feature flag. Call it multiple times for
// multiple flags.
func WithBeta(flag string) Option {
	return func(o *options) { o.beta = append(o.beta, flag) }
}

// WithMaxTokens sets the default max output tokens used when a request leaves
// [ai.Request.MaxTokens] nil (default 4096). The Messages API requires the
// field, so a value is always sent.
func WithMaxTokens(n int) Option {
	return func(o *options) { o.maxTokens = n }
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
// "claude-sonnet-4-5"). Configuration problems surface on the first call.
func New(model string, opts ...Option) *Model {
	o := options{apiVersion: defaultAPIVersion, maxTokens: defaultMaxTokens}
	for _, opt := range opts {
		opt(&o)
	}

	if o.cfg.APIKey == "" {
		o.cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}

	return &Model{
		model:      model,
		apiVersion: o.apiVersion,
		beta:       o.beta,
		maxTokens:  o.maxTokens,
		client:     httpx.New(o.cfg, defaultBaseURL),
		apiKey:     o.cfg.APIKey,
	}
}

// Provider implements ai.LanguageModel.
func (m *Model) Provider() ai.Provider { return ai.ProviderAnthropic }

// ModelID implements ai.LanguageModel.
func (m *Model) ModelID() string { return m.model }

// Capabilities implements ai.LanguageModel. Every current Claude model is
// text+vision+tools+structured with prompt caching; reasoning applies to the
// 3.7+/4 families but reporting it for all is harmless (unknown models still
// generate).
func (m *Model) Capabilities() ai.Capabilities {
	return ai.Capabilities{
		Text:             true,
		Vision:           true,
		Tools:            true,
		StructuredOutput: true,
		Reasoning:        true,
		PromptCaching:    true,
	}
}

// authHeaders returns the per-request authentication and versioning headers.
func (m *Model) authHeaders() http.Header {
	h := http.Header{}
	if m.apiKey != "" {
		h.Set("x-api-key", m.apiKey)
	}

	h.Set("anthropic-version", m.apiVersion)

	for _, flag := range m.beta {
		h.Add("anthropic-beta", flag)
	}

	return h
}
