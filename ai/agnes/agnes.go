// Package agnes implements ai.ImageModel against the Agnes Image API without
// the vendor SDK: text-to-image generation and multi-image editing through a
// single endpoint.
//
// Agnes Images is shaped like the OpenAI Images API but is not
// wire-compatible with it: size is a required tier ("1K"–"4K") combined with a
// separate ratio, editing travels through extra_body.image of the same
// generations endpoint instead of a dedicated edits endpoint, and batch count,
// masks, variations, and streaming are not documented. The adapter therefore
// owns its request encoding instead of layering on ai/openai.
package agnes

import (
	"net/http"
	"os"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/httpx"
)

// Base URLs of the two Agnes sites. Both speak the same protocol; only the
// host differs.
const (
	// BaseURLCN is the mainland-China endpoint and the default.
	BaseURLCN = "https://api.agnes-ai.cn/v1"
	// BaseURLGlobal is the international endpoint; select it with
	// [WithBaseURL].
	BaseURLGlobal = "https://apihub.agnes-ai.com/v1"
)

// DefaultBaseURL returns the endpoint [NewImageModel] uses, [BaseURLCN].
func DefaultBaseURL() string { return BaseURLCN }

// ImageModel is an ai.ImageModel (and ai.ImageEditor) backed by the Agnes
// Image API. Create one with [NewImageModel]; it is immutable and safe for
// concurrent use.
type ImageModel struct {
	model        string
	client       *httpx.Client
	apiKey       string
	capabilities *ai.Capabilities
}

// Compile-time interface checks.
var (
	_ ai.ImageModel  = (*ImageModel)(nil)
	_ ai.ImageEditor = (*ImageModel)(nil)
)

// Option configures an [ImageModel].
type Option func(*options)

type options struct {
	cfg          httpx.Config
	capabilities *ai.Capabilities
	apiKeySet    bool
}

// WithAPIKey sets the API key. Defaults to the AGNES_API_KEY environment
// variable.
func WithAPIKey(key string) Option {
	return func(o *options) {
		o.cfg.APIKey = key
		o.apiKeySet = true
	}
}

// WithBaseURL points the model at a different endpoint, including any path
// prefix: [BaseURLCN] (the default), [BaseURLGlobal], or a compatible
// gateway. Non-HTTPS or private endpoints additionally need [WithAllowHTTP] /
// [WithAllowPrivateIPs].
func WithBaseURL(url string) Option {
	return func(o *options) { o.cfg.BaseURL = url }
}

// WithHTTPClient supplies a custom *http.Client. The client is used as-is:
// transport tuning and the SSRF dial guard become the caller's
// responsibility.
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

// WithAllowHTTP permits a plain-HTTP base URL (local gateways).
func WithAllowHTTP() Option {
	return func(o *options) { o.cfg.AllowHTTP = true }
}

// WithAllowPrivateIPs disables the SSRF guard for private/loopback endpoints
// (local gateways).
func WithAllowPrivateIPs() Option {
	return func(o *options) { o.cfg.AllowPrivateIPs = true }
}

// WithMaxStreamLineSize raises the per-line SSE ceiling (default 1 MiB). Agnes
// documents no streaming surface, so it only fills the constructor slot the
// other adapters accept.
func WithMaxStreamLineSize(n int) Option {
	return func(o *options) { o.cfg.MaxStreamLineSize = n }
}

// WithCapabilities overrides the static capability report.
func WithCapabilities(capabilities ai.Capabilities) Option {
	return func(o *options) { o.capabilities = &capabilities }
}

// NewImageModel returns an image model bound to the given model ID (for
// example "agnes-image-2.5-flash"). Configuration problems (such as an
// invalid base URL) surface on the first call, not from New.
func NewImageModel(model string, opts ...Option) *ImageModel {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}

	if !o.apiKeySet {
		o.cfg.APIKey = os.Getenv("AGNES_API_KEY")
	}

	return &ImageModel{
		model:        model,
		client:       httpx.New(o.cfg, BaseURLCN),
		apiKey:       o.cfg.APIKey,
		capabilities: o.capabilities,
	}
}

// Provider implements ai.ImageModel.
func (m *ImageModel) Provider() ai.Provider { return ai.ProviderAgnes }

// ModelID implements ai.ImageModel.
func (m *ImageModel) ModelID() string { return m.model }

// Capabilities implements ai.ImageModel. It is a static hint, never a call
// gate.
func (m *ImageModel) Capabilities() ai.Capabilities {
	if m.capabilities != nil {
		return *m.capabilities
	}

	return ai.Capabilities{ImageGeneration: true}
}

// authHeaders returns the per-request authentication headers.
func (m *ImageModel) authHeaders() http.Header {
	h := http.Header{}
	if m.apiKey != "" {
		h.Set("Authorization", "Bearer "+m.apiKey)
	}

	return h
}
