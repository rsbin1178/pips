package openai

import (
	"maps"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/internal/jsonx"
)

// RequestOptions is the openai entry for [ai.Request.ProviderOptions]:
//
//	req.ProviderOptions = map[ai.Provider]any{
//	    ai.ProviderOpenAI: openai.RequestOptions{ExtraFields: map[string]any{"seed": 7}},
//	}
type RequestOptions struct {
	// ExtraFields is merged into the top level of the outgoing JSON request
	// body after translation, overriding colliding keys. It is the escape
	// hatch for provider parameters the portable [ai.Request] does not model
	// (seed, logit_bias, user, parallel_tool_calls, service_tier, ...).
	ExtraFields map[string]any
}

// requestOptions extracts this provider's options from a request.
func requestOptions(req ai.Request) RequestOptions {
	if raw, ok := req.ProviderOptions[ai.ProviderOpenAI]; ok {
		if opts, ok := raw.(RequestOptions); ok {
			return opts
		}
	}

	return RequestOptions{}
}

// mergeExtraFields folds opts.ExtraFields into an already-encoded JSON
// object. A no-op when there are no extras.
func mergeExtraFields(body any, extra map[string]any) (any, error) {
	if len(extra) == 0 {
		return body, nil
	}

	encoded, err := jsonx.Marshal(body)
	if err != nil {
		return nil, err
	}

	var asMap map[string]any
	if err := jsonx.Unmarshal(encoded, &asMap); err != nil {
		return nil, err
	}

	maps.Copy(asMap, extra)

	return asMap, nil
}

// errorBody is OpenAI's error envelope, shared by both API surfaces.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"` // string or number depending on endpoint
	} `json:"error"`
}

// decodeError builds the httpx.ErrorDecoder for this adapter.
func decodeError(status int, retryAfter time.Duration, body []byte) error {
	apiErr := ai.NewError(ai.ProviderOpenAI, status, string(body))
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
