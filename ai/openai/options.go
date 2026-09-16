package openai

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/apierr"
	"github.com/rsbin1178/pips/ai/internal/httpx"
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
	// DisableBuiltinTools suppresses all provider-executed tools (web search,
	// code interpreter, file search) for this request.
	DisableBuiltinTools bool
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

// imageReservedFields are the request keys the Images API types own. Image
// requests need their own list rather than the chat/Responses one, which also
// blocks documented image parameters. Keys whose typed field is optional and
// left unset stay deliverable through ExtraFields (background,
// response_format, ...); a key that collides with an already-set typed value
// still fails the merge.
//
// stream is reserved on purpose: only [ai.ImageStreamer] can consume an SSE
// response, so letting a unary call set it would produce an unparseable body.
var imageReservedFields = []string{
	"model", "prompt", "n", "size", "quality", "output_format",
	"output_compression", "partial_images", "input_fidelity",
	"moderation", "style", "user", "stream", "image", "image[]", "mask", "images",
}

// mergeImageExtraFields folds image ExtraFields into an already-encoded JSON
// request body. A no-op when there are no extras.
func mergeImageExtraFields(body any, extra map[string]any) (any, error) {
	return jsonx.MergeExtraFields(body, extra, imageReservedFields...)
}

// mergeImageFormFields renders image ExtraFields as multipart form fields,
// rejecting keys that a typed field already set. Multipart bodies are flat, so
// only scalar JSON values are accepted; nested values, reserved keys, typed
// collisions, and credential-shaped keys return [jsonx.ErrUnsafeExtension].
// Fields come back in a stable order.
func mergeImageFormFields(extra map[string]any, typed []httpx.FormField) ([]httpx.FormField, error) {
	if len(extra) == 0 {
		return nil, nil
	}

	// Reuse the JSON merge seam for the shared size, reserved-path, and
	// credential-shape checks, then render the flat scalar result.
	merged, err := jsonx.MergeExtraFields(map[string]any{}, extra, imageReservedFields...)
	if err != nil {
		return nil, err
	}

	values, ok := merged.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: image extra fields did not encode as an object", jsonx.ErrUnsafeExtension)
	}

	set := make(map[string]struct{}, len(typed))
	for _, field := range typed {
		set[field.Name] = struct{}{}
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	fields := make([]httpx.FormField, 0, len(keys))

	for _, key := range keys {
		if _, taken := set[key]; taken {
			return nil, fmt.Errorf("%w: multipart field %q collides with a typed field", jsonx.ErrUnsafeExtension, key)
		}

		value, err := imageFormValue(key, values[key])
		if err != nil {
			return nil, err
		}

		fields = append(fields, httpx.FormField{Name: key, Value: value})
	}

	return fields, nil
}

// imageFormValue renders one scalar ExtraFields value as a form field value.
func imageFormValue(key string, value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case bool:
		return strconv.FormatBool(typed), nil
	case json.Number:
		return typed.String(), nil
	default:
		return "", fmt.Errorf("%w: multipart field %q must be a scalar JSON value", jsonx.ErrUnsafeExtension, key)
	}
}

// decodeError builds the httpx.ErrorDecoder for this model's provider.
func (m *Model) decodeError(status int, retryAfter time.Duration, body []byte) error {
	return apierr.Decode(m.provider, status, retryAfter, body)
}
