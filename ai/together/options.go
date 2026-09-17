package together

import (
	"net/http"

	"github.com/rsbin1178/pips/ai/cohere"
	"github.com/rsbin1178/pips/ai/internal/clientopts"
	"github.com/rsbin1178/pips/ai/openai"
)

// Option configures a Together model.
type Option func(*clientopts.Options)

func toOpenAIOptions(opts []Option) []openai.Option {
	var o clientopts.Options
	for _, opt := range opts {
		opt(&o)
	}

	return o.ToOpenAIOptions(DefaultBaseURL, "TOGETHER_API_KEY")
}

func toCohereOptions(opts []Option) []cohere.Option {
	var o clientopts.Options
	for _, opt := range opts {
		opt(&o)
	}

	return o.ToCohereOptions(DefaultBaseURL, "TOGETHER_API_KEY")
}

// WithAPIKey sets the Together API key. Defaults to the TOGETHER_API_KEY environment variable.
func WithAPIKey(key string) Option {
	return func(o *clientopts.Options) {
		o.SetAPIKey(key)
	}
}

// WithBaseURL sets the base URL for Together API endpoints (default: https://api.together.ai/v1).
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
