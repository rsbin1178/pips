package qwen

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/httpx"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// DefaultHTTPAPIURL is the international DashScope native HTTP API base used
// by text rerank and asynchronous image generation.
const DefaultHTTPAPIURL = "https://dashscope-intl.aliyuncs.com/api/v1"

// DefaultChinaHTTPAPIURL is the mainland China DashScope native HTTP API base.
const DefaultChinaHTTPAPIURL = "https://dashscope.aliyuncs.com/api/v1"

// rerankPath is the DashScope text-rerank endpoint, relative to the native
// API base. It serves qwen3-rerank, gte-rerank-v2, and qwen3-vl-rerank.
const rerankPath = "services/rerank/text-rerank/text-rerank"

// RerankModel is an ai.RerankModel backed by the DashScope text-rerank API.
// Create one with [NewRerankModel]; it is immutable and safe for concurrent
// use.
type RerankModel struct {
	model  string
	client *httpx.Client
	apiKey string
}

// Compile-time interface check.
var _ ai.RerankModel = (*RerankModel)(nil)

// RerankOptions carries DashScope rerank extensions. Put it in
// [ai.RerankRequest.ProviderOptions] under [ai.ProviderQwen].
type RerankOptions struct {
	// Instruct guides the ranking policy; it applies to qwen3-rerank and
	// qwen3-vl-rerank. DashScope recommends writing it in English.
	Instruct string
}

// NewRerankModel returns a rerank model bound to the given model ID (for
// example "qwen3-rerank" or "gte-rerank-v2"). It defaults to reading the
// DASHSCOPE_API_KEY environment variable and to [DefaultHTTPAPIURL].
func NewRerankModel(model string, opts ...Option) *RerankModel {
	o := newOptions(opts)

	return &RerankModel{
		model:  model,
		client: httpx.New(o.ToHTTPXConfig(DefaultHTTPAPIURL, "DASHSCOPE_API_KEY"), DefaultHTTPAPIURL),
		apiKey: o.ResolvedAPIKey("DASHSCOPE_API_KEY"),
	}
}

// Provider implements ai.RerankModel.
func (m *RerankModel) Provider() ai.Provider { return ai.ProviderQwen }

// ModelID implements ai.RerankModel.
func (m *RerankModel) ModelID() string { return m.model }

// Capabilities reports, best effort, what the bound model supports. It is a
// static hint, never a call gate.
func (m *RerankModel) Capabilities() ai.Capabilities {
	return ai.Capabilities{Reranking: true}
}

// Rerank implements ai.RerankModel.
func (m *RerankModel) Rerank(ctx context.Context, req ai.RerankRequest) (*ai.RerankResponse, error) {
	if err := validateRerankRequest(req); err != nil {
		return nil, err
	}

	body := rerankRequest{
		Model: m.model,
		Input: rerankInput{Query: req.Query, Documents: req.Documents},
	}

	opts := rerankOptions(req.ProviderOptions)
	if req.TopN != nil || req.ReturnDocuments || opts.Instruct != "" {
		body.Parameters = &rerankParameters{
			TopN:            req.TopN,
			ReturnDocuments: ai.Ptr(req.ReturnDocuments),
			Instruct:        opts.Instruct,
		}
	}

	var parsed rerankResponse

	raw, err := m.client.PostJSON(ctx, rerankPath, m.authHeaders(), body, &parsed, decodeError())
	if err != nil {
		return nil, fmt.Errorf("qwen: rerank: %w", err)
	}

	if err := responseError(parsed.Code, parsed.Message, raw); err != nil {
		return nil, fmt.Errorf("qwen: rerank: %w", err)
	}

	out := &ai.RerankResponse{
		Results: make([]ai.RerankResult, len(parsed.Output.Results)),
		Usage:   ai.Usage{InputTokens: parsed.Usage.TotalTokens},
		Raw:     raw,
	}

	for index, datum := range parsed.Output.Results {
		if datum.Index < 0 || datum.Index >= len(req.Documents) {
			return nil, fmt.Errorf(
				"qwen: rerank: result index %d out of range [0, %d)",
				datum.Index, len(req.Documents),
			)
		}

		document := ""
		if datum.Document != nil {
			document = datum.Document.Text
		}

		if document == "" && req.ReturnDocuments {
			document = req.Documents[datum.Index]
		}

		out.Results[index] = ai.RerankResult{
			Index:          datum.Index,
			RelevanceScore: datum.RelevanceScore,
			Document:       document,
		}
	}

	return out, nil
}

