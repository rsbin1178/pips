package gemini

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/ai"
)

// EmbeddingModel is an ai.EmbeddingModel backed by Gemini's embedContent
// endpoint (for example "text-embedding-004" or "gemini-embedding-001").
// Create one with [NewEmbeddingModel].
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
func (m *EmbeddingModel) Provider() ai.Provider { return m.model.provider }

// ModelID implements ai.EmbeddingModel.
func (m *EmbeddingModel) ModelID() string { return m.model.model }

type embedContentRequest struct {
	Requests []embedSingle `json:"requests"`
}

type embedSingle struct {
	Model                string      `json:"model"`
	Content              wireContent `json:"content"`
	OutputDimensionality *int        `json:"outputDimensionality,omitempty"`
}

type batchEmbedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

// Embed implements ai.EmbeddingModel using the batchEmbedContents endpoint so
// multiple inputs are embedded in one request.
func (m *EmbeddingModel) Embed(ctx context.Context, req ai.EmbeddingRequest) (*ai.EmbeddingResponse, error) {
	modelName := "models/" + m.model.model

	body := embedContentRequest{Requests: make([]embedSingle, len(req.Input))}
	for i, input := range req.Input {
		body.Requests[i] = embedSingle{
			Model:                modelName,
			Content:              wireContent{Parts: []wirePart{{Text: input}}},
			OutputDimensionality: req.Dimensions,
		}
	}

	var parsed batchEmbedResponse

	raw, err := m.model.client.PostJSON(
		ctx, m.model.methodPath("batchEmbedContents"), m.model.authHeaders(), body, &parsed,
		decodeError(m.model.provider),
	)
	if err != nil {
		return nil, fmt.Errorf("gemini: batchEmbedContents: %w", err)
	}

	out := &ai.EmbeddingResponse{
		Embeddings: make([][]float32, len(parsed.Embeddings)),
		Raw:        raw,
	}
	for i, emb := range parsed.Embeddings {
		out.Embeddings[i] = emb.Values
	}

	return out, nil
}
