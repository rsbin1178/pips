package anthropic

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func textOf(parts []ai.Part) string {
	var b strings.Builder

	for _, part := range parts {
		if text, ok := part.(ai.TextPart); ok {
			b.WriteString(text.Text)
		}
	}

	return b.String()
}

// mergeExtraFields folds provider-option extras into an already-structured
// request body. A no-op when there are no extras.
func mergeExtraFields(body any, extra map[string]any) (any, error) {
	return jsonx.MergeExtraFields(body, extra,
		"model", "messages", "system", "max_tokens", "temperature", "top_p", "top_k",
		"stop_sequences", "stream", "tools", "tool_choice", "output_config", "thinking",
		"cache_control",
	)
}

// errorBody is Anthropic's error envelope.
type errorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// decodeError builds the httpx.ErrorDecoder for this adapter. Anthropic's 529
// "overloaded_error" is mapped to the retryable overload class by
// [ai.ClassifyStatus].
func decodeError(provider ai.Provider) func(int, time.Duration, []byte) error {
	return func(status int, retryAfter time.Duration, body []byte) error {
		apiErr := ai.NewError(provider, status, string(body))
		apiErr.RetryAfter = retryAfter
		apiErr.Raw = body

		var parsed errorBody
		if err := jsonx.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
			apiErr.Message = parsed.Error.Message
			apiErr.Type = parsed.Error.Type
		}

		return apiErr
	}
}
