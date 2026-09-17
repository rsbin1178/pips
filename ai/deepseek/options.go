package deepseek

import (
	"net/http"

	"github.com/rsbin1178/pips/ai/anthropic"
	"github.com/rsbin1178/pips/ai/internal/clientopts"
	"github.com/rsbin1178/pips/ai/openai"
)

// Option configures a DeepSeek model.
type Option func(*clientopts.Options)

func toOpenAIOptions(opts []Option) []openai.Option {
	var o clientopts.Options
	for _, opt := range opts {
		opt(&o)
	}

	return o.ToOpenAIOptions(DefaultBaseURL, "DEEPSEEK_API_KEY")
}

func toAnthropicOptions(opts []Option) []anthropic.Option {
	var o clientopts.Options
	for _, opt := range opts {
		opt(&o)
	}

	return o.ToAnthropicOptions(DefaultAnthropicBaseURL, "DEEPSEEK_API_KEY")
}

// WithAPIKey sets the DeepSeek API key. Defaults to the DEEPSEEK_API_KEY environment variable.
func WithAPIKey(key string) Option {
	return func(o *clientopts.Options) {
		o.SetAPIKey(key)
	}
}

// WithBaseURL sets the base URL for DeepSeek API endpoints (default: https://api.deepseek.com).
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
