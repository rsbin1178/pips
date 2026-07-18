package openai

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/ai"
)

const embeddingsPath = "embeddings"

// EmbeddingModel is an ai.EmbeddingModel backed by OpenAI's embeddings
// endpoint (for example "text-embedding-3-small"). Create one with
// [NewEmbeddingModel].
type EmbeddingModel struct {
	model *Model
}

var _ ai.EmbeddingModel = (*EmbeddingModel)(nil)

// NewEmbeddingModel returns an embedding model bound to the given model ID. It
// accepts the same options as [New].
func NewEmbeddingModel(model string, opts ...Option) *EmbeddingModel {
	return &EmbeddingModel{model: New(model, opts...)}
}

// Provider implements ai.EmbeddingModel.
func (m *EmbeddingModel) Provider() ai.Provider { return ai.ProviderOpenAI }

// ModelID implements ai.EmbeddingModel.
func (m *EmbeddingModel) ModelID() string { return m.model.model }

type embeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions *int     `json:"dimensions,omitempty"`
}

type embeddingResponse struct {
	Data  []embeddingDatum `json:"data"`
	Usage *struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

type embeddingDatum struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// Embed implements ai.EmbeddingModel.
func (m *EmbeddingModel) Embed(ctx context.Context, req ai.EmbeddingRequest) (*ai.EmbeddingResponse, error) {
	body := embeddingRequest{
		Model:      m.model.model,
		Input:      req.Input,
		Dimensions: req.Dimensions,
	}

	var parsed embeddingResponse

	raw, err := m.model.client.PostJSON(ctx, embeddingsPath, m.model.authHeaders(), body, &parsed, decodeError)
	if err != nil {
		return nil, fmt.Errorf("openai: embeddings: %w", err)
	}

	out := &ai.EmbeddingResponse{
		Embeddings: make([][]float32, len(parsed.Data)),
		Raw:        raw,
	}
	// The API returns data in input order, but index is authoritative.
	for _, datum := range parsed.Data {
		if datum.Index < 0 || datum.Index >= len(out.Embeddings) {
			return nil, fmt.Errorf("openai: embedding index %d out of range", datum.Index)
		}

		out.Embeddings[datum.Index] = datum.Embedding
	}

	if parsed.Usage != nil {
		out.Usage = ai.Usage{InputTokens: parsed.Usage.PromptTokens}
	}

	return out, nil
}
