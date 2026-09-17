package minimax

import (
	"net/http"

	"github.com/rsbin1178/pips/ai/anthropic"
	"github.com/rsbin1178/pips/ai/internal/clientopts"
	"github.com/rsbin1178/pips/ai/openai"
)

type options struct {
	clientopts.Options
}

// Option configures a MiniMax chat, Messages, or image model.
type Option func(*options)

func newOptions(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	return o
}

func toOpenAIOptions(opts []Option) []openai.Option {
	o := newOptions(opts)

	return o.ToOpenAIOptions(DefaultBaseURL, "MINIMAX_API_KEY")
}

func toAnthropicOptions(opts []Option) []anthropic.Option {
	o := newOptions(opts)

	options := o.ToAnthropicOptions(DefaultAnthropicBaseURL, "MINIMAX_API_KEY")

	// MiniMax's Anthropic-compatible route documents x-api-key and Bearer but
	// rejects x-api-key alone in practice ("carry the API secret key in the
	// 'Authorization' field"). Authorization takes precedence when both are
	// present, so sending both is the compatible choice.
	if key := o.ResolvedAPIKey("MINIMAX_API_KEY"); key != "" {
		options = append(options, anthropic.WithHeader("Authorization", "Bearer "+key))
	}

	return options
}

// WithAPIKey sets the MiniMax API key. Defaults to the MINIMAX_API_KEY
// environment variable.
func WithAPIKey(key string) Option {
	return func(o *options) {
		o.SetAPIKey(key)
	}
}

// WithBaseURL sets the base URL. Chat and the image API default to
// [DefaultBaseURL]; [NewAnthropic] defaults to [DefaultAnthropicBaseURL].
func WithBaseURL(url string) Option {
	return func(o *options) {
		o.SetBaseURL(url)
	}
}

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(client *http.Client) Option {
	return func(o *options) {
		o.SetHTTPClient(client)
	}
}

// WithHeader adds a custom HTTP header to every request.
func WithHeader(key, value string) Option {
	return func(o *options) {
		o.AddHeader(key, value)
	}
}

// WithAllowHTTP permits plain-HTTP base URLs (local proxy / mock endpoints).
func WithAllowHTTP() Option {
	return func(o *options) {
		o.SetAllowHTTP()
	}
}

// WithAllowPrivateIPs disables the SSRF dial guard for private/loopback addresses.
func WithAllowPrivateIPs() Option {
	return func(o *options) {
		o.SetAllowPrivateIPs()
	}
}
