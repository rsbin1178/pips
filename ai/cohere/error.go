package cohere

import (
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

// decodeError returns an httpx.ErrorDecoder for the given provider. It handles
// OpenAI-shaped error envelopes, Cohere-shaped error bodies {"message": "..."},
// and FastAPI/TEI {"detail": "..."} errors, mapping the HTTP status code
// to canonical ai sentinels.
func decodeError(provider ai.Provider) func(status int, retryAfter time.Duration, body []byte) error {
	return func(status int, retryAfter time.Duration, body []byte) error {
		apiErr := ai.NewError(provider, status, string(body))
		apiErr.RetryAfter = retryAfter
		apiErr.Raw = body

		// 1. Try OpenAI-shaped envelope: {"error":{"message","type","code"}}
		var openAIEnv struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
				Code    any    `json:"code"`
			} `json:"error"`
		}
		if err := jsonx.Unmarshal(body, &openAIEnv); err == nil && openAIEnv.Error.Message != "" {
			apiErr.Message = openAIEnv.Error.Message
			apiErr.Type = openAIEnv.Error.Type

			if code, ok := openAIEnv.Error.Code.(string); ok {
				apiErr.Code = code
			}

			return apiErr
		}

		// 2. Try Cohere/FastAPI shape: {"message": "..."} or {"detail": "..."}
		var cohereEnv struct {
			Message string `json:"message"`
			Detail  any    `json:"detail"`
		}
		if err := jsonx.Unmarshal(body, &cohereEnv); err == nil {
			if cohereEnv.Message != "" {
				apiErr.Message = cohereEnv.Message
				return apiErr
			}

			if s, ok := cohereEnv.Detail.(string); ok && s != "" {
				apiErr.Message = s
				return apiErr
			}
		}

		return apiErr
	}
}
