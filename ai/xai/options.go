package xai

import (
	"net/http"

	"github.com/rsbin1178/pips/ai/internal/clientopts"
	"github.com/rsbin1178/pips/ai/openai"
)

type options struct {
	clientopts.Options
	api openai.API
}

// Option configures an xAI model.
type Option func(*options)

func toOpenAIOptions(opts []Option) []openai.Option {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	res := o.ToOpenAIOptions(DefaultBaseURL, "XAI_API_KEY")
	if o.api != "" {
		res = append(res, openai.WithAPI(o.api))
	}

	return res
}

// WithAPI selects which wire API surface to talk to (e.g. openai.APIResponses or openai.APIChatCompletions).
// Defaults to openai.APIResponses.
func WithAPI(api openai.API) Option {
	return func(o *options) {
		o.api = api
	}
}

// WithAPIKey sets the xAI API key. Defaults to the XAI_API_KEY environment variable.
func WithAPIKey(key string) Option {
	return func(o *options) {
		o.SetAPIKey(key)
	}
}

// WithBaseURL sets the base URL for xAI API endpoints (default: https://api.x.ai/v1).
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
