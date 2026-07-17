package gemini

import (
	"context"
	"fmt"

	"github.com/rsbin/pips/ai"
)

type countTokensResponse struct {
	TotalTokens int `json:"totalTokens"`
}

// CountTokens implements ai.TokenCounter against the :countTokens endpoint.
// It returns the request's input-token count without running inference. The
// endpoint accepts the same body shape as generateContent.
func (m *Model) CountTokens(ctx context.Context, req ai.Request) (int, error) {
	body, err := requestFrom(req)
	if err != nil {
		return 0, err
	}

	var parsed countTokensResponse
	if _, err := m.client.PostJSON(ctx, m.methodPath("countTokens"), m.authHeaders(), body, &parsed, decodeError); err != nil {
		return 0, fmt.Errorf("gemini: countTokens: %w", err)
	}

	return parsed.TotalTokens, nil
}
