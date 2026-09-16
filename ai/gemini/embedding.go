package gemini

import (
	"context"
	"fmt"

	"github.com/rsbin1178/pips/ai"
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
	TaskType             string      `json:"taskType,omitempty"`
	Title                string      `json:"title,omitempty"`
}

type batchEmbedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

func mapTaskType(tt ai.EmbeddingTaskType) (string, error) {
	switch tt {
	case "":
		return "", nil
	case ai.EmbeddingTaskTypeQuery:
		return "RETRIEVAL_QUERY", nil
	case ai.EmbeddingTaskTypeDocument:
		return "RETRIEVAL_DOCUMENT", nil
	case ai.EmbeddingTaskTypeSimilarity:
		return "SEMANTIC_SIMILARITY", nil
	case ai.EmbeddingTaskTypeClassification:
		return "CLASSIFICATION", nil
	case ai.EmbeddingTaskTypeClustering:
		return "CLUSTERING", nil
	case ai.EmbeddingTaskTypeQuestionAnswer:
		return "QUESTION_ANSWERING", nil
	case ai.EmbeddingTaskTypeFactCheck:
		return "FACT_VERIFICATION", nil
	case ai.EmbeddingTaskTypeCodeQuery:
		return "CODE_RETRIEVAL_QUERY", nil
	default:
		return "", fmt.Errorf("gemini: unsupported embedding task type %q: %w", tt, ai.ErrUnsupported)
	}
}

// Embed implements ai.EmbeddingModel using the batchEmbedContents endpoint so
// multiple inputs are embedded in one request.
func (m *EmbeddingModel) Embed(ctx context.Context, req ai.EmbeddingRequest) (*ai.EmbeddingResponse, error) {
	if req.EncodingFormat != "" && req.EncodingFormat != ai.EmbeddingEncodingFormatFloat {
		return nil, fmt.Errorf("gemini: unsupported encoding format %q: %w", req.EncodingFormat, ai.ErrUnsupported)
	}

	taskType, err := mapTaskType(req.TaskType)
	if err != nil {
		return nil, err
	}

	modelName := "models/" + m.model.model

	body := embedContentRequest{Requests: make([]embedSingle, len(req.Input))}
	for i, input := range req.Input {
		body.Requests[i] = embedSingle{
			Model:                modelName,
			Content:              wireContent{Parts: []wirePart{{Text: input}}},
			OutputDimensionality: req.Dimensions,
			TaskType:             taskType,
			Title:                req.Title,
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
