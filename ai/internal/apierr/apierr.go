// Package apierr decodes the OpenAI-shaped error envelope every
// OpenAI-dialect provider returns into an [ai.Error]. Adapters call it from
// their httpx.ErrorDecoder instead of each carrying its own copy of the
// envelope parsing.
package apierr

import (
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// envelope is the shared error body: {"error":{"message","type","code"}}. code
// arrives as a string on most endpoints and as a number on a few; only a
// string is a usable portable code.
type envelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Decode turns an OpenAI-shaped error envelope into an *ai.Error. The message
// defaults to the raw body when the envelope is absent or unparseable,
// RetryAfter and Raw are preserved, and the wrapped sentinel comes from
// [ai.ClassifyStatus].
func Decode(provider ai.Provider, status int, retryAfter time.Duration, body []byte) error {
	apiErr := ai.NewError(provider, status, string(body))
	apiErr.RetryAfter = retryAfter
	apiErr.Raw = body

	var parsed envelope
	if err := jsonx.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		apiErr.Message = parsed.Error.Message
		apiErr.Type = parsed.Error.Type

		if code, ok := parsed.Error.Code.(string); ok {
			apiErr.Code = code
		}
	}

	return apiErr
}
