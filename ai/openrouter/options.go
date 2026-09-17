package openrouter

import (
	"net/http"

	"github.com/rsbin1178/pips/ai/internal/clientopts"
	"github.com/rsbin1178/pips/ai/openai"
)

// Attribution headers documented by OpenRouter.
const (
	refererHeader = "HTTP-Referer"
	titleHeader   = "X-OpenRouter-Title"
)

type options struct {
	clientopts.Options
	referer  string
	appTitle string
}

// Option configures an OpenRouter model.
type Option func(*options)

func toOpenAIOptions(opts []Option) []openai.Option {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	if o.referer != "" {
		o.AddHeader(refererHeader, o.referer)
	}

	if o.appTitle != "" {
		o.AddHeader(titleHeader, o.appTitle)
	}

	return o.ToOpenAIOptions(DefaultBaseURL, "OPENROUTER_API_KEY")
}

// WithAPIKey sets the OpenRouter API key. Defaults to the OPENROUTER_API_KEY
// environment variable.
func WithAPIKey(key string) Option {
	return func(o *options) {
		o.SetAPIKey(key)
	}
}

// WithBaseURL sets the base URL for OpenRouter API endpoints (default:
// https://openrouter.ai/api/v1).
func WithBaseURL(url string) Option {
	return func(o *options) {
		o.SetBaseURL(url)
	}
}

// WithReferer sets the optional HTTP-Referer attribution header.
func WithReferer(url string) Option {
	return func(o *options) {
		o.referer = url
	}
}

// WithAppTitle sets the optional X-OpenRouter-Title attribution header.
func WithAppTitle(title string) Option {
	return func(o *options) {
		o.appTitle = title
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
