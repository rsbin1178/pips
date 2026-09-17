package groq

import (
	"net/http"

	"github.com/rsbin1178/pips/ai/internal/clientopts"
	"github.com/rsbin1178/pips/ai/openai"
)

// ReasoningFormat values supported by Groq reasoning models (e.g. DeepSeek-R1, Qwen).
const (
	ReasoningFormatParsed = "parsed"
	ReasoningFormatRaw    = "raw"
	ReasoningFormatHidden = "hidden"
)

// RequestOptions returns an openai.RequestOptions configured with Groq's reasoning_format parameter.
func RequestOptions(reasoningFormat string) openai.RequestOptions {
	return openai.RequestOptions{
		ExtraFields: map[string]any{
			"reasoning_format": reasoningFormat,
		},
	}
}

// Option configures a Groq model.
type Option func(*clientopts.Options)

func toOpenAIOptions(opts []Option) []openai.Option {
	var o clientopts.Options
	for _, opt := range opts {
		opt(&o)
	}

	return o.ToOpenAIOptions(DefaultBaseURL, "GROQ_API_KEY")
}

// WithAPIKey sets the Groq API key. Defaults to the GROQ_API_KEY environment variable.
func WithAPIKey(key string) Option {
	return func(o *clientopts.Options) {
		o.SetAPIKey(key)
	}
}

// WithBaseURL sets the base URL for Groq API endpoints (default: https://api.groq.com/openai/v1).
func WithBaseURL(url string) Option {
	return func(o *clientopts.Options) {
		o.SetBaseURL(url)
	}
}

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(client *http.Client) Option {
	return func(o *clientopts.Options) {
		o.SetHTTPClient(client)
	}
}

// WithHeader adds a custom HTTP header to every request.
func WithHeader(key, value string) Option {
	return func(o *clientopts.Options) {
		o.AddHeader(key, value)
	}
}

// WithAllowHTTP permits plain-HTTP base URLs (local proxy / mock endpoints).
func WithAllowHTTP() Option {
	return func(o *clientopts.Options) {
		o.SetAllowHTTP()
	}
}

// WithAllowPrivateIPs disables the SSRF dial guard for private/loopback addresses.
func WithAllowPrivateIPs() Option {
	return func(o *clientopts.Options) {
		o.SetAllowPrivateIPs()
	}
}
