package cohere

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rsbin1178/pips/ai"
)

const rerankPath = "rerank"

type rerankWireRequest struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            *int     `json:"top_n,omitempty"`
	ReturnDocuments bool     `json:"return_documents,omitempty"`
	MaxTokensPerDoc *int     `json:"max_tokens_per_doc,omitempty"`
	RankFields      []string `json:"rank_fields,omitempty"`
	Priority        *int     `json:"priority,omitempty"`
}

type rerankWireResponse struct {
	ID      string             `json:"id"`
	Results []rerankWireResult `json:"results"`
	Meta    *rerankMeta        `json:"meta"`
	Usage   *rerankUsage       `json:"usage"`
}

type rerankWireResult struct {
	Index          int             `json:"index"`
	RelevanceScore float64         `json:"relevance_score"`
	Document       json.RawMessage `json:"document,omitempty"`
}

type rerankMeta struct {
	Tokens *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"tokens"`
	BilledUnits *struct {
		SearchUnits int `json:"search_units"`
	} `json:"billed_units"`
}

type rerankUsage struct {
	TotalTokens  int `json:"total_tokens"`
	PromptTokens int `json:"prompt_tokens"`
}

func validateRequest(req ai.RerankRequest) error {
	if strings.TrimSpace(req.Query) == "" {
		return fmt.Errorf("cohere: query cannot be empty: %w", ai.ErrInvalidRequest)
	}

	if len(req.Documents) == 0 {
		return fmt.Errorf("cohere: documents cannot be empty: %w", ai.ErrInvalidRequest)
	}

	if req.TopN != nil && *req.TopN <= 0 {
		return fmt.Errorf("cohere: top_n must be greater than 0: %w", ai.ErrInvalidRequest)
	}

	return nil
}

func extractUsage(parsed *rerankWireResponse) ai.Usage {
	if parsed.Meta != nil && parsed.Meta.Tokens != nil {
		return ai.Usage{
			InputTokens:  parsed.Meta.Tokens.InputTokens,
			OutputTokens: parsed.Meta.Tokens.OutputTokens,
		}
	}

	if parsed.Usage != nil {
		inputTokens := parsed.Usage.PromptTokens
		if inputTokens == 0 {
			inputTokens = parsed.Usage.TotalTokens
		}

		return ai.Usage{
			InputTokens: inputTokens,
		}
	}

	return ai.Usage{}
}

// Rerank implements ai.RerankModel against the Cohere Rerank API or compatible
// endpoint.
func (m *RerankModel) Rerank(ctx context.Context, req ai.RerankRequest) (*ai.RerankResponse, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}

	providerOpts := extractRerankOptions(req.ProviderOptions, m.provider)

	wireReq := rerankWireRequest{
		Model:           m.model,
		Query:           req.Query,
		Documents:       req.Documents,
		TopN:            req.TopN,
		ReturnDocuments: req.ReturnDocuments,
		MaxTokensPerDoc: providerOpts.MaxTokensPerDoc,
		RankFields:      providerOpts.RankFields,
		Priority:        providerOpts.Priority,
	}

	payload, err := mergeRerankExtraFields(wireReq, providerOpts.ExtraFields)
	if err != nil {
		return nil, fmt.Errorf("cohere: merge extra fields: %w", err)
	}

	var parsed rerankWireResponse

	raw, err := m.client.PostJSON(ctx, rerankPath, m.authHeaders(), payload, &parsed, decodeError(m.provider))
	if err != nil {
		return nil, fmt.Errorf("%s: rerank: %w", m.provider, err)
	}

	out := &ai.RerankResponse{
		Results: make([]ai.RerankResult, len(parsed.Results)),
		Usage:   extractUsage(&parsed),
		Raw:     raw,
	}

	for i, datum := range parsed.Results {
		if datum.Index < 0 || datum.Index >= len(req.Documents) {
			return nil, fmt.Errorf("cohere: result index %d out of range [0, %d)", datum.Index, len(req.Documents))
		}

		docText, err := decodeDocument(datum.Document)
		if err != nil {
			return nil, fmt.Errorf("cohere: decode document at index %d: %w", datum.Index, err)
		}

		if docText == "" && req.ReturnDocuments {
			docText = req.Documents[datum.Index]
		}

		out.Results[i] = ai.RerankResult{
			Index:          datum.Index,
			RelevanceScore: datum.RelevanceScore,
			Document:       docText,
		}
	}

	return out, nil
}

// decodeDocument decodes polymorphic document formats: string, object with
// "text" property (Cohere default), or empty/null.
func decodeDocument(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", fmt.Errorf("unmarshal string document: %w", err)
		}

		return s, nil
	}

	if trimmed[0] == '{' {
		var obj struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return "", fmt.Errorf("unmarshal object document: %w", err)
		}

		return obj.Text, nil
	}

	return string(trimmed), nil
}
