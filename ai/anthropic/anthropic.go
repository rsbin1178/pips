// Package anthropic implements ai.LanguageModel against Anthropic's Messages
// API (POST /v1/messages) without the vendor SDK. It supports text, vision,
// tool use, native structured output, extended thinking,
// streaming, prompt caching, and the count_tokens endpoint.
package anthropic

import (
	"net/http"
	"os"
	"slices"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/internal/httpx"
)

const (
	defaultBaseURL    = "https://api.anthropic.com/v1"
	defaultAPIVersion = "2023-06-01"
	filesAPIBeta      = "files-api-2025-04-14"
	// defaultMaxTokens is sent when a request omits MaxTokens, since the
	// Messages API requires the field.
	defaultMaxTokens = 4096
)

// DefaultBaseURL returns the official Anthropic Messages endpoint used by New.
func DefaultBaseURL() string { return defaultBaseURL }

// Model is an ai.LanguageModel backed by the Anthropic Messages API. Create
// one with [New]; it is immutable and safe for concurrent use.
type Model struct {
	model      string
	provider   ai.Provider
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
	provider   ai.Provider
	apiVersion string
	beta       []string
	maxTokens  int
}

// WithProvider sets the service identity independently from the Anthropic
// wire protocol. It is intended for compatible gateways and self-hosted APIs.
func WithProvider(provider ai.Provider) Option {
	return func(o *options) { o.provider = provider }
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
	o := options{
		apiVersion: defaultAPIVersion,
		maxTokens:  defaultMaxTokens,
		provider:   ai.ProviderAnthropic,
	}
	for _, opt := range opts {
		opt(&o)
	}

	if o.cfg.APIKey == "" {
		o.cfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}

	return &Model{
		model:      model,
		provider:   o.provider,
		apiVersion: o.apiVersion,
		beta:       o.beta,
		maxTokens:  o.maxTokens,
		client:     httpx.New(o.cfg, defaultBaseURL),
		apiKey:     o.cfg.APIKey,
	}
}

// Provider implements ai.LanguageModel.
func (m *Model) Provider() ai.Provider { return m.provider }

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
		Documents:        true,
		Tools:            true,
		StructuredOutput: true,
		Reasoning:        true,
		PromptCaching:    true,
		TokenCounting:    true,
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

func (m *Model) requestHeaders(req ai.Request) http.Header {
	h := m.authHeaders()
	if requestHasFileID(req) && !slices.Contains(h.Values("anthropic-beta"), filesAPIBeta) {
		h.Add("anthropic-beta", filesAPIBeta)
	}

	return h
}

func requestHasFileID(req ai.Request) bool {
	for _, msg := range req.Messages {
		switch msg := msg.(type) {
		case ai.UserMessage:
			if partsHaveFileID(msg.Parts) {
				return true
			}
		case ai.ToolMessage:
			if partsHaveFileID(msg.Parts) {
				return true
			}
		}
	}

	return false
}

func partsHaveFileID[T ai.Part](parts []T) bool {
	for _, part := range parts {
		switch p := any(part).(type) {
		case ai.ImagePart:
			if p.Source.IsID() {
				return true
			}
		case ai.FilePart:
			if p.Source.IsID() {
				return true
			}
		case ai.ToolResultPart:
			if partsHaveFileID(p.Content) {
				return true
			}
		}
	}

	return false
}
