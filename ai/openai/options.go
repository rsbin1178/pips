package openai

import (
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// RequestOptions is the openai entry for [ai.Request.ProviderOptions]:
//
//	req.ProviderOptions = map[ai.Provider]any{
//	    ai.ProviderOpenAI: openai.RequestOptions{ExtraFields: map[string]any{"service_tier": "flex"}},
//	}
type RequestOptions struct {
	// MinP is a compatible-provider sampling cutoff not shared by the native
	// OpenAI, Anthropic, and Gemini protocols.
	MinP *float64
	// RepetitionPenalty is a compatible-provider sampling control.
	RepetitionPenalty *float64
	// ExtraFields is merged into the top level of the outgoing JSON request
	// body after translation using bounded recursive add-only semantics. It is
	// the escape hatch for non-reserved provider parameters the portable
	// [ai.Request] does not model (logit_bias, user, service_tier, ...).
	ExtraFields map[string]any
}

// requestOptions extracts this provider's options from a request.
func requestOptions(req ai.Request, provider ai.Provider) RequestOptions {
	if raw, ok := req.ProviderOptions[provider]; ok {
		if opts, ok := raw.(RequestOptions); ok {
			return opts
		}
	}

	if provider != ai.ProviderOpenAI {
		if raw, ok := req.ProviderOptions[ai.ProviderOpenAI]; ok {
			if opts, ok := raw.(RequestOptions); ok {
				return opts
			}
		}
	}

	return RequestOptions{}
}

// mergeExtraFields folds opts.ExtraFields into an already-encoded JSON
// object. A no-op when there are no extras.
func mergeExtraFields(body any, extra map[string]any) (any, error) {
	return jsonx.MergeExtraFields(body, extra,
		"model", "input", "messages", "instructions", "tools", "tool_choice",
		"stream", "stream_options", "response_format", "text", "temperature", "top_p",
		"top_k", "min_p", "max_tokens", "max_completion_tokens", "max_output_tokens",
		"stop", "reasoning", "reasoning_effort", "thinking", "seed",
		"frequency_penalty", "presence_penalty", "repetition_penalty", "logprobs",
		"top_logprobs", "include", "store", "background", "previous_response_id",
		"conversation", "prompt", "n", "size", "quality",
	)
}

// errorBody is OpenAI's error envelope, shared by both API surfaces.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"` // string or number depending on endpoint
	} `json:"error"`
}

// decodeError builds the httpx.ErrorDecoder for this model's provider.
func (m *Model) decodeError(status int, retryAfter time.Duration, body []byte) error {
	apiErr := ai.NewError(m.provider, status, string(body))
	apiErr.RetryAfter = retryAfter
	apiErr.Raw = body

	var parsed errorBody
	if err := jsonx.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		apiErr.Message = parsed.Error.Message
		apiErr.Type = parsed.Error.Type

		if code, ok := parsed.Error.Code.(string); ok {
			apiErr.Code = code
		}
	}

	return apiErr
}
