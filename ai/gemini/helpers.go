package gemini

import (
	"maps"
	"strings"
	"time"

	"github.com/rsbin/pips/ai"
	"github.com/rsbin/pips/ai/internal/jsonx"
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

// errorBody is Gemini's error envelope.
type errorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func decodeError(status int, retryAfter time.Duration, body []byte) error {
	apiErr := ai.NewError(ai.ProviderGemini, status, string(body))
	apiErr.RetryAfter = retryAfter
	apiErr.Raw = body

	var parsed errorBody
	if err := jsonx.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		apiErr.Message = parsed.Error.Message
		apiErr.Type = parsed.Error.Status
	}

	return apiErr
}
