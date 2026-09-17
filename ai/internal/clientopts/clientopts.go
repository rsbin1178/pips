// Package clientopts provides shared option types and converters for provider facades.
package clientopts

import (
	"cmp"
	"net/http"
	"os"

	"github.com/rsbin1178/pips/ai/anthropic"
	"github.com/rsbin1178/pips/ai/cohere"
	"github.com/rsbin1178/pips/ai/internal/httpx"
	"github.com/rsbin1178/pips/ai/openai"
)

// Options holds common client configuration shared across provider facades.
type Options struct {
	APIKey          string
	APIKeySet       bool
	BaseURL         string
	HTTPClient      *http.Client
	Header          http.Header
	AllowHTTP       bool
	AllowPrivateIPs bool
}

// SetAPIKey sets an explicit API key.
func (o *Options) SetAPIKey(key string) {
	o.APIKey = key
	o.APIKeySet = true
}

// SetBaseURL sets an explicit base URL.
func (o *Options) SetBaseURL(url string) {
	o.BaseURL = url
}

// SetHTTPClient sets a custom HTTP client.
func (o *Options) SetHTTPClient(client *http.Client) {
	o.HTTPClient = client
}

// AddHeader adds a custom HTTP header to all requests.
func (o *Options) AddHeader(key, value string) {
	if o.Header == nil {
		o.Header = http.Header{}
	}

	o.Header.Add(key, value)
}

// SetAllowHTTP permits plain-HTTP base URLs (e.g. for local proxies or tests).
func (o *Options) SetAllowHTTP() {
	o.AllowHTTP = true
}

// SetAllowPrivateIPs permits connections to private or loopback IPs.
func (o *Options) SetAllowPrivateIPs() {
	o.AllowPrivateIPs = true
}

// ResolvedAPIKey returns the configured API key, falling back to the first non-empty environment variable.
func (o *Options) ResolvedAPIKey(envVars ...string) string {
	if o.APIKeySet {
		return o.APIKey
	}

	for _, env := range envVars {
		if val := os.Getenv(env); val != "" {
			return val
		}
	}

	return ""
}

// ResolvedBaseURL returns the configured base URL, falling back to defaultBaseURL.
func (o *Options) ResolvedBaseURL(defaultBaseURL string) string {
	return cmp.Or(o.BaseURL, defaultBaseURL)
}

// ToHTTPXConfig converts Options into an httpx.Config. Facades whose adapter
// owns its wire format (image generation, rerank, task-based APIs) use it
// instead of delegating to the openai/anthropic/cohere option families.
func (o *Options) ToHTTPXConfig(defaultBaseURL string, envVars ...string) httpx.Config {
	cfg := httpx.Config{
		APIKey:          o.ResolvedAPIKey(envVars...),
		BaseURL:         o.ResolvedBaseURL(defaultBaseURL),
		HTTPClient:      o.HTTPClient,
		AllowHTTP:       o.AllowHTTP,
		AllowPrivateIPs: o.AllowPrivateIPs,
	}
	if o.Header != nil {
		cfg.Header = o.Header.Clone()
	}

	return cfg
}

// ToOpenAIOptions converts Options into a slice of openai.Option.
//
//nolint:dupl // adapter boilerplate conversion
func (o *Options) ToOpenAIOptions(defaultBaseURL string, envVars ...string) []openai.Option {
	opts := []openai.Option{
		openai.WithBaseURL(o.ResolvedBaseURL(defaultBaseURL)),
		openai.WithAPIKey(o.ResolvedAPIKey(envVars...)),
	}

	if o.HTTPClient != nil {
		opts = append(opts, openai.WithHTTPClient(o.HTTPClient))
	}

	for k, vals := range o.Header {
		for _, v := range vals {
			opts = append(opts, openai.WithHeader(k, v))
		}
	}

	if o.AllowHTTP {
		opts = append(opts, openai.WithAllowHTTP())
	}

	if o.AllowPrivateIPs {
		opts = append(opts, openai.WithAllowPrivateIPs())
	}

	return opts
}

// ToCohereOptions converts Options into a slice of cohere.Option.
//
//nolint:dupl // adapter boilerplate conversion
func (o *Options) ToCohereOptions(defaultBaseURL string, envVars ...string) []cohere.Option {
	opts := []cohere.Option{
		cohere.WithBaseURL(o.ResolvedBaseURL(defaultBaseURL)),
		cohere.WithAPIKey(o.ResolvedAPIKey(envVars...)),
	}

	if o.HTTPClient != nil {
		opts = append(opts, cohere.WithHTTPClient(o.HTTPClient))
	}

	for k, vals := range o.Header {
		for _, v := range vals {
			opts = append(opts, cohere.WithHeader(k, v))
		}
	}

	if o.AllowHTTP {
		opts = append(opts, cohere.WithAllowHTTP())
	}

	if o.AllowPrivateIPs {
		opts = append(opts, cohere.WithAllowPrivateIPs())
	}

	return opts
}

// ToAnthropicOptions converts Options into a slice of anthropic.Option.
//
//nolint:dupl // adapter boilerplate conversion
func (o *Options) ToAnthropicOptions(defaultBaseURL string, envVars ...string) []anthropic.Option {
	opts := []anthropic.Option{
		anthropic.WithBaseURL(o.ResolvedBaseURL(defaultBaseURL)),
		anthropic.WithAPIKey(o.ResolvedAPIKey(envVars...)),
	}

	if o.HTTPClient != nil {
		opts = append(opts, anthropic.WithHTTPClient(o.HTTPClient))
	}

	for k, vals := range o.Header {
		for _, v := range vals {
			opts = append(opts, anthropic.WithHeader(k, v))
		}
	}

	if o.AllowHTTP {
		opts = append(opts, anthropic.WithAllowHTTP())
	}

	if o.AllowPrivateIPs {
		opts = append(opts, anthropic.WithAllowPrivateIPs())
	}

	return opts
}
