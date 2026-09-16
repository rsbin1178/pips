package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/rsbin1178/pips/ai"
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
func (m *EmbeddingModel) Provider() ai.Provider { return m.model.provider }

// ModelID implements ai.EmbeddingModel.
func (m *EmbeddingModel) ModelID() string { return m.model.model }

type embeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     *int     `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
}

type embeddingResponse struct {
	Data  []embeddingDatum `json:"data"`
	Usage *struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

type embeddingDatum struct {
	Index     int             `json:"index"`
	Embedding json.RawMessage `json:"embedding"`
}

// Embed implements ai.EmbeddingModel.
func (m *EmbeddingModel) Embed(ctx context.Context, req ai.EmbeddingRequest) (*ai.EmbeddingResponse, error) {
	var encodingFormat string

	switch req.EncodingFormat {
	case ai.EmbeddingEncodingFormatBase64:
		encodingFormat = "base64"
	case ai.EmbeddingEncodingFormatFloat:
		encodingFormat = "float"
	case "":
		// leave unset for provider default (float)
	default:
		return nil, fmt.Errorf("openai: unsupported encoding format %q: %w", req.EncodingFormat, ai.ErrUnsupported)
	}

	body := embeddingRequest{
		Model:          m.model.model,
		Input:          req.Input,
		Dimensions:     req.Dimensions,
		EncodingFormat: encodingFormat,
	}

	var parsed embeddingResponse

	raw, err := m.model.client.PostJSON(ctx, embeddingsPath, m.model.authHeaders(), body, &parsed, m.model.decodeError)
	if err != nil {
		return nil, fmt.Errorf("%s: embeddings: %w", m.model.label(), err)
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

		vec, err := decodeEmbedding(datum.Embedding)
		if err != nil {
			return nil, err
		}

		out.Embeddings[datum.Index] = vec
	}

	if parsed.Usage != nil {
		out.Usage = ai.Usage{InputTokens: parsed.Usage.PromptTokens}
	}

	return out, nil
}

func decodeEmbedding(raw json.RawMessage) ([]float32, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("openai: empty embedding value")
	}

	if trimmed[0] == '[' {
		var vec []float32
		if err := json.Unmarshal(trimmed, &vec); err != nil {
			return nil, fmt.Errorf("openai: unmarshal float embedding: %w", err)
		}

		return vec, nil
	}

	if trimmed[0] == '"' {
		var b64 string
		if err := json.Unmarshal(trimmed, &b64); err != nil {
			return nil, fmt.Errorf("openai: unmarshal base64 embedding string: %w", err)
		}

		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("openai: decode base64 embedding: %w", err)
		}

		if len(decoded)%4 != 0 {
			return nil, fmt.Errorf("openai: invalid base64 embedding byte length %d", len(decoded))
		}

		vec := make([]float32, len(decoded)/4)
		for i := range vec {
			bits := binary.LittleEndian.Uint32(decoded[i*4 : (i+1)*4])
			vec[i] = math.Float32frombits(bits)
		}

		return vec, nil
	}

	return nil, fmt.Errorf("openai: unexpected embedding payload prefix %q", trimmed[0])
}