// authHeaders returns the per-request authentication headers.
func (m *RerankModel) authHeaders() http.Header {
	h := http.Header{}
	if m.apiKey != "" {
		h.Set("Authorization", "Bearer "+m.apiKey)
	}

	return h
}

func validateRerankRequest(req ai.RerankRequest) error {
	if req.Query == "" {
		return fmt.Errorf("qwen: rerank: query is required: %w", ai.ErrInvalidRequest)
	}

	if len(req.Documents) == 0 {
		return fmt.Errorf("qwen: rerank: documents cannot be empty: %w", ai.ErrInvalidRequest)
	}

	if req.TopN != nil && *req.TopN <= 0 {
		return fmt.Errorf("qwen: rerank: top_n must be greater than 0: %w", ai.ErrInvalidRequest)
	}

	return nil
}

func rerankOptions(values map[ai.Provider]any) RerankOptions {
	if values == nil {
		return RerankOptions{}
	}

	opts, _ := values[ai.ProviderQwen].(RerankOptions)

	return opts
}

// decodeError maps a DashScope error envelope onto an ai.Error.
func decodeError() httpx.ErrorDecoder {
	return func(status int, retryAfter time.Duration, body []byte) error {
		apiErr := ai.NewError(ai.ProviderQwen, status, string(body))
		apiErr.RetryAfter = retryAfter
		apiErr.Raw = body

		var envelope struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Error   *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := jsonx.Unmarshal(body, &envelope); err == nil {
			apiErr.Code = envelope.Code

			switch {
			case envelope.Message != "":
				apiErr.Message = envelope.Message
			case envelope.Error != nil && envelope.Error.Message != "":
				apiErr.Message = envelope.Error.Message
			}
		}

		return apiErr
	}
}

// responseError converts a non-empty DashScope code on a 2xx response into an
// ai.Error. DashScope reports some failures in the body rather than the status.
func responseError(code, message string, raw []byte) error {
	if code == "" {
		return nil
	}

	apiErr := ai.NewError(ai.ProviderQwen, 0, message)
	apiErr.Code = code
	apiErr.Raw = raw

	return apiErr.WithSentinel(dashScopeSentinel(code))
}

// dashScopeSentinel maps a DashScope error code to the closest ai sentinel.
func dashScopeSentinel(code string) error {
	switch code {
	case "Throttling", "Throttling.RateQuota", "Throttling.AllocationQuota":
		return ai.ErrRateLimited
	case "InvalidApiKey", "AuthenticationError":
		return ai.ErrAuth
	default:
		return ai.ErrInvalidRequest
	}
}

// Wire types. DashScope wraps inputs and tunables in nested objects.
type rerankRequest struct {
	Model      string            `json:"model"`
	Input      rerankInput       `json:"input"`
	Parameters *rerankParameters `json:"parameters,omitempty"`
}

type rerankInput struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type rerankParameters struct {
	TopN            *int   `json:"top_n,omitempty"`
	ReturnDocuments *bool  `json:"return_documents,omitempty"`
	Instruct        string `json:"instruct,omitempty"`
}

type rerankResponse struct {
	Output struct {
		Results []rerankResult `json:"results"`
	} `json:"output"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type rerankResult struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
	Document       *struct {
		Text string `json:"text"`
	} `json:"document"`
}
