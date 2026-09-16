package cohere

import (
	"net/http"
	"os"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/httpx"
)

// DefaultBaseURL is the official Cohere v1 API endpoint used by [NewRerankModel].
const DefaultBaseURL = "https://api.cohere.com/v1"

// RerankModel is an ai.RerankModel backed by the Cohere Rerank API or compatible
// endpoints. Create one with [NewRerankModel]; it is immutable and safe for concurrent use.
type RerankModel struct {
	model        string
	provider     ai.Provider
	client       *httpx.Client
	apiKey       string
	capabilities *ai.Capabilities
}

// Compile-time interface checks.
var _ ai.RerankModel = (*RerankModel)(nil)

// Option configures a [RerankModel].
type Option func(*options)

type options struct {
	cfg          httpx.Config
	provider     ai.Provider
	capabilities *ai.Capabilities
	apiKeySet    bool
}

// NewRerankModel returns a rerank model bound to the given model ID (for example
// "rerank-v3.5" or "rerank-multilingual-v3.0"). Configuration problems (such as
// an invalid base URL) surface on the first call, not from NewRerankModel.
func NewRerankModel(model string, opts ...Option) *RerankModel {
	o := options{
		provider: ai.ProviderCohere,
	}
	for _, opt := range opts {
		opt(&o)
	}

	if !o.apiKeySet {
		o.cfg.APIKey = os.Getenv("COHERE_API_KEY")
	}

	return &RerankModel{
		model:        model,
		provider:     o.provider,
		client:       httpx.New(o.cfg, DefaultBaseURL),
		apiKey:       o.cfg.APIKey,
		capabilities: o.capabilities,
	}
}

// Provider implements ai.RerankModel.
func (m *RerankModel) Provider() ai.Provider { return m.provider }

// ModelID implements ai.RerankModel.
func (m *RerankModel) ModelID() string { return m.model }

// Capabilities reports, best effort, what the bound model supports. It is a
// static hint, never a call gate.
func (m *RerankModel) Capabilities() ai.Capabilities {
	if m.capabilities != nil {
		return *m.capabilities
	}

	return ai.Capabilities{Reranking: true}
}

// authHeaders returns the per-request authentication headers.
func (m *RerankModel) authHeaders() http.Header {
	h := http.Header{}
	if m.apiKey != "" {
		h.Set("Authorization", "Bearer "+m.apiKey)
	}

	return h
}
