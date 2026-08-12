package gemini

import (
	"strings"
	"time"

	"github.com/rsbin1178/pips/ai"
	"github.com/rsbin1178/pips/ai/internal/jsonx"
)

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
		"contents", "systemInstruction", "cachedContent", "tools", "toolConfig",
		"generationConfig.temperature", "generationConfig.topP", "generationConfig.topK",
		"generationConfig.maxOutputTokens", "generationConfig.stopSequences",
		"generationConfig.responseMimeType", "generationConfig.responseSchema",
		"generationConfig.seed", "generationConfig.frequencyPenalty",
		"generationConfig.presencePenalty", "generationConfig.responseLogprobs",
		"generationConfig.logprobs", "generationConfig.responseModalities",
		"generationConfig.thinkingConfig",
	)
}

// errorBody is Gemini's error envelope.
type errorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func decodeError(provider ai.Provider) func(int, time.Duration, []byte) error {
	return func(status int, retryAfter time.Duration, body []byte) error {
		apiErr := ai.NewError(provider, status, string(body))
		apiErr.RetryAfter = retryAfter
		apiErr.Raw = body

		var parsed errorBody
		if err := jsonx.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
			apiErr.Message = parsed.Error.Message
			apiErr.Type = parsed.Error.Status
		}

		return apiErr
	}
}
