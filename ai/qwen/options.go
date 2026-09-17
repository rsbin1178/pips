package qwen

import (
	"net/http"
	"time"

	"github.com/rsbin1178/pips/ai/internal/clientopts"
	"github.com/rsbin1178/pips/ai/openai"
)

// Task polling defaults for DashScope's asynchronous image generation API.
const (
	defaultPollInterval = 3 * time.Second
	defaultPollTimeout  = 5 * time.Minute
)

type options struct {
	clientopts.Options
	pollInterval time.Duration
	pollTimeout  time.Duration
}

// Option configures a Qwen model, embedding model, rerank model, or image
// model.
type Option func(*options)

func newOptions(opts []Option) options {
	o := options{
		pollInterval: defaultPollInterval,
		pollTimeout:  defaultPollTimeout,
	}
	for _, opt := range opts {
		opt(&o)
	}

	return o
}

func toOpenAIOptions(opts []Option) []openai.Option {
	o := newOptions(opts)

	return o.ToOpenAIOptions(DefaultBaseURL, "DASHSCOPE_API_KEY")
}

// WithAPIKey sets the DashScope API key. Defaults to the DASHSCOPE_API_KEY
// environment variable.
func WithAPIKey(key string) Option {
	return func(o *options) {
		o.SetAPIKey(key)
	}
}

// WithBaseURL sets the base URL. Chat and embeddings default to
// [DefaultBaseURL]; rerank and image generation default to [DefaultHTTPAPIURL].
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

// WithPollInterval sets how often an asynchronous image task is polled
// (default 3s).
func WithPollInterval(interval time.Duration) Option {
	return func(o *options) {
		o.pollInterval = interval
	}
}

// WithPollTimeout bounds an asynchronous image task when the caller's context
// carries no deadline (default 5m).
func WithPollTimeout(timeout time.Duration) Option {
	return func(o *options) {
		o.pollTimeout = timeout
	}
}
