package cohere

import (
	"net/http"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// WithAPIKey sets the provider API key. Defaults to the COHERE_API_KEY
// environment variable.
func WithAPIKey(key string) Option {
	return func(o *options) {
		o.cfg.APIKey = key
		o.apiKeySet = true
	}
}

// WithBaseURL points the model at a different endpoint, including any path
// prefix: [DefaultBaseURL] (the default), "https://api.cohere.com/v2",
// a compatible cloud service, or a local server. Non-HTTPS or private
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

// WithAllowHTTP permits a plain-HTTP base URL (local inference servers).
func WithAllowHTTP() Option {
	return func(o *options) { o.cfg.AllowHTTP = true }
}

// WithAllowPrivateIPs disables the SSRF guard for private/loopback endpoints
// (local inference servers).
func WithAllowPrivateIPs() Option {
	return func(o *options) { o.cfg.AllowPrivateIPs = true }
}

// WithProvider overrides the provider identity reported by [RerankModel.Provider].
// Defaults to [ai.ProviderCohere].
func WithProvider(provider ai.Provider) Option {
	return func(o *options) { o.provider = provider }
}

// WithCapabilities overrides the static capability report.
func WithCapabilities(capabilities ai.Capabilities) Option {
	return func(o *options) { o.capabilities = &capabilities }
}

// RerankOptions is the Cohere entry for [ai.RerankRequest.ProviderOptions]:
//
//	req.ProviderOptions = map[ai.Provider]any{
//	    ai.ProviderCohere: cohere.RerankOptions{
//	        MaxTokensPerDoc: pips.Int(4096),
//	    },
//	}
type RerankOptions struct {
	// MaxTokensPerDoc limits the maximum number of tokens to consider per document.
	// Long documents are automatically truncated.
	MaxTokensPerDoc *int
	// RankFields specifies the keys in structured documents to consider for ranking.
	RankFields []string
	// Priority controls request handling priority during system load (0-999).
	Priority *int

	// ExtraFields is merged into the top level of the outgoing request body
	// using bounded, add-only semantics. It is the escape hatch for vendor
	// parameters this adapter does not model.
	ExtraFields map[string]any
}

// extractRerankOptions extracts this provider's options from a ProviderOptions map,
// checking the model's actual provider first, then ai.ProviderCohere.
func extractRerankOptions(options map[ai.Provider]any, provider ai.Provider) RerankOptions {
	if options == nil {
		return RerankOptions{}
	}

	if raw, ok := options[provider]; ok {
		if opts, ok := raw.(RerankOptions); ok {
			return opts
		}
	}

	if raw, ok := options[ai.ProviderCohere]; ok {
		if opts, ok := raw.(RerankOptions); ok {
			return opts
		}
	}

	return RerankOptions{}
}

// rerankReservedFields are the request keys this adapter's typed fields own.
var rerankReservedFields = []string{
	"model", "query", "documents", "top_n", "return_documents",
	"max_tokens_per_doc", "rank_fields", "priority",
}

// mergeRerankExtraFields folds opts.ExtraFields into an already-encoded JSON
// request body.
func mergeRerankExtraFields(body any, extra map[string]any) (any, error) {
	return jsonx.MergeExtraFields(body, extra, rerankReservedFields...)
}
