// Package openai implements ai.LanguageModel against OpenAI's wire protocols
// without the vendor SDK. It speaks both API surfaces:
//
//   - Chat Completions (POST /v1/chat/completions) — the de-facto industry
//     standard, also spoken by DeepSeek, Groq, OpenRouter, Ollama, vLLM, and
//     other compatible endpoints (point BaseURL at them).
//   - Responses (POST /v1/responses) — OpenAI's newer surface with
//     first-class reasoning support.
//
// Select a surface with [WithAPI]; the default [APIAuto] routes
// reasoning-family models (o-series, gpt-5*) to Responses and everything
// else to Chat Completions.
package openai

import (
	"cmp"
	"net/http"
	"os"
	"strings"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/internal/httpx"
)

// API selects which OpenAI API surface a [Model] talks to.
type API string

// API surfaces.
const (
	// APIAuto picks per model: reasoning families (o1/o3/o4, gpt-5) use
	// Responses, everything else Chat Completions.
	APIAuto API = "auto"
	// APIChatCompletions forces POST /v1/chat/completions.
	APIChatCompletions API = "chat_completions"
	// APIResponses forces POST /v1/responses.
	APIResponses API = "responses"
)

const defaultBaseURL = "https://api.openai.com/v1"

// Wire-format string constants shared by the Chat Completions and Responses
// adapters.
const (
	typeFunction     = "function"
	typeFunctionCall = "function_call"
	typeMessage      = "message"
	typeReasoning    = "reasoning"
)

// Model is an ai.LanguageModel backed by the OpenAI API. Create one with
// [New]; it is immutable and safe for concurrent use.
type Model struct {
	model        string
	provider     ai.Provider
	api          API
	compat       Compatibility
	capabilities *ai.Capabilities
	client       *httpx.Client
	apiKey       string
}

// Compile-time interface checks.
var (
	_ ai.LanguageModel = (*Model)(nil)
)

// Option configures a [Model].
type Option func(*options)

type options struct {
	cfg          httpx.Config
	api          API
	provider     ai.Provider
	compat       Compatibility
	capabilities *ai.Capabilities
	apiKeySet    bool
}

// WithAPIKey sets the API key. Defaults to the OPENAI_API_KEY environment
// variable.
func WithAPIKey(key string) Option {
	return func(o *options) {
		o.cfg.APIKey = key
		o.apiKeySet = true
	}
}

// WithBaseURL points the model at a different endpoint, including any path
// prefix (for example "https://api.deepseek.com/v1"). Non-HTTPS or private
// endpoints additionally need [WithAllowHTTP] / [WithAllowPrivateIPs].
func WithBaseURL(url string) Option {
	return func(o *options) { o.cfg.BaseURL = url }
}

// WithHTTPClient supplies a custom *http.Client. The client is used as-is:
// transport tuning and the SSRF dial guard become the caller's
// responsibility.
func WithHTTPClient(client *http.Client) Option {
	return func(o *options) { o.cfg.HTTPClient = client }
}

// WithHeader adds a header to every request (for example OpenAI-Organization
// or OpenAI-Project). It overrides same-named adapter headers.
func WithHeader(key, value string) Option {
	return func(o *options) {
		if o.cfg.Header == nil {
			o.cfg.Header = http.Header{}
		}

		o.cfg.Header.Add(key, value)
	}
}

// WithAPI pins the API surface instead of [APIAuto] selection.
func WithAPI(api API) Option {
	return func(o *options) { o.api = api }
}

// WithProvider sets the service identity independently of the OpenAI wire
// protocol. Use it for OpenAI-compatible providers so responses, errors,
// streams, capabilities, and ProviderOptions retain the real identity.
func WithProvider(provider ai.Provider) Option {
	return func(o *options) { o.provider = provider }
}

// WithCapabilities overrides the static OpenAI model table. Compatible
// provider profiles use conservative, provider-specific capability data.
func WithCapabilities(capabilities ai.Capabilities) Option {
	return func(o *options) { o.capabilities = &capabilities }
}

// WithCompatibility configures documented wire differences on an
// OpenAI-shaped endpoint. Its zero value preserves OpenAI behavior.
func WithCompatibility(compat Compatibility) Option {
	return func(o *options) { o.compat = compat }
}

// WithCompatMode tunes requests for OpenAI-compatible third-party endpoints:
// the deprecated max_tokens field is sent instead of max_completion_tokens,
// and stream_options.include_usage is omitted. Combine with [WithBaseURL].
func WithCompatMode() Option {
	return func(o *options) {
		o.compat.MaxTokensField = MaxTokensFieldLegacy
		o.compat.StreamUsage = StreamUsageOmit
	}
}

// WithAllowHTTP permits a plain-HTTP base URL (local inference servers).
func WithAllowHTTP() Option {
	return func(o *options) { o.cfg.AllowHTTP = true }
}

// WithAllowPrivateIPs disables the SSRF guard for private/loopback endpoints
// (local inference servers).
func WithAllowPrivateIPs() Option {
	return func(o *options) { o.cfg.AllowPrivateIPs = true }
}

// WithMaxStreamLineSize raises the per-line SSE ceiling (default 1 MiB).
func WithMaxStreamLineSize(n int) Option {
	return func(o *options) { o.cfg.MaxStreamLineSize = n }
}

// New returns a Model bound to the given model ID (for example "gpt-4o").
// Configuration problems (such as an invalid base URL) surface on the first
// call, not from New.
func New(model string, opts ...Option) *Model {
	o := options{api: APIAuto, provider: ai.ProviderOpenAI}
	for _, opt := range opts {
		opt(&o)
	}

	if !o.apiKeySet {
		o.cfg.APIKey = os.Getenv("OPENAI_API_KEY")
	}

	return &Model{
		model:        model,
		provider:     cmp.Or(o.provider, ai.ProviderOpenAI),
		api:          cmp.Or(o.api, APIAuto),
		compat:       o.compat,
		capabilities: o.capabilities,
		client:       httpx.New(o.cfg, defaultBaseURL),
		apiKey:       o.cfg.APIKey,
	}
}

// Provider implements ai.LanguageModel.
func (m *Model) Provider() ai.Provider { return m.provider }

// ModelID implements ai.LanguageModel.
func (m *Model) ModelID() string { return m.model }

func (m *Model) label() string {
	return string(m.provider)
}

// resolveAPI applies APIAuto routing for the bound model.
func (m *Model) resolveAPI() API {
	if m.api != APIAuto {
		return m.api
	}

	if isReasoningModel(m.model) {
		return APIResponses
	}

	return APIChatCompletions
}

// isReasoningModel reports whether the model belongs to a reasoning family
// that requires (or works best on) the Responses API.
func isReasoningModel(model string) bool {
	for _, prefix := range []string{"o1", "o3", "o4", "gpt-5"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}

	return false
}

// authHeaders returns the per-request authentication headers.
func (m *Model) authHeaders() http.Header {
	h := http.Header{}
	if m.apiKey != "" {
		h.Set("Authorization", "Bearer "+m.apiKey)
	}

	return h
}
